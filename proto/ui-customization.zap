# ui-customization.zap — canonical wire schema for the UiCustomization service.
#
# Dialect: zap-spec (the `package … / Field Type @off` grammar that
# github.com/zap-proto/go/cmd/zapgen consumes). This is the SAME dialect the
# capability schema (zap-spec/capabilities.zap) is written in, and it is what
# THIS repo — a Go service binary — compiles to Go views via `make zap-gen`.
#
# The console (web/proto/ui-customization.zap) keeps the *capnp-es* dialect of
# the same logical contract (file-id + `interface { method @n () -> (…) }`),
# which capnp-es compiles to TS. The two dialects are NOT interchangeable
# (zapgen has no `interface` production; capnp-es has no bare `struct … @off`),
# so each toolchain owns the encoding it can parse. The field set below is the
# byte-for-byte contract both sides agree on. See CLAUDE.md "dialects".
#
# RPC surface (hand-dispatched in server/, exactly as cap/ hand-writes Verify
# on top of zapgen'd views — zapgen emits data views, never method stubs):
#
#   interface UiCustomization @ MsgTypeRouterBase (200) {
#     get        @0 () -> (config :UiCustomizationConfig)   # requires PermSessionRead
#     getModules @1 () -> (result :VisibleModules)          # requires PermSessionRead
#   }
#
# Permission model: the caller's verified Capability (CapKindIAMSession = 0x01)
# carries a u64 Permissions bitmask. Both methods gate on PermSessionRead
# (1 << 0) — the single chokepoint server.requirePermission(cap, bit). See
# zap-spec/capabilities_kinds.md.

package uic

# UiCustomizationConfig mirrors the object the tRPC `get` procedure returned.
# `present` is false when the holder lacks the self-host-ui-customization
# entitlement (the wire model of the old tRPC `null`).
struct UiCustomizationConfig {
    Present                 bool         @0    # false => not entitled (was tRPC null)
    Hostname                text         @4
    DocumentationHref       text         @12
    SupportHref             text         @20
    FeedbackHref            text         @28
    LogoLightModeHref       text         @36
    LogoDarkModeHref        text         @44
    DefaultModelAdapter     text         @52
    DefaultBaseUrlOpenAI    text         @60
    DefaultBaseUrlAnthropic text         @68
    DefaultBaseUrlAzure     text         @76
    VisibleModules          list<text>   @84
}

# VisibleModules is just the product-module list — a distinct, pipeline-able
# read so a caller can fan out config + modules without two round trips.
struct VisibleModules {
    Modules list<text> @0
}
