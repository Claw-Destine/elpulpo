package balancer

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"elpulpo/internal/config"
	"elpulpo/internal/health"
)

// The selector is the bridge between three things that change independently —
// the configured route, what health says is routable, and what the registry
// says is busy — and every one of its answers is a routing decision. The policy
// itself is pinned in balancer_test.go against hand-built routes; what is
// pinned here is the compiling: that a member is only offered when the route
// table really routes it, that the numbers on it come from the shared registry,
// and that the alias list and the dashboard feed agree with the pick.

const selBase = "qwen3.8:27b"

// stubKV keeps a SettingsManager in memory. The tests shorten the probe interval
// to make a server flip up and down inside a second, and nothing here persists
// settings anywhere.
type stubKV struct{ vals map[string]string }

func (s stubKV) GetSetting(_ context.Context, k string) (string, bool, error) {
	v, ok := s.vals[k]
	return v, ok, nil
}

func (s stubKV) SetSettings(_ context.Context, kv map[string]string) error {
	for k, v := range kv {
		s.vals[k] = v
	}
	return nil
}

// fakeUpstream is a model server: a model list that can be made to stop
// answering and made to answer again, which is the only way to move a server
// between routable and not through the real health loop.
type fakeUpstream struct {
	srv     *httptest.Server
	port    int
	failing atomic.Bool
}

func newFakeUpstream(t *testing.T, ip string, models ...string) *fakeUpstream {
	t.Helper()
	ln, err := net.Listen("tcp", ip+":0")
	if err != nil {
		t.Fatalf("listen on %s: %v", ip, err)
	}
	f := &fakeUpstream{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.failing.Load() || !strings.HasSuffix(r.URL.Path, "/api/tags") {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var b strings.Builder
		b.WriteString(`{"models":[`)
		for i, m := range models {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"name":%q}`, m)
		}
		b.WriteString(`]}`)
		io.WriteString(w, b.String())
	})
	f.srv = httptest.NewUnstartedServer(handler)
	f.srv.Listener = ln
	f.srv.Start()
	f.port = ln.Addr().(*net.TCPAddr).Port
	t.Cleanup(f.srv.Close)
	return f
}

// selectorFixture is one selector over two real servers on two hosts, with the
// health loop running at one-second probes.
type selectorFixture struct {
	sel            *Selector
	mgr            *health.Manager
	regs           *Registry
	fast, slow     *fakeUpstream
	idFast, idSlow string
}

func newSelectorFixture(t *testing.T) *selectorFixture {
	t.Helper()
	set := config.NewSettingsManager(stubKV{vals: map[string]string{}}, discardLog())
	if _, v := set.Update(context.Background(), map[string]string{
		"health_interval": "5s", "probe_timeout": "1s",
	}); len(v) != 0 {
		t.Fatalf("settings rejected: %+v", v)
	}
	mgr := health.NewManager(set, discardLog())
	mgr.Start()
	t.Cleanup(mgr.Stop)

	fast := newFakeUpstream(t, "127.0.0.1", selBase)
	slow := newFakeUpstream(t, "127.0.0.2", selBase)
	cfg := &config.Config{Hosts: []config.Host{
		{ID: "minion1", HostAddresses: []string{"127.0.0.1"},
			Servers: []config.Server{{ID: "ollama", Port: fast.port, API: "ollama"}}},
		{ID: "minion2", HostAddresses: []string{"127.0.0.2"},
			Servers: []config.Server{{ID: "ollama", Port: slow.port, API: "ollama"}}},
	}}
	mgr.Apply(&config.Snapshot{Config: cfg, Source: "test"})

	f := &selectorFixture{
		mgr:    mgr,
		regs:   NewRegistry(),
		fast:   fast,
		slow:   slow,
		idFast: config.PublishID(selBase, "ollama", "minion1"),
		idSlow: config.PublishID(selBase, "ollama", "minion2"),
	}
	f.sel = NewSelector(mgr, f.regs)
	selEventually(t, "both servers routable", func() bool {
		_, a := mgr.Route(f.idFast)
		_, b := mgr.Route(f.idSlow)
		return a && b
	})
	return f
}

// route re-applies a configuration with the given routes and hands back the
// compiled view of one alias. Re-applying is what the proxy does per request:
// the routes are read from the live document, never cached.
func (f *selectorFixture) route(t *testing.T, routes ...config.Route) *config.Config {
	t.Helper()
	cfg := &config.Config{Hosts: []config.Host{
		{ID: "minion1", HostAddresses: []string{"127.0.0.1"},
			Servers: []config.Server{{ID: "ollama", Port: f.fast.port, API: "ollama"}}},
		{ID: "minion2", HostAddresses: []string{"127.0.0.2"},
			Servers: []config.Server{{ID: "ollama", Port: f.slow.port, API: "ollama"}}},
	}}
	if len(routes) > 0 {
		cfg.LoadBalancer = &config.LoadBalancer{Routes: routes}
	}
	f.mgr.Apply(&config.Snapshot{Config: cfg, Source: "test"})
	return cfg
}

func selEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("condition never held within 15s: %s", what)
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func configuredRoute(alias string, members ...config.RouteMember) config.Route {
	return config.Route{Alias: alias, Description: "test route", Members: members}
}

func memberOf(t *testing.T, r Route, model string) Member {
	t.Helper()
	for _, m := range r.Members {
		if m.Model == model {
			return m
		}
	}
	t.Fatalf("member %q missing from the compiled route %+v", model, r)
	return Member{}
}

// A member is offered only when the route table actually routes its id and the
// server behind it is up with an address to send a request to — an id nobody
// reports, or a server still electing an address, must not be picked, because
// picking it turns a would-be balanced request into a 502.
func TestCompileOffersOnlyRoutableMembers(t *testing.T) {
	f := newSelectorFixture(t)
	ghost := config.PublishID(selBase, "ollama", "minion9") // host does not exist at all
	cfg := f.route(t, configuredRoute("qwen",
		config.RouteMember{Model: f.idFast}, // no cost: the default must materialise
		config.RouteMember{Model: ghost, Cost: ptr(3)},
	))

	r, _, ok := f.sel.Resolve(cfg, "qwen")
	if !ok {
		t.Fatal("a route with one answerable member must resolve")
	}
	fast := memberOf(t, r, f.idFast)
	if !fast.Available() || fast.Target == nil {
		t.Fatalf("a routable member must be offered: %+v", fast)
	}
	if fast.Cost != config.DefaultMemberCost {
		t.Fatalf("an omitted cost must read as the default, got %v", fast.Cost)
	}
	ghosted := memberOf(t, r, ghost)
	if ghosted.Available() || ghosted.Target != nil {
		t.Fatalf("a member nothing routes must never be offered: %+v", ghosted)
	}

	// A route whose every member is dark resolves to nothing at all: the alias
	// is withdrawn rather than aimed at a machine that cannot answer.
	_, _, ok = f.sel.Resolve(f.route(t, configuredRoute("dark",
		config.RouteMember{Model: ghost})), "dark")
	if ok {
		t.Fatal("a route with no answerable member must not resolve")
	}
	if _, _, ok := f.sel.Resolve(cfg, "never-configured"); ok {
		t.Fatal("an unknown alias must not resolve")
	}
}

// The load on a member is the registry's count for that published id: the same
// number every route containing it reads, and every request admitted through it
// moves. Two routes sharing one member must therefore agree about its load.
func TestLoadIsTheModelsNotTheRoute(t *testing.T) {
	f := newSelectorFixture(t)
	one := config.RouteMember{Model: f.idFast, Cost: ptr(1)}
	two := config.RouteMember{Model: f.idSlow, Cost: ptr(1)}
	cfg := f.route(t,
		configuredRoute("first", one, two),
		configuredRoute("second", two, one), // same members, other preference
	)

	f.regs.Add(f.idFast, 1) // one connection — from anywhere at all
	for _, alias := range []string{"first", "second"} {
		r, _, ok := f.sel.Resolve(cfg, alias)
		if !ok {
			t.Fatalf("route %q must resolve", alias)
		}
		if m := memberOf(t, r, f.idFast); m.InFlight != 1 || m.Load != 1 {
			t.Fatalf("route %q must see the shared connection: %+v", alias, m)
		}
		if m := memberOf(t, r, f.idSlow); m.InFlight != 0 {
			t.Fatalf("the other member must read as idle on %q: %+v", alias, m)
		}
	}

	// Because the count is shared, it is also what the alias list and the
	// dashboard feed are built from — nothing gets its own idea of load.
	if got := f.sel.Aliases(cfg); !slices.Equal(got, []string{"first", "second"}) {
		t.Fatalf("serving aliases = %v, want both, sorted", got)
	}
	view := f.sel.View(cfg)
	if len(view) != 2 {
		t.Fatalf("View must compile every route, got %d", len(view))
	}
	for _, r := range view {
		_, picked, ok := f.sel.Resolve(cfg, r.Alias)
		if !ok {
			t.Fatalf("route %q must resolve", r.Alias)
		}
		var preferred []string
		for _, m := range r.Members {
			if m.Preferred != (m.Model == picked.Model) {
				t.Fatalf("route %q: Preferred disagrees with Resolve on %+v", r.Alias, m)
			}
			if m.Preferred {
				preferred = append(preferred, m.Model)
			}
		}
		// Both routes have the busy box at load 1 and the idle one at 0, so the
		// idle box wins on both regardless of preference order.
		if len(preferred) != 1 || preferred[0] != f.idSlow {
			t.Fatalf("route %q must prefer the idle member, marked %v", r.Alias, preferred)
		}
	}
}

// An alias is published while any member can answer and withdrawn when none
// can: the alias list is the same computation as the pick, over every route, so
// /v1/models and the Load balancer screen cannot disagree with the proxy. A
// member that cannot answer keeps its row, unoffered. (A server actually going
// dark and coming back is the same code path driven by the health loop —
// acceptance scenario 52 walks it with a real upstream.)
func TestAliasesFollowWhatCanAnswer(t *testing.T) {
	f := newSelectorFixture(t)
	ghost := config.PublishID(selBase, "ollama", "minion9")
	cfg := f.route(t,
		configuredRoute("both",
			config.RouteMember{Model: f.idFast, Cost: ptr(1)},
			config.RouteMember{Model: f.idSlow, Cost: ptr(3)}),
		configuredRoute("darkonly", config.RouteMember{Model: ghost, Cost: ptr(1)}),
		configuredRoute("partiallydark",
			config.RouteMember{Model: ghost, Cost: ptr(1)},
			config.RouteMember{Model: f.idSlow, Cost: ptr(1)}),
	)
	if got := f.sel.Aliases(cfg); !slices.Equal(got, []string{"both", "partiallydark"}) {
		t.Fatalf("aliases = %v, want the two routes with something to answer, sorted", got)
	}
	r, picked, ok := f.sel.Resolve(cfg, "partiallydark")
	if !ok || picked.Model != f.idSlow {
		t.Fatalf("the answerable member must take the request: %+v ok=%v", picked, ok)
	}
	if m := memberOf(t, r, ghost); m.Available() || m.Preferred {
		t.Fatalf("a member nothing routes stays listed but unoffered: %+v", m)
	}

	// Nothing serving at all means nothing published: a fleet with no routes
	// answerable answers no alias.
	empty := f.route(t, configuredRoute("dark", config.RouteMember{Model: ghost}))
	if got := f.sel.Aliases(empty); len(got) != 0 {
		t.Fatalf("a route with no answerable member must not be published: %v", got)
	}
}

func ptr(f float64) *float64 { return &f }
