package acceptance_test

// Second half of the dashboard-dependent scenarios: catalogue handling,
// hand-edit races against open forms, the CSRF marker, the open-access
// banner, hosts-only exports and the settings ranges — all driven through
// /dashboard/* and /api/*.

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"elpulpo/internal/app"
	"elpulpo/internal/config"
	"elpulpo/internal/testutil"
)

// dashSettings is the raw /api/settings document.
func dashSettings(t *testing.T, h *testutil.Harness) map[string]any {
	t.Helper()
	st, body := h.Get("/api/settings")
	if st != 200 {
		t.Fatalf("GET /api/settings: %d %s", st, body)
	}
	var out map[string]any
	mustJSON(t, body, &out)
	return out
}

// dashBannerOpen reports whether the rendered page carries the persistent
// open-access banner.
func dashBannerOpen(html string) bool {
	return strings.Contains(html, "Unauthenticated access.") &&
		strings.Contains(html, "ELPULPO_PROXY_TOKEN is empty") &&
		strings.Contains(html, "ELPULPO_DASHBOARD_PASSWORD is empty")
}

func dashErrOf(t *testing.T, body string) string {
	t.Helper()
	var e dashErr
	mustJSON(t, body, &e)
	return e.Error
}

// --- scenario 34 ------------------------------------------------------------

// TestAcceptance_34_CatalogNoMatchStaysUnpriced: a usage name with no exact
// catalogue match is refused (never guessed at), nothing is written, and it
// stays "no price set" on the Prices screen.
func TestAcceptance_34_CatalogNoMatchStaysUnpriced(t *testing.T) {
	h := testutil.Start(t)
	up := testutil.NewUpstream(t, "openai", "nonexistent-model-xyz")
	h.ApplyConfig(&config.Config{Hosts: []config.Host{
		{ID: "solo", HostAddresses: []string{"127.0.0.1"}, Servers: []config.Server{
			{ID: "api", Port: up.Port(), API: "openai"},
		}},
	}})
	h.WaitForModel("nonexistent-model-xyz-openai@solo")
	dashChat(t, h, "nonexistent-model-xyz-openai@solo")

	section := dashNoPriceSection(t, h)
	if !strings.Contains(section, "nonexistent-model-xyz") {
		t.Fatalf("name is not listed as no price set:\n%s", section)
	}
	if !strings.Contains(section, "no catalogue match") {
		t.Fatalf("a fuzzy offer must not be made:\n%s", section)
	}
	before := h.ConfigYAML()

	st, body := h.CSRFPost("/dashboard/action/catalog/apply", map[string]any{
		"model": "nonexistent-model-xyz", "h": dashHash(t, h),
	})
	if st != 400 {
		t.Fatalf("catalog/apply without a match: %d %s", st, body)
	}
	if msg := dashErrOf(t, body); !strings.Contains(msg, "no exact catalogue match") {
		t.Fatalf("error %q must say there is no exact match", msg)
	}

	if got := h.ConfigYAML(); got != before {
		t.Fatalf("config changed after a refused apply:\n%s", got)
	}
	second := h.ConfigYAML()
	if strings.Contains(second, "prices") {
		t.Fatalf("no prices may be written for it:\n%s", second)
	}
	if !strings.Contains(dashNoPriceSection(t, h), "nonexistent-model-xyz") {
		t.Fatal("the name must still be listed as no price set")
	}
}

// --- scenario 35 ------------------------------------------------------------

// TestAcceptance_35_CatalogOverwriteConfirmation: applying over a live entry
// is refused until overwrite is confirmed; the refusal writes nothing.
func TestAcceptance_35_CatalogOverwriteConfirmation(t *testing.T) {
	h := testutil.Start(t)
	up := testutil.NewUpstream(t, "openai", "gpt-4o")
	h.ApplyConfig(&config.Config{
		Hosts: []config.Host{
			{ID: "solo", HostAddresses: []string{"127.0.0.1"}, Servers: []config.Server{
				{ID: "api", Port: up.Port(), API: "openai"},
			}},
		},
		Prices: &config.Prices{Currency: "USD", Models: []config.ModelPrice{
			{Model: "gpt-4o", Input: dashF(1.00), Output: dashF(2.00)},
		}},
	})
	h.WaitForModel("gpt-4o-openai@solo")
	dashChat(t, h, "gpt-4o-openai@solo")
	before := h.ConfigYAML()

	st, body := h.CSRFPost("/dashboard/action/catalog/apply", map[string]any{
		"provider": "openai", "model": "gpt-4o", "h": dashHash(t, h),
	})
	if st != 409 {
		t.Fatalf("apply over a live entry without overwrite: %d %s", st, body)
	}
	if msg := dashErrOf(t, body); !strings.Contains(msg, "overwrite") {
		t.Fatalf("error %q must ask for the overwrite confirmation", msg)
	}
	if got := h.ConfigYAML(); got != before {
		t.Fatalf("config changed by a refused overwrite:\n--- before ---\n%s\n--- after ---\n%s", before, got)
	}

	st, body = h.CSRFPost("/dashboard/action/catalog/apply", map[string]any{
		"provider": "openai", "model": "gpt-4o", "overwrite": true, "h": dashHash(t, h),
	})
	if st != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("apply with overwrite confirmed: %d %s", st, body)
	}
	var doc config.Config
	exported := h.ConfigYAML()
	if err := yaml.Unmarshal([]byte(exported), &doc); err != nil {
		t.Fatalf("export no longer parses: %v\n%s", err, exported)
	}
	var entry config.ModelPrice
	found := false
	if doc.Prices != nil {
		if doc.Prices.Currency != "USD" {
			t.Fatalf("overwrite changed the document currency: %q", doc.Prices.Currency)
		}
		for _, m := range doc.Prices.Models {
			if m.Model == "gpt-4o" {
				entry, found = m, true
			}
		}
	}
	if !found {
		t.Fatalf("no gpt-4o entry after the overwrite:\n%s", exported)
	}
	if *entry.Input != 2.50 || *entry.Output != 10.00 {
		t.Fatalf("the entry is %+v after the overwrite: want the catalogue rates 2.50 / 10.00, not the live 1.00 / 2.00",
			entry)
	}
}

// --- scenario 36 ------------------------------------------------------------

// TestAcceptance_36_CatalogRefusesNonUSD: with a document currency other
// than USD no catalogue rate is written automatically — no conversion.
func TestAcceptance_36_CatalogRefusesNonUSD(t *testing.T) {
	h := testutil.Start(t)
	up := testutil.NewUpstream(t, "openai", "gpt-4o")
	h.ApplyConfig(&config.Config{
		Hosts: []config.Host{
			{ID: "solo", HostAddresses: []string{"127.0.0.1"}, Servers: []config.Server{
				{ID: "api", Port: up.Port(), API: "openai"},
			}},
		},
		Prices: &config.Prices{Currency: "EUR", Models: []config.ModelPrice{
			{Model: "economy-model", Input: dashF(1.00), Output: dashF(1.00)},
		}},
	})
	h.WaitForModel("gpt-4o-openai@solo")
	dashChat(t, h, "gpt-4o-openai@solo")
	before := h.ConfigYAML()
	if !strings.Contains(before, "currency: EUR") {
		t.Fatalf("precondition: EUR document currency:\n%s", before)
	}

	st, body := h.CSRFPost("/dashboard/action/catalog/apply", map[string]any{
		"provider": "openai", "model": "gpt-4o", "overwrite": true, "h": dashHash(t, h),
	})
	if st != 400 {
		t.Fatalf("apply into a EUR document: %d %s", st, body)
	}
	if msg := dashErrOf(t, body); !strings.Contains(msg, "no currency conversion") {
		t.Fatalf("error %q must refuse to convert currencies", msg)
	}
	if got := h.ConfigYAML(); got != before {
		t.Fatalf("config changed by a refused apply:\n%s", got)
	}
	if strings.Contains(h.ConfigYAML(), "gpt-4o") {
		t.Fatal("no USD rate may be written into a EUR document")
	}
	if s := dashSummaryAt(t, h, "group_by=model"); s.Currency != "EUR" || s.GrandTotal.Amount != 0 {
		t.Fatalf("summary over an unpriced EUR doc = %+v (currency %s)", s.GrandTotal, s.Currency)
	}
}

// --- scenario 37 ------------------------------------------------------------

// TestAcceptance_37_CatalogueExposedOffline: everything works with nothing
// but the configured hosts to talk to; the JSON API and the Prices screen
// both expose the catalogue version, as_of, currency and the stale flag.
func TestAcceptance_37_CatalogueExposedOffline(t *testing.T) {
	h := testutil.Start(t) // no hosts configured: no route to anything at all
	if ids := h.ModelIDs(); len(ids) != 0 {
		t.Fatalf("no hosts means no models, got %s", testutil.ModelNames(ids))
	}
	st, body := h.Get("/api/health")
	if st != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("/api/health: %d %s", st, body)
	}

	st, body = h.Get("/api/catalogue")
	if st != 200 {
		t.Fatalf("GET /api/catalogue: %d %s", st, body)
	}
	var raw map[string]any
	mustJSON(t, body, &raw)
	if _, ok := raw["stale"]; !ok {
		t.Fatalf("catalogue JSON carries no stale flag: %s", body)
	}
	var cat struct {
		Version  string `json:"version"`
		AsOf     string `json:"as_of"`
		Currency string `json:"currency"`
		Stale    bool   `json:"stale"`
		Rates    []struct {
			Provider        string   `json:"provider"`
			Model           string   `json:"model"`
			Input           *float64 `json:"input"`
			Output          *float64 `json:"output"`
			CachedInput     *float64 `json:"cached_input"`
			ReasoningOutput *float64 `json:"reasoning_output"`
		} `json:"rates"`
	}
	mustJSON(t, body, &cat)
	if cat.Version == "" || cat.Version != h.App.Catalogue.File.Catalogue.Version {
		t.Fatalf("version = %q, want the bundled %q", cat.Version, h.App.Catalogue.File.Catalogue.Version)
	}
	asOf, err := time.Parse("2006-01-02", cat.AsOf)
	if err != nil {
		t.Fatalf("as_of %q is not YYYY-MM-DD", cat.AsOf)
	}
	if cat.Currency != "USD" {
		t.Fatalf("currency = %q, want USD", cat.Currency)
	}
	if len(cat.Rates) < 20 {
		t.Fatalf("catalogue exposes %d rates, want the hand-curated two dozen or more", len(cat.Rates))
	}
	for i, r := range cat.Rates {
		if r.Provider == "" || r.Model == "" || r.Input == nil || r.Output == nil {
			t.Fatalf("rates[%d] is incomplete: %+v", i, r)
		}
	}
	// Staleness is derived from as_of alone — no network involved.
	if want := time.Since(asOf) > 180*24*time.Hour; cat.Stale != want {
		t.Fatalf("stale = %v, as_of %s says %v", cat.Stale, cat.AsOf, want)
	}

	st, html := h.Get("/dashboard/part/prices")
	if st != 200 {
		t.Fatalf("GET /dashboard/part/prices: %d %s", st, html)
	}
	for _, want := range []string{"Version", cat.Version, "as of", cat.AsOf, "USD"} {
		if !strings.Contains(html, want) {
			t.Fatalf("Prices screen does not show catalogue %q", want)
		}
	}

	// Staleness is driven by as_of alone: an operator-supplied catalogue
	// older than 180 days is marked stale — offline, with no update check
	// of any kind behind it.
	dir := t.TempDir()
	seed := dir + "/prices.yaml"
	if err := os.WriteFile(seed, []byte("catalogue:\n"+
		"    version: \"2024.01\"\n"+
		"    as_of: 2024-01-01\n"+
		"    currency: USD\n"+
		"    rates:\n"+
		"    -   provider: openai\n"+
		"        model: gpt-4o\n"+
		"        input: 1.00\n"+
		"        output: 2.00\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h2 := testutil.Start(t, func(o *app.Options) { o.CataloguePath = seed })
	st, body = h2.Get("/api/catalogue")
	if st != 200 {
		t.Fatalf("GET /api/catalogue (override): %d %s", st, body)
	}
	var aged struct {
		Version  string `json:"version"`
		AsOf     string `json:"as_of"`
		Stale    bool   `json:"stale"`
		Currency string `json:"currency"`
		Rates    []struct {
			Model string `json:"model"`
		} `json:"rates"`
	}
	mustJSON(t, body, &aged)
	if aged.Version != "2024.01" || aged.AsOf != "2024-01-01" || aged.Currency != "USD" || len(aged.Rates) != 1 {
		t.Fatalf("the override catalogue is not the one in use: %s", body)
	}
	if !aged.Stale {
		t.Fatalf("a 2024 catalogue must be marked stale: %s", body)
	}
	if st, html = h2.Get("/dashboard/part/prices"); st != 200 || !strings.Contains(html, "stale") {
		t.Fatalf("the Prices screen does not label the aged catalogue stale (status %d)", st)
	}
}

// --- scenario 40 ------------------------------------------------------------

// TestAcceptance_40_StaleSaveRefused: the form's hash went stale because the
// file was edited on disk after it was read; the save is refused with 409 and
// the file keeps the hand edit.
func TestAcceptance_40_StaleSaveRefused(t *testing.T) {
	h := testutil.Start(t)
	up1 := testutil.NewUpstream(t, "ollama", "first-model")
	h.ApplyConfig(&config.Config{Hosts: []config.Host{
		{ID: "solo", HostAddresses: []string{"127.0.0.1"}, Servers: []config.Server{
			{ID: "llm", Port: up1.Port(), API: "ollama"},
		}},
	}})
	h.WaitForModel("first-model-ollama@solo")

	// The dashboard form was rendered: this is the hash it carries.
	oldHash := dashHash(t, h)
	if oldHash == "" {
		t.Fatal("a saved configuration must carry a hash")
	}

	// A hand edit on disk adds a second host with a live server.
	up2 := testutil.NewUpstreamOn(t, "127.0.0.2", "ollama", "second-model")
	edited := fmt.Sprintf(`hosts:
-   host_addresses: [127.0.0.1]
    id: solo
    servers:
    -   port: %d
        api: ollama
        id: llm
-   host_addresses: [127.0.0.2]
    id: host2
    servers:
    -   port: %d
        api: ollama
        id: llm
`, up1.Port(), up2.Port())
	if err := os.WriteFile(h.App.Opts.ConfigPath, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	h.WaitForModel("second-model-ollama@host2")
	h.Eventually(5*time.Second, "the watcher to adopt the new file hash", func() bool {
		return dashHash(t, h) != oldHash
	})
	onDisk, err := os.ReadFile(h.App.Opts.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != edited {
		t.Fatalf("a hand edit must be honoured as written:\n%s", onDisk)
	}

	// The form is saved with the hash it was rendered with.
	doc := []map[string]any{dashHostDoc("solo", []string{"127.0.0.1"}, dashServerDoc("llm", up1.Port(), "ollama"))}
	st, body := dashSaveDoc(t, h, oldHash, nil, doc...)
	if st != 409 {
		t.Fatalf("stale save: %d %s", st, body)
	}
	if msg := dashErrOf(t, body); !strings.Contains(msg, "changed on disk") {
		t.Fatalf("stale save must say \"changed on disk, reload first\", got %q", msg)
	}
	after, err := os.ReadFile(h.App.Opts.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(onDisk) {
		t.Fatalf("the refused save wrote the file anyway:\n%s", after)
	}
	if ids := strings.Join(h.ModelIDs(), " "); !strings.Contains(ids, "second-model-ollama@host2") {
		t.Fatalf("the refused save disturbed the live config: %s", ids)
	}

	// After a reload (a fresh hash) the same save goes through.
	st, body = dashSaveDoc(t, h, dashHash(t, h), nil, doc...)
	if st != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("save with a fresh hash: %d %s", st, body)
	}
	h.WaitForNoModel("second-model-ollama@host2")
}

// --- scenario 41 ------------------------------------------------------------

// TestAcceptance_41_CSRFMarkerRequired: a POST to a config-mutating route
// without the marker header — cookie present or not — gets 403 and changes
// nothing.
func TestAcceptance_41_CSRFMarkerRequired(t *testing.T) {
	h := testutil.Start(t)
	startNaming(t, h)
	h.WaitForModel("qwen3.8:27b-ollama@minion1")
	before := h.ConfigYAML()
	tok := h.IssueCSRF()
	payload := fmt.Sprintf(`{"id":"minion2","h":%q}`, dashHash(t, h))
	jsonHdr := map[string]string{"Content-Type": "application/json"}

	check := func(what string, st int, body string) {
		t.Helper()
		if st != 403 {
			t.Fatalf("%s: status %d, want 403 (%s)", what, st, body)
		}
		if !strings.Contains(body, "csrf check failed") {
			t.Fatalf("%s: body %q", what, body)
		}
	}

	// (a) The elpulpo_csrf cookie rides along (as a browser would send it),
	// but the marker header a cross-site form cannot set is missing.
	st, body := h.Do(http.MethodPost, "/dashboard/action/host/delete", jsonHdr, strings.NewReader(payload))
	check("cookie, no marker header", st, body)

	// (b) The marker without the cookie: no session, no mutation.
	req, err := http.NewRequest(http.MethodPost, h.URL("/dashboard/action/host/delete"), strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("cookieless POST: %v", err)
	}
	defer resp.Body.Close()
	cookieless := make([]byte, 512)
	n, _ := resp.Body.Read(cookieless)
	check("marker, no cookie", resp.StatusCode, string(cookieless[:n]))

	// (c) A marker that does not equal the cookie.
	st, body = h.Do(http.MethodPost, "/dashboard/action/host/delete", map[string]string{
		"Content-Type": "application/json", "X-CSRF-Token": strings.Repeat("0", 32),
	}, strings.NewReader(payload))
	check("mismatched marker", st, body)

	// Nothing was configured: minion2 is still there, the file never moved.
	ids := dashHostIDs(t, h)
	if strings.Join(ids, " ") != "minion1 minion2" {
		t.Fatalf("/api/config hosts = %v, want minion1 and minion2 intact", ids)
	}
	if got := h.ConfigYAML(); got != before {
		t.Fatalf("live config changed despite the 403s:\n%s", got)
	}
	// The same action with cookie + matching marker does go through.
	st, body = h.CSRFPost("/dashboard/action/host/delete", map[string]any{
		"id": "minion2", "h": dashHash(t, h),
	})
	if st != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("properly marked delete: %d %s", st, body)
	}
}

// --- scenario 42 (dashboard variant) ---------------------------------------

// TestAcceptance_42_DashboardBannerOnEveryStart: with the proxy token and the
// dashboard password empty, the banner is on every page after every start —
// not only the first.
func TestAcceptance_42_DashboardBannerOnEveryStart(t *testing.T) {
	dir := t.TempDir()
	h1 := testutil.Start(t, func(o *app.Options) {
		o.ConfigPath = dir + "/elpulpo.yaml"
		o.DataDir = dir + "/data"
	})
	for _, page := range []string{"/dashboard/servers", "/dashboard/prices", "/dashboard/stats", "/dashboard/settings"} {
		st, html := h1.Get(page)
		if st != 200 {
			t.Fatalf("GET %s: %d", page, st)
		}
		if !dashBannerOpen(html) {
			t.Fatalf("%s has no open-access banner on the first start:\n%s", page, html)
		}
	}
	if !strings.Contains(h1.LogString(), "ELPULPO_PROXY_TOKEN is empty") ||
		!strings.Contains(h1.LogString(), "ELPULPO_DASHBOARD_PASSWORD is empty") {
		t.Fatalf("first start missing the WARN lines:\n%s", h1.LogString())
	}
	h1.App.Stop()

	// Restart: a fresh app on the very same config file and data dir.
	h2 := testutil.Start(t, func(o *app.Options) {
		o.ConfigPath = dir + "/elpulpo.yaml"
		o.DataDir = dir + "/data"
	})
	st, html := h2.Get("/dashboard/servers")
	if st != 200 {
		t.Fatalf("GET /dashboard/servers after restart: %d", st)
	}
	if !dashBannerOpen(html) {
		t.Fatalf("the banner is gone after a restart:\n%s", html)
	}
	if !strings.Contains(h2.LogString(), "ELPULPO_PROXY_TOKEN is empty") ||
		!strings.Contains(h2.LogString(), "ELPULPO_DASHBOARD_PASSWORD is empty") {
		t.Fatalf("restart missing the WARN lines:\n%s", h2.LogString())
	}
}

// --- scenario 45 (dashboard variant) ---------------------------------------

// TestAcceptance_45_HostsOnlyExportRoute: a hosts-only document saved through
// the dashboard exports without a prices key, and savings read as disabled.
func TestAcceptance_45_HostsOnlyExportRoute(t *testing.T) {
	h := testutil.Start(t)
	up := testutil.NewUpstream(t, "ollama", "only-model")
	st, body := dashSaveDoc(t, h, "", nil,
		dashHostDoc("solo", []string{"127.0.0.1"}, dashServerDoc("llm", up.Port(), "ollama")))
	if st != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("hosts-only save: %d %s", st, body)
	}
	h.WaitForModel("only-model-ollama@solo")

	export := h.ConfigYAML()
	if !strings.Contains(export, "hosts") {
		t.Fatalf("export of a hosts-only config has no hosts section:\n%s", export)
	}
	if !strings.Contains(export, "id: solo") {
		t.Fatalf("export lost the host:\n%s", export)
	}
	if strings.Contains(export, "prices") {
		t.Fatalf("an empty prices section must be omitted, not written:\n%s", export)
	}
	if ct, cd := dashExportHeaders(t, h.URL("/dashboard/export/config.yaml")); !strings.Contains(ct, "text/yaml") ||
		!strings.Contains(cd, "attachment") || !strings.Contains(cd, "elpulpo.yaml") {
		t.Fatalf("config.yaml headers = %q / %q", ct, cd)
	}

	// The omission is not an accident of a never-written section: writing a
	// prices section through the dashboard and dropping it again (`prices:
	// null`) returns a hosts-only export as well.
	st, body = dashSaveDoc(t, h, dashHash(t, h),
		dashPricesDoc("USD", dashPriceDoc("only-model", 0.25, 1.00)),
		dashHostDoc("solo", []string{"127.0.0.1"}, dashServerDoc("llm", up.Port(), "ollama")))
	if st != 200 {
		t.Fatalf("prices save: %d %s", st, body)
	}
	if with := h.ConfigYAML(); !strings.Contains(with, "prices") || !strings.Contains(with, "currency: USD") {
		t.Fatalf("a saved prices section must appear in the export:\n%s", with)
	}
	st, body = h.CSRFPost("/dashboard/action/prices/save", map[string]any{"prices": nil, "h": dashHash(t, h)})
	if st != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("prices/save null must drop the section: %d %s", st, body)
	}
	if after := h.ConfigYAML(); after != export {
		t.Fatalf("dropping the prices section must return the hosts-only export:\n%s", after)
	}

	dashChat(t, h, "only-model-ollama@solo")
	if s := dashSummaryAt(t, h, "group_by=model"); s.SavingsOn || s.Currency != "" || s.GrandTotal.Amount != 0 {
		t.Fatalf("savings must read as disabled with no prices: savings_on=%v currency=%q amount=%v",
			s.SavingsOn, s.Currency, s.GrandTotal.Amount)
	}
}

// --- scenario 46 (dashboard variant) ---------------------------------------

// TestAcceptance_46_SettingsRangesViaDashboard: an out-of-range setting is
// refused by POST settings/save with a field-level error naming the allowed
// range; the live value stays until a valid save arrives.
func TestAcceptance_46_SettingsRangesViaDashboard(t *testing.T) {
	h := testutil.Start(t)
	beforeLive := h.App.Settings.Get().HealthInterval
	apiBefore := dashSettings(t, h)
	beforeAPI, hadKey := apiBefore["health_interval"]
	if !hadKey {
		t.Fatal("/api/settings exposes no health_interval")
	}

	violation := func(field string, payload map[string]any) config.Violation {
		t.Helper()
		payload["h"] = "irrelevant-settings-save-carries-no-config-hash"
		st, body := h.CSRFPost("/dashboard/action/settings/save", payload)
		if st != 422 {
			t.Fatalf("settings/save %v: %d %s", payload, st, body)
		}
		var vs struct {
			Violations []config.Violation `json:"violations"`
		}
		mustJSON(t, body, &vs)
		if len(vs.Violations) == 0 {
			t.Fatalf("settings/save %v returned no violations: %s", payload, body)
		}
		for _, v := range vs.Violations {
			if v.Path == field {
				return v
			}
		}
		t.Fatalf("settings/save %v reported no violation for %q: %s", payload, field, body)
		return config.Violation{}
	}

	v := violation("health_interval", map[string]any{"health_interval": "1s"})
	if !strings.Contains(v.Msg, "5s") || !strings.Contains(v.Msg, "3600s") {
		t.Fatalf("health_interval error %q must name the allowed range 5s..3600s", v.Msg)
	}
	if v2 := violation("max_request_size", map[string]any{"max_request_size": "2GiB"}); !strings.Contains(v2.Msg, "1GiB") {
		t.Fatalf("max_request_size error %q must name the allowed range up to 1GiB", v2.Msg)
	}

	if got := h.App.Settings.Get().HealthInterval; got != beforeLive {
		t.Fatalf("live health_interval = %v after the rejection, want %v", got, beforeLive)
	}
	if got := dashSettings(t, h)["health_interval"]; got != beforeAPI {
		t.Fatalf("/api/settings health_interval = %v after the rejection, want %v", got, beforeAPI)
	}

	st, body := h.CSRFPost("/dashboard/action/settings/save", map[string]any{"health_interval": "10s"})
	if st != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("valid settings/save: %d %s", st, body)
	}
	if got := h.App.Settings.Get().HealthInterval; got != 10*time.Second {
		t.Fatalf("live health_interval = %v after a valid save", got)
	}
	if got := dashSettings(t, h)["health_interval"]; got != float64(10*time.Second) {
		t.Fatalf("/api/settings health_interval = %v, want 10s", got)
	}
}
