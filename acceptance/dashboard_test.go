package acceptance_test

// Dashboard-dependent acceptance scenarios (specs/functional.specs.md,
// acceptance table). Every test here drives the dashboard HTTP layer: pages,
// htmx fragments, POST actions, the CSV/YAML exports and the read-only JSON
// API. Scenarios that only needed the store or the proxy live in
// proxy_test.go and configfile_test.go.

import (
	"encoding/csv"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"elpulpo/internal/config"
	"elpulpo/internal/testutil"
)

// --- helpers ---------------------------------------------------------------

// dashServerDoc / dashHostDoc build the *document-shaped* JSON the config
// actions expect (the YAML field names of the config document, which is what
// the contract says a config body carries).
func dashServerDoc(id string, port int, api string) map[string]any {
	return map[string]any{"id": id, "port": port, "api": api}
}

func dashHostDoc(id string, addrs []string, servers ...map[string]any) map[string]any {
	return map[string]any{"id": id, "host_addresses": addrs, "servers": servers}
}

func dashPriceDoc(model string, in, out float64, aliases ...string) map[string]any {
	e := map[string]any{"model": model, "input": in, "output": out}
	if len(aliases) > 0 {
		e["aliases"] = aliases
	}
	return e
}

func dashPricesDoc(currency string, entries ...map[string]any) map[string]any {
	return map[string]any{"currency": currency, "models": entries}
}

// dashSaveDoc POSTs a whole configuration document through the dashboard.
func dashSaveDoc(t *testing.T, h *testutil.Harness, hash string, prices map[string]any, hosts ...map[string]any) (int, string) {
	t.Helper()
	body := map[string]any{"hosts": hosts, "h": hash}
	if prices != nil {
		body["prices"] = prices
	}
	return h.CSRFPost("/dashboard/action/config/save", body)
}

// dashAdoptCSRF adopts the elpulpo_csrf marker the client jar already holds.
// Any dashboard GET issues that cookie, after which h.IssueCSRF can no longer
// see it being set (it only reports a cookie it just issued), so the value has
// to be read back from the jar before a POST can carry it in the header.
func dashAdoptCSRF(t *testing.T, h *testutil.Harness) {
	t.Helper()
	if h.CsrfC != "" {
		return
	}
	u, err := url.Parse(h.URL("/dashboard/servers"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range h.Jar.Cookies(u) {
		if c.Name == "elpulpo_csrf" {
			h.CsrfC = c.Value
		}
	}
	if h.CsrfC == "" {
		h.IssueCSRF() // no page fetched yet: let the harness issue one
	}
	if h.CsrfC == "" {
		t.Fatal("no elpulpo_csrf marker available")
	}
}

// dashHash reads the live config hash — the value an open form carries as "h".
func dashHash(t *testing.T, h *testutil.Harness) string {
	t.Helper()
	dashAdoptCSRF(t, h) // the save that follows carries this hash and the marker
	st, body := h.Get("/api/config")
	if st != 200 {
		t.Fatalf("GET /api/config: %d %s", st, body)
	}
	var doc struct {
		Hash string `json:"hash"`
	}
	mustJSON(t, body, &doc)
	return doc.Hash
}

// dashHostIDs returns the host ids the JSON API reports for the live config.
func dashHostIDs(t *testing.T, h *testutil.Harness) []string {
	t.Helper()
	st, body := h.Get("/api/config")
	if st != 200 {
		t.Fatalf("GET /api/config: %d %s", st, body)
	}
	var doc struct {
		Config struct {
			Hosts []struct {
				ID string `json:"id"`
			} `json:"hosts"`
		} `json:"config"`
	}
	mustJSON(t, body, &doc)
	var ids []string
	for _, hs := range doc.Config.Hosts {
		ids = append(ids, hs.ID)
	}
	return ids
}

// dashChat posts a chat completion and fails unless it routes and answers 200.
func dashChat(t *testing.T, h *testutil.Harness, model string) {
	t.Helper()
	st, body := h.Chat("", map[string]any{
		"model":    model,
		"messages": []any{map[string]string{"role": "user", "content": "hello there, how are you?"}},
	})
	if st != 200 {
		t.Fatalf("chat %s: %d %s", model, st, body)
	}
}

// dashChatStatus posts a chat and returns the status the client saw.
func dashChatStatus(t *testing.T, h *testutil.Harness, model string) (int, string) {
	t.Helper()
	return h.Chat("", map[string]any{
		"model":    model,
		"messages": []any{map[string]string{"role": "user", "content": "hello there, how are you?"}},
	})
}

// dashCSV parses an export body into header-keyed records.
func dashCSV(t *testing.T, body string) (map[string]bool, []map[string]string) {
	t.Helper()
	recs, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatalf("parse CSV %q: %v", body, err)
	}
	if len(recs) == 0 {
		t.Fatalf("CSV export has no header line: %q", body)
	}
	header := map[string]bool{}
	for _, name := range recs[0] {
		header[name] = true
	}
	var out []map[string]string
	for _, rec := range recs[1:] {
		m := map[string]string{}
		for i, name := range recs[0] {
			if i < len(rec) {
				m[name] = rec[i]
			}
		}
		out = append(out, m)
	}
	return header, out
}

// dashExportHeaders reads the attachment headers of an export.
func dashExportHeaders(t *testing.T, url string) (string, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	return resp.Header.Get("Content-Type"), resp.Header.Get("Content-Disposition")
}

// dashNoPriceSection returns the rendered "No price set" block of the prices
// fragment (up to the catalogue panel).
func dashNoPriceSection(t *testing.T, h *testutil.Harness) string {
	t.Helper()
	h.DrainWriter()     // the view derives the list from the usage table
	dashAdoptCSRF(t, h) // this GET is what issues the CSRF cookie
	st, html := h.Get("/dashboard/part/prices")
	if st != 200 {
		t.Fatalf("GET /dashboard/part/prices: %d %s", st, html)
	}
	i := strings.Index(html, "No price set")
	if i < 0 {
		t.Fatalf("prices fragment has no \"No price set\" section:\n%s", html)
	}
	rest := html[i:]
	if j := strings.Index(rest, "Price catalogue"); j > 0 {
		rest = rest[:j]
	}
	return rest
}

// dashNear compares money amounts computed by the same expression.
func dashNear(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// dashAmount is the spec's savings formula for one priced group.
func dashAmount(in, out int64, priceIn, priceOut float64) float64 {
	return (float64(in)*priceIn + float64(out)*priceOut) / 1e6
}

// dashF is a rate pointer (a price entry only exists when its rates are set).
func dashF(v float64) *float64 { return &v }

// --- JSON shapes of the dashboard API ---------------------------------------

type dashGroup struct {
	Key             string   `json:"key"`
	Members         []string `json:"members"`
	Requests        int64    `json:"requests"`
	TokensIn        int64    `json:"tokens_in"`
	TokensOut       int64    `json:"tokens_out"`
	TokensCached    int64    `json:"tokens_cached"`
	TokensReasoning int64    `json:"tokens_reasoning"`
	EstimatedRows   int64    `json:"estimated_rows"`
	Amount          float64  `json:"amount"`
	Priced          bool     `json:"priced"`
	NoPrice         bool     `json:"no_price"`
}

type dashSummary struct {
	GroupBy      string      `json:"group_by"`
	Currency     string      `json:"currency"`
	SavingsOn    bool        `json:"savings_on"`
	Groups       []dashGroup `json:"groups"`
	GrandTotal   dashGroup   `json:"grand_total"`
	NoPriceNames []string    `json:"no_price_models"`
}

func (s *dashSummary) group(t *testing.T, key string) dashGroup {
	t.Helper()
	for _, g := range s.Groups {
		if g.Key == key {
			return g
		}
	}
	t.Fatalf("summary has no group %q: %+v", key, s.Groups)
	return dashGroup{}
}

func (s *dashSummary) hasGroup(key string) bool {
	for _, g := range s.Groups {
		if g.Key == key {
			return true
		}
	}
	return false
}

type dashPreview struct {
	ImportID string `json:"import_id"`
	Preview  struct {
		Added   []string `json:"added_hosts"`
		Removed []string `json:"removed_hosts"`
		Changed []string `json:"changed_hosts"`
	} `json:"preview"`
}

type dashErr struct {
	Error string `json:"error"`
}

func dashSummaryAt(t *testing.T, h *testutil.Harness, query string) dashSummary {
	t.Helper()
	// The dashboard re-reads every 5 s; a test must not read a half-written table.
	h.DrainWriter()
	st, body := h.Get("/api/usage/summary?" + query)
	if st != 200 {
		t.Fatalf("GET /api/usage/summary?%s: %d %s", query, st, body)
	}
	var s dashSummary
	mustJSON(t, body, &s)
	return s
}

// --- scenario 2 -------------------------------------------------------------

// TestAcceptance_2_HostDeleteViaDashboard: minion2 is deleted through
// POST /dashboard/action/host/delete. Within one health interval GET /v1/models
// returns the three minion1 ids, and the very same process keeps serving.
func TestAcceptance_2_HostDeleteViaDashboard(t *testing.T) {
	h := testutil.Start(t)
	n := startNaming(t, h)
	for _, id := range wantIDs {
		h.WaitForModel(id)
	}

	st, body := h.CSRFPost("/dashboard/action/host/delete", map[string]any{
		"id": "minion2", "h": dashHash(t, h),
	})
	if st != 200 {
		t.Fatalf("host/delete: %d %s", st, body)
	}
	if !strings.Contains(body, `"ok":true`) {
		t.Fatalf("host/delete outcome %s", body)
	}

	// Within one health interval (the harness runs 5 s) both minion2 ids
	// are gone and exactly the three minion1 ids remain.
	h.WaitForNoModel("gpt-oss:120b-ollama@minion2")
	h.WaitForNoModel("qwen3.8:27b-ollama@minion2")
	want := []string{
		"deepseek-v4-flash-vllm@minion1",
		"gemma4:31b-ollama@minion1",
		"qwen3.8:27b-ollama@minion1",
	}
	ids := h.ModelIDs()
	if strings.Join(ids, " ") != strings.Join(want, " ") {
		t.Fatalf("published ids = %s, want exactly %s", testutil.ModelNames(ids), testutil.ModelNames(want))
	}
	if got := dashHostIDs(t, h); len(got) != 1 || got[0] != "minion1" {
		t.Fatalf("/api/config hosts = %v, want only minion1", got)
	}

	// Nothing else happened: no restart, the surviving server still routes.
	time.Sleep(1200 * time.Millisecond) // let any in-flight minion2 probe land
	probeHits := n.Up2.Hits.Load()
	st, body = h.Chat("", map[string]any{
		"model":    "gemma4:31b-ollama@minion1",
		"messages": []any{map[string]string{"role": "user", "content": "still here?"}},
	})
	if st != 200 {
		t.Fatalf("request after the delete: %d %s", st, body)
	}
	if st, body := h.Get("/api/health"); st != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("/api/health after the delete: %d %s", st, body)
	}
	// A full health interval later, the deleted host is never probed again.
	time.Sleep(6 * time.Second)
	if after := n.Up2.Hits.Load(); after > probeHits {
		t.Fatalf("deleted host is still being probed: %d -> %d hits", probeHits, after)
	}
}

// --- scenario 13 ------------------------------------------------------------

// TestAcceptance_13_RowsCSVMatchesSummary: rows.csv filtered to this month
// and status ok contains exactly those rows; the JSON summary over the same
// query agrees with the sums re-parsed from the file; an export of an empty
// selection is a header-only file.
func TestAcceptance_13_RowsCSVMatchesSummary(t *testing.T) {
	h := testutil.Start(t)
	upOK := testutil.NewUpstream(t, "ollama", "alpha-model")
	up500 := testutil.NewUpstreamOn(t, "127.0.0.2", "openai", "beta-model")
	upSlow := testutil.NewUpstreamOn(t, "127.0.0.3", "openai", "gamma-model")
	upEst := testutil.NewUpstreamOn(t, "127.0.0.4", "openai", "delta-model")
	up500.Status = 500 // upstream_error rows
	upEst.NoUsage = true
	h.ApplyConfig(&config.Config{Hosts: []config.Host{
		{ID: "hosta", HostAddresses: []string{"127.0.0.1"}, Servers: []config.Server{
			{ID: "llm", Port: upOK.Port(), API: "ollama"},
		}},
		{ID: "hostb", HostAddresses: []string{"127.0.0.2"}, Servers: []config.Server{
			{ID: "api", Port: up500.Port(), API: "openai"},
		}},
		{ID: "hostc", HostAddresses: []string{"127.0.0.3"}, Servers: []config.Server{
			{ID: "api", Port: upSlow.Port(), API: "openai"},
		}},
		{ID: "hostd", HostAddresses: []string{"127.0.0.4"}, Servers: []config.Server{
			{ID: "api", Port: upEst.Port(), API: "openai"},
		}},
	}})
	h.WaitForModel("alpha-model-ollama@hosta")
	h.WaitForModel("beta-model-openai@hostb")
	h.WaitForModel("gamma-model-openai@hostc")
	h.WaitForModel("delta-model-openai@hostd")

	dashChat(t, h, "alpha-model-ollama@hosta")
	dashChat(t, h, "alpha-model-ollama@hosta")
	dashChat(t, h, "delta-model-openai@hostd") // estimated (no usage upstream)
	if st, body := dashChatStatus(t, h, "beta-model-openai@hostb"); st != 502 {
		t.Fatalf("upstream-500 chat: %d %s", st, body)
	}
	// One upstream_timeout row: the server stalls past first_byte_timeout.
	upSlow.FirstByteDelay = 3 * time.Second
	if v := h.UpdateSettings(map[string]string{"first_byte_timeout": "1s"}); len(v) > 0 {
		t.Fatalf("settings: %v", v)
	}
	if st, body := dashChatStatus(t, h, "gamma-model-openai@hostc"); st != 504 {
		t.Fatalf("stalled chat: %d %s", st, body)
	}

	// The seed itself: ok x3 (one estimated), upstream_error, upstream_timeout.
	counts := map[string]int{}
	for _, r := range h.Rows() {
		counts[r.Status]++
	}
	if counts["ok"] != 3 || counts["upstream_error"] != 1 || counts["upstream_timeout"] != 1 {
		t.Fatalf("seeded rows = %+v, want 3 ok / 1 upstream_error / 1 upstream_timeout", counts)
	}

	// The month the server computes its preset from.
	now := time.Now().In(time.Local)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local)
	monthEnd := monthStart.AddDate(0, 1, 0)

	const q = "period=month&status=ok"
	st, csvBody := h.Get("/dashboard/export/rows.csv?" + q)
	if st != 200 {
		t.Fatalf("rows.csv: %d %s", st, csvBody)
	}
	header, data := dashCSV(t, csvBody)
	for _, col := range []string{"timestamp", "host", "server", "model", "status", "tokens_in", "tokens_out", "estimated"} {
		if !header[col] {
			t.Fatalf("rows.csv is missing the %q column: %v", col, header)
		}
	}
	if len(data) != 3 {
		t.Fatalf("rows.csv has %d data lines, want the 3 rows matching the filter:\n%s", len(data), csvBody)
	}
	var sumIn, sumOut, sumEst int64
	for _, r := range data {
		if r["status"] != "ok" {
			t.Fatalf("row outside the status filter: %+v", r)
		}
		ts, err := time.Parse(time.RFC3339Nano, r["timestamp"])
		if err != nil {
			t.Fatalf("timestamp %q is not ISO 8601: %v", r["timestamp"], err)
		}
		if ts.Before(monthStart) || !ts.Before(monthEnd) {
			t.Fatalf("timestamp %s outside this month [%s,%s)", r["timestamp"], monthStart, monthEnd)
		}
		in, err := strconv.ParseInt(r["tokens_in"], 10, 64)
		if err != nil {
			t.Fatalf("tokens_in %q: %v", r["tokens_in"], err)
		}
		out, err := strconv.ParseInt(r["tokens_out"], 10, 64)
		if err != nil {
			t.Fatalf("tokens_out %q: %v", r["tokens_out"], err)
		}
		if r["estimated"] != "true" && r["estimated"] != "false" {
			t.Fatalf("estimated %q is not true/false", r["estimated"])
		}
		if r["estimated"] == "true" {
			sumEst++
		}
		sumIn += in
		sumOut += out
	}
	if sumEst != 1 {
		t.Fatalf("export carries %d estimated rows, want 1", sumEst)
	}

	// The displayed totals must equal the sums over the exported lines.
	s := dashSummaryAt(t, h, q)
	if s.GrandTotal.Requests != int64(len(data)) {
		t.Fatalf("summary requests = %d, export has %d lines", s.GrandTotal.Requests, len(data))
	}
	if s.GrandTotal.TokensIn != sumIn || s.GrandTotal.TokensOut != sumOut {
		t.Fatalf("summary tokens = %d/%d, export sums = %d/%d",
			s.GrandTotal.TokensIn, s.GrandTotal.TokensOut, sumIn, sumOut)
	}
	if s.GrandTotal.EstimatedRows != sumEst {
		t.Fatalf("summary estimated_rows = %d, export has %d", s.GrandTotal.EstimatedRows, sumEst)
	}
	if ct, cd := dashExportHeaders(t, h.URL("/dashboard/export/rows.csv?"+q)); !strings.Contains(ct, "text/csv") ||
		!strings.Contains(cd, "attachment") {
		t.Fatalf("rows.csv headers = %q / %q", ct, cd)
	}

	// A selection matching nothing is a header-only file.
	st, empty := h.Get("/dashboard/export/rows.csv?period=month&status=cancelled")
	if st != 200 {
		t.Fatalf("empty rows.csv: %d %s", st, empty)
	}
	lines := strings.Split(strings.TrimRight(empty, "\r\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("empty export must be header-only, got %d lines:\n%s", len(lines), empty)
	}
	if s := dashSummaryAt(t, h, "period=month&status=cancelled"); s.GrandTotal.Requests != 0 {
		t.Fatalf("summary over the same empty filter = %+v", s.GrandTotal)
	}
}

// --- scenario 14 ------------------------------------------------------------

// TestAcceptance_14_PricesCoverOneModelOnly: with a price set for one model
// alone, the grand total covers that model only and the other group is
// marked no price set.
func TestAcceptance_14_PricesCoverOneModelOnly(t *testing.T) {
	h := testutil.Start(t)
	upA := testutil.NewUpstream(t, "ollama", "alpha-model")
	upB := testutil.NewUpstreamOn(t, "127.0.0.2", "openai", "beta-model")
	h.ApplyConfig(&config.Config{
		Hosts: []config.Host{
			{ID: "hosta", HostAddresses: []string{"127.0.0.1"}, Servers: []config.Server{
				{ID: "llm", Port: upA.Port(), API: "ollama"},
			}},
			{ID: "hostb", HostAddresses: []string{"127.0.0.2"}, Servers: []config.Server{
				{ID: "api", Port: upB.Port(), API: "openai"},
			}},
		},
		Prices: &config.Prices{Currency: "USD", Models: []config.ModelPrice{
			{Model: "alpha-model", Input: dashF(0.25), Output: dashF(1.00)},
		}},
	})
	h.WaitForModel("alpha-model-ollama@hosta")
	h.WaitForModel("beta-model-openai@hostb")
	dashChat(t, h, "alpha-model-ollama@hosta")
	dashChat(t, h, "beta-model-openai@hostb")

	s := dashSummaryAt(t, h, "group_by=model")
	if !s.SavingsOn || s.Currency != "USD" {
		t.Fatalf("summary flags savings %v currency %q", s.SavingsOn, s.Currency)
	}
	if len(s.Groups) != 2 {
		t.Fatalf("groups = %+v, want one per model", s.Groups)
	}
	wantAlpha := dashAmount(111, 42, 0.25, 1.00)
	a := s.group(t, "alpha-model")
	if !a.Priced || a.NoPrice || !dashNear(a.Amount, wantAlpha) {
		t.Fatalf("priced group = %+v, want amount %v", a, wantAlpha)
	}
	b := s.group(t, "beta-model")
	if b.Priced || !b.NoPrice || b.Amount != 0 {
		t.Fatalf("unpriced group = %+v, want no_price with no amount", b)
	}
	if !dashNear(s.GrandTotal.Amount, wantAlpha) {
		t.Fatalf("grand total %v must cover the priced model alone (%v)", s.GrandTotal.Amount, wantAlpha)
	}
	if !s.GrandTotal.Priced || !s.GrandTotal.NoPrice {
		t.Fatalf("grand total flags = priced %v no_price %v", s.GrandTotal.Priced, s.GrandTotal.NoPrice)
	}
	if strings.Join(s.NoPriceNames, " ") != "beta-model" {
		t.Fatalf("no_price_models = %v, want [beta-model]", s.NoPriceNames)
	}
}

// --- scenario 21 ------------------------------------------------------------

// TestAcceptance_21_AliasGroupedUnderOneModel: a price entry for
// qwen3.8:27b with alias qwen27 prices rows reported under both names, and
// the model grouping merges them into one group with one amount.
func TestAcceptance_21_AliasGroupedUnderOneModel(t *testing.T) {
	h := testutil.Start(t)
	upBase := testutil.NewUpstream(t, "ollama", "qwen3.8:27b")
	upAlias := testutil.NewUpstreamOn(t, "127.0.0.2", "ollama", "qwen27")
	h.ApplyConfig(&config.Config{
		Hosts: []config.Host{
			{ID: "host1", HostAddresses: []string{"127.0.0.1"}, Servers: []config.Server{
				{ID: "ollama", Port: upBase.Port(), API: "ollama"},
			}},
			{ID: "host2", HostAddresses: []string{"127.0.0.2"}, Servers: []config.Server{
				{ID: "ollama", Port: upAlias.Port(), API: "ollama"},
			}},
		},
		Prices: &config.Prices{Currency: "USD", Models: []config.ModelPrice{
			{Model: "qwen3.8:27b", Aliases: []string{"qwen27"}, Input: dashF(0.25), Output: dashF(1.00)},
		}},
	})
	h.WaitForModel("qwen3.8:27b-ollama@host1")
	h.WaitForModel("qwen27-ollama@host2")
	dashChat(t, h, "qwen3.8:27b-ollama@host1")
	dashChat(t, h, "qwen27-ollama@host2")

	s := dashSummaryAt(t, h, "group_by=model")
	if len(s.Groups) != 1 {
		t.Fatalf("groups = %+v, want both rows merged under the price entry", s.Groups)
	}
	g := s.group(t, "qwen3.8:27b")
	wantAmount := dashAmount(222, 84, 0.25, 1.00)
	if g.Requests != 2 || g.TokensIn != 222 || g.TokensOut != 84 {
		t.Fatalf("merged group = %+v, want 2 requests / 222 in / 84 out", g)
	}
	if !dashNear(g.Amount, wantAmount) {
		t.Fatalf("merged amount = %v, want the hand-computed %v", g.Amount, wantAmount)
	}
	if !dashNear(s.GrandTotal.Amount, wantAmount) {
		t.Fatalf("grand total = %v, want %v", s.GrandTotal.Amount, wantAmount)
	}
	if strings.Join(g.Members, " ") != "qwen27-ollama@host2 qwen3.8:27b-ollama@host1" {
		t.Fatalf("group members = %v, want both published ids", g.Members)
	}
	if len(s.NoPriceNames) != 0 {
		t.Fatalf("nothing may be unpriced here, got %v", s.NoPriceNames)
	}
}

// --- scenario 23 ------------------------------------------------------------

// TestAcceptance_23_NoPriceSetListed: a model in usage that matches no entry
// is unpriced, excluded from the grand total, and listed on the Prices
// screen under "no price set".
func TestAcceptance_23_NoPriceSetListed(t *testing.T) {
	h := testutil.Start(t)
	upA := testutil.NewUpstream(t, "ollama", "alpha-model")
	upB := testutil.NewUpstreamOn(t, "127.0.0.2", "openai", "mystery-model")
	h.ApplyConfig(&config.Config{
		Hosts: []config.Host{
			{ID: "hosta", HostAddresses: []string{"127.0.0.1"}, Servers: []config.Server{
				{ID: "llm", Port: upA.Port(), API: "ollama"},
			}},
			{ID: "hostb", HostAddresses: []string{"127.0.0.2"}, Servers: []config.Server{
				{ID: "api", Port: upB.Port(), API: "openai"},
			}},
		},
		Prices: &config.Prices{Currency: "USD", Models: []config.ModelPrice{
			{Model: "alpha-model", Input: dashF(0.25), Output: dashF(1.00)},
		}},
	})
	h.WaitForModel("alpha-model-ollama@hosta")
	h.WaitForModel("mystery-model-openai@hostb")
	dashChat(t, h, "alpha-model-ollama@hosta")
	dashChat(t, h, "mystery-model-openai@hostb")

	s := dashSummaryAt(t, h, "group_by=model")
	wantAlpha := dashAmount(111, 42, 0.25, 1.00)
	if !dashNear(s.GrandTotal.Amount, wantAlpha) {
		t.Fatalf("grand total %v must exclude the unpriced model (%v)", s.GrandTotal.Amount, wantAlpha)
	}
	if g := s.group(t, "mystery-model"); g.Priced || !g.NoPrice || g.Amount != 0 {
		t.Fatalf("unpriced group = %+v", g)
	}
	if !strings.Contains(strings.Join(s.NoPriceNames, " "), "mystery-model") {
		t.Fatalf("no_price_models = %v", s.NoPriceNames)
	}

	section := dashNoPriceSection(t, h)
	if !strings.Contains(section, "mystery-model") {
		t.Fatalf("Prices screen does not list the unmatched name:\n%s", section)
	}
	if strings.Contains(section, "alpha-model") {
		t.Fatalf("priced name leaked into the no-price section:\n%s", section)
	}
}

// --- scenario 24 ------------------------------------------------------------

// TestAcceptance_24_ImportRoundTrip: exporting the configuration and
// importing it back unchanged previews no changes, leaves /v1/models alone,
// and the second export is byte-identical.
func TestAcceptance_24_ImportRoundTrip(t *testing.T) {
	h := testutil.Start(t)
	startNaming(t, h)
	for _, id := range wantIDs {
		h.WaitForModel(id)
	}
	before := h.ConfigYAML()
	idsBefore := strings.Join(h.ModelIDs(), " ")

	st, body := h.CSRFPost("/dashboard/action/config/import", map[string]any{"yaml": before})
	if st != 200 {
		t.Fatalf("config/import: %d %s", st, body)
	}
	var p dashPreview
	mustJSON(t, body, &p)
	if p.ImportID == "" {
		t.Fatalf("no import_id in %s", body)
	}
	if len(p.Preview.Added)+len(p.Preview.Removed)+len(p.Preview.Changed) != 0 {
		t.Fatalf("unchanged import previews changes: %s", body)
	}

	// Confirm by re-posting the same YAML (what the UI does).
	st, body = h.CSRFPost("/dashboard/action/config/import/apply",
		map[string]any{"yaml": before, "h": dashHash(t, h)})
	if st != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("import/apply (yaml): %d %s", st, body)
	}
	if ids := strings.Join(h.ModelIDs(), " "); ids != idsBefore {
		t.Fatalf("/v1/models changed after a no-op import: %s vs %s", ids, idsBefore)
	}
	if got := h.ConfigYAML(); got != before {
		t.Fatalf("second export is not byte-identical:\n--- first ---\n%s\n--- second ---\n%s", before, got)
	}

	// The staged-import path behaves the same way.
	st, body = h.CSRFPost("/dashboard/action/config/import", map[string]any{"yaml": before})
	if st != 200 {
		t.Fatalf("config/import (second): %d %s", st, body)
	}
	var p2 dashPreview
	mustJSON(t, body, &p2)
	st, body = h.CSRFPost("/dashboard/action/config/import/apply",
		map[string]any{"import_id": p2.ImportID, "h": dashHash(t, h)})
	if st != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("import/apply (import_id): %d %s", st, body)
	}
	if got := h.ConfigYAML(); got != before {
		t.Fatalf("export drifted after the staged import:\n%s", got)
	}
	if ids := strings.Join(h.ModelIDs(), " "); ids != idsBefore {
		t.Fatalf("/v1/models changed: %s vs %s", ids, idsBefore)
	}
}

// --- scenario 25 ------------------------------------------------------------

// TestAcceptance_25_ImportReportsEveryViolation: an import with an
// unsupported adapter, a duplicate host id and a colliding name segment is
// refused with all three paths; nothing is applied.
func TestAcceptance_25_ImportReportsEveryViolation(t *testing.T) {
	h := testutil.Start(t)
	n := startNaming(t, h)
	for _, id := range wantIDs {
		h.WaitForModel(id)
	}
	before := h.ConfigYAML()
	idsBefore := strings.Join(h.ModelIDs(), " ")

	bad := fmt.Sprintf(`hosts:
-   host_addresses: [127.0.0.1]
    id: minion1
    servers:
    -   port: %d
        api: ollama
        id: ollama
    -   port: %d
        api: openai
        id: vllm
        postfix: ollama
-   host_addresses: [127.0.0.2]
    id: minion1
    servers:
    -   port: %d
        api: anthropic
        id: llm
`, n.Up1.Port(), n.UpVllm.Port(), n.Up2.Port())

	st, body := h.CSRFPost("/dashboard/action/config/import", map[string]any{"yaml": bad})
	if st != 422 {
		t.Fatalf("import of an invalid document: %d %s", st, body)
	}
	var vs struct {
		Violations []config.Violation `json:"violations"`
	}
	mustJSON(t, body, &vs)
	paths := map[string]config.Violation{}
	for _, v := range vs.Violations {
		paths[v.Path] = v
	}
	for _, want := range []struct{ path, msg string }{
		{"hosts[1].servers[0].api", "unsupported adapter"},
		{"hosts[1].id", "duplicate host id"},
		{"hosts[0].servers[1].postfix", "collide"},
	} {
		v, ok := paths[want.path]
		if !ok {
			t.Fatalf("violation %q missing, got: %s", want.path, body)
		}
		if !strings.Contains(v.Msg, want.msg) {
			t.Fatalf("violation %q msg %q must mention %q", want.path, v.Msg, want.msg)
		}
		if v.Line <= 0 {
			t.Fatalf("violation %q carries no line number: %+v", want.path, v)
		}
	}

	// Nothing was applied and nothing was staged.
	if got := h.ConfigYAML(); got != before {
		t.Fatalf("live config changed after a refused import:\n%s", got)
	}
	if ids := strings.Join(h.ModelIDs(), " "); ids != idsBefore {
		t.Fatalf("/v1/models changed: %s vs %s", ids, idsBefore)
	}
	st, body = h.CSRFPost("/dashboard/action/config/import/apply",
		map[string]any{"import_id": "not-a-real-import", "h": dashHash(t, h)})
	if st != 400 {
		t.Fatalf("apply of a never-staged import: %d %s", st, body)
	}
}

// --- scenario 26 ------------------------------------------------------------

// TestAcceptance_26_ImportDropsHost: importing a document without minion2
// previews it as removed and, once confirmed, behaves like scenario 2.
func TestAcceptance_26_ImportDropsHost(t *testing.T) {
	h := testutil.Start(t)
	startNaming(t, h)
	for _, id := range wantIDs {
		h.WaitForModel(id)
	}

	live := h.App.Store.Current().Config
	trimmed := &config.Config{}
	for _, hs := range live.Hosts {
		if hs.ID != "minion2" {
			trimmed.Hosts = append(trimmed.Hosts, hs)
		}
	}
	docs := string(config.Canonical(trimmed))

	st, body := h.CSRFPost("/dashboard/action/config/import", map[string]any{"yaml": docs})
	if st != 200 {
		t.Fatalf("config/import: %d %s", st, body)
	}
	var p dashPreview
	mustJSON(t, body, &p)
	if strings.Join(p.Preview.Removed, " ") != "minion2" {
		t.Fatalf("preview.removed_hosts = %v, want [minion2]: %s", p.Preview.Removed, body)
	}
	if len(p.Preview.Added) != 0 || len(p.Preview.Changed) != 0 {
		t.Fatalf("preview must only report the removal: %s", body)
	}

	st, body = h.CSRFPost("/dashboard/action/config/import/apply",
		map[string]any{"yaml": docs, "h": dashHash(t, h)})
	if st != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("import/apply: %d %s", st, body)
	}

	// Behaviour matches scenario 2: three ids within one health interval.
	h.WaitForNoModel("gpt-oss:120b-ollama@minion2")
	h.WaitForNoModel("qwen3.8:27b-ollama@minion2")
	ids := h.ModelIDs()
	if len(ids) != 3 {
		t.Fatalf("published ids = %s, want the 3 minion1 ids", testutil.ModelNames(ids))
	}
	for _, id := range ids {
		if !strings.HasSuffix(id, "@minion1") {
			t.Fatalf("unexpected survivor %q in %s", id, testutil.ModelNames(ids))
		}
	}
	if got := dashHostIDs(t, h); len(got) != 1 || got[0] != "minion1" {
		t.Fatalf("/api/config hosts = %v, want only minion1", got)
	}
}

// --- scenario 27 (dashboard variant) ---------------------------------------

// TestAcceptance_27_FirstSaveViaDashboard: with no config file on disk, the
// first save through the dashboard creates the file and publishes the host.
func TestAcceptance_27_FirstSaveViaDashboard(t *testing.T) {
	h := testutil.Start(t)
	if _, err := os.Stat(h.App.Opts.ConfigPath); !os.IsNotExist(err) {
		t.Fatalf("the scenario starts without a config file: %v", err)
	}
	if ids := h.ModelIDs(); len(ids) != 0 {
		t.Fatalf("empty config must publish nothing, got %s", testutil.ModelNames(ids))
	}
	up := testutil.NewUpstream(t, "ollama", "firstboot-model")

	st, body := dashSaveDoc(t, h, "", nil,
		dashHostDoc("solo", []string{"127.0.0.1"}, dashServerDoc("llm", up.Port(), "ollama")))
	if st != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("first config/save: %d %s", st, body)
	}
	data, err := os.ReadFile(h.App.Opts.ConfigPath)
	if err != nil {
		t.Fatalf("the first save must create the file: %v", err)
	}
	if !strings.Contains(string(data), "id: solo") {
		t.Fatalf("created file does not carry the host:\n%s", data)
	}
	h.WaitForModel("firstboot-model-ollama@solo")
	ids := h.ModelIDs()
	if len(ids) != 1 || ids[0] != "firstboot-model-ollama@solo" {
		t.Fatalf("published ids = %s", testutil.ModelNames(ids))
	}
}

// --- scenario 32 ------------------------------------------------------------

// TestAcceptance_32_SummaryCSVMatchesRows: the summary CSV's per-model sums
// equal the sums re-computed over rows.csv with the same filters, and the
// amounts match the reference prices in force.
func TestAcceptance_32_SummaryCSVMatchesRows(t *testing.T) {
	const (
		alphaIn, alphaOut = 4.00, 10.00
		betaIn, betaOut   = 8.00, 16.00
	)
	h := testutil.Start(t)
	upA := testutil.NewUpstream(t, "ollama", "alpha-model")
	upB := testutil.NewUpstreamOn(t, "127.0.0.2", "openai", "beta-model")
	h.ApplyConfig(&config.Config{
		Hosts: []config.Host{
			{ID: "hosta", HostAddresses: []string{"127.0.0.1"}, Servers: []config.Server{
				{ID: "llm", Port: upA.Port(), API: "ollama"},
			}},
			{ID: "hostb", HostAddresses: []string{"127.0.0.2"}, Servers: []config.Server{
				{ID: "api", Port: upB.Port(), API: "openai"},
			}},
		},
		Prices: &config.Prices{Currency: "USD", Models: []config.ModelPrice{
			{Model: "alpha-model", Input: dashF(alphaIn), Output: dashF(alphaOut)},
			{Model: "beta-model", Input: dashF(betaIn), Output: dashF(betaOut)},
		}},
	})
	h.WaitForModel("alpha-model-ollama@hosta")
	h.WaitForModel("beta-model-openai@hostb")
	dashChat(t, h, "alpha-model-ollama@hosta")
	dashChat(t, h, "alpha-model-ollama@hosta")
	dashChat(t, h, "beta-model-openai@hostb")

	if ct, cd := dashExportHeaders(t, h.URL("/dashboard/export/summary.csv?group_by=model&period=month")); !strings.Contains(ct, "text/csv") ||
		!strings.Contains(cd, "attachment") {
		t.Fatalf("summary.csv headers = %q / %q", ct, cd)
	}
	h.DrainWriter() // every routed request must have reached the table

	// Re-sum the rows export by base model name.
	st, rowsBody := h.Get("/dashboard/export/rows.csv?period=month")
	if st != 200 {
		t.Fatalf("rows.csv: %d %s", st, rowsBody)
	}
	_, rows := dashCSV(t, rowsBody)
	type sum struct{ n, in, out int64 }
	byModel := map[string]*sum{}
	for _, r := range rows {
		base := config.BaseModelOf(r["model"])
		s := byModel[base]
		if s == nil {
			s = &sum{}
			byModel[base] = s
		}
		s.n++
		in, _ := strconv.ParseInt(r["tokens_in"], 10, 64)
		out, _ := strconv.ParseInt(r["tokens_out"], 10, 64)
		s.in += in
		s.out += out
	}
	if len(byModel) < 2 {
		t.Fatalf("rows.csv covers %d models, want at least 2:\n%s", len(byModel), rowsBody)
	}

	st, sumBody := h.Get("/dashboard/export/summary.csv?group_by=model&period=month")
	if st != 200 {
		t.Fatalf("summary.csv: %d %s", st, sumBody)
	}
	header, recs := dashCSV(t, sumBody)
	for _, col := range []string{"group_by", "group", "requests", "tokens_in", "tokens_out", "amount", "currency"} {
		if !header[col] {
			t.Fatalf("summary.csv is missing the %q column", col)
		}
	}
	rates := map[string][2]float64{"alpha-model": {alphaIn, alphaOut}, "beta-model": {betaIn, betaOut}}
	seenModels, totalAmount, totalReqs := 0, 0.0, int64(0)
	for _, r := range recs {
		switch r["group_by"] {
		case "model":
			seenModels++
			base := r["group"]
			s, ok := byModel[base]
			if !ok {
				t.Fatalf("summary.csv has a model group %q with no rows behind it:\n%s", base, sumBody)
			}
			if r["requests"] != strconv.FormatInt(s.n, 10) ||
				r["tokens_in"] != strconv.FormatInt(s.in, 10) ||
				r["tokens_out"] != strconv.FormatInt(s.out, 10) {
				t.Fatalf("group %q sums %v/%v/%v differ from the rows export %v/%v/%v",
					base, r["requests"], r["tokens_in"], r["tokens_out"], s.n, s.in, s.out)
			}
			want := dashAmount(s.in, s.out, rates[base][0], rates[base][1])
			got, err := strconv.ParseFloat(r["amount"], 64)
			if err != nil {
				t.Fatalf("amount %q: %v", r["amount"], err)
			}
			if math.Abs(got-want) > 1e-9 {
				t.Fatalf("group %q amount %v, hand-computed %v", base, got, want)
			}
			if r["currency"] != "USD" {
				t.Fatalf("amount currency = %q, want USD", r["currency"])
			}
			totalAmount += want
			totalReqs += s.n
		case "total":
			if r["requests"] != strconv.FormatInt(totalReqs, 10) {
				t.Fatalf("TOTAL requests %q, want %d", r["requests"], totalReqs)
			}
			got, err := strconv.ParseFloat(r["amount"], 64)
			if err != nil {
				t.Fatalf("TOTAL amount %q: %v", r["amount"], err)
			}
			if math.Abs(got-totalAmount) > 1e-9 {
				t.Fatalf("TOTAL amount %v, want %v", got, totalAmount)
			}
		}
	}
	if seenModels != len(byModel) {
		t.Fatalf("summary.csv has %d model groups, rows export has %d", seenModels, len(byModel))
	}
	// The spec's summary export carries the host and day blocks too.
	blockSeen := map[string]bool{}
	for _, r := range recs {
		blockSeen[r["group_by"]] = true
	}
	for _, b := range []string{"model", "host", "day", "total"} {
		if !blockSeen[b] {
			t.Fatalf("summary.csv has no %q block:\n%s", b, sumBody)
		}
	}
}

// --- scenario 33 ------------------------------------------------------------

// TestAcceptance_33_CatalogApplyWritesRates: a usage name the catalogue
// matches exactly is offered on the Prices screen; applying it writes the
// rates into the configuration, the total updates and the export carries them.
func TestAcceptance_33_CatalogApplyWritesRates(t *testing.T) {
	h := testutil.Start(t)
	up := testutil.NewUpstream(t, "openai", "gpt-4o")
	h.ApplyConfig(&config.Config{Hosts: []config.Host{
		{ID: "solo", HostAddresses: []string{"127.0.0.1"}, Servers: []config.Server{
			{ID: "api", Port: up.Port(), API: "openai"},
		}},
	}})
	h.WaitForModel("gpt-4o-openai@solo")
	dashChat(t, h, "gpt-4o-openai@solo")

	section := dashNoPriceSection(t, h)
	if !strings.Contains(section, "gpt-4o") {
		t.Fatalf("Prices screen does not offer the name:\n%s", section)
	}
	if !strings.Contains(section, "openai") || !strings.Contains(section, "Apply") {
		t.Fatalf("Prices screen shows no catalogue offer with an Apply button:\n%s", section)
	}
	before := h.ConfigYAML()
	if strings.Contains(before, "prices") {
		t.Fatalf("precondition: no prices yet:\n%s", before)
	}

	st, body := h.CSRFPost("/dashboard/action/catalog/apply", map[string]any{
		"provider": "openai", "model": "gpt-4o", "h": dashHash(t, h),
	})
	if st != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("catalog/apply: %d %s", st, body)
	}

	exported := h.ConfigYAML()
	if !strings.Contains(exported, "gpt-4o") {
		t.Fatalf("export carries no gpt-4o rates:\n%s", exported)
	}
	var doc config.Config
	if err := yaml.Unmarshal([]byte(exported), &doc); err != nil {
		t.Fatalf("export no longer parses: %v\n%s", err, exported)
	}
	if doc.Prices == nil || doc.Prices.Currency != "USD" || len(doc.Prices.Models) != 1 {
		t.Fatalf("prices section = %+v", doc.Prices)
	}
	entry := doc.Prices.Models[0]
	if entry.Model != "gpt-4o" || *entry.Input != 2.50 || *entry.Output != 10.00 {
		t.Fatalf("applied entry = %+v, want the catalogue rates 2.50 / 10.00", entry)
	}
	if entry.CachedInput == nil || *entry.CachedInput != 1.25 {
		t.Fatalf("applied cached_input = %v, want 1.25", entry.CachedInput)
	}

	s := dashSummaryAt(t, h, "group_by=model")
	g := s.group(t, "gpt-4o")
	if !g.Priced || g.NoPrice {
		t.Fatalf("gpt-4o still unpriced after apply: %+v", g)
	}
	want := dashAmount(111, 42, 2.50, 10.00)
	if !dashNear(s.GrandTotal.Amount, want) || s.GrandTotal.Amount <= 0 {
		t.Fatalf("grand total = %v, want %v", s.GrandTotal.Amount, want)
	}
	if len(s.NoPriceNames) != 0 {
		t.Fatalf("no_price_models = %v after the apply", s.NoPriceNames)
	}
	if strings.Contains(dashNoPriceSection(t, h), "gpt-4o") {
		t.Fatal("gpt-4o is still listed as no price set")
	}
}
