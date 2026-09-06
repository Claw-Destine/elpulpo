package acceptance_test

// The Servers screen as the operator sees it: the live state table on top,
// the published-model list directly under it, and the "Configure servers"
// editor below that. All three are independent htmx regions, so the refresh
// contract is asserted here on both sides: a mutation answers
// `HX-Trigger: elpulpo-changed`, and every region re-reads itself — which
// only works while the fragment it fetches carries its own container.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"elpulpo/internal/config"
	"elpulpo/internal/testutil"
)

// --- helpers ---------------------------------------------------------------

// dashRegion GETs one self-refreshing region of the Servers screen: the
// state table (empty scope), the model list ("models") or the forms ("forms").
func dashRegion(t *testing.T, h *testutil.Harness, scope string) string {
	t.Helper()
	path := "/dashboard/part/servers"
	if scope != "" {
		path += "?scope=" + scope
	}
	st, body := h.Get(path)
	if st != 200 {
		t.Fatalf("GET %s: %d %s", path, st, body)
	}
	return body
}

// dashBetween returns the page markup from the first occurrence of start to
// the next occurrence of stop, so a region can be inspected on its own.
func dashBetween(t *testing.T, html, start, stop string) string {
	t.Helper()
	i := strings.Index(html, start)
	if i < 0 {
		t.Fatalf("page has no %s:\n%s", start, html)
	}
	j := strings.Index(html[i:], stop)
	if j < 0 {
		t.Fatalf("nothing after %s is closed by %s:\n%s", start, stop, html)
	}
	return html[i : i+j]
}

// dashMutation is a same-origin mutation that keeps the response headers:
// the refresh contract lives in HX-Trigger, which Harness.Do discards.
func dashMutation(t *testing.T, h *testutil.Harness, path string, body map[string]any) (int, string, http.Header) {
	t.Helper()
	dashAdoptCSRF(t, h)
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, h.URL(path), bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Csrf-Token", h.CsrfC)
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out), resp.Header
}

// dashTriggersOnChange is what makes the open Servers tab reload its regions:
// every accepted mutation must answer with this header.
func dashTriggersOnChange(t *testing.T, path string, hdr http.Header) {
	t.Helper()
	if got := hdr.Get("HX-Trigger"); got != "elpulpo-changed" {
		t.Fatalf("%s must answer HX-Trigger: elpulpo-changed so the open tab refreshes, got %q", path, got)
	}
}

// --- scenario 47 -----------------------------------------------------------

// TestAcceptance_47_ServersScreenListsModelsAndRefreshes: the Servers screen
// is the state table, then the available models, then the "Configure servers"
// editor; each region refreshes itself, and the mutations that change the
// configuration announce themselves so a model added or removed through the
// dashboard appears or disappears without a reload.
func TestAcceptance_47_ServersScreenListsModelsAndRefreshes(t *testing.T) {
	h := testutil.Start(t)
	n := startNaming(t, h)
	for _, id := range wantIDs {
		h.WaitForModel(id)
	}

	st, html := h.Get("/dashboard/servers")
	if st != 200 {
		t.Fatalf("GET /dashboard/servers: %d %s", st, html)
	}

	// 1. The editor is named for what it does.
	if !strings.Contains(html, "<h2>Configure servers</h2>") {
		t.Fatalf("the forms are not headed \"Configure servers\":\n%s", html)
	}
	if strings.Contains(html, "Hosts &amp; servers") {
		t.Fatal("the old \"Hosts & servers\" heading is still on the screen")
	}

	// 2. State table, then models, then the editor.
	stateAt := strings.Index(html, `id="servers-state"`)
	modelsAt := strings.Index(html, `id="servers-models"`)
	configAt := strings.Index(html, `id="servers-config"`)
	if stateAt < 0 || modelsAt < 0 || configAt < 0 {
		t.Fatalf("the Servers screen must stack three regions, got state=%d models=%d config=%d",
			stateAt, modelsAt, configAt)
	}
	if !(stateAt < modelsAt && modelsAt < configAt) {
		t.Fatalf("the model list must sit between the table and the editor: %d < %d < %d",
			stateAt, modelsAt, configAt)
	}
	if head := strings.Index(html, "<h2>Configure servers</h2>"); head < modelsAt {
		t.Fatalf("the editor heading must sit below the model list: heading=%d models=%d", head, modelsAt)
	}

	// 3. Every region polls and re-reads itself on a change. The container is
	//    part of the fragment, so the outerHTML swap restores the poller
	//    instead of killing it after the first refresh.
	for _, want := range []string{
		`<div id="servers-state" hx-get="/dashboard/part/servers" hx-trigger="every 5s, elpulpo-changed from:body" hx-swap="outerHTML">`,
		`<div id="servers-models" hx-get="/dashboard/part/servers?scope=models" hx-trigger="every 5s, elpulpo-changed from:body" hx-swap="outerHTML">`,
		`<div id="servers-config" hx-get="/dashboard/part/servers?scope=forms" hx-trigger="elpulpo-changed from:body" hx-swap="outerHTML">`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("Servers screen is missing the refresh wiring %s", want)
		}
	}

	// 4. The model list is exactly what GET /v1/models answers.
	region := dashBetween(t, html, `id="servers-models"`, `id="servers-config"`)
	for _, id := range h.ModelIDs() {
		if !strings.Contains(region, id) {
			t.Fatalf("%s is served by /v1/models but missing from the model list:\n%s", id, region)
		}
	}
	for _, want := range []string{"deepseek-v4-flash", "minion1", "minion2", ">available<"} {
		if !strings.Contains(region, want) {
			t.Fatalf("the model list must name %q:\n%s", want, region)
		}
	}
	if got := strings.Count(region, "<tr>") - 1; got != len(wantIDs) { // minus the header row
		t.Fatalf("the model list has %d rows, want %d:\n%s", got, len(wantIDs), region)
	}

	// 5. Removing a server refreshes both the list and the editor.
	path := "/dashboard/action/server/delete"
	st, body, hdr := dashMutation(t, h, path, map[string]any{
		"host_id": "minion1", "id": "vllm", "h": dashHash(t, h),
	})
	if st != 200 {
		t.Fatalf("server/delete: %d %s", st, body)
	}
	dashTriggersOnChange(t, path, hdr)
	h.WaitForNoModel("deepseek-v4-flash-vllm@minion1")
	if region = dashRegion(t, h, "models"); strings.Contains(region, "deepseek-v4-flash-vllm@minion1") {
		t.Fatalf("the removed model is still offered:\n%s", region)
	}
	if forms := dashRegion(t, h, "forms"); strings.Contains(forms, "vllm") {
		t.Fatalf("the editor still shows the removed server:\n%s", forms)
	}

	// 6. Adding a host with a server puts its models on the list.
	up3 := testutil.NewUpstreamOn(t, "127.0.0.3", "ollama", "llama3.3:70b")
	path = "/dashboard/action/host/save"
	st, body, hdr = dashMutation(t, h, path, map[string]any{
		"host": dashHostDoc("minion3", []string{"127.0.0.3"},
			dashServerDoc("ollama", up3.Port(), "ollama")),
		"h": dashHash(t, h),
	})
	if st != 200 {
		t.Fatalf("host/save: %d %s", st, body)
	}
	dashTriggersOnChange(t, path, hdr)
	h.WaitForModel("llama3.3:70b-ollama@minion3")
	if region = dashRegion(t, h, "models"); !strings.Contains(region, "llama3.3:70b-ollama@minion3") {
		t.Fatalf("the new model is not listed:\n%s", region)
	}

	// 7. An applied YAML import replaces the configuration and refreshes the
	//    same two regions — minion3 and its model are gone.
	imported := fmt.Sprintf("hosts:\n"+
		"- id: minion1\n  host_addresses: [127.0.0.1]\n  servers:\n  - id: ollama\n    port: %d\n    api: ollama\n"+
		"- id: minion2\n  host_addresses: [127.0.0.2]\n  servers:\n  - id: ollama\n    port: %d\n    api: ollama\n",
		n.Up1.Port(), n.Up2.Port())
	path = "/dashboard/action/config/import/apply"
	st, body, hdr = dashMutation(t, h, path, map[string]any{"yaml": imported, "h": dashHash(t, h)})
	if st != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("config/import/apply: %d %s", st, body)
	}
	dashTriggersOnChange(t, path, hdr)
	h.WaitForNoModel("llama3.3:70b-ollama@minion3")
	if region = dashRegion(t, h, "models"); strings.Contains(region, "minion3") {
		t.Fatalf("an imported-away host is still in the model list:\n%s", region)
	}
	if state := dashRegion(t, h, ""); strings.Contains(state, "minion3") {
		t.Fatalf("an imported-away host is still in the state table:\n%s", state)
	}
	for _, id := range []string{"qwen3.8:27b-ollama@minion1", "gpt-oss:120b-ollama@minion2"} {
		if !strings.Contains(region, id) {
			t.Fatalf("the import dropped %s from the model list:\n%s", id, region)
		}
	}
}

// --- scenario 48 -----------------------------------------------------------

// TestAcceptance_48_ServersScreenFollowsStateAndModelChanges: the two live
// regions answer from the same state /v1 routing reads, so an upstream that
// loads, unloads or dies is reflected without a reload: the unloaded name
// leaves the list, and a name whose server went dark stays listed as
// withdrawn — the screen explains the 404 instead of denying the model.
func TestAcceptance_48_ServersScreenFollowsStateAndModelChanges(t *testing.T) {
	h := testutil.Start(t)
	if v := h.UpdateSettings(map[string]string{"health_interval": "5s"}); len(v) > 0 {
		t.Fatalf("health_interval: %+v", v)
	}
	up := testutil.NewUpstream(t, "ollama", "qwen3.8:27b", "gemma4:31b")
	h.ApplyConfig(&config.Config{Hosts: []config.Host{
		{ID: "m", HostAddresses: []string{"127.0.0.1"}, Servers: []config.Server{
			{ID: "llm", Port: up.Port(), API: "ollama"},
		}},
	}})
	h.WaitForModel("qwen3.8:27b-llm@m")
	h.WaitForModel("gemma4:31b-llm@m")
	h.Eventually(10*time.Second, "both models are listed", func() bool {
		region := dashRegion(t, h, "models")
		return strings.Contains(region, "qwen3.8:27b-llm@m") &&
			strings.Contains(region, "gemma4:31b-llm@m")
	})

	// The upstream unloads one model: it disappears from both regions.
	up.SetModels("qwen3.8:27b")
	h.WaitForNoModel("gemma4:31b-llm@m")
	h.Eventually(20*time.Second, "the unloaded model leaves the model list", func() bool {
		return !strings.Contains(dashRegion(t, h, "models"), "gemma4:31b-llm@m")
	})
	h.Eventually(20*time.Second, "the state table counts the model that is left", func() bool {
		return strings.Contains(dashRegion(t, h, ""), `<td class="num" title="qwen3.8:27b-llm@m">1</td>`)
	})

	// The server stops answering entirely.
	up.Close()
	h.Eventually(30*time.Second, "server marked down after three failures", func() bool {
		s, ok := stateFor(h, "m", "llm")
		return ok && !s.Up && s.ConsecFails >= 3
	})
	models := dashRegion(t, h, "models")
	if !strings.Contains(models, "qwen3.8:27b-llm@m") {
		t.Fatalf("a known model must stay listed while its server is down:\n%s", models)
	}
	if !strings.Contains(models, ">withdrawn<") {
		t.Fatalf("the model of a down server must be flagged withdrawn:\n%s", models)
	}
	if strings.Contains(models, ">available<") {
		t.Fatalf("nothing may stay available behind a down server:\n%s", models)
	}
	if !strings.Contains(models, "not_available") {
		t.Fatalf("the list must explain what a withdrawn name answers:\n%s", models)
	}
	if state := dashRegion(t, h, ""); !strings.Contains(state, `class="down"`) {
		t.Fatalf("the state table must show the server down:\n%s", state)
	}
}
