package server_test

import (
	"context"
	"os"
	"testing"
	"time"

	basetests "github.com/hanzoai/base/tests"
	luxlog "github.com/luxfi/log"
	zaplib "github.com/luxfi/zap"
	zcap "github.com/zap-proto/go/cap"

	"github.com/hanzoai/ui-customization/server"
)

const testOrg = "test-org"

// newService spins up a Base test app with the ui_customization collection
// provisioned + seeded, a ZAP router node listening, and returns the node id,
// listen address, and a cleanup func.
func newService(t *testing.T, port int) (addr, peerID string, cleanup func()) {
	t.Helper()

	app, err := basetests.NewTestApp()
	if err != nil {
		t.Fatalf("new test app: %v", err)
	}
	if err := server.EnsureCollection(app); err != nil {
		t.Fatalf("ensure collection: %v", err)
	}
	if err := server.SeedDefaultRow(app, testOrg); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	logger := luxlog.New("component", "uic-test")
	node := zaplib.NewNode(zaplib.NodeConfig{
		NodeID:      "uic-test-srv",
		Port:        port,
		NoDiscovery: true,
	})
	srv := server.NewServer(app, logger, testOrg, zcap.Verifier{})
	srv.Register(node)
	if err := node.Start(); err != nil {
		t.Fatalf("node start: %v", err)
	}

	return "127.0.0.1:" + itoa(port), "uic-test-srv", func() {
		node.Stop()
		app.Cleanup()
	}
}

// newClient dials the service with a synthetic CapKindIAMSession cap holding
// the given permissions.
func newClient(t *testing.T, addr, peerID string, perms uint64, port int) (*server.Client, func()) {
	t.Helper()
	capBuf, err := server.SyntheticCap(perms)
	if err != nil {
		t.Fatalf("synthetic cap: %v", err)
	}
	cli := zaplib.NewNode(zaplib.NodeConfig{
		NodeID:      "uic-test-cli-" + itoa(port), // unique per client connection
		Port:        port,
		NoDiscovery: true,
	})
	if err := cli.Start(); err != nil {
		t.Fatalf("client node start: %v", err)
	}
	c, err := server.Dial(cli, addr, peerID, capBuf)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// Let the handshake settle.
	time.Sleep(150 * time.Millisecond)
	return c, func() { cli.Stop() }
}

func TestGetReturnsSeededConfig(t *testing.T) {
	os.Setenv("HANZO_UI_PRESENT", "true")
	os.Setenv("HANZO_UI_API_HOST", "ui.example.test")
	defer os.Unsetenv("HANZO_UI_PRESENT")
	defer os.Unsetenv("HANZO_UI_API_HOST")

	addr, peerID, stop := newService(t, 19610)
	defer stop()
	cli, stopCli := newClient(t, addr, peerID, server.PermSessionRead, 19611)
	defer stopCli()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg, err := cli.Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !cfg.Present() {
		t.Fatalf("expected present=true for seeded entitled org")
	}
	if got := cfg.Hostname(); got != "ui.example.test" {
		t.Fatalf("hostname = %q, want ui.example.test", got)
	}
	if cfg.VisibleModules().Len() == 0 {
		t.Fatalf("expected non-empty visibleModules")
	}
	t.Logf("Get OK: present=%v hostname=%q modules=%d",
		cfg.Present(), cfg.Hostname(), cfg.VisibleModules().Len())
}

func TestGetModulesReturnsList(t *testing.T) {
	addr, peerID, stop := newService(t, 19612)
	defer stop()
	cli, stopCli := newClient(t, addr, peerID, server.PermSessionRead, 19613)
	defer stopCli()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	mods, err := cli.GetModules(ctx)
	if err != nil {
		t.Fatalf("GetModules: %v", err)
	}
	n := mods.Modules().Len()
	if n == 0 {
		t.Fatalf("expected non-empty module list")
	}
	// Spot-check the first element decodes to a known module.
	first := string(mods.Modules().BytesAt(0))
	t.Logf("GetModules OK: %d modules, first=%q", n, first)
	if first == "" {
		t.Fatalf("first module decoded empty")
	}
}

// TestPermissionDenied proves the bitmask chokepoint: a cap WITHOUT
// PermSessionRead is rejected before any data is read.
func TestPermissionDenied(t *testing.T) {
	addr, peerID, stop := newService(t, 19614)
	defer stop()
	// perms = 0 → no PermSessionRead bit.
	cli, stopCli := newClient(t, addr, peerID, 0, 19615)
	defer stopCli()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := cli.Get(ctx); err == nil {
		t.Fatalf("expected Get to be rejected without PermSessionRead")
	} else {
		t.Logf("correctly denied: %v", err)
	}
}

// TestPipeliningShipsSecondBeforeFirstAnswer is the load-bearing pipelining
// proof: the second call (getModules, pipelined off get's promise) must ship
// before the first call's answer resolves. We assert on the instrumented send
// log that BOTH "send" events precede the FIRST "recv" event.
func TestPipeliningShipsSecondBeforeFirstAnswer(t *testing.T) {
	addr, peerID, stop := newService(t, 19616)
	defer stop()

	// Two client connections: get travels on cli, the pipelined getModules on
	// dep. Independent connections let the server process both concurrently —
	// genuine in-flight pipelining (one connection is strictly FIFO). Both
	// clients append to the SAME send log so ordering is observed across them.
	var log []server.SendEvent
	cli, stopCli := newClient(t, addr, peerID, server.PermSessionRead, 19617)
	defer stopCli()
	cli.WithSendLog(&log)
	dep, stopDep := newClient(t, addr, peerID, server.PermSessionRead, 19618)
	defer stopDep()
	dep.WithSendLog(&log)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg, mods, err := cli.Pipeline(ctx, dep)
	if err != nil {
		t.Fatalf("Pipeline: %v", err)
	}
	_ = cfg
	if mods.Modules().Len() == 0 {
		t.Fatalf("expected modules from pipelined call")
	}

	// Analyze the send log: find the index of the first "recv" and assert that
	// two "send" events occurred before it.
	firstRecv := -1
	sendsBeforeFirstRecv := 0
	for i, e := range log {
		if e.Kind == "recv" {
			firstRecv = i
			break
		}
		if e.Kind == "send" {
			sendsBeforeFirstRecv++
		}
	}
	t.Logf("send log: %s", formatLog(log))
	if firstRecv == -1 {
		t.Fatalf("no recv events recorded")
	}
	if sendsBeforeFirstRecv < 2 {
		t.Fatalf("pipelining violated: only %d sends before first answer resolved; want 2",
			sendsBeforeFirstRecv)
	}
	t.Logf("PIPELINING PROVEN: %d calls shipped before the first answer resolved",
		sendsBeforeFirstRecv)
}

// --- tiny local helpers (avoid pulling strconv/fmt into the hot path) -------

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func formatLog(log []server.SendEvent) string {
	out := ""
	for _, e := range log {
		method := "get"
		if e.Method == server.MethodGetModules {
			method = "getModules"
		}
		out += e.Kind + "(" + method + ",p=" + itoa(int(e.PromiseID)) + ",t=" + itoa(int(e.Target)) + ") "
	}
	return out
}
