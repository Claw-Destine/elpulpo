package config_test

import (
	"strings"
	"testing"

	"elpulpo/internal/config"
)

// A price alias names no endpoint — it is only an exact-match key onto its
// entry's price — so it may carry single spaces between words, letting an
// entry cover the spaced spelling of a model name ("Qwen3.8 Flash Next").
func TestPriceAliasAllowsSingleSpaces(t *testing.T) {
	priceDoc := func(alias string) string {
		return "prices:\n    currency: USD\n    models:\n    -   model: qwen3.8:27b\n" +
			"        aliases: [" + alias + "]\n        input: 1\n        output: 1\n"
	}
	for _, a := range []string{
		"qwen27",
		"Qwen3.8 Flash Next",
		"Qwen3.8-Flash_Next:27b",
		"A B",
		strings.Repeat("a", 64),
	} {
		cfg, v := config.ParseAndValidate([]byte(priceDoc(a)))
		if len(v) > 0 {
			t.Fatalf("alias %q must be accepted: %v", a, v)
		}
		if got := cfg.Prices.Models[0].Aliases; len(got) != 1 || got[0] != a {
			t.Fatalf("alias %q not stored as typed: %#v", a, got)
		}
	}
}

// The spaces allowed are word separators only: leading, trailing or doubled
// spaces (and everything the strict shape always refused) stay violations.
func TestPriceAliasSpacesStayWordSeparators(t *testing.T) {
	priceDoc := func(alias string) string {
		return "prices:\n    currency: USD\n    models:\n    -   model: qwen3.8:27b\n" +
			"        aliases: [" + alias + "]\n        input: 1\n        output: 1\n"
	}
	for _, a := range []string{
		`" qwen"`,    // leading space
		`"qwen "`,    // trailing space
		`"qwen  27"`, // doubled space
		`"Qwen3.8 Flash Next "`,
		"qwen@27",   // '@' stays out of the alias namespace
		"-qwen",     // must start alphanumeric
		`"qwen,27"`, // a comma is the list separator, never alias text
		strings.Repeat("a", 65),
	} {
		if _, v := config.ParseAndValidate([]byte(priceDoc(a))); len(v) == 0 {
			t.Fatalf("alias %s must be rejected", a)
		}
	}
}

// The violation message names the rule and quotes the offending alias.
func TestPriceAliasViolationMessage(t *testing.T) {
	src := "prices:\n    currency: USD\n    models:\n    -   model: qwen3.8:27b\n" +
		"        aliases: [\"qwen  27\"]\n        input: 1\n        output: 1\n"
	_, v := config.ParseAndValidate([]byte(src))
	if len(v) != 1 {
		t.Fatalf("one violation expected, got %v", v)
	}
	if v[0].Path != "prices.models[0].aliases[0]" ||
		!strings.Contains(v[0].Msg, "alias must match") ||
		!strings.Contains(v[0].Msg, "qwen  27") {
		t.Fatalf("unexpected violation: %+v", v[0])
	}
}

// A spaced alias survives the canonical export and reparse unchanged, and
// two spaced aliases that differ collide like any other names.
func TestSpacedPriceAliasCanonicalRoundTrip(t *testing.T) {
	src := "prices:\n    currency: USD\n    models:\n" +
		"    -   model: qwen3.8:27b\n        aliases: [Qwen3.8 Flash Next, qwen flash]\n" +
		"        input: 1\n        output: 1\n"
	cfg := parse(t, src)
	first := config.Canonical(cfg)
	if _, v := config.ParseAndValidate(first); len(v) > 0 {
		t.Fatalf("canonical export with a spaced alias must reparse: %v\n%s", v, first)
	}
	cfg2 := parse(t, string(first))
	if got := cfg2.Prices.Models[0].Aliases; len(got) != 2 ||
		got[0] != "Qwen3.8 Flash Next" && got[1] != "Qwen3.8 Flash Next" {
		t.Fatalf("spaced alias lost in round-trip: %#v\n%s", got, first)
	}
	if second := config.Canonical(cfg2); string(first) != string(second) {
		t.Fatalf("canonical export not byte-stable:\n%s\n---\n%s", first, second)
	}

	collide := "prices:\n    currency: USD\n    models:\n" +
		"    -   model: a-model\n        aliases: [Qwen3.8 Flash Next]\n        input: 1\n        output: 1\n" +
		"    -   model: b-model\n        aliases: [Qwen3.8 Flash Next]\n        input: 1\n        output: 1\n"
	_, v := config.ParseAndValidate([]byte(collide))
	joined := ""
	for _, x := range v {
		joined += x.Error() + "\n"
	}
	if !strings.Contains(joined, `duplicate model/alias "Qwen3.8 Flash Next"`) {
		t.Fatalf("spaced aliases must collide like any names, got:\n%s", joined)
	}
}

// A route alias is a published name in /v1/models, so it keeps the strict
// shape: spaces stay refused there.
func TestRouteAliasStillRejectsSpaces(t *testing.T) {
	hosts := "hosts:\n-   host_addresses: [h]\n    id: minion1\n    servers:\n" +
		"    -   port: 11434\n        api: ollama\n        id: ollama\n"
	for _, alias := range []string{"qwen fast", " qwen", "qwen ", "qwen  fast"} {
		src := hosts + "loadbalancer:\n    routes:\n    -   alias: \"" + alias +
			"\"\n        models:\n        -   model: x-ollama@minion1\n"
		if _, v := config.ParseAndValidate([]byte(src)); len(v) == 0 {
			t.Fatalf("route alias %q must be rejected", alias)
		}
	}
}
