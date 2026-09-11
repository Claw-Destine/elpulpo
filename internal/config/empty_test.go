package config_test

import (
	"testing"

	"gopkg.in/yaml.v3"

	"elpulpo/internal/config"
)

// Empty is what the dashboard's "configure your first host" banner keys off, so
// the load-balancer section has to count: a document that already carries a
// route is configured even with no hosts yet, while an empty section is the
// same as no section at all.
func TestEmptyAccountsForLoadBalancer(t *testing.T) {
	// Decoded straight from a document (the way the dashboard reads one back),
	// because a route member must name a configured server: ParseAndValidate
	// can never hand back a route without hosts, so the banner decision is
	// taken on the decoded document.
	decode := func(t *testing.T, src string) *config.Config {
		t.Helper()
		var c config.Config
		if err := yaml.Unmarshal([]byte(src), &c); err != nil {
			t.Fatalf("%q: %v", src, err)
		}
		return &c
	}

	routes := "loadbalancer:\n    routes:\n    -   alias: qwen\n        models:\n        -   model: qwen3.8:27b-ollama@minion1\n"
	if c := decode(t, routes); c.Empty() {
		t.Fatalf("a document whose only content is a route is configured, not empty")
	}
	// Same thing built in memory, so the rule does not hinge on the decoder.
	if c := (&config.Config{LoadBalancer: &config.LoadBalancer{
		Routes: []config.Route{{Alias: "qwen", Members: []config.RouteMember{{Model: config.PublishID("qwen3.8:27b", "ollama", "minion1")}}}},
	}}); c.Empty() {
		t.Fatal("a config carrying a route must not report itself empty")
	}

	// An empty load-balancer section carries nothing, exactly like absent ones.
	for _, src := range []string{
		"loadbalancer:\n",
		"hosts: []\nloadbalancer:\n    routes: []\n",
		"hosts: []\nprices:\n    currency: USD\n    models: []\nloadbalancer:\n    routes: []\n",
	} {
		if !parse(t, src).Empty() {
			t.Fatalf("%q: an empty loadbalancer section must stay empty", src)
		}
	}

	// A freshly configured, untouched document is empty.
	if c := (&config.Config{}); !c.Empty() {
		t.Fatal("an unconfigured config must be empty")
	}
	if c := decode(t, ""); !c.Empty() {
		t.Fatal("an undecoded document must be empty")
	}
}
