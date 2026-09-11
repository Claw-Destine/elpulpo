package config_test

import (
	"strings"
	"testing"

	"elpulpo/internal/config"
)

// The load-balancer document, in the shape the dashboard writes it.
const lbDoc = `
hosts:
-   host_addresses: [192.168.1.101]
    id: minion1
    servers:
    -   port: 11434
        api: ollama
        id: ollama
-   host_addresses: [192.168.1.102]
    id: minion2
    servers:
    -   port: 11434
        api: ollama
        id: ollama
    -   port: 8000
        api: openai
        id: vllm
loadbalancer:
    routes:
    -   alias: zebra
        models:
        -   model: qwen3.8:27b-ollama@minion1
            cost: 1
    -   alias: qwen
        description: Qwen on whichever box is free
        models:
        -   model: qwen3.8:27b-ollama@minion1
            cost: 1
        -   model: qwen3.8:27b-ollama@minion2
            cost: 3
        -   model: deepseek-vllm-vllm@minion2
`

func TestLoadBalancerParses(t *testing.T) {
	cfg := parse(t, lbDoc)
	// Parsing keeps the document order; normalising (which every save and
	// export does) sorts routes by alias and leaves the members alone: member
	// order is the preference order the balancer reads.
	if got := cfg.Routes(); got[0].Alias != "zebra" || got[1].Alias != "qwen" {
		t.Fatalf("parser must keep document order: %v, %v", got[0].Alias, got[1].Alias)
	}
	config.Normalize(cfg)
	routes := cfg.Routes()
	if len(routes) != 2 {
		t.Fatalf("want 2 routes, got %d", len(routes))
	}
	if routes[0].Alias != "qwen" || routes[1].Alias != "zebra" {
		t.Fatalf("routes not sorted by alias: %v, %v", routes[0].Alias, routes[1].Alias)
	}
	q := routes[0]
	if q.Description == "" {
		t.Fatal("description lost")
	}
	if len(q.Members) != 3 {
		t.Fatalf("want 3 members, got %d", len(q.Members))
	}
	if q.Members[0].Model != "qwen3.8:27b-ollama@minion1" || q.Members[2].Model != "deepseek-vllm-vllm@minion2" {
		t.Fatalf("member order changed: %v", q.Members)
	}
	if got := q.Members[1].CostOrDefault(); got != 3 {
		t.Fatalf("cost 3 became %v", got)
	}
	// A member with no cost is the default of 1, not 0.
	if got := q.Members[2].CostOrDefault(); got != config.DefaultMemberCost {
		t.Fatalf("missing cost should default to 1, got %v", got)
	}
	if _, ok := cfg.RouteFor("qwen"); !ok {
		t.Fatal("RouteFor(qwen) not found")
	}
	if _, ok := cfg.RouteFor("QWEN"); ok {
		t.Fatal("route lookup must be exact")
	}
}

// The section is optional: a hosts-only document carries no routes and an
// empty one is omitted from the export (like prices).
func TestLoadBalancerSectionOptional(t *testing.T) {
	cfg := parse(t, "hosts:\n-   host_addresses: [a]\n    id: h\n    servers: []\n")
	if cfg.LoadBalancer != nil {
		t.Fatalf("absent section must stay nil: %+v", cfg.LoadBalancer)
	}
	if len(cfg.Routes()) != 0 {
		t.Fatal("no routes expected")
	}
	for _, src := range []string{"hosts: []\nloadbalancer:\n    routes: []\n", "hosts: []\nloadbalancer:\n"} {
		c := parse(t, src)
		if b := config.Canonical(c); strings.Contains(string(b), "loadbalancer") {
			t.Fatalf("an empty loadbalancer section must be omitted from the export:\n%s", b)
		}
	}
}

// Every rule that makes a route usable is reported with its path, all of them
// in one pass.
func TestLoadBalancerValidation(t *testing.T) {
	hosts := "hosts:\n-   host_addresses: [a]\n    id: minion1\n    servers:\n-   port: 1\n    api: ollama\n    id: ollama\n"
	cases := []struct {
		name, lb, wantPath, wantMsg string
	}{
		{"no alias", "loadbalancer:\n    routes:\n    -   models:\n        -   model: x-ollama@minion1\n", "loadbalancer.routes[0]", `missing required field "alias"`},
		{"alias with @", "loadbalancer:\n    routes:\n    -   alias: qwen@minion1\n        models:\n        -   model: x-ollama@minion1\n", "loadbalancer.routes[0].alias", "contain no"},
		{"alias empty", "loadbalancer:\n    routes:\n    -   alias: \"\"\n        models:\n        -   model: x-ollama@minion1\n", "loadbalancer.routes[0].alias", "must not be empty"},
		{"no models", "loadbalancer:\n    routes:\n    -   alias: qwen\n", "loadbalancer.routes[0]", `missing required field "models"`},
		{"empty models", "loadbalancer:\n    routes:\n    -   alias: qwen\n        models: []\n", "loadbalancer.routes[0].models", "non-empty"},
		{"bare model name", "loadbalancer:\n    routes:\n    -   alias: qwen\n        models:\n        -   model: qwen3.8:27b\n", "loadbalancer.routes[0].models[0].model", "published model id"},
		{"unknown host", "loadbalancer:\n    routes:\n    -   alias: qwen\n        models:\n        -   model: x-ollama@minion9\n", "loadbalancer.routes[0].models[0].model", `no host "minion9" in the hosts section`},
		{"unknown server", "loadbalancer:\n    routes:\n    -   alias: qwen\n        models:\n        -   model: x-vllm@minion1\n", "loadbalancer.routes[0].models[0].model", `no server "vllm" on host "minion1"`},
		{"zero cost", "loadbalancer:\n    routes:\n    -   alias: qwen\n        models:\n        -   model: x-ollama@minion1\n            cost: 0\n", "loadbalancer.routes[0].models[0].cost", "greater than 0"},
		{"negative cost", "loadbalancer:\n    routes:\n    -   alias: qwen\n        models:\n        -   model: x-ollama@minion1\n            cost: -2\n", "loadbalancer.routes[0].models[0].cost", "greater than 0"},
		{"duplicate member", "loadbalancer:\n    routes:\n    -   alias: qwen\n        models:\n        -   model: x-ollama@minion1\n        -   model: x-ollama@minion1\n", "loadbalancer.routes[0].models[1].model", "duplicate member model"},
		{"duplicate alias", "loadbalancer:\n    routes:\n    -   alias: qwen\n        models:\n        -   model: x-ollama@minion1\n    -   alias: Qwen\n        models:\n        -   model: x-ollama@minion1\n", "loadbalancer.routes[1].alias", "duplicate route alias"},
		{"unknown field", "loadbalancer:\n    routes:\n    -   alias: qwen\n        strategy: round-robin\n        models:\n        -   model: x-ollama@minion1\n", "loadbalancer.routes[0].strategy", "unknown field"},
		{"unknown lb field", "loadbalancer:\n    default_cost: 2\n    routes: []\n", "loadbalancer.default_cost", "unknown field"},
	}
	for _, tc := range cases {
		_, v := config.ParseAndValidate([]byte(hosts + tc.lb))
		if len(v) == 0 {
			t.Fatalf("%s: expected a violation, got none", tc.name)
		}
		var hit bool
		for _, viol := range v {
			if viol.Path == tc.wantPath && strings.Contains(viol.Msg, tc.wantMsg) {
				hit = true
				break
			}
		}
		if !hit {
			t.Fatalf("%s: want %s ~ %q, got %v", tc.name, tc.wantPath, tc.wantMsg, v)
		}
	}
}

// A duplicated alias is reported on both offending paths, like every other
// symmetric rule.
func TestLoadBalancerDuplicateAliasReportsBoth(t *testing.T) {
	src := "hosts:\n-   host_addresses: [a]\n    id: minion1\n    servers:\n-   port: 1\n    api: ollama\n    id: ollama\n" +
		"loadbalancer:\n    routes:\n    -   alias: qwen\n        models:\n        -   model: x-ollama@minion1\n" +
		"    -   alias: qwen\n        models:\n        -   model: y-ollama@minion1\n"
	_, v := config.ParseAndValidate([]byte(src))
	paths := map[string]bool{}
	for _, viol := range v {
		if strings.Contains(viol.Msg, "duplicate route alias") {
			paths[viol.Path] = true
		}
	}
	if !paths["loadbalancer.routes[0].alias"] || !paths["loadbalancer.routes[1].alias"] {
		t.Fatalf("both aliases must be reported, got %v", paths)
	}
}

// The canonical export materialises every cost, keeps the member order, and
// is byte-stable across a round trip (scenario 24's rule, applied to routes).
func TestLoadBalancerCanonical(t *testing.T) {
	cfg := parse(t, lbDoc)
	first := config.Canonical(cfg)
	again := parse(t, string(first))
	second := config.Canonical(again)
	if string(first) != string(second) {
		t.Fatalf("not byte-stable:\n%s\n---\n%s", first, second)
	}
	out := string(first)
	for _, want := range []string{"cost: 1", "cost: 3", "alias: qwen", "loadbalancer:", "routes:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("canonical output must materialise %q:\n%s", want, out)
		}
	}
	// A cost that was absent comes back as the default 1.
	if line := lineAfter(out, "model: deepseek-vllm-vllm@minion2"); line != "cost: 1" {
		t.Fatalf("an omitted cost must be materialised as 1, got %q:\n%s", line, out)
	}
	// qwen sorts before zebra, and inside qwen the members keep their order.
	if strings.Index(out, "alias: qwen") > strings.Index(out, "alias: zebra") {
		t.Fatalf("routes must be sorted by alias:\n%s", out)
	}
	i1 := strings.Index(out, "qwen3.8:27b-ollama@minion1")
	i2 := strings.Index(out, "deepseek-vllm-vllm@minion2")
	if i1 > i2 {
		t.Fatalf("member order must survive the canonical form:\n%s", out)
	}
}

// The section is only meaningful when every member names a configured server,
// so removing that server leaves an invalid document — which is why a save
// that would orphan a route is refused rather than silently applied.
func TestLoadBalancerMemberMustNameAConfiguredServer(t *testing.T) {
	cfg := parse(t, lbDoc)
	if b := config.Canonical(cfg); len(b) == 0 {
		t.Fatal("empty canonical form")
	}
	// Drop the server the last member names.
	c, err := parseRemovingVllm(t, lbDoc)
	if err != nil {
		t.Fatal(err)
	}
	_, v := config.ParseAndValidate(config.Canonical(c))
	if len(v) == 0 {
		t.Fatal("expected the orphaned member to be refused")
	}
}

// lineAfter returns the trimmed line following the one containing needle.
func lineAfter(doc, needle string) string {
	lines := strings.Split(doc, "\n")
	for i, l := range lines {
		if strings.Contains(l, needle) && i+1 < len(lines) {
			return strings.TrimSpace(lines[i+1])
		}
	}
	return ""
}

func parseRemovingVllm(t *testing.T, src string) (*config.Config, error) {
	t.Helper()
	cfg := parse(t, src)
	for i := range cfg.Hosts {
		if cfg.Hosts[i].ID != "minion2" {
			continue
		}
		out := cfg.Hosts[i].Servers[:0]
		for _, s := range cfg.Hosts[i].Servers {
			if s.ID != "vllm" {
				out = append(out, s)
			}
		}
		cfg.Hosts[i].Servers = out
	}
	return cfg, nil
}
