# ui-customization

The first Hanzo Base-native Go service binary, and the architectural reference
for the console tRPC→ZAP migration. It serves the **UiCustomization** capability
(UI branding, hrefs, default model adapters, visible product modules) that the
Next.js console previously ran in-process as a tRPC router hitting Prisma.

**Pattern:** Go binary built on [Hanzo Base](../base) (embedded encrypted SQLite
+ plugins) exposing a typed [ZAP](../zap) capability-RPC interface. No Prisma, no
Postgres-as-source-of-truth, no Mongo, no tRPC in the backend.

```
.zap schema (source of truth)  ──zapgen──▶  gen/ (Go views)
        │                                        │
        └──capnp-es──▶ console TS client         ▼
                                   server/  ─ ZAP RPC handler (cap-gated)
                                   main.go  ─ base.New() + ZAP router :9999
                                              Base HTTP (health/metrics) :8090
                                              vault → per-org encrypted SQLite
```

## Run

```bash
make build
./ui-customization serve --http=127.0.0.1:8090 --zap=127.0.0.1:9999
```

Optional per-org encrypted SQLite (vault plugin): add `--vaultDir=/data/vaults`.
Config is seeded once from `HANZO_UI_*` env (see `server/collections.go`),
then lives in the `ui_customization` collection.

## Smoke test

In-process suite (ZAP RPC + permission gate + pipelining proof):

```bash
make test
```

Out-of-process probe against a live binary:

```bash
./ui-customization serve --zap=127.0.0.1:9999 &
go run ./cmd/probe --addr 127.0.0.1:9999 --peer ui-customization
```

## The interface

Two read methods on a `CapKindIAMSession` capability, both gated on
`PermSessionRead` (1<<0):

- `get() -> UiCustomizationConfig` — full config (`present=false` when the org
  is not entitled, mirroring the old tRPC `null`).
- `getModules() -> VisibleModules` — just the product-module list; a distinct,
  pipeline-able read so a caller fans out config + modules without two round
  trips.

Capability auth is the single chokepoint `Server.authorize`: it Wraps the
opaque capability buffer, enforces `Kind == CapKindIAMSession` and the
`PermSessionRead` bit, and (when an issuer registry is wired) verifies the
signature per [zap-spec/SPEC.md §2.3](../../zap-proto/zap-spec/SPEC.md). The
signature step is stubbed in bootstrap (TODO in `server.go`) — Kind +
Permissions are always enforced.

## Wiring the console to this service (handoff)

The console keeps its existing TS client (same `.zap` contract, compiled to TS
by capnp-es instead of Go). Its ZAP bridge substitutes the in-process Server
for a ZAP client to this service:

1. Set the service URL in console's runtime:
   ```
   UI_CUSTOMIZATION_ZAP_URL=tcp://127.0.0.1:9999
   ```
2. In `console/web/src/server/zap/dispatch.ts`, when `UI_CUSTOMIZATION_ZAP_URL`
   is set, route the `UiCustomization` capability to a ZAP client dialed at that
   URL instead of constructing `uiCustomizationServer(ctx)` in-process.
3. The wire envelope is `(method:u32, promiseID:u32, target:u32, cap:bytes,
   payload:bytes)` at ZAP msgType **200**; responses are `(status:u32,
   promiseID:u32, body:bytes)`. Method ordinals match the schema: `get`=0,
   `getModules`=1. The TS client encodes the cap as the opaque ZAP capability
   buffer and the body as the capnp-es-compiled struct bytes.
4. Delete `console/web/src/features/ui-customization/uiCustomizationServer.ts`
   once the ZAP route is live.

This repo does **not** edit console.

## Layout

| Path | What |
|------|------|
| `proto/ui-customization.zap` | Canonical schema (zap-spec dialect → Go). |
| `gen/` | zapgen output (`make zap-gen`). Generated; do not edit. |
| `server/wire.go` | Transport envelope codec + msgType/method/status consts. |
| `server/server.go` | ZAP RPC handler: cap auth, dispatch, promise pipelining. |
| `server/collections.go` | Base collection schema + env seed. |
| `server/modules.go` | Product-module list + env visibility (mirrors console). |
| `server/client.go` | Typed pipelining client + `SyntheticCap`. |
| `main.go` | Binary: `base.New()` + vault + ZAP router. |
| `cmd/probe/` | Out-of-process smoke probe. |

Container registry: `ghcr.io/hanzoai/ui-customization` (CI-built, multi-arch).
