package dashboard

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The Prices screen submits its aliases column as one comma-separated text
// input per row, so the list parser is what decides whether a spaced alias
// survives the form: it splits on commas only and trims the outer whitespace
// of each entry, keeping the spaces between words.

func priceFormFields(t *testing.T, form url.Values) map[string]string {
	t.Helper()
	r := httptest.NewRequest("POST", "/dashboard/action/prices/save", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	fields, err := bodyFields(r)
	if err != nil {
		t.Fatalf("bodyFields: %v", err)
	}
	return fields
}

func TestPriceFormKeepsSpacedAliases(t *testing.T) {
	v := url.Values{"currency": {"USD"}}
	v.Add("model", "qwen3.8:27b")
	v.Add("aliases", "qwen27, Qwen3.8 Flash Next ,qwen-flash")
	v.Add("input", "1")
	v.Add("output", "2")
	v.Add("model", "") // the trailing (add entry) row
	v.Add("aliases", "")
	v.Add("input", "")
	v.Add("output", "")

	p, err := parsePriceSection(priceFormFields(t, v))
	if err != nil {
		t.Fatalf("parsePriceSection: %v", err)
	}
	if p == nil || len(p.Models) != 1 {
		t.Fatalf("one entry expected: %#v", p)
	}
	want := []string{"qwen27", "Qwen3.8 Flash Next", "qwen-flash"}
	got := p.Models[0].Aliases
	if len(got) != len(want) {
		t.Fatalf("aliases = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("aliases = %#v, want %#v", got, want)
		}
	}
}

func TestSplitLinesListKeepsInnerSpaces(t *testing.T) {
	got := splitLinesList(" Qwen3.8 Flash Next, qwen27\n qwen-flash ")
	want := []string{"Qwen3.8 Flash Next", "qwen27", "qwen-flash"}
	if len(got) != len(want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	}
}
