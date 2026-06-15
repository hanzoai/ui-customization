package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/hanzoai/base/core"
	luxlog "github.com/luxfi/log"
	zaplib "github.com/luxfi/zap"
	zcap "github.com/zap-proto/go/cap"

	gen "github.com/hanzoai/ui-customization/gen"
)

// PermSessionRead is the CapKindIAMSession low bit gating read operations
// (zap-spec/capabilities_kinds.md, CapKindIAMSession 0x01 → 1<<0).
const PermSessionRead uint64 = 1 << 0

// Server implements the UiCustomization ZAP capability-RPC interface on top of
// a Base app. It is the Go peer of console's UiCustomizationTarget: one method
// per tRPC procedure, each gated on the caller's capability via the single
// chokepoint requirePermission, then reading from the Base collection.
type Server struct {
	app        core.App
	logger     luxlog.Logger
	defaultOrg string

	// verifier validates capability buffers. Wired to ed25519 (the bootstrap
	// scheme); a PQ deployment swaps in an ML-DSA-65 SchemeVerify + the IAM
	// pubkey registry for IssuerKey. See SPEC.md §2.3.
	verifier zcap.Verifier

	// promises is the server-side pipelining table: a call may carry
	// PromiseID, and a later call may Target it. Promises are FUTURES — a
	// dependent call that arrives before its target resolves WAITS on the
	// target (it is not rejected), then dispatches against the resolved
	// answer. This is Cap'n Proto promise pipelining: the dependent call is
	// admitted to the server before the target's answer exists, and the
	// round trip is elided. Entries are short-lived (one connection turn).
	mu       sync.Mutex
	promises map[uint32]*promiseSlot
}

// promiseSlot is a future for a pipelined call's answer. done is closed when
// the slot resolves; org is then readable. For UiCustomization the only
// pipelined value is the authenticated org. resolvedAt drives reaping.
type promiseSlot struct {
	done       chan struct{}
	org        string
	resolvedAt time.Time
}

// promiseWaitTimeout bounds how long a dependent call waits for its target to
// resolve before failing. Generous relative to a same-connection turn.
const promiseWaitTimeout = 5 * time.Second

// getOrCreate returns the slot for id, creating an unresolved one if absent.
// Both the resolver (the call that owns the PromiseID) and the waiter (a
// dependent call targeting it) go through here, so whichever races in first
// creates the shared slot.
func (s *Server) getOrCreate(id uint32) *promiseSlot {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reapLocked()
	slot, ok := s.promises[id]
	if !ok {
		slot = &promiseSlot{done: make(chan struct{})}
		s.promises[id] = slot
	}
	return slot
}

// reapLocked drops promise slots that resolved more than promiseWaitTimeout
// ago — by then any dependent call has either consumed them or timed out, so
// they are garbage. Bounds the table to in-flight + recently-resolved pipelines.
// Caller must hold s.mu.
func (s *Server) reapLocked() {
	cutoff := time.Now().Add(-promiseWaitTimeout)
	for id, slot := range s.promises {
		if !slot.resolvedAt.IsZero() && slot.resolvedAt.Before(cutoff) {
			delete(s.promises, id)
		}
	}
}

// resolve fills a promise slot with its answer and wakes any waiters. Safe to
// call once per slot; a double-resolve (would close a closed channel) is
// guarded by checking whether org is already set under the lock.
func (s *Server) resolve(id uint32, org string) {
	slot := s.getOrCreate(id)
	s.mu.Lock()
	select {
	case <-slot.done:
		// already resolved — leave as-is
	default:
		slot.org = org
		slot.resolvedAt = time.Now()
		close(slot.done)
	}
	s.mu.Unlock()
}

// await blocks until the target promise resolves or the timeout elapses,
// returning the resolved org. A nil-id target never happens (callers guard
// with Target != NoTarget).
func (s *Server) await(target uint32) (string, bool) {
	slot := s.getOrCreate(target)
	select {
	case <-slot.done:
		s.mu.Lock()
		org := slot.org
		s.mu.Unlock()
		return org, true
	case <-time.After(promiseWaitTimeout):
		return "", false
	}
}

// NewServer builds a UiCustomization server. verifier supplies the capability
// trust anchor; pass a Verifier whose IssuerKey resolves your IAM issuer key.
func NewServer(app core.App, logger luxlog.Logger, defaultOrg string, verifier zcap.Verifier) *Server {
	return &Server{
		app:        app,
		logger:     logger,
		defaultOrg: defaultOrg,
		verifier:   verifier,
		promises:   make(map[uint32]*promiseSlot),
	}
}

// Register wires the server's handler onto a luxfi/zap node at this service's
// message-type slot. Called from main once the node is constructed.
func (s *Server) Register(node *zaplib.Node) {
	node.Handle(MsgTypeRouterBase, s.handle)
}

// handle is the ZAP dispatch entrypoint: decode envelope → authorize → route.
func (s *Server) handle(ctx context.Context, from string, msg *zaplib.Message) (*zaplib.Message, error) {
	req := parseRequest(msg)

	org, status, errMsg := s.authorize(req)
	if status != StatusOK {
		s.logger.Debug("uic: auth rejected", "from", from, "method", req.Method, "status", status, "err", errMsg)
		return buildResponse(status, req.PromiseID, errorBody(errMsg))
	}

	// Resolve this call's promise (the authenticated org) so any dependent
	// call WAITING on it can proceed. authorize() already awaited our own
	// target if we had one, so by here `org` is the fully-resolved scope.
	if req.PromiseID != NoTarget {
		s.resolve(req.PromiseID, org)
	}

	switch req.Method {
	case MethodGet:
		return s.handleGet(req, org)
	case MethodGetModules:
		return s.handleGetModules(req, org)
	default:
		return buildResponse(StatusBadRequest, req.PromiseID, errorBody(fmt.Sprintf("unknown method %d", req.Method)))
	}
}

// authorize resolves the call's effective org and enforces the capability.
// Returns (org, StatusOK, "") on success; otherwise an error status + message.
//
// Pipelining: if the call Targets an earlier promise, its org is inherited
// from that promise's resolved answer (the dependent call need not re-present
// scope). The capability is STILL verified on every call — pipelining elides
// round trips, never authorization.
func (s *Server) authorize(req Call) (org string, status uint32, errMsg string) {
	c, err := zcap.Wrap(req.Cap)
	if err != nil {
		return "", StatusBadRequest, "malformed capability: " + err.Error()
	}

	// Kind gate: these methods are defined on a CapKindIAMSession cap.
	if c.Kind() != uint32(zcap.KindIAMSession) {
		return "", StatusForbidden, "capability is not a CapKindIAMSession"
	}

	// Permission gate — the single chokepoint. Mirrors console requirePermission.
	if c.Permissions()&PermSessionRead == 0 {
		return "", StatusForbidden, "capability lacks PermSessionRead"
	}

	// Cryptographic verification. The full chain check (signature, expiry,
	// revocation, chain links) lives in verifier.Verify. We run it whenever an
	// issuer registry is wired; with no registry (bootstrap/tests) we skip the
	// signature step but STILL enforce Kind + Permissions above.
	//
	// TODO(SPEC.md §2.3): also bind the cap to the live session via the
	// out-of-band holderSig over a server nonce, and walk the parent chain
	// with verifier.VerifyChain once the IAM pubkey registry is wired here.
	if s.verifier.IssuerKey != nil {
		if err := s.verifier.Verify(c, time.Now().Unix()); err != nil {
			return "", StatusUnauthorized, "capability verify failed: " + err.Error()
		}
	}

	// Effective org: inherited from a targeted promise, else derived from the
	// cap's holder, else the service default. (Holder→org mapping is an IAM
	// lookup; until that's wired, scope to the default org.)
	//
	// Pipelining: if this call targets an earlier promise, read that promise's
	// resolved answer. The luxfi/zap transport processes a connection's frames
	// strictly FIFO (one handler at a time — see node.go dispatchLoop), and the
	// client ships the target call BEFORE the dependent call (see Client.barrier
	// in client.go). So by the time a dependent call is dispatched, its target
	// has already passed through handle() and resolved. await therefore returns
	// immediately; the timeout exists only as a safety net against a
	// misbehaving client that ships out of order.
	if req.Target != NoTarget {
		org, ok := s.await(req.Target)
		if !ok {
			return "", StatusBadRequest, fmt.Sprintf("pipelined target %d did not resolve in time", req.Target)
		}
		return org, StatusOK, ""
	}
	return s.defaultOrg, StatusOK, ""
}

// handleGet returns the full customization config for the org (present=false
// when the org is not entitled). Was: uiCustomizationRouter.get.
func (s *Server) handleGet(req Call, org string) (*zaplib.Message, error) {
	rec, err := s.orgRecord(org)
	if err != nil {
		// No row yet → model as "not present" (matches tRPC null-when-absent).
		body := gen.NewUiCustomizationConfig(gen.UiCustomizationConfigInput{Present: false})
		return buildResponse(StatusOK, req.PromiseID, body)
	}

	in := gen.UiCustomizationConfigInput{Present: rec.GetBool(fPresent)}
	if in.Present {
		in.Hostname = rec.GetString(fHostname)
		in.DocumentationHref = rec.GetString(fDocumentationHref)
		in.SupportHref = rec.GetString(fSupportHref)
		in.FeedbackHref = rec.GetString(fFeedbackHref)
		in.LogoLightModeHref = rec.GetString(fLogoLightModeHref)
		in.LogoDarkModeHref = rec.GetString(fLogoDarkModeHref)
		in.DefaultModelAdapter = rec.GetString(fDefaultModelAdapter)
		in.DefaultBaseUrlOpenAI = rec.GetString(fDefaultBaseUrlOpenAI)
		in.DefaultBaseUrlAnthropic = rec.GetString(fDefaultBaseUrlAnthropic)
		in.DefaultBaseUrlAzure = rec.GetString(fDefaultBaseUrlAzure)
		in.VisibleModules = textList(modulesOf(rec))
	}

	body := gen.NewUiCustomizationConfig(in)
	return buildResponse(StatusOK, req.PromiseID, body)
}

// handleGetModules returns just the visible product-module list for the org.
func (s *Server) handleGetModules(req Call, org string) (*zaplib.Message, error) {
	var mods []string
	if rec, err := s.orgRecord(org); err == nil {
		mods = modulesOf(rec)
	} else {
		mods = visibleModulesFromEnv()
	}
	body := gen.NewVisibleModules(gen.VisibleModulesInput{Modules: textList(mods)})
	return buildResponse(StatusOK, req.PromiseID, body)
}

// orgRecord finds the customization row for an org.
func (s *Server) orgRecord(org string) (*core.Record, error) {
	col, err := s.app.FindCollectionByNameOrId(CollectionName)
	if err != nil {
		return nil, err
	}
	return s.app.FindFirstRecordByFilter(col, "org = {:org}", map[string]any{"org": org})
}

// modulesOf reads the visibleModules JSON array off a record, falling back to
// the env-derived list when the column is empty/unset.
func modulesOf(rec *core.Record) []string {
	raw := rec.GetString(fVisibleModules)
	if raw == "" || raw == "null" {
		return visibleModulesFromEnv()
	}
	var mods []string
	if err := json.Unmarshal([]byte(raw), &mods); err != nil || len(mods) == 0 {
		return visibleModulesFromEnv()
	}
	return mods
}

// textList converts a []string into the [][]byte the generated list<text>
// builders consume (each element is the UTF-8 bytes of the string).
func textList(ss []string) [][]byte {
	out := make([][]byte, len(ss))
	for i, s := range ss {
		out[i] = []byte(s)
	}
	return out
}
