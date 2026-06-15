package server

import (
	"strings"
)

// productModules is the canonical list of Hanzo product modules — kept
// byte-for-byte in sync with the console's productModuleSchema.ts PRODUCT_MODULES.
var productModules = []string{
	"dashboards",
	"tracing",
	"evaluation",
	"prompt-management",
	"playground",
	"datasets",
	"search",
	"agents",
	"bots",
	"tasks",
	"functions",
	"kms",
	"infrastructure",
	"base",
	"zt",
	"referrals",
}

// visibleModulesFromEnv resolves the visible product-module list from the two
// env knobs, mirroring console getVisibleProductModules exactly:
//   - HANZO_UI_VISIBLE_PRODUCT_MODULES set  → that allow-list (precedence)
//   - HANZO_UI_HIDDEN_PRODUCT_MODULES set   → all modules minus the deny-list
//   - neither set                           → all modules
func visibleModulesFromEnv() []string {
	visible := env("HANZO_UI_VISIBLE_PRODUCT_MODULES")
	hidden := env("HANZO_UI_HIDDEN_PRODUCT_MODULES")

	if visible != "" {
		return parseModulesList(visible)
	}
	if hidden != "" {
		deny := make(map[string]struct{})
		for _, m := range parseModulesList(hidden) {
			deny[m] = struct{}{}
		}
		out := make([]string, 0, len(productModules))
		for _, m := range productModules {
			if _, blocked := deny[m]; !blocked {
				out = append(out, m)
			}
		}
		return out
	}
	out := make([]string, len(productModules))
	copy(out, productModules)
	return out
}

// parseModulesList splits a comma-separated env value into known modules,
// lower-casing and dropping anything not in productModules (matches console).
func parseModulesList(input string) []string {
	input = strings.TrimSpace(input)
	if input == "" {
		return []string{}
	}
	known := make(map[string]struct{}, len(productModules))
	for _, m := range productModules {
		known[m] = struct{}{}
	}
	out := []string{}
	for _, part := range strings.Split(strings.ToLower(input), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, ok := known[part]; ok {
			out = append(out, part)
		}
	}
	return out
}
