package server

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	zaplib "github.com/luxfi/zap"
	zcap "github.com/zap-proto/go/cap"

	gen "github.com/hanzoai/ui-customization/gen"
)

// Client is a UiCustomization ZAP capability-RPC client. It is what console's
// bridge substitutes for the in-process Server: a thin typed wrapper over a
// luxfi/zap connection that ships the verified capability with every call.
//
// Holds the caller's opaque capability buffer and the transport node. Construct
// with Dial, then call Get / GetModules / Pipeline.
type Client struct {
	node   *zaplib.Node
	peerID string
	capBuf []byte

	promiseSeq uint32 // monotonic PromiseID allocator

	// sendLog records, in order, every call shipped — used by the smoke test
	// to prove a pipelined dependent call ships before the first answer
	// resolves. Optional; nil disables instrumentation.
	logMu   sync.Mutex
	sendLog *[]SendEvent
}

// SendEvent is one entry in the instrumentation log: a call left the client
// (Sent) or its answer arrived (Recv), with a monotonic sequence number.
type SendEvent struct {
	Seq       uint64
	Kind      string // "send" or "recv"
	Method    uint32
	PromiseID uint32
	Target    uint32
	At        time.Time
}

var sendEventSeq uint64

// pipelineIDSeq hands out process-unique promise ids for pipelined call groups.
// Starts high (above any per-client counter) so a pipeline id never collides
// with a plain per-call PromiseID. Promise ids are a cross-connection
// correlation namespace, so global uniqueness is required.
var pipelineIDSeq uint32 = 1 << 20

func nextPipelineID() uint32 { return atomic.AddUint32(&pipelineIDSeq, 1) }

// Dial constructs a Client over an already-started local node, connecting to
// the service at addr (e.g. "127.0.0.1:9999"). capBuf is the caller's opaque
// capability buffer (a zcap.Cap.Bytes()). peerID is the service's ZAP node id.
func Dial(node *zaplib.Node, addr, peerID string, capBuf []byte) (*Client, error) {
	if err := node.ConnectDirect(addr); err != nil {
		return nil, fmt.Errorf("uic client: connect %s: %w", addr, err)
	}
	return &Client{node: node, peerID: peerID, capBuf: capBuf}, nil
}

// WithSendLog attaches an instrumentation slice the client appends send/recv
// events to. Returns the client for chaining.
func (c *Client) WithSendLog(log *[]SendEvent) *Client {
	c.sendLog = log
	return c
}

func (c *Client) record(kind string, method, promiseID, target uint32) {
	if c.sendLog == nil {
		return
	}
	c.logMu.Lock()
	*c.sendLog = append(*c.sendLog, SendEvent{
		Seq:       atomic.AddUint64(&sendEventSeq, 1),
		Kind:      kind,
		Method:    method,
		PromiseID: promiseID,
		Target:    target,
		At:        time.Now(),
	})
	c.logMu.Unlock()
}

func (c *Client) nextPromise() uint32 {
	return atomic.AddUint32(&c.promiseSeq, 1)
}

// call ships one request and blocks for its correlated response.
func (c *Client) call(ctx context.Context, method, promiseID, target uint32) (Response, error) {
	msg, err := buildRequest(Call{
		Method:    method,
		PromiseID: promiseID,
		Target:    target,
		Cap:       c.capBuf,
	})
	if err != nil {
		return Response{}, err
	}
	c.record("send", method, promiseID, target)
	resp, err := c.node.Call(ctx, c.peerID, msg)
	if err != nil {
		return Response{}, err
	}
	c.record("recv", method, promiseID, target)
	return parseResponse(resp), nil
}

// Get calls UiCustomization.get and decodes the typed config view.
func (c *Client) Get(ctx context.Context) (gen.UiCustomizationConfig, error) {
	resp, err := c.call(ctx, MethodGet, c.nextPromise(), NoTarget)
	if err != nil {
		return gen.UiCustomizationConfig{}, err
	}
	if resp.Status != StatusOK {
		return gen.UiCustomizationConfig{}, fmt.Errorf("get: status %d: %s", resp.Status, resp.Body)
	}
	return gen.WrapUiCustomizationConfig(resp.Body)
}

// GetModules calls UiCustomization.getModules and decodes the module list.
func (c *Client) GetModules(ctx context.Context) (gen.VisibleModules, error) {
	resp, err := c.call(ctx, MethodGetModules, c.nextPromise(), NoTarget)
	if err != nil {
		return gen.VisibleModules{}, err
	}
	if resp.Status != StatusOK {
		return gen.VisibleModules{}, fmt.Errorf("getModules: status %d: %s", resp.Status, resp.Body)
	}
	return gen.WrapVisibleModules(resp.Body)
}

// Pipeline issues get @0 and getModules @1 such that getModules is in flight at
// the server BEFORE get's answer resolves — Cap'n Proto promise pipelining.
// getModules Targets get's PromiseID; the server resolves get's answer (the
// authenticated org) and only then dispatches the promised getModules against
// it — no intermediate round trip to the client.
//
// Transport note (load-bearing): the luxfi/zap transport processes a single
// connection's frames strictly FIFO — one handler runs to completion (response
// written) before the next frame is read (see node.go dispatchLoop). So genuine
// concurrent in-flight calls require the two calls to travel on SEPARATE
// connections, where the server runs two dispatch loops concurrently and its
// promise table (Server.await/resolve) coordinates them. `dep` is therefore a
// SECOND client connection over which the dependent getModules call is shipped;
// pass a Client dialed on its own *zaplib.Node. When dep == c (one connection),
// this still works but degrades to sequential (no overlap) because of FIFO.
//
// Proof (on the shared send log both clients append to): getModules' send
// precedes get's recv — the dependent call was on the wire before the call it
// depends on had answered. The server's await() blocks getModules until get
// resolves the promise, which is the pipelining join.
func (c *Client) Pipeline(ctx context.Context, dep *Client) (gen.UiCustomizationConfig, gen.VisibleModules, error) {
	// Promise IDs are a correlation namespace shared across the pipeline group
	// (both connections), so they must be globally distinct — the server keys
	// its promise table by id alone, since a dependent call resolves a target
	// shipped on a DIFFERENT connection. nextPipelineID hands out process-unique
	// ids; get and getModules get two distinct ones, and getModules Targets get's.
	getPromise := nextPipelineID()
	modPromise := nextPipelineID()

	var (
		cfg    gen.UiCustomizationConfig
		mods   gen.VisibleModules
		getErr error
		modErr error
		wg     sync.WaitGroup
	)
	// barrier releases the dependent send only after get's send is committed to
	// its wire, so the server resolves get's promise id before (or concurrently
	// with) the dependent call's await — never after a spurious timeout.
	barrier := make(chan struct{})
	wg.Add(2)

	// Call #1: get @0 on connection c — the promise the dependent call targets.
	go func() {
		defer wg.Done()
		close(barrier)
		resp, err := c.call(ctx, MethodGet, getPromise, NoTarget)
		if err != nil {
			getErr = err
			return
		}
		if resp.Status != StatusOK {
			getErr = fmt.Errorf("get: status %d: %s", resp.Status, resp.Body)
			return
		}
		cfg, getErr = gen.WrapUiCustomizationConfig(resp.Body)
	}()

	// Call #2: getModules @1 on connection dep, pipelined off get's promise.
	// Shipped without awaiting get's answer; the server holds it until get
	// resolves the promise.
	go func() {
		defer wg.Done()
		<-barrier
		resp, err := dep.call(ctx, MethodGetModules, modPromise, getPromise)
		if err != nil {
			modErr = err
			return
		}
		if resp.Status != StatusOK {
			modErr = fmt.Errorf("getModules: status %d: %s", resp.Status, resp.Body)
			return
		}
		mods, modErr = gen.WrapVisibleModules(resp.Body)
	}()

	wg.Wait()
	if getErr != nil {
		return gen.UiCustomizationConfig{}, gen.VisibleModules{}, getErr
	}
	if modErr != nil {
		return gen.UiCustomizationConfig{}, gen.VisibleModules{}, modErr
	}
	return cfg, mods, nil
}

// SyntheticCap mints an in-memory CapKindIAMSession capability for tests and
// bootstrap: ed25519-signed (the SPEC bootstrap scheme), holding PermSessionRead.
// The signature is real (ed25519); production wires an IAM-issued cap instead.
// Returns the opaque buffer to pass to Dial.
func SyntheticCap(perms uint64) ([]byte, error) {
	signer, err := zcap.NewEd25519Signer()
	if err != nil {
		return nil, err
	}
	c, err := zcap.Issue(zcap.Issuance{
		Kind:        uint32(zcap.KindIAMSession),
		Holder:      signer.Public(),
		Permissions: perms,
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	}, signer)
	if err != nil {
		return nil, err
	}
	return c.Bytes(), nil
}
