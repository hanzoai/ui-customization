package server

import (
	"os"

	"github.com/hanzoai/base/core"
	"github.com/hanzoai/base/tools/hook"
)

// CollectionName is the single Base collection backing UiCustomization data.
// One row per org (keyed by `org`); the service scopes reads to the org in
// the verified capability. Backed by Base's encrypted SQLite (vault plugin
// KEK per org) when vault is registered; plain SQLite otherwise.
const CollectionName = "ui_customization"

// Field names mirror the Prisma/tRPC model the old uiCustomizationRouter read.
// `present` is the entitlement flag (the wire model of tRPC's `null`).
const (
	fOrg                     = "org"
	fPresent                 = "present"
	fHostname                = "hostname"
	fDocumentationHref       = "documentationHref"
	fSupportHref             = "supportHref"
	fFeedbackHref            = "feedbackHref"
	fLogoLightModeHref       = "logoLightModeHref"
	fLogoDarkModeHref        = "logoDarkModeHref"
	fDefaultModelAdapter     = "defaultModelAdapter"
	fDefaultBaseUrlOpenAI    = "defaultBaseUrlOpenAI"
	fDefaultBaseUrlAnthropic = "defaultBaseUrlAnthropic"
	fDefaultBaseUrlAzure     = "defaultBaseUrlAzure"
	fVisibleModules          = "visibleModules" // JSON array of strings
)

// RegisterCollections ensures the ui_customization collection exists and seeds
// the default org row from env on first boot. Idempotent: re-running finds the
// existing collection/row and no-ops. Wired via OnBootstrap so it runs once at
// startup, before the ZAP listener accepts calls.
func RegisterCollections(app core.App, defaultOrg string) {
	app.OnBootstrap().Bind(&hook.Handler[*core.BootstrapEvent]{
		Id: "uiCustomizationCollections",
		Func: func(e *core.BootstrapEvent) error {
			// Run the rest of bootstrap first so the DB is initialized.
			if err := e.Next(); err != nil {
				return err
			}
			if err := EnsureCollection(app); err != nil {
				return err
			}
			return SeedDefaultRow(app, defaultOrg)
		},
	})
}

// EnsureCollection creates the ui_customization collection if absent. Exported
// so callers that bootstrap the app themselves (tests, one-shot migrations)
// can provision it directly rather than via the OnBootstrap hook. Idempotent.
func EnsureCollection(app core.App) error {
	if _, err := app.FindCollectionByNameOrId(CollectionName); err == nil {
		return nil // already exists
	}

	col := core.NewBaseCollection(CollectionName)
	col.Fields.Add(&core.TextField{Name: fOrg, Required: true, Max: 255})
	col.Fields.Add(&core.BoolField{Name: fPresent})
	for _, name := range []string{
		fHostname, fDocumentationHref, fSupportHref, fFeedbackHref,
		fLogoLightModeHref, fLogoDarkModeHref, fDefaultModelAdapter,
		fDefaultBaseUrlOpenAI, fDefaultBaseUrlAnthropic, fDefaultBaseUrlAzure,
	} {
		col.Fields.Add(&core.TextField{Name: name, Max: 2048})
	}
	col.Fields.Add(&core.JSONField{Name: fVisibleModules, MaxSize: 65536})
	col.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	col.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})

	// Unique index on org: one customization row per organization.
	col.AddIndex("idx_ui_customization_org", true, fOrg, "")

	return app.Save(col)
}

// SeedDefaultRow writes the org's customization from env, mirroring the values
// the tRPC `get` procedure read directly from env. Skipped if a row for the org
// already exists (config lives in the DB after first boot). Exported alongside
// EnsureCollection for direct provisioning.
func SeedDefaultRow(app core.App, org string) error {
	col, err := app.FindCollectionByNameOrId(CollectionName)
	if err != nil {
		return err
	}
	if _, err := app.FindFirstRecordByFilter(col, "org = {:org}", map[string]any{"org": org}); err == nil {
		return nil // already seeded
	}

	rec := core.NewRecord(col)
	rec.Set(fOrg, org)
	// present mirrors the self-host-ui-customization entitlement. The plan
	// gate lived in console; here it's a stored flag seeded from env so the
	// service is self-contained. Default false (not entitled) unless set.
	rec.Set(fPresent, env("HANZO_UI_PRESENT") == "true")
	rec.Set(fHostname, env("HANZO_UI_API_HOST"))
	rec.Set(fDocumentationHref, env("HANZO_UI_DOCUMENTATION_HREF"))
	rec.Set(fSupportHref, env("HANZO_UI_SUPPORT_HREF"))
	rec.Set(fFeedbackHref, env("HANZO_UI_FEEDBACK_HREF"))
	rec.Set(fLogoLightModeHref, env("HANZO_UI_LOGO_LIGHT_MODE_HREF"))
	rec.Set(fLogoDarkModeHref, env("HANZO_UI_LOGO_DARK_MODE_HREF"))
	rec.Set(fDefaultModelAdapter, env("HANZO_UI_DEFAULT_MODEL_ADAPTER"))
	rec.Set(fDefaultBaseUrlOpenAI, env("HANZO_UI_DEFAULT_BASE_URL_OPENAI"))
	rec.Set(fDefaultBaseUrlAnthropic, env("HANZO_UI_DEFAULT_BASE_URL_ANTHROPIC"))
	rec.Set(fDefaultBaseUrlAzure, env("HANZO_UI_DEFAULT_BASE_URL_AZURE"))
	rec.Set(fVisibleModules, visibleModulesFromEnv())

	return app.Save(rec)
}

func env(k string) string { return os.Getenv(k) }
