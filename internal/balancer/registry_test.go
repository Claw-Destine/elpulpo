package balancer

import "testing"

// The registry is consulted on every request and every admission, from
// goroutines that know nothing about each other, so its edges are worth
// pinning apart from the happy path: what a stray release does, whether a
// snapshot can be written back into it, and whether it works when it was never
// wired up at all.

func TestRegistrySurvivesAnUnpairedRelease(t *testing.T) {
	r := NewRegistry()
	// A decrement with no matching increment should never resurrect a key or
	// count "backwards" into negative load — the proxy's defer runs once per
	// request, so an unpaired release means a bug elsewhere, and the balancer
	// must not then start preferring the id it is supposed to be avoiding.
	r.Add("qwen3.8:27b-ollama@minion1", -1)
	if got := r.InFlight("qwen3.8:27b-ollama@minion1"); got != 0 {
		t.Fatalf("an unpaired release must not produce load %d", got)
	}
	if _, ok := r.Counts()["qwen3.8:27b-ollama@minion1"]; ok {
		t.Fatal("an id that never ran must stay out of the snapshot")
	}
	r.Add("qwen3.8:27b-ollama@minion1", 1)
	r.Add("qwen3.8:27b-ollama@minion1", -1)
	r.Add("qwen3.8:27b-ollama@minion1", -1)
	if got := r.InFlight("qwen3.8:27b-ollama@minion1"); got != 0 {
		t.Fatalf("load must stop at zero, got %d", got)
	}
}

func TestRegistrySnapshotIsACopy(t *testing.T) {
	r := NewRegistry()
	r.Add("a", 2)
	snap := r.Counts()
	snap["a"] = 99
	snap["b"] = 7
	if r.InFlight("a") != 2 {
		t.Fatalf("writing the snapshot changed the registry: %d", r.InFlight("a"))
	}
	if r.InFlight("b") != 0 {
		t.Fatal("a snapshot key must not invent traffic")
	}
	// And the snapshot is a moment in time, not a live view: later traffic must
	// not appear in a map already handed to a caller.
	r.Add("a", 1)
	if snap["a"] != 99 {
		t.Fatalf("the old snapshot moved under its owner: %v", snap["a"])
	}
	if r.InFlight("a") != 3 {
		t.Fatalf("the registry itself must keep counting: %d", r.InFlight("a"))
	}
}

// The proxy and the dashboard both hold a *Registry through Deps; a construction
// path that leaves it nil must not take the process down on the first request.
func TestNilRegistryIsSafeToUse(t *testing.T) {
	var r *Registry
	r.Add("a", 1) // must not panic
	if r.InFlight("a") != 0 {
		t.Fatal("a nil registry reports no traffic")
	}
	if got := r.Counts(); got == nil || len(got) != 0 {
		t.Fatalf("a nil registry must answer with an empty map, got %v", got)
	}
}
