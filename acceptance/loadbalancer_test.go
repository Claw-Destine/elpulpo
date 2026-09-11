package acceptance_test

// Load balancing: a route alias is a name in GET /v1/models that stands in
// front of several published models. Each member carries a cost — what one
// in-flight connection to it is worth — and the balancer keeps the in-flight
// cost equal across the members that can answer, preferring the first model on
// the list while nothing is running.
//
// These scenarios own the four things the feature has to guarantee: the alias
// is part of the request vocabulary (50), the split follows the costs and not
// the connection count (51), a dark member steps aside without taking the name
// down with it (52), and the Load balancer screen builds, reorders and removes
// routes through the same validation every other save goes through (53).

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"elpulpo/internal/config"
	"elpulpo/internal/testutil"
)

// --- helpers ---------------------------------------------------------------

func lbCost(f float64) *float64 { return &f }

// lbFleet is two hosts serving the same base model, which is the situation a
// route exists for: minion1 is the fast box (cost 1), minion2 the slow one
// (cost 3).
type lbFleet struct {
	fast, slow *testutil.Upstream
	idFast     string
	idSlow     string
}

func lbStart(t *testing.T, h *testutil.Harness, members []config.RouteMember) *lbFleet {
	t.Helper()
	fast := testutil.NewUpstreamOn(t, "127.0.0.1", "ollama", "qwen3.8:27b")
	slow := testutil.NewUpstreamOn(t, "127.0.0.2", "ollama", "qwen3.8:27b")
	cfg := &config.Config{
		Hosts: []config.Host{
			testutil.Host("minion1", []string{"127.0.0.1"}, testutil.Server("ollama", fast.Port(), "ollama")),
			testutil.Host("minion2", []string{"127.0.0.2"}, testutil.Server("ollama", slow.Port(), "ollama")),
		},
	}
	if members != nil {
		cfg.LoadBalancer = &config.LoadBalancer{Routes: []config.Route{{
			Alias: "qwen", Description: "Qwen on whichever box is free", Members: members,
		}}}
	}
	h.ApplyConfig(cfg)
	f := &lbFleet{fast: fast, slow: slow,
		idFast: "qwen3.8:27b-ollama@minion1", idSlow: "qwen3.8:27b-ollama@minion2"}
	h.WaitForModel(f.idFast)
	h.WaitForModel(f.idSlow)
	return f
}

// lbChat posts a chat against the alias, or any other model name.
func lbChat(t *testing.T, h *testutil.Harness, model string) (int, string) {
	t.Helper()
	return h.Chat("", map[string]any{
		"model":    model,
		"messages": []any{map[string]string{"role": "user", "content": "hi"}},
	})
}

func lbInFlight(h *testutil.Harness, f *lbFleet) (int64, int64) {
	c := h.App.Inflight.Counts()
	return c[f.idFast], c[f.idSlow]
}

// heldStream starts a streaming request that stays in flight: the fake upstream
// stalls after its chunks and holds the response open. The returned channel
// yields "" (or an error text) once the request has fully finished.
func heldStream(t *testing.T, h *testutil.Harness, model string) <-chan string {
	t.Helper()
	done := make(chan string, 1)
	body, _ := json.Marshal(map[string]any{
		"model": model, "stream": true,
		"messages": []any{map[string]string{"role": "user", "content": "hi"}},
	})
	go func() {
		req, err := http.NewRequest(http.MethodPost, h.URL("/v1/chat/completions"), strings.NewReader(string(body)))
		if err != nil {
			done <- err.Error()
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- err.Error()
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			done <- string(b)
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body) // ends when the hold is released
		done <- ""
	}()
	return done
}

// lbRoute is one route as the JSON feed reports it. Reading the screen through
// this rather than its markup lets an assertion be about the numbers — a
// withdrawn member keeping its cost and its zero load is a fact about the data,
// not about a word in a table cell.
type lbRoute struct {
	Serving bool `json:"serving"`
	Members []struct {
		Model     string `json:"model"`
		Cost      string `json:"cost"`
		InFlight  int64  `json:"in_flight"`
		Load      string `json:"load"`
		Available bool   `json:"available"`
		Preferred bool   `json:"preferred"`
	} `json:"members"`
}

func lbRouteFeed(t *testing.T, h *testutil.Harness, alias string) lbRoute {
	t.Helper()
	var v struct {
		Routes []struct {
			Alias string `json:"alias"`
			lbRoute
		} `json:"routes"`
	}
	_, body := h.Get("/api/loadbalancer")
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("/api/loadbalancer: %v\n%s", err, body)
	}
	for _, r := range v.Routes {
		if r.Alias == alias {
			return r.lbRoute
		}
	}
	t.Fatalf("route %q is missing from /api/loadbalancer: %s", alias, body)
	return lbRoute{}
}

// lbLive is the Load balancer screen's live region; lbForms the editor, whose
// rows carry the config hash the next save is guarded with.
func lbLive(t *testing.T, h *testutil.Harness) string {
	t.Helper()
	st, body := h.Get("/dashboard/part/loadbalancer")
	if st != 200 {
		t.Fatalf("GET part/loadbalancer: %d %s", st, body)
	}
	return body
}

func lbForms(t *testing.T, h *testutil.Harness) string {
	t.Helper()
	st, body := h.Get("/dashboard/part/loadbalancer?scope=forms")
	if st != 200 {
		t.Fatalf("GET part/loadbalancer?scope=forms: %d %s", st, body)
	}
	return body
}

func lbHas(t *testing.T, html, needle string, want bool) {
	t.Helper()
	if got := strings.Contains(html, needle); got != want {
		t.Fatalf("screen contains %q = %v, want %v:\n%s", needle, got, want, html)
	}
}

func lbHasModel(h *testutil.Harness, id string) bool {
	for _, m := range h.ModelIDs() {
		if m == id {
			return true
		}
	}
	return false
}

// lbForm posts a form-encoded dashboard mutation — what htmx sends, repeated
// keys and all — and keeps the response headers.
func lbForm(t *testing.T, h *testutil.Harness, path string, vals url.Values) (int, string, http.Header) {
	t.Helper()
	if h.CsrfC == "" {
		h.IssueCSRF()
	}
	req, err := http.NewRequest(http.MethodPost, h.URL(path), strings.NewReader(vals.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Csrf-Token", h.CsrfC)
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

// lbRouteForm is the route form as the browser submits it: one model and one
// cost field per row, in screen order.
func lbRouteForm(hash, alias, description string, models, costs []string) url.Values {
	v := url.Values{}
	v.Set("h", hash)
	v.Set("alias", alias)
	v.Set("description", description)
	for i := range models {
		v.Add("model", models[i])
		if i < len(costs) {
			v.Add("cost", costs[i])
		}
	}
	return v
}

// --- 50 --------------------------------------------------------------------

// The alias joins the published ids: GET /v1/models is the whole requestable
// vocabulary, and while nothing is in flight the first model on the list serves.
func TestAcceptance_50_RouteAliasIsRequestable(t *testing.T) {
	h := testutil.Start(t)
	f := lbStart(t, h, []config.RouteMember{
		{Model: "qwen3.8:27b-ollama@minion1", Cost: lbCost(1)},
		{Model: "qwen3.8:27b-ollama@minion2", Cost: lbCost(3)},
	})
	h.Eventually(9*time.Second, "alias qwen published", func() bool { return lbHasModel(h, "qwen") })

	ids := h.ModelIDs()
	if len(ids) != 3 {
		t.Fatalf("want 2 published ids + the alias, got %v", testutil.ModelNames(ids))
	}
	for i := 1; i < len(ids); i++ {
		if ids[i-1] > ids[i] {
			t.Fatalf("ids must stay sorted with the alias in: %v", testutil.ModelNames(ids))
		}
	}

	// Nothing in flight: every request takes the first member, cost ignored.
	for i := 0; i < 3; i++ {
		st, body := lbChat(t, h, "qwen")
		if st != 200 {
			t.Fatalf("chat via alias: %d %s\nlog:\n%s", st, body, h.LogString())
		}
		// Response identity: the client asked for the alias and sees it back.
		if !strings.Contains(body, `"model":"qwen"`) {
			t.Fatalf("response must carry the alias it was asked for: %s", body)
		}
	}
	if slow := len(f.slow.ChatBodies()); slow != 0 {
		t.Fatalf("an idle fleet must use the first member only, slow box got %d", slow)
	}
	if fast := len(f.fast.ChatBodies()); fast != 3 {
		t.Fatalf("want 3 requests on the first member, got %d", fast)
	}

	// The published ids still work on their own, untouched by the alias.
	if st, body := lbChat(t, h, f.idSlow); st != 200 {
		t.Fatalf("direct published id: %d %s", st, body)
	}
	if n := len(f.slow.ChatBodies()); n != 1 {
		t.Fatalf("direct request must reach its own server, got %d there", n)
	}

	// Usage records the member that served, not the alias: savings key off the
	// model that was actually running, and the log names both.
	h.DrainWriter()
	var servedFast, servedSlow int
	for _, r := range h.Rows() {
		switch r.Model {
		case f.idFast:
			servedFast++
		case f.idSlow:
			servedSlow++
		default:
			t.Fatalf("usage row names an unexpected model %q (the alias is never the row's model)", r.Model)
		}
	}
	if servedFast != 3 || servedSlow != 1 {
		t.Fatalf("rows must name the serving member: fast=%d slow=%d", servedFast, servedSlow)
	}
	// Exactly the balanced requests name their route. A request aimed at a
	// published id has no route to name, and must not grow an empty one: the log
	// is how an operator tells the two kinds of request apart.
	balanced, plain := 0, 0
	for _, line := range strings.Split(h.LogString(), "\n") {
		if !strings.Contains(line, `msg="chat request"`) {
			continue
		}
		if strings.Contains(line, "route=qwen") {
			balanced++
			continue
		}
		if strings.Contains(line, "route=") {
			t.Fatalf("a request that came through no route must not log one: %s", line)
		}
		plain++
	}
	if balanced != 3 || plain != 1 {
		t.Fatalf("3 balanced requests and 1 direct one must log 3 routed lines and 1 plain "+
			"line, saw %d and %d:\n%s", balanced, plain, h.LogString())
	}

	// A route may deliberately borrow a name a price entry covers. The route is
	// what makes the name routable — nothing is inferred from the price list —
	// and the row still names the member that served, which is what savings key
	// off. A pricing alias is never routable on its own; declaring a route is.
	if st, body := h.CSRFPost("/dashboard/action/prices/save", map[string]any{
		"h": h.App.Store.Current().FileHash,
		"prices": map[string]any{
			"currency": "USD",
			"models": []any{map[string]any{
				"model": "qwen3.8:27b", "aliases": []any{"qwen"},
				"input": 1.0, "output": 2.0,
			}},
		},
	}); st != 200 {
		t.Fatalf("a price entry sharing the alias's spelling: %d %s", st, body)
	}
	if st, body := lbChat(t, h, "qwen"); st != 200 {
		t.Fatalf("a route named like a pricing alias must still route: %d %s", st, body)
	}
	h.DrainWriter()
	if rows := h.Rows(); rows[len(rows)-1].Model != f.idFast {
		t.Fatalf("the row must still name the serving member, not the borrowed name: %s",
			rows[len(rows)-1].Model)
	}
}

// --- 51 --------------------------------------------------------------------

// Under load the split follows the costs, not the connection count: with costs
// 1 and 3 the fast box carries three times as many connections before the
// in-flight costs meet.
func TestAcceptance_51_InFlightCostBalancing(t *testing.T) {
	h := testutil.Start(t)
	f := lbStart(t, h, []config.RouteMember{
		{Model: "qwen3.8:27b-ollama@minion1", Cost: lbCost(1)},
		{Model: "qwen3.8:27b-ollama@minion2", Cost: lbCost(3)},
	})
	h.Eventually(9*time.Second, "alias qwen published", func() bool { return lbHasModel(h, "qwen") })

	// Both fakes stall after their chunks, so every request stays in flight and
	// the balancer has to look at the other members.
	holdFast, holdSlow := make(chan struct{}), make(chan struct{})
	f.fast.SetHold(holdFast)
	f.slow.SetHold(holdSlow)

	const n = 8
	dones := make([]<-chan string, 0, n)
	for i := 0; i < n; i++ {
		dones = append(dones, heldStream(t, h, "qwen"))
		// Admit one at a time: the balancer must see the previous request
		// before it chooses for the next one.
		want := int64(i + 1)
		h.Eventually(9*time.Second, "request admitted", func() bool {
			a, b := lbInFlight(h, f)
			return a+b == want
		})
	}

	// Greedy least-cost, admitted one by one: 6 to the cheap box, 2 to the
	// expensive one — loads 6 and 6, equal, which is the point.
	a, b := lbInFlight(h, f)
	if a != 6 || b != 2 {
		t.Fatalf("costs 1:3 must split 8 concurrent requests 6:2, got fast=%d slow=%d", a, b)
	}

	// The screen shows the same numbers the decision was made from.
	live := lbLive(t, h)
	lbHas(t, live, "qwen3.8:27b-ollama@minion2", true)
	lbHas(t, live, ">6</td>", true)
	lbHas(t, live, ">2</td>", true)

	close(holdSlow)
	close(holdFast)
	for _, d := range dones {
		if msg := <-d; msg != "" {
			t.Fatalf("held stream ended badly: %s\nlog:\n%s", msg, h.LogString())
		}
	}
	if fast, slow := len(f.fast.ChatBodies()), len(f.slow.ChatBodies()); fast != 6 || slow != 2 {
		t.Fatalf("upstreams must have seen 6/2, saw %d/%d", fast, slow)
	}
	// The upstream sees the base model name, never the alias.
	for _, body := range f.fast.ChatBodies() {
		if got := string(body["model"]); got != `"qwen3.8:27b"` {
			t.Fatalf("upstream must see the base model name, saw %s", got)
		}
	}
	a, b = lbInFlight(h, f)
	if a != 0 || b != 0 {
		t.Fatalf("in-flight must drain when the streams end, got %d/%d", a, b)
	}
}

// --- 52 --------------------------------------------------------------------

// A member that goes dark steps aside: the alias keeps serving from the rest,
// and only when every member is gone does the name leave the model list and
// answer 404 not_available.
func TestAcceptance_52_MemberFailureStepsAside(t *testing.T) {
	h := testutil.Start(t)
	f := lbStart(t, h, []config.RouteMember{
		{Model: "qwen3.8:27b-ollama@minion1", Cost: lbCost(1)},
		{Model: "qwen3.8:27b-ollama@minion2", Cost: lbCost(3)},
	})
	h.Eventually(9*time.Second, "alias qwen published", func() bool { return lbHasModel(h, "qwen") })

	if st, body := lbChat(t, h, "qwen"); st != 200 {
		t.Fatalf("first request: %d %s", st, body)
	}
	if n := len(f.fast.ChatBodies()); n != 1 {
		t.Fatalf("preference order must serve first, fast box saw %d", n)
	}

	f.fast.Close()
	h.WaitForNoModel(f.idFast)
	if !lbHasModel(h, "qwen") {
		t.Fatalf("the alias stays published while a member can still answer: %v",
			testutil.ModelNames(h.ModelIDs()))
	}
	h.Eventually(9*time.Second, "requests move to the surviving member", func() bool {
		st, _ := lbChat(t, h, "qwen")
		return st == 200
	})
	if len(f.slow.ChatBodies()) == 0 {
		t.Fatal("the surviving member must be serving")
	}
	// The withdrawn member is still named on the Load balancer screen, with its
	// cost and a zero load: the operator sees why the traffic moved instead of
	// wondering, and sees that the route has not quietly forgotten the machine.
	lbHas(t, lbLive(t, h), "withdrawn", true)
	dark := lbRouteFeed(t, h, "qwen")
	if !dark.Serving {
		t.Fatal("the route serves from the survivor, so its alias stays published")
	}
	if len(dark.Members) != 2 {
		t.Fatalf("a member that cannot answer keeps its row, saw %d rows", len(dark.Members))
	}
	for _, m := range dark.Members {
		if m.Model != f.idFast {
			continue
		}
		if m.Available {
			t.Fatalf("the dark member must read as unavailable: %+v", m)
		}
		if m.Cost != "1" || m.InFlight != 0 || m.Load != "0" {
			t.Fatalf("the withdrawn member must keep its cost with a zero load: %+v", m)
		}
	}

	// The published id of the dark box still answers not_available, alias or
	// no alias: routing did not forget it exists.
	st, body := lbChat(t, h, f.idFast)
	if st != 404 || !strings.Contains(body, "not_available") {
		t.Fatalf("a dark published id must answer not_available, got %d %s", st, body)
	}

	f.slow.Close()
	h.WaitForNoModel(f.idSlow)
	h.Eventually(9*time.Second, "alias withdrawn with no member left", func() bool {
		return !lbHasModel(h, "qwen")
	})
	st, body = lbChat(t, h, "qwen")
	if st != 404 || !strings.Contains(body, "not_available") {
		t.Fatalf("a route with no serving member is known but unavailable, got %d %s", st, body)
	}
	if strings.Contains(body, "not_configured") {
		t.Fatalf("the alias is configured, so not_configured would be wrong: %s", body)
	}
	// A name that exists nowhere is still an unknown id.
	st, body = lbChat(t, h, "nosuchthing")
	if st != 404 || !strings.Contains(body, "not_configured") {
		t.Fatalf("unknown name must be not_configured, got %d %s", st, body)
	}
}

// --- 53 --------------------------------------------------------------------

// The Load balancer tab: create a route, reorder its members, delete one, and
// be refused when the route would not be routable — every mutation on the same
// hash-guarded save path the other screens use.
func TestAcceptance_53_LoadBalancerScreen(t *testing.T) {
	h := testutil.Start(t)
	f := lbStart(t, h, nil) // hosts only: no routes yet
	h.IssueCSRF()

	st, body := h.Get("/dashboard/loadbalancer")
	if st != 200 || !strings.Contains(body, "Load balancer") {
		t.Fatalf("the Load balancer page must render: %d %s", st, body)
	}
	if _, serversPage := h.Get("/dashboard/servers"); !strings.Contains(serversPage, "/dashboard/loadbalancer") {
		t.Fatal("the tab must be in the nav of every page")
	}
	lbHas(t, lbForms(t, h), "No routes yet", true)

	// A route that names a server which does not exist is refused with the
	// field-level violation, and nothing is applied.
	bad := lbRouteForm(h.App.Store.Current().FileHash, "ghost", "no such box",
		[]string{"qwen3.8:27b-vllm@minion1"}, []string{"1"})
	st, body, _ = lbForm(t, h, "/dashboard/action/route/save", bad)
	if st != 422 || !strings.Contains(body, "loadbalancer.routes[0].models[0].model") {
		t.Fatalf("an unroutable member must be refused per field, got %d %s", st, body)
	}
	if lbHasModel(h, "ghost") {
		t.Fatal("a refused route must not appear in /v1/models")
	}

	// A real route: alias, description, one member to start with.
	good := lbRouteForm(h.App.Store.Current().FileHash, "qwen", "Qwen on whichever box is free",
		[]string{"qwen3.8:27b-ollama@minion1"}, []string{"1"})
	st, body, hdr := lbForm(t, h, "/dashboard/action/route/save", good)
	if st != 200 {
		t.Fatalf("route/save: %d %s\nlog:\n%s", st, body, h.LogString())
	}
	if hdr.Get("HX-Trigger") != "elpulpo-changed" {
		t.Fatalf("a mutation must ask open tabs to reload, got %q", hdr.Get("HX-Trigger"))
	}
	h.Eventually(9*time.Second, "the new alias in /v1/models", func() bool { return lbHasModel(h, "qwen") })

	// The editor offers the published ids to pick from, with the saved route in it.
	forms := lbForms(t, h)
	lbHas(t, forms, `<option value="qwen3.8:27b-ollama@minion1">`, true)
	lbHas(t, forms, `<option value="qwen3.8:27b-ollama@minion2">`, true)
	lbHas(t, forms, `name="original_alias" value="qwen"`, true)
	lbHas(t, lbLive(t, h), "in /v1/models", true)

	// Add the second member through the blank row of the route's own form.
	v := lbRouteForm(h.App.Store.Current().FileHash, "qwen", "Qwen on whichever box is free",
		[]string{"qwen3.8:27b-ollama@minion1", "qwen3.8:27b-ollama@minion2"}, []string{"1", "3"})
	v.Set("original_alias", "qwen")
	if st, body, _ = lbForm(t, h, "/dashboard/action/route/save", v); st != 200 {
		t.Fatalf("adding a member: %d %s", st, body)
	}
	doc := h.ConfigYAML()
	if strings.Index(doc, "@minion1") > strings.Index(doc, "@minion2") {
		t.Fatalf("member order is preference order and must survive the save:\n%s", doc)
	}
	if !strings.Contains(doc, "cost: 3") {
		t.Fatalf("the cost must be in the exported document:\n%s", doc)
	}

	// The screen offers the published ids to pick from, and the live feed of
	// loads is machine-readable too — the same numbers the panel is built from.
	r := lbRouteFeed(t, h, "qwen")
	if !r.Serving || len(r.Members) != 2 {
		t.Fatalf("the feed must carry the saved route in full: %+v", r)
	}
	if r.Members[0].Cost != "1" || r.Members[1].Cost != "3" ||
		!r.Members[0].Preferred || r.Members[1].Preferred {
		t.Fatalf("an idle route must prefer its first member, with the saved costs: %+v", r.Members)
	}
	// The raw feed answers with an object even while nothing is in flight:
	// consumers should not have to special-case an idle fleet.
	if _, raw := h.Get("/api/loadbalancer"); !strings.Contains(raw, `"in_flight":{`) {
		t.Fatalf("in_flight must be an object, even while empty: %s", raw)
	}

	// A row's buttons address the row by position, never by the model id it
	// happened to hold: htmx submits the edited inputs as well, so an id-named
	// action would aim at a row the operator has already renamed.
	lbHas(t, lbForms(t, h), `name="move_up" value="0"`, true)
	lbHas(t, lbForms(t, h), `name="delete_member" value="1"`, true)

	// Renaming a route is the same upsert under its original alias, not a second
	// route that happens to look like the first.
	ren := lbRouteForm(h.App.Store.Current().FileHash, "qwen-fast", "Qwen on whichever box is free",
		[]string{"qwen3.8:27b-ollama@minion1", "qwen3.8:27b-ollama@minion2"}, []string{"1", "3"})
	ren.Set("original_alias", "qwen")
	if st, body, _ = lbForm(t, h, "/dashboard/action/route/save", ren); st != 200 {
		t.Fatalf("renaming a route: %d %s", st, body)
	}
	renamed := h.ConfigYAML()
	if strings.Count(renamed, "alias:") != 1 || strings.Contains(renamed, "alias: qwen\n") {
		t.Fatalf("a rename must replace the route, not add one:\n%s", renamed)
	}
	h.Eventually(9*time.Second, "the renamed alias in /v1/models", func() bool {
		return lbHasModel(h, "qwen-fast") && !lbHasModel(h, "qwen")
	})
	back := lbRouteForm(h.App.Store.Current().FileHash, "qwen", "Qwen on whichever box is free",
		[]string{"qwen3.8:27b-ollama@minion1", "qwen3.8:27b-ollama@minion2"}, []string{"1", "3"})
	back.Set("original_alias", "qwen-fast")
	if st, body, _ = lbForm(t, h, "/dashboard/action/route/save", back); st != 200 {
		t.Fatalf("renaming back: %d %s", st, body)
	}

	// Both new mutations are guarded like every other one: a stale hash refuses
	// before anything is written, and no CSRF token means no change at all.
	stale := lbRouteForm("not-the-current-hash", "qwen", "",
		[]string{"qwen3.8:27b-ollama@minion1"}, []string{"1"})
	if st, body, _ = lbForm(t, h, "/dashboard/action/route/save", stale); st != 409 ||
		!strings.Contains(body, "changed on disk") {
		t.Fatalf("a stale route/save must be refused, got %d %s", st, body)
	}
	// No CSRF token: the mutation is refused and nothing is written.
	csrfless, noCSRF := h.Do(http.MethodPost, "/dashboard/action/route/save",
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
		strings.NewReader(lbRouteForm(h.App.Store.Current().FileHash, "sneaky", "",
			[]string{"qwen3.8:27b-ollama@minion1"}, []string{"1"}).Encode()))
	if csrfless != 403 || !strings.Contains(noCSRF, "csrf check failed") {
		t.Fatalf("route/save without a CSRF token: %d %s", csrfless, noCSRF)
	}
	if lbHasModel(h, "sneaky") {
		t.Fatal("a request that failed the CSRF check must change nothing")
	}

	// Order moves one step per click: down on the first row swaps the two.
	v = lbRouteForm(h.App.Store.Current().FileHash, "qwen", "Qwen on whichever box is free",
		[]string{"qwen3.8:27b-ollama@minion1", "qwen3.8:27b-ollama@minion2"}, []string{"1", "3"})
	v.Set("original_alias", "qwen")
	v.Set("move_down", "0")
	if st, body, _ = lbForm(t, h, "/dashboard/action/route/save", v); st != 200 {
		t.Fatalf("moving a member: %d %s", st, body)
	}
	doc = h.ConfigYAML()
	if strings.Index(doc, "@minion2") > strings.Index(doc, "@minion1") {
		t.Fatalf("move_down must have made minion2 the preferred member:\n%s", doc)
	}
	// And the preference is what routing now follows.
	if st, body := lbChat(t, h, "qwen"); st != 200 {
		t.Fatalf("chat after reorder: %d %s", st, body)
	}
	if n := len(f.slow.ChatBodies()); n != 1 {
		t.Fatalf("the reordered first member must take the request, slow box saw %d", n)
	}
	// Up on the row that is now last moves it back to the front.
	v = lbRouteForm(h.App.Store.Current().FileHash, "qwen", "Qwen on whichever box is free",
		[]string{"qwen3.8:27b-ollama@minion2", "qwen3.8:27b-ollama@minion1"}, []string{"3", "1"})
	v.Set("original_alias", "qwen")
	v.Set("move_up", "1")
	if st, body, _ = lbForm(t, h, "/dashboard/action/route/save", v); st != 200 {
		t.Fatalf("moving a member up: %d %s", st, body)
	}
	if doc = h.ConfigYAML(); strings.Index(doc, "@minion1") > strings.Index(doc, "@minion2") {
		t.Fatalf("move_up must have returned minion1 to the front:\n%s", doc)
	}
	if st, body := lbChat(t, h, "qwen"); st != 200 {
		t.Fatalf("chat after moving up: %d %s", st, body)
	}
	if n := len(f.fast.ChatBodies()); n != 1 {
		t.Fatalf("the moved-back member must take the request, fast box saw %d", n)
	}

	// An import that only changes a route says so — under its own key, so the
	// host preview stays the honest host diff it always was.
	doc = string(config.Canonical(h.App.Store.Current().Config))
	if !strings.Contains(doc, "cost: 3") {
		t.Fatalf("expected a cost of 3 to edit in the exported document:\n%s", doc)
	}
	st, body = h.CSRFPost("/dashboard/action/config/import",
		map[string]any{"yaml": strings.Replace(doc, "cost: 3", "cost: 4", 1)})
	if st != 200 {
		t.Fatalf("config/import: %d %s", st, body)
	}
	var prev struct {
		Preview map[string][]string `json:"preview"`
	}
	if err := json.Unmarshal([]byte(body), &prev); err != nil {
		t.Fatalf("preview: %v\n%s", err, body)
	}
	if got := strings.Join(prev.Preview["changed_routes"], " "); got != "qwen" {
		t.Fatalf("changed_routes = %v, want [qwen]: %s", prev.Preview["changed_routes"], body)
	}
	if len(prev.Preview["added_hosts"]) != 0 || len(prev.Preview["removed_hosts"]) != 0 ||
		len(prev.Preview["changed_hosts"]) != 0 {
		t.Fatalf("a routes-only import must leave the host preview empty: %s", body)
	}

	// Dropping a member leaves the other; dropping the last one is refused — a
	// route that serves nothing has to be deleted, not emptied.
	v = lbRouteForm(h.App.Store.Current().FileHash, "qwen", "",
		[]string{"qwen3.8:27b-ollama@minion1", "qwen3.8:27b-ollama@minion2"}, []string{"1", "3"})
	v.Set("original_alias", "qwen")
	v.Set("delete_member", "1")
	if st, body, _ = lbForm(t, h, "/dashboard/action/route/save", v); st != 200 {
		t.Fatalf("deleting a member: %d %s", st, body)
	}
	if strings.Contains(h.ConfigYAML(), "@minion2") {
		t.Fatalf("the deleted member must be gone:\n%s", h.ConfigYAML())
	}
	v = lbRouteForm(h.App.Store.Current().FileHash, "qwen", "",
		[]string{"qwen3.8:27b-ollama@minion1"}, []string{"1"})
	v.Set("original_alias", "qwen")
	v.Set("delete_member", "0")
	if st, body, _ = lbForm(t, h, "/dashboard/action/route/save", v); st != 422 {
		t.Fatalf("emptying a route must be refused, got %d %s", st, body)
	}

	// Deleting the route takes the name out of the model list.
	del := url.Values{"h": {h.App.Store.Current().FileHash}, "alias": {"qwen"}}
	st, body, hdr = lbForm(t, h, "/dashboard/action/route/delete", del)
	if st != 200 || hdr.Get("HX-Trigger") != "elpulpo-changed" {
		t.Fatalf("route/delete: %d %s", st, body)
	}
	h.Eventually(9*time.Second, "alias removed from /v1/models", func() bool {
		return !lbHasModel(h, "qwen")
	})
	if st, body := lbChat(t, h, "qwen"); st != 404 || !strings.Contains(body, "not_configured") {
		t.Fatalf("a deleted alias is an unknown name again, got %d %s", st, body)
	}
	// The servers themselves are untouched by the whole exercise.
	if !lbHasModel(h, f.idFast) || !lbHasModel(h, f.idSlow) {
		t.Fatalf("published ids must be unaffected: %v", testutil.ModelNames(h.ModelIDs()))
	}
}

// --- 54 --------------------------------------------------------------------

// A request aimed straight at a published id is load for every route that
// contains it: the count belongs to the model, not to the route that happened
// to admit it. Two routes with opposite preference orders share one member, and
// traffic that never touched a route moves both of them.
func TestAcceptance_54_InFlightLoadIsSharedAcrossRoutes(t *testing.T) {
	h := testutil.Start(t)
	f := lbStart(t, h, []config.RouteMember{
		{Model: "qwen3.8:27b-ollama@minion1", Cost: lbCost(1)},
		{Model: "qwen3.8:27b-ollama@minion2", Cost: lbCost(1)},
	})
	// The second route offers the same two members the other way round.
	st, body, _ := lbForm(t, h, "/dashboard/action/route/save",
		lbRouteForm(h.App.Store.Current().FileHash, "qwen-second", "Same boxes, other order",
			[]string{"qwen3.8:27b-ollama@minion2", "qwen3.8:27b-ollama@minion1"}, []string{"1", "1"}))
	if st != 200 {
		t.Fatalf("second route: %d %s", st, body)
	}
	h.Eventually(9*time.Second, "both aliases published", func() bool {
		return lbHasModel(h, "qwen") && lbHasModel(h, "qwen-second")
	})

	hold := make(chan struct{})
	f.fast.SetHold(hold)
	f.slow.SetHold(hold)
	release := sync.OnceFunc(func() { close(hold) })
	defer release()

	// One request straight to the published id of the fast box, holding.
	direct := heldStream(t, h, f.idFast)
	h.Eventually(9*time.Second, "direct request in flight", func() bool {
		a, _ := lbInFlight(h, f)
		return a == 1
	})

	// Both routes report that one connection as load on their member, because
	// they are looking at the same number.
	bothInFlight := func(alias string, want map[string]int64) bool {
		var v struct {
			Routes []struct {
				Alias   string `json:"alias"`
				Members []struct {
					Model    string `json:"model"`
					InFlight int64  `json:"in_flight"`
				} `json:"members"`
			} `json:"routes"`
		}
		if _, resp := h.Get("/api/loadbalancer"); json.Unmarshal([]byte(resp), &v) != nil {
			t.Fatalf("bad /api/loadbalancer: %s", resp)
		}
		for _, r := range v.Routes {
			if r.Alias != alias {
				continue
			}
			for _, m := range r.Members {
				if m.InFlight != want[m.Model] {
					return false
				}
			}
			return true
		}
		return false
	}
	shared := map[string]int64{f.idFast: 1, f.idSlow: 0}
	h.Eventually(9*time.Second, "first route sees the direct request", func() bool {
		return bothInFlight("qwen", shared)
	})
	if !bothInFlight("qwen-second", shared) {
		t.Fatalf("the second route must see the same load: %v", shared)
	}

	// So the direct request changes what both routes decide, whichever member
	// each of them prefers: the busy box is now the busier option for both.
	if st, body := lbChat(t, h, "qwen"); st != 200 {
		t.Fatalf("chat via the first route: %d %s", st, body)
	}
	if st, body := lbChat(t, h, "qwen-second"); st != 200 {
		t.Fatalf("chat via the second route: %d %s", st, body)
	}
	release()
	if msg := <-direct; msg != "" {
		t.Fatalf("direct stream ended badly: %s", msg)
	}
	if n := len(f.fast.ChatBodies()); n != 1 {
		t.Fatalf("only the direct request may reach the fast box, saw %d", n)
	}
	if n := len(f.slow.ChatBodies()); n != 2 {
		t.Fatalf("both routes must have stepped aside to the second box, saw %d", n)
	}
}

// --- 55 --------------------------------------------------------------------

// A request waiting for a concurrency slot is already aimed at its machine, so
// it counts as load: otherwise a burst would keep choosing the box it cannot
// yet serve and queue every request behind it.
func TestAcceptance_55_QueuedRequestStillCountsAsLoad(t *testing.T) {
	h := testutil.Start(t)
	fast := testutil.NewUpstreamOn(t, "127.0.0.1", "ollama", "qwen3.8:27b")
	slow := testutil.NewUpstreamOn(t, "127.0.0.2", "ollama", "qwen3.8:27b")
	limited := testutil.Server("ollama", fast.Port(), "ollama")
	limited.MaxConcurrency = 1 // one at a time; the second waits for the slot
	cfg := &config.Config{
		Hosts: []config.Host{
			testutil.Host("minion1", []string{"127.0.0.1"}, limited),
			testutil.Host("minion2", []string{"127.0.0.2"},
				testutil.Server("ollama", slow.Port(), "ollama")),
		},
		LoadBalancer: &config.LoadBalancer{Routes: []config.Route{{
			Alias: "qwen",
			Members: []config.RouteMember{
				{Model: "qwen3.8:27b-ollama@minion1", Cost: lbCost(1)},
				{Model: "qwen3.8:27b-ollama@minion2", Cost: lbCost(1)},
			},
		}}},
	}
	h.ApplyConfig(cfg)
	f := &lbFleet{fast: fast, slow: slow,
		idFast: "qwen3.8:27b-ollama@minion1", idSlow: "qwen3.8:27b-ollama@minion2"}
	h.WaitForModel(f.idFast)
	h.WaitForModel(f.idSlow)

	holdFast, holdSlow := make(chan struct{}), make(chan struct{})
	f.fast.SetHold(holdFast)
	f.slow.SetHold(holdSlow)
	release := sync.OnceFunc(func() { close(holdFast); close(holdSlow) })
	defer release()

	// Four requests, admitted one at a time, equal costs, minion1 first:
	// 1 → minion1 (idle route, first member), 2 → minion2 (it is the free one),
	// 3 → minion1 again (loads tie at 1) where it queues behind the first, and
	// 4 must see minion1 carrying two requests and go to minion2.
	dones := make([]<-chan string, 0, 4)
	for i := 0; i < 4; i++ {
		dones = append(dones, heldStream(t, h, "qwen"))
		want := int64(i + 1)
		h.Eventually(9*time.Second, "request admitted", func() bool {
			a, b := lbInFlight(h, f)
			return a+b == want
		})
	}
	a, b := lbInFlight(h, f)
	if a != 2 || b != 2 {
		release()
		t.Fatalf("equal costs must still split 2:2 with one box queueing, got %d/%d", a, b)
	}

	// The queued request is in flight but has not reached its machine: the
	// balancer's view and the server's slot count are two different numbers, and
	// the balancer reads the first.
	if n := len(f.fast.ChatBodies()); n != 1 {
		release()
		t.Fatalf("the fast box serves one at a time, so it must have seen exactly 1 request, saw %d", n)
	}
	if n := len(f.slow.ChatBodies()); n != 2 {
		release()
		t.Fatalf("the second box must have taken the other two, saw %d", n)
	}
	lbHas(t, lbLive(t, h), ">2</td>", true)

	release()
	for _, d := range dones {
		if msg := <-d; msg != "" {
			t.Fatalf("held stream ended badly: %s\nlog:\n%s", msg, h.LogString())
		}
	}
	// Drained, and the queued request did run once the slot freed.
	if n := len(f.fast.ChatBodies()); n != 2 {
		t.Fatalf("the queued request must reach its machine when the slot frees, saw %d", n)
	}
	if a, b := lbInFlight(h, f); a != 0 || b != 0 {
		t.Fatalf("in-flight must drain to zero, got %d/%d", a, b)
	}
}
