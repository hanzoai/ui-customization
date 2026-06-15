// Command ui-customization is the first Hanzo Base-native Go service binary:
// a typed ZAP capability-RPC backend for UI customization, built on Hanzo Base
// (embedded SQLite + plugins). It replaces console's in-process tRPC router.
//
// Architecture (the reference pattern 11+ more services follow):
//
//	base.New()                    → Base app: embedded SQLite, hooks, migrations
//	  ├── vault (optional)        → per-org encrypted SQLite shard (KEK)
//	  ├── zap.MustRegister        → generic ORM transport (msgType 100–103)
//	  └── server.Register(node)   → THIS service's typed router (msgType 200)
//	apis.NewRouter(app)           → sidecar HTTP (health/metrics), NOT app data
//	app.Start()                   → serves HTTP :8090 + ZAP :9999
//
// The .zap schema (proto/) is the source of truth; gen/ is its Go projection.
package main

import (
	"context"
	"crypto/rand"
	"log"
	"os"

	"github.com/hanzoai/base"
	"github.com/hanzoai/base/core"
	"github.com/hanzoai/base/plugins/vault"
	"github.com/hanzoai/base/tools/hook"
	luxlog "github.com/luxfi/log"
	zaplib "github.com/luxfi/zap"
	zcap "github.com/zap-proto/go/cap"

	"github.com/hanzoai/ui-customization/server"
)

func main() {
	app := base.New()

	var zapAddr string
	app.RootCmd.PersistentFlags().StringVar(&zapAddr, "zap", envOr("ZAP_ADDR", "127.0.0.1:9999"),
		"address for the typed ZAP capability-RPC listener")

	var defaultOrg string
	app.RootCmd.PersistentFlags().StringVar(&defaultOrg, "org", envOr("UI_CUSTOMIZATION_ORG", "default"),
		"default organization scope when a capability carries no org binding")

	var vaultDir string
	app.RootCmd.PersistentFlags().StringVar(&vaultDir, "vaultDir", os.Getenv("VAULT_DIR"),
		"directory for per-org encrypted SQLite shards (enables the vault plugin)")

	app.RootCmd.ParseFlags(os.Args[1:])

	// Optional: per-org encrypted SQLite backing via the vault plugin. Enabled
	// only when --vaultDir is set so local dev stays single-file SQLite. The
	// master KEK comes from KMS in production; a process-ephemeral key is used
	// when VAULT_MASTER_KEY is unset (dev only — shards won't persist across
	// restarts, which is correct for throwaway dev data).
	if vaultDir != "" {
		vault.MustRegister(app, vault.Config{
			Enabled:   true,
			DataDir:   vaultDir,
			OrgID:     defaultOrg,
			MasterKey: masterKey(),
		})
	}

	// Ensure the ui_customization collection + seed the default org row.
	server.RegisterCollections(app, defaultOrg)

	// Stand up the typed ZAP router alongside Base's serve lifecycle. We run a
	// dedicated luxfi/zap node for the capability RPC (NoDiscovery: direct dial
	// only — service discovery is the gateway's job, not mDNS here).
	logger := luxlog.New("component", "ui-customization")
	node := zaplib.NewNode(zaplib.NodeConfig{
		NodeID:      "ui-customization",
		Port:        portOf(zapAddr),
		NoDiscovery: true,
	})

	// Verifier: bootstrap (ed25519, no issuer registry → Kind+Permissions
	// enforced, signature step skipped). Wire IssuerKey to the IAM pubkey
	// registry to enable full cryptographic verification.
	srv := server.NewServer(app, logger, defaultOrg, zcap.Verifier{})
	srv.Register(node)

	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id: "uiCustomizationZapNode",
		Func: func(e *core.ServeEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			if err := node.Start(); err != nil {
				return err
			}
			logger.Info("ui-customization ZAP router listening", "addr", zapAddr, "msgType", server.MsgTypeRouterBase)
			return nil
		},
	})
	app.OnTerminate().Bind(&hook.Handler[*core.TerminateEvent]{
		Id: "uiCustomizationZapNodeStop",
		Func: func(e *core.TerminateEvent) error {
			node.Stop()
			return e.Next()
		},
	})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}

// masterKey returns the 32-byte vault master KEK: from VAULT_MASTER_KEY (hex
// or raw 32 bytes) in production, else a process-ephemeral random key for dev.
func masterKey() []byte {
	if v := os.Getenv("VAULT_MASTER_KEY"); len(v) >= 32 {
		return []byte(v)[:32]
	}
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	return k
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// portOf extracts the port from a host:port address, defaulting to 9999.
func portOf(addr string) int {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			p := 0
			for _, c := range addr[i+1:] {
				if c < '0' || c > '9' {
					return 9999
				}
				p = p*10 + int(c-'0')
			}
			if p == 0 {
				return 9999
			}
			return p
		}
	}
	return 9999
}

var _ = context.Background // reserved for future graceful-shutdown wiring
