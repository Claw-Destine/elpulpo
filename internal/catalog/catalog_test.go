package catalog_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"elpulpo/internal/catalog"
)

// A broken embedded catalogue must fail the build of the product, so the
// seed file is validated here.
func TestEmbeddedCatalogueValid(t *testing.T) {
	c, err := catalog.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	if c.File.Catalogue.Currency != "USD" {
		t.Fatalf("currency %q", c.File.Catalogue.Currency)
	}
	if len(c.File.Catalogue.Rates) < 20 {
		t.Fatalf("expected a seed list of ~24 models, got %d", len(c.File.Catalogue.Rates))
	}
	_ = c.Stale // staleness is a label, not a failure; a young seed is expected
}

func TestMatchIsExactCaseInsensitive(t *testing.T) {
	c, err := catalog.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Match("GPT-4o"); len(got) != 1 || got[0].Model != "gpt-4o" {
		t.Fatalf("case-insensitive exact match failed: %+v", got)
	}
	if got := c.Match("qwen3.8:27b"); len(got) != 0 {
		t.Fatalf("self-hosted names must not fuzzy-match: %+v", got)
	}
	if got := c.Match("gpt-4"); len(got) != 0 {
		t.Fatalf("prefix must not match: %+v", got)
	}
}

const badOverride = `catalogue:
    version: "2026.01"
    as_of: 2026-01-01
    currency: EUR
    rates: []
`

func TestOverride(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "prices.yaml")

	// Invalid override (wrong currency) keeps the embedded one and says so.
	os.WriteFile(p, []byte(badOverride), 0o600)
	c, err := catalog.LoadWithOverride(p)
	if err == nil {
		t.Fatal("expected error")
	}
	if c == nil || len(c.File.Catalogue.Rates) < 20 {
		t.Fatalf("embedded catalogue must stay in place: %v", c)
	}

	good := strings.Replace(badOverride, "currency: EUR", "currency: USD", 1)
	os.WriteFile(p, []byte(good), 0o600)
	c, err = catalog.LoadWithOverride(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.File.Catalogue.Rates) != 0 || c.FromEnv != p {
		t.Fatalf("override not applied: %+v", c)
	}
}

func TestStaleLabel(t *testing.T) {
	src := `catalogue:
    version: "2025.01"
    as_of: 2025-01-01
    currency: USD
    rates:
    -   provider: openai
        model: x
        input: 1.0
        output: 1.0
`
	c, err := catalog.Load([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Stale {
		t.Fatalf("expected stale for as_of %s (>180 days: %v)", c.File.Catalogue.AsOf, time.Since(c.AsOfDate()))
	}
}
