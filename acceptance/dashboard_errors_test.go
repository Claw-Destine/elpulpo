package acceptance_test

// The answer an operator gets when the server refuses.
//
// htmx swaps a response into its target only when the status is 2xx/3xx, so a
// refused mutation is never rendered: the screen stays exactly as it was and
// the only trace of the refusal is the JSON body the browser was handed.
// internal/dashboard/assets/app.js is what turns that body back into a message
// beside the button that was clicked. Two things have to stay true for that to
// work, and both are pinned here: every way the forms can be refused answers
// with text the script can show (never a 2xx, never an empty body), and the
// pages keep CSP's `default-src 'self'` honest — one same-origin script beside
// htmx, no inline JavaScript anywhere, so the behaviour cannot creep back into
// `hx-on:` attributes one form at a time.

import (
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"testing"

	"elpulpo/internal/config"
	"elpulpo/internal/testutil"
)

// dashForm builds the x-www-form-urlencoded body a dashboard form submits.
func dashForm(kv map[string]string) url.Values {
	v := url.Values{}
	for k, val := range kv {
		v.Set(k, val)
	}
	return v
}

// dashGetHeaders GETs a dashboard URL and returns the response headers.
func dashGetHeaders(t *testing.T, h *testutil.Harness, path string) http.Header {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.URL(path), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: status %d", path, resp.StatusCode)
	}
	return resp.Header.Clone()
}

// dashRefusal is what the screen has to work with: one of the two shapes the
// contract fixes — a sentence, or the field-level list.
type dashRefusal struct {
	Error      string `json:"error"`
	Violations []struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Msg  string `json:"msg"`
	} `json:"violations"`
}

func (r dashRefusal) text() string {
	if r.Error != "" {
		return r.Error
	}
	var parts []string
	for _, v := range r.Violations {
		parts = append(parts, v.Path+": "+v.Msg)
	}
	return strings.Join(parts, "; ")
}

// dashPages and dashFragments are the screens the operator edits, and the
// regions htmx swaps into them.
var (
	dashPages = []string{"/dashboard/servers", "/dashboard/loadbalancer", "/dashboard/prices",
		"/dashboard/stats", "/dashboard/settings"}
	dashFragments = []string{"/dashboard/part/servers", "/dashboard/part/servers?scope=forms",
		"/dashboard/part/servers?scope=models", "/dashboard/part/loadbalancer",
		"/dashboard/part/loadbalancer?scope=forms", "/dashboard/part/prices",
		"/dashboard/part/settings", "/dashboard/part/stats"}
)

// TestDashboard_RefusedMutationsSaySo: every way the dashboard forms can be
// refused must answer a 4xx carrying either an error sentence or the violations
// list, with something readable in it. A refusal that answered 2xx, or with a
// body that reads as nothing, would be an invisible refusal again — which is
// the bug itself, not a detail of how it is displayed.
func TestDashboard_RefusedMutationsSaySo(t *testing.T) {
	h := testutil.Start(t)
	h.IssueCSRF()
	h.ApplyConfig(&config.Config{Hosts: []config.Host{
		{ID: "minion1", HostAddresses: []string{"127.0.0.1"}, Servers: []config.Server{
			{ID: "ollama", Port: 11434, API: "ollama", Scheme: "http"},
		}},
	}})
	hash := dashHash(t, h)

	cases := []struct {
		name   string
		path   string
		form   map[string]string
		status int // the 4xx this refusal is defined to answer
	}{
		{"host whose id the grammar forbids", "/dashboard/action/host/save",
			map[string]string{"id": "Minion One", "addresses": "192.168.1.1", "h": hash}, 422},
		{"server on a host that is not there", "/dashboard/action/server/save",
			map[string]string{"host_id": "ghost", "id": "ollama", "api": "ollama", "port": "11434", "scheme": "http", "h": hash}, 400},
		{"route whose member names no server", "/dashboard/action/route/save",
			map[string]string{"alias": "qwen", "model": "qwen3.8:27b-ollama@nowhere", "cost": "1", "h": hash}, 422},
		{"route cost that is not a number", "/dashboard/action/route/save",
			map[string]string{"alias": "qwen", "model": "qwen3.8:27b-ollama@minion1", "cost": "cheap", "h": hash}, 400},
		{"route with no alias", "/dashboard/action/route/save",
			map[string]string{"alias": "", "model": "qwen3.8:27b-ollama@minion1", "cost": "1", "h": hash}, 400},
		{"route that does not exist, deleted", "/dashboard/action/route/delete",
			map[string]string{"alias": "never-named", "h": hash}, 400},
		{"price rate that is not a number", "/dashboard/action/prices/save",
			map[string]string{"currency": "USD", "model": "gpt-4o", "input": "free", "output": "2.50", "h": hash}, 400},
		{"settings outside the allowed range", "/dashboard/action/settings/save",
			map[string]string{"health_interval": "9999s"}, 422},
		{"prune with retention off", "/dashboard/action/prune/run", map[string]string{}, 400},
		{"save against a hash that is no longer current", "/dashboard/action/host/save",
			map[string]string{"id": "minion2", "addresses": "192.168.1.2", "h": "0000000000"}, 409},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st, body := h.CSRFPostForm(c.path, dashForm(c.form))
			if st != c.status {
				t.Fatalf("%s answered %d, want %d — body %s", c.path, st, c.status, body)
			}
			var r dashRefusal
			mustJSON(t, body, &r)
			if r.text() == "" {
				t.Fatalf("%s refused with %d and nothing to show the operator: %s", c.path, st, body)
			}
			if r.Error == "" {
				for _, v := range r.Violations {
					if strings.TrimSpace(v.Path) == "" || strings.TrimSpace(v.Msg) == "" {
						t.Fatalf("a violation with no path or no msg cannot be shown beside a field: %s", body)
					}
				}
			}
		})
	}

	// The 403 is the one the operator cannot read off the server's own words:
	// the marker this page was rendered with is no longer the current one, and
	// only a reload mends it. Nothing may change on the way.
	before := h.ConfigYAML()
	st, body := dashFormPostNoCSRF(t, h, "/dashboard/action/host/save",
		map[string]string{"id": "minion9", "addresses": "192.168.1.9"})
	if st != 403 {
		t.Fatalf("a mutation without the CSRF header: %d %s", st, body)
	}
	if !strings.Contains(dashErrOf(t, body), "csrf") {
		t.Fatalf("the 403 must name the check that failed: %s", body)
	}
	if got := h.ConfigYAML(); got != before {
		t.Fatal("a refused mutation wrote the configuration")
	}
}

// dashFormPostNoCSRF posts a form the way the browser does, minus the
// X-CSRF-Token header — the cookie still rides along in the jar.
func dashFormPostNoCSRF(t *testing.T, h *testutil.Harness, path string, form map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.URL(path),
		strings.NewReader(dashForm(form).Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

// TestDashboard_NoFormPostsToAnActionThatDoesNotExist: a form whose hx-post is
// misspelled fails in exactly the invisible way this screen is about — the
// action mux answers 404 and nothing on the page was ever asked to change.
func TestDashboard_NoFormPostsToAnActionThatDoesNotExist(t *testing.T) {
	h := testutil.Start(t)
	h.IssueCSRF()
	// A fleet with something to edit: the empty screens show only the "add"
	// forms, and it is the edit and delete forms that carry the rest of the
	// actions.
	h.ApplyConfig(&config.Config{
		Hosts: []config.Host{{
			ID: "minion1", HostAddresses: []string{"127.0.0.1"},
			Servers: []config.Server{{ID: "ollama", Port: 11434, API: "ollama", Scheme: "http"}},
		}},
		LoadBalancer: &config.LoadBalancer{Routes: []config.Route{{
			Alias:   "qwen",
			Members: []config.RouteMember{{Model: "qwen3.8:27b-ollama@minion1"}},
		}}},
	})

	hxPost := regexp.MustCompile(`hx-post="([^"]+)"`)
	seen := map[string]bool{}
	for _, page := range append(append([]string{}, dashPages...), dashFragments...) {
		st, body := h.Get(page)
		if st != 200 {
			t.Fatalf("GET %s: %d", page, st)
		}
		for _, m := range hxPost.FindAllStringSubmatch(body, -1) {
			seen[m[1]] = true
		}
	}
	// Every mutation the contract lists, minus config/save, which no form
	// posts (the YAML panel goes through import + apply).
	if len(seen) < 11 {
		t.Fatalf("only %d hx-post endpoints found across the screens: %v", len(seen), seen)
	}
	var paths []string
	for path := range seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if !strings.HasPrefix(path, "/dashboard/action/") {
			t.Fatalf("a form posts to %s, which is not a mutation endpoint", path)
		}
		st, body := h.CSRFPost(path, map[string]any{})
		if st == 404 || strings.Contains(body, "unknown action") {
			t.Fatalf("a form on screen posts to %s, which no action answers (%d %s)", path, st, body)
		}
	}
}

// TestDashboard_EveryPageLinksTheOneScriptAndNoInlineJS: app.js is the whole
// client side beyond htmx. It is same-origin, so `default-src 'self'` admits
// it; an `hx-on:` attribute would need unsafe-eval and an inline handler needs
// unsafe-inline, so neither may appear on any screen — and the CSP must stay as
// tight as the day these pages were written. A fragment carries no script at
// all: it is swapped into a page that already loaded both.
func TestDashboard_EveryPageLinksTheOneScriptAndNoInlineJS(t *testing.T) {
	h := testutil.Start(t)

	onAttr := regexp.MustCompile(`\son[a-z]+="`)
	for _, page := range dashPages {
		st, body := h.Get(page)
		if st != 200 {
			t.Fatalf("GET %s: %d", page, st)
		}
		if !strings.Contains(body, `<script src="/dashboard/static/app.js" defer="defer"></script>`) {
			t.Fatalf("%s does not link the script that shows a refused mutation", page)
		}
		if !strings.Contains(body, `<script src="/dashboard/static/htmx.min.js"`) {
			t.Fatalf("%s lost htmx", page)
		}
		if n := strings.Count(body, "<script"); n != 2 {
			t.Fatalf("%s links %d scripts, want exactly htmx and app.js", page, n)
		}
	}
	for _, part := range dashFragments {
		st, body := h.Get(part)
		if st != 200 {
			t.Fatalf("GET %s: %d", part, st)
		}
		if n := strings.Count(body, "<script"); n != 0 {
			t.Fatalf("%s carries %d scripts; a swapped-in fragment must not load one again", part, n)
		}
	}
	for _, html := range append(append([]string{}, mustGetAll(t, h, dashPages)...), mustGetAll(t, h, dashFragments)...) {
		if strings.Contains(html, "hx-on") {
			t.Fatal("a screen carries hx-on attributes, which default-src 'self' cannot run")
		}
		if m := onAttr.FindString(html); m != "" {
			t.Fatalf("a screen carries the inline event handler %q", m)
		}
	}

	csp := dashGetHeaders(t, h, "/dashboard/servers").Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("the pages answer without a CSP")
	}
	for _, forbidden := range []string{"unsafe-inline", "unsafe-eval"} {
		if strings.Contains(csp, forbidden) {
			t.Fatalf("CSP %q admits %s: inline JavaScript is back", csp, forbidden)
		}
	}
	if !strings.Contains(csp, "default-src 'self'") {
		t.Fatalf("CSP %q no longer restricts the source to this origin", csp)
	}
}

// TestDashboard_AppIsServedSameOriginAndAsScript: the script is an embedded
// asset like the stylesheet — one origin, a script content type, and no CORS
// header ever (scenario 44 is structural for the dashboard, assets included).
func TestDashboard_AppIsServedSameOriginAndAsScript(t *testing.T) {
	h := testutil.Start(t)
	hdr := dashGetHeaders(t, h, "/dashboard/static/app.js")
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Fatalf("app.js answers Content-Type %q", ct)
	}
	if origin := hdr.Get("Access-Control-Allow-Origin"); origin != "" {
		t.Fatalf("app.js answers Access-Control-Allow-Origin %q", origin)
	}
	if csp := hdr.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Fatalf("app.js is served outside the dashboard CSP: %q", csp)
	}
	st, body := h.Get("/dashboard/static/app.js")
	if st != 200 || len(body) < 1000 {
		t.Fatalf("app.js: status %d, %d bytes", st, len(body))
	}
	// The hook the whole thing hangs on is htmx's own answer event, not a
	// private part of it, and the message is written as text.
	for _, want := range []string{"htmx:afterRequest", "htmx:beforeRequest", "textContent"} {
		if !strings.Contains(body, want) {
			t.Fatalf("app.js no longer uses %q", want)
		}
	}
	// It must not carry the machinery htmx already has, nor a second one, nor
	// any way of turning server text into markup.
	for _, forbidden := range []string{"XMLHttpRequest", "fetch(", "eval(", "innerHTML", "new Function"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("app.js reaches for %q: it should read what htmx already has and write text, nothing else", forbidden)
		}
	}
	if st, _ := h.Do(http.MethodPost, "/dashboard/static/app.js", nil, nil); st < 400 {
		t.Fatalf("POST /dashboard/static/app.js answered %d; assets are read-only", st)
	}
}

// mustGetAll is the body of each path, for the scans that run over all of them
// at once.
func mustGetAll(t *testing.T, h *testutil.Harness, paths []string) []string {
	t.Helper()
	var out []string
	for _, p := range paths {
		st, body := h.Get(p)
		if st != 200 {
			t.Fatalf("GET %s: %d", p, st)
		}
		out = append(out, body)
	}
	return out
}
