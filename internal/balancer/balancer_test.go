package balancer

import (
	"sync"
	"testing"

	"elpulpo/internal/health"
)

// avail/cost are the two facts the policy reads per member; the target is a
// placeholder — pick never looks inside it, only at whether one exists.
func member(model string, cost float64, inFlight int64, up bool) Member {
	m := Member{Model: model, Cost: cost, InFlight: inFlight, Load: cost * float64(inFlight)}
	if up {
		m.Target = &health.Target{Published: model}
	}
	return m
}

func route(members ...Member) Route {
	return Route{Alias: "r", Members: members}
}

// Nothing in flight: the request goes to the first model on the list, whatever
// the costs say — the preference order is the whole answer.
func TestPickPrefersFirstWhenIdle(t *testing.T) {
	r := route(member("b", 3, 0, true), member("a", 1, 0, true), member("c", 2, 0, true))
	i, ok := r.pick()
	if !ok || i != 0 {
		t.Fatalf("idle fleet must take the first member, got %d ok=%v", i, ok)
	}
}

// An idle first member that is down must not stop the route: the earliest
// member that can answer takes the request.
func TestPickSkipsUnavailable(t *testing.T) {
	r := route(member("a", 1, 0, false), member("b", 1, 0, false), member("c", 5, 0, true))
	i, _ := r.pick()
	if i != 2 {
		t.Fatalf("want the only available member (index 2), got %d", i)
	}
	r = route(member("a", 1, 0, false), member("b", 1, 4, false))
	if _, ok := r.pick(); ok {
		t.Fatal("a route with no available member must not pick")
	}
}

// The policy equalises in-flight *cost*, not connection count: with costs 1
// and 3 the cheap machine carries three times as many connections before the
// loads meet.
func TestPickEqualisesCost(t *testing.T) {
	cases := []struct {
		name string
		a, b Member
		want int
	}{
		{"cheap still lighter", member("fast", 1, 1, true), member("slow", 3, 0, true), 1},
		{"one slow request outweighs one fast", member("fast", 1, 1, true), member("slow", 3, 1, true), 0},
		{"one more fast request exactly balances", member("fast", 1, 2, true), member("slow", 3, 1, true), 0},
		{"exact tie goes to the earlier row", member("fast", 1, 3, true), member("slow", 3, 1, true), 0},
		{"overloaded fast sheds to slow", member("fast", 1, 5, true), member("slow", 3, 1, true), 1},
	}
	for _, tc := range cases {
		i, ok := route(tc.a, tc.b).pick()
		if !ok || i != tc.want {
			t.Fatalf("%s: want member %d, got %d (ok=%v)", tc.name, tc.want, i, ok)
		}
	}
}

// Following the picks through a sequence is what "keep the in-flight cost
// equal" means in practice: costs 1 and 3 settle at a 3:1 split of
// connections, because that is where the two loads meet.
func TestPickSequenceConvergesOnCostRatio(t *testing.T) {
	a, b := Member{Cost: 1}, Member{Cost: 3}
	a.Target, b.Target = &health.Target{Published: "a"}, &health.Target{Published: "b"}
	byA := 0
	for step := 0; step < 100; step++ {
		r := route(a, b)
		i, _ := r.pick()
		if i == 0 {
			a.InFlight++
			a.Load = a.Cost * float64(a.InFlight)
			byA++
		} else {
			b.InFlight++
			b.Load = b.Cost * float64(b.InFlight)
		}
	}
	if want := 75; byA < want-6 || byA > want+6 {
		t.Fatalf("100 requests at costs 1:3 should put ~75 on the cheap member, got %d", byA)
	}
}

// An unavailable member is never picked, however idle it is.
func TestPickNeverPicksDown(t *testing.T) {
	a := member("down", 1, 0, false)
	b := member("up", 9, 99, true)
	i, ok := route(a, b).pick()
	if !ok || i != 1 {
		t.Fatalf("down member must never win, got %d", i)
	}
}

func TestRegistryCounts(t *testing.T) {
	r := NewRegistry()
	if r.InFlight("x") != 0 {
		t.Fatal("a fresh registry knows nothing")
	}
	r.Add("x", 1)
	r.Add("x", 1)
	r.Add("y", 1)
	if r.InFlight("x") != 2 || r.InFlight("y") != 1 {
		t.Fatalf("counts wrong: %v", r.Counts())
	}
	r.Add("y", -1)
	if r.InFlight("y") != 0 {
		t.Fatal("y should be idle again")
	}
	if _, ok := r.Counts()["y"]; ok {
		t.Fatal("an idle id must leave the snapshot, not sit at zero")
	}
	// Concurrent traffic is the normal case here: every request increments and
	// later decrements the same id, so the counter never goes negative.
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Add("z", 1)
			r.Add("z", -1)
		}()
	}
	wg.Wait()
	if r.InFlight("z") != 0 {
		t.Fatalf("unbalanced accounting: %d", r.InFlight("z"))
	}
}

func TestRouteServing(t *testing.T) {
	if route(member("a", 1, 0, false)).Serving() {
		t.Fatal("no available member means the alias must not be published")
	}
	if !route(member("a", 1, 0, false), member("b", 1, 0, true)).Serving() {
		t.Fatal("one available member makes the alias serve")
	}
}
