// Package balancer turns the load-balancer section of the configuration into
// routing decisions. It counts how many requests are in flight to each
// published model id and picks, among the members of a route that can answer
// right now, the one whose in-flight *cost* is smallest — cost being what one
// in-flight connection to that member is charged, so a machine carrying a
// smaller cost takes proportionally more traffic.
package balancer

import (
	"sort"
	"sync"

	"elpulpo/internal/config"
	"elpulpo/internal/health"
)

// Registry counts in-flight requests per published model id. Every routed
// request lands here — addressed directly or through a route alias — so a
// machine kept busy by direct traffic is visibly busy to the routes that
// include it.
type Registry struct {
	mu sync.Mutex
	n  map[string]int64
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{n: map[string]int64{}} }

// Add tracks one connection starting (+1) or finishing (-1) on a published id.
func (r *Registry) Add(id string, delta int64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.n == nil {
		r.n = map[string]int64{}
	}
	r.n[id] += delta
	if r.n[id] <= 0 {
		// An idle id is absent rather than zero: the map holds only what is
		// busy, so it cannot grow with the fleet's history.
		delete(r.n, id)
	}
	r.mu.Unlock()
}

// InFlight is how many requests are running against a published id now.
func (r *Registry) InFlight(id string) int64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n[id]
}

// Counts snapshots the busy ids, for the dashboard and the JSON API.
func (r *Registry) Counts() map[string]int64 {
	if r == nil {
		return map[string]int64{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int64, len(r.n))
	for k, v := range r.n {
		out[k] = v
	}
	return out
}

// Member is one route member as the balancer sees it right now: its cost,
// where a request for it would go, and the load that makes the choice.
type Member struct {
	Model    string         `json:"model"`     // published id, as configured
	Cost     float64        `json:"cost"`      // units of load one in-flight connection costs
	InFlight int64          `json:"in_flight"` // requests running against this id now
	Load     float64        `json:"load"`      // Cost × InFlight — the quantity the policy equalises
	Target   *health.Target `json:"-"`         // nil while nothing routes this id

	// Preferred is set on the member the policy picked.
	Preferred bool `json:"preferred"`
}

// Available reports whether this member can take a request right now.
func (m Member) Available() bool { return m.Target != nil }

// Route is one configured alias, compiled against live health and in-flight
// state. Members keep the configured order: it is the preference order.
type Route struct {
	Alias       string
	Description string
	Members     []Member
}

// Serving reports whether at least one member can answer, which is what makes
// the alias a name GET /v1/models publishes.
func (r Route) Serving() bool {
	for _, m := range r.Members {
		if m.Available() {
			return true
		}
	}
	return false
}

// pick applies the policy across the members that can answer: the request goes
// to the one carrying the least in-flight cost, and when several carry the
// same — including the case where nothing is in flight at all, so every load
// is 0 — to the earliest one on the list.
func (r Route) pick() (int, bool) {
	best, bestLoad := -1, 0.0
	for i, m := range r.Members {
		if !m.Available() {
			continue
		}
		if best < 0 || m.Load < bestLoad {
			best, bestLoad = i, m.Load
		}
	}
	return best, best >= 0
}

// Selector compiles routes and picks a member, reading live health state and
// the in-flight registry.
type Selector struct {
	health *health.Manager
	regs   *Registry
}

// NewSelector wires the selector to the route catalogue and the registry.
func NewSelector(m *health.Manager, regs *Registry) *Selector {
	return &Selector{health: m, regs: regs}
}

// compile turns configured members into live ones.
func (s *Selector) compile(r *config.Route) Route {
	out := Route{Alias: r.Alias, Description: r.Description}
	for _, m := range r.Members {
		cost := m.CostOrDefault()
		in := s.regs.InFlight(m.Model)
		cand := Member{Model: m.Model, Cost: cost, InFlight: in, Load: cost * float64(in)}
		if t, ok := s.health.Route(m.Model); ok && t != nil && t.State.Up() && t.State.ActiveAddr() != "" {
			cand.Target = t
		}
		out.Members = append(out.Members, cand)
	}
	return out
}

// Resolve compiles one route and returns it with the member the policy picks.
// ok=false means the alias is configured but no member can answer right now.
func (s *Selector) Resolve(cfg *config.Config, alias string) (Route, Member, bool) {
	r, found := cfg.RouteFor(alias)
	if !found {
		return Route{}, Member{}, false
	}
	rt := s.compile(r)
	i, ok := rt.pick()
	if !ok {
		return rt, Member{}, false
	}
	rt.Members[i].Preferred = true
	return rt, rt.Members[i], true
}

// Aliases lists the route aliases that can answer right now, sorted — the
// names of serving routes, which is exactly what GET /v1/models adds to the
// published ids of healthy servers.
func (s *Selector) Aliases(cfg *config.Config) []string {
	var out []string
	rs := cfg.Routes()
	for i := range rs {
		if s.compile(&rs[i]).Serving() {
			out = append(out, rs[i].Alias)
		}
	}
	sort.Strings(out)
	return out
}

// View compiles every configured route, in the order the configuration
// holds them, for the dashboard and the JSON API.
func (s *Selector) View(cfg *config.Config) []Route {
	rs := cfg.Routes()
	out := make([]Route, 0, len(rs))
	for i := range rs {
		rt := s.compile(&rs[i])
		if j, ok := rt.pick(); ok {
			rt.Members[j].Preferred = true
		}
		out = append(out, rt)
	}
	return out
}
