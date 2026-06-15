# CLAUDE.md — contract for AI helpers

This is the reference Hanzo Base-native Go service. Keep it lean and exemplary;
11+ services clone this pattern.

## The one rule

**The `.zap` schema is the source of truth.** `proto/ui-customization.zap`
defines the data structs; `gen/` is its Go projection via `make zap-gen`. Never
hand-edit `gen/`. Change the schema, regenerate, then update `server/`.

## Two schema dialects (do not conflate)

- `proto/ui-customization.zap` is the **zap-spec dialect** (`package … /
  Field Type @off`) that `github.com/zap-proto/go/cmd/zapgen` compiles to Go.
  This is what THIS repo (a Go service) consumes.
- The console keeps a **capnp-es dialect** copy (file-id + `interface { method
  @n () -> (…) }`) that capnp-es compiles to TS. The two are NOT interchangeable
  (zapgen has no `interface`; capnp-es has no bare `struct … @off`). Each
  toolchain owns the encoding it parses; the field set is the shared contract.

## Build / test

- Pure-Go always: `CGO_ENABLED=0`. CGO pulls blst/accel C deps that need a full
  toolchain and break reproducibility. `make` sets this + `GOWORK=off`.
- `GOWORK=off`: this repo is self-contained via go.mod `replace`s; a parent
  `go.work` must not capture it.
- `make zap-gen` and `make build` are **idempotent** (byte-identical regen,
  reproducible binary). Keep them so — no timestamps/paths in generated output.
- `make test` runs the in-process ZAP RPC + permission + pipelining suite. Show
  it passing; don't claim "done" without it.

## Architecture invariants (DRY, orthogonal, decomplected)

- **Three wire layers, separated:** transport (`luxfi/zap` Node, msgType 200) /
  envelope (`server/wire.go`) / payload (`gen/` typed views). The capability is
  carried as OPAQUE bytes through all three — auth is a value, not a place.
- **One auth chokepoint:** `Server.authorize`. Kind + `PermSessionRead` bitmask
  always enforced; signature verify gated on a wired issuer registry (TODO →
  SPEC.md §2.3). Do not scatter permission checks into the method handlers.
- **One backend:** Hanzo Base. No Prisma, Postgres-as-source-of-truth, Mongo,
  Redis, tRPC, nginx. Data lives in the `ui_customization` Base collection
  (encrypted SQLite via the vault plugin when `--vaultDir` is set).
- **Pipelining is real:** `getModules` can target `get`'s promise; the server's
  promise table (`await`/`resolve`) joins them. Genuine in-flight pipelining
  needs the two calls on SEPARATE connections (the transport is FIFO per
  connection) — see `Client.Pipeline` and the pipelining test.

## Do not

- Build Docker images locally (CI does, multi-arch → ghcr.io/hanzoai).
- Push to GitHub from here unless asked.
- Add plugins this one service doesn't need. Lean binary.
- Touch the console — wiring is documented in README, not done here.
