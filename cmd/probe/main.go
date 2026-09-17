// Command probe exercises a LIVE ui-customization service over ZAP — the
// out-of-process smoke test. It mints a synthetic CapKindIAMSession capability
// (PermSessionRead), connects to the service at --addr, and calls Get,
// GetModules, and a pipelined Get+GetModules, printing each result. Exit 0 on
// success, non-zero on any failure.
//
//	go run ./cmd/probe --addr 127.0.0.1:9999 --peer ui-customization
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	zaplib "github.com/luxfi/zap"

	"github.com/hanzoai/ui-customization/server"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9999", "service ZAP address")
	peer := flag.String("peer", "ui-customization", "service ZAP node id")
	flag.Parse()

	if err := run(*addr, *peer); err != nil {
		fmt.Fprintln(os.Stderr, "PROBE FAILED:", err)
		os.Exit(1)
	}
	fmt.Println("PROBE OK")
}

func run(addr, peer string) error {
	capBuf, err := server.SyntheticCap(server.PermSessionRead)
	if err != nil {
		return fmt.Errorf("mint cap: %w", err)
	}

	// get + pipelined-getModules need two connections (FIFO transport). Each
	// gets a UNIQUE node id — the server rejects duplicate peer ids (EOF on
	// handshake), so id collisions silently drop the second connection.
	clientN := 0
	mkClient := func(log *[]server.SendEvent) (*server.Client, func(), error) {
		clientN++
		node := zaplib.NewNode(zaplib.NodeConfig{
			NodeID:      fmt.Sprintf("uic-probe-%d-%d", os.Getpid(), clientN),
			Port:        0, // OS-assigned ephemeral port
			NoDiscovery: true,
		})
		if err := node.Start(); err != nil {
			return nil, nil, err
		}
		c, err := server.Dial(node, addr, peer, capBuf)
		if err != nil {
			node.Stop()
			return nil, nil, err
		}
		if log != nil {
			c.WithSendLog(log)
		}
		return c, node.Stop, nil
	}

	var log []server.SendEvent
	cli, stop1, err := mkClient(&log)
	if err != nil {
		return err
	}
	defer stop1()
	dep, stop2, err := mkClient(&log)
	if err != nil {
		return err
	}
	defer stop2()

	time.Sleep(200 * time.Millisecond) // handshake settle

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 1. Get
	cfg, err := cli.Get(ctx)
	if err != nil {
		return fmt.Errorf("Get: %w", err)
	}
	fmt.Printf("Get: present=%v hostname=%q modules=%d\n",
		cfg.Present(), cfg.Hostname(), cfg.VisibleModules().Len())

	// 2. GetModules
	mods, err := cli.GetModules(ctx)
	if err != nil {
		return fmt.Errorf("GetModules: %w", err)
	}
	fmt.Printf("GetModules: %d modules\n", mods.Modules().Len())

	// 3. Pipelined Get + GetModules
	log = log[:0]
	pcfg, pmods, err := cli.Pipeline(ctx, dep)
	if err != nil {
		return fmt.Errorf("Pipeline: %w", err)
	}
	fmt.Printf("Pipeline: get.present=%v modules=%d\n", pcfg.Present(), pmods.Modules().Len())
	fmt.Printf("Pipeline send log: %s\n", fmtLog(log))

	// Verify the pipelining invariant: ≥2 sends before the first recv.
	sends, firstRecv := 0, -1
	for i, e := range log {
		if e.Kind == "recv" {
			firstRecv = i
			break
		}
		sends++
	}
	if firstRecv == -1 || sends < 2 {
		return fmt.Errorf("pipelining not observed: %d sends before first recv", sends)
	}
	fmt.Printf("Pipelining verified: %d calls in flight before the first answer\n", sends)
	return nil
}

func fmtLog(log []server.SendEvent) string {
	var out strings.Builder
	for _, e := range log {
		m := "get"
		if e.Method == server.MethodGetModules {
			m = "getModules"
		}
		fmt.Fprintf(&out, "%s(%s,p=%d,t=%d) ", e.Kind, m, e.PromiseID, e.Target)
	}
	return out.String()
}
