package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"elpulpo/internal/config"
)

const fullDoc = `hosts:
-   host_addresses:
    - 192.168.1.101
    - 10.8.0.5
    id: minion1
    description: "Rack box 1"
    servers:
    -   port: 11434
        api: ollama
        id: ollama
        description: "GPU inference"
    -   port: 8000
        api: openai
        id: vllm
        scheme: https
        auth_token: "s3cret"
        max_concurrency: 4
prices:
    currency: USD
    models:
    -   model: qwen3.8:27b
        aliases: [qwen27, Qwen3-27B]
        input: 0.25
        output: 1.00
        cached_input: 0.10
        reasoning_output: 1.00
loadbalancer:
    routes:
    -   alias: Turbo
        description: "Prefer the vllm port"
        models:
        -   model: qwen3.8:27b-vllm@minion1
        -   model: gpt-oss:120b-vllm@minion1
            cost: 3
    -   alias: auto
        models:
        -   model: qwen3.8:27b-ollama@minion1
            cost: 2.5
`

func parse(t *testing.T, src string) *config.Config {
	t.Helper()
	cfg, v := config.ParseAndValidate([]byte(src))
	if len(v) > 0 {
		t.Fatalf("unexpected violations: %v", v)
	}
	return cfg
}

// The canonical export materialises defaults, sorts everything and is
// byte-stable (scenario 24's foundation).
func TestCanonicalRoundTrip(t *testing.T) {
	cfg := parse(t, fullDoc)
	first := config.Canonical(cfg)
	cfg2 := parse(t, string(first))
	second := config.Canonical(cfg2)
	if string(first) != string(second) {
		t.Fatalf("not byte-stable:\n%s\n---\n%s", first, second)
	}
	out := string(first)
	for _, want := range []string{"max_concurrency: 0", "scheme: http", `auth_token: ""`, "- Qwen3-27B", "- qwen27"} {
		if !strings.Contains(out, want) {
			t.Fatalf("canonical output must materialise %q:\n%s", want, out)
		}
	}
	golden := filepath.Join("testdata", "canonical.golden")
	if b, err := os.ReadFile(golden); err == nil {
		if string(b) != out {
			t.Fatalf("golden mismatch:\ngot:\n%s\nwant:\n%s", out, b)
		}
	} else {
		if err := os.WriteFile(golden, []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("golden written: regenerate to confirm")
	}
	// Re-parsing the canonical form yields the same config.
	if _, v := config.ParseAndValidate(first); len(v) > 0 {
		t.Fatalf("canonical output invalid: %v", v)
	}
}

// A hosts-only document exports without any prices key (scenario 45).
func TestCanonicalOmitsEmptyPrices(t *testing.T) {
	cfg := parse(t, "hosts:\n-   host_addresses: [a]\n    id: h\n    servers: []\n")
	if b := config.Canonical(cfg); strings.Contains(string(b), "prices") {
		t.Fatalf("prices must be omitted:\n%s", b)
	}
	cfg.Prices = &config.Prices{Currency: "USD"}
	if b := config.Canonical(cfg); strings.Contains(string(b), "prices") {
		t.Fatalf("prices with no entries must still be omitted:\n%s", b)
	}
}

// An empty document is a valid empty configuration.
func TestEmptyConfig(t *testing.T) {
	for _, src := range []string{"", "{}", "hosts: []\n"} {
		cfg, v := config.ParseAndValidate([]byte(src))
		if len(v) > 0 {
			t.Fatalf("%q: %v", src, v)
		}
		if cfg == nil || !cfg.Empty() {
			t.Fatalf("%q: not empty", src)
		}
	}
}

// Validation reports every violation with its path and line in one pass
// (scenarios 11, 22, 25).
func TestValidationPathsAndLines(t *testing.T) {
	bad := `hosts:
-   host_addresses: [a.example]
    id: one
    servers:
    -   port: 11434
        api: ollama
        id: s1
-   host_addresses: [a.example]
    id: one
    servers:
    -   port: 11434
        api: ollama
        id: s1
        postfix: fast
    -   port: 8000
        api: anthropic
        id: s1
`
	_, v := config.ParseAndValidate([]byte(bad))
	if len(v) == 0 {
		t.Fatal("expected violations")
	}
	joined := ""
	for _, x := range v {
		joined += x.Error() + "\n"
	}
	for _, want := range []string{
		`duplicate host id`,
		`hosts[1].host_addresses[0]`,
		`hosts[1].servers[1].api`,
		`unsupported adapter "anthropic"`,
		`duplicate server id`,
		`hosts[1].servers[0].postfix`,
		`no longer supported`,
		"(line ",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("violations must contain %q, got:\n%s", want, joined)
		}
	}
}

func TestPublishedIDGrammar(t *testing.T) {
	base, seg, host, ok := config.SplitPublishedID("qwen3.8:27b-ollama@minion1")
	if !ok || base != "qwen3.8:27b" || seg != "ollama" || host != "minion1" {
		t.Fatalf("split: %q %q %q %v", base, seg, host, ok)
	}
	base, seg, host, ok = config.SplitPublishedID("gpt-oss:120b-vllm@minion2")
	if !ok || base != "gpt-oss:120b" || seg != "vllm" || host != "minion2" {
		t.Fatalf("hyphen-in-name split: %q %q %q %v", base, seg, host, ok)
	}
	if _, _, _, ok := config.SplitPublishedID("noatsign"); ok {
		t.Fatal("bare name must not parse")
	}
	if got := config.BaseModelOf("qwen3.8:27b-vllm@minion1"); got != "qwen3.8:27b" {
		t.Fatalf("base: %q", got)
	}
}

func TestYAMLTags(t *testing.T) {
	var c config.Config
	if err := yaml.Unmarshal([]byte(fullDoc), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Hosts) != 1 || c.Hosts[0].Servers[0].NameSegment() != "ollama" ||
		c.Hosts[0].Servers[1].NameSegment() != "vllm" {
		t.Fatalf("decode: %+v", c)
	}
	if c.Prices == nil || c.Prices.Models[0].Aliases[0] != "qwen27" {
		t.Fatalf("prices: %+v", c.Prices)
	}
}
