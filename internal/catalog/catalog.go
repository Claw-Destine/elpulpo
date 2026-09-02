// Package catalog carries the embedded, read-only reference price catalogue
// of current provider prices, plus the ELPULPO_PRICE_CATALOGUE override.
// El Pulpo never fetches anything: rates change when the binary is upgraded
// or the operator supplies a file.
package catalog

import (
	_ "embed"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed prices.yaml
var embedded []byte

// Rate is one catalogue entry: provider, cloud model name and the four
// per-1M rates in USD.
type Rate struct {
	Provider        string   `yaml:"provider" json:"provider"`
	Model           string   `yaml:"model" json:"model"`
	Input           *float64 `yaml:"input" json:"input"`
	Output          *float64 `yaml:"output" json:"output"`
	CachedInput     *float64 `yaml:"cached_input,omitempty" json:"cached_input,omitempty"`
	ReasoningOutput *float64 `yaml:"reasoning_output,omitempty" json:"reasoning_output,omitempty"`
}

// Meta is the catalogue header.
type Meta struct {
	Version  string `yaml:"version" json:"version"`
	AsOf     string `yaml:"as_of" json:"as_of"` // YYYY-MM-DD
	Currency string `yaml:"currency" json:"currency"`
}

// File is the whole catalogue document.
type File struct {
	Catalogue struct {
		Meta  `yaml:",inline"`
		Rates []Rate `yaml:"rates"`
	} `yaml:"catalogue"`
}

// StaleAge is the point past which a catalogue is labelled stale.
const StaleAge = 180 * 24 * time.Hour

// Catalogue is a loaded catalogue with lookup helpers.
type Catalogue struct {
	File    File
	Stale   bool   // older than StaleAge
	FromEnv string // path of the override in effect, "" if embedded
}

// Validate enforces the invariants that fail the build: every entry has
// provider/model/input/output, the currency is USD and as_of parses.
func (f *File) Validate() error {
	c := &f.Catalogue
	if c.Currency != "USD" {
		return fmt.Errorf("catalogue currency must be USD, got %q", c.Currency)
	}
	if _, err := time.Parse("2006-01-02", c.AsOf); err != nil {
		return fmt.Errorf("catalogue as_of %q does not parse as YYYY-MM-DD", c.AsOf)
	}
	if c.Version == "" {
		return fmt.Errorf("catalogue version must not be empty")
	}
	for i, r := range c.Rates {
		if r.Provider == "" || r.Model == "" {
			return fmt.Errorf("rates[%d]: provider and model are required", i)
		}
		if r.Input == nil || r.Output == nil {
			return fmt.Errorf("rates[%d] (%s/%s): input and output are required", i, r.Provider, r.Model)
		}
		if *r.Input < 0 || *r.Output < 0 {
			return fmt.Errorf("rates[%d] (%s/%s): rates must be >= 0", i, r.Provider, r.Model)
		}
		if r.CachedInput != nil && *r.CachedInput < 0 {
			return fmt.Errorf("rates[%d] (%s/%s): cached_input must be >= 0", i, r.Provider, r.Model)
		}
		if r.ReasoningOutput != nil && *r.ReasoningOutput < 0 {
			return fmt.Errorf("rates[%d] (%s/%s): reasoning_output must be >= 0", i, r.Provider, r.Model)
		}
	}
	return nil
}

// Load parses and validates catalogue bytes.
func Load(data []byte) (*Catalogue, error) {
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("catalogue YAML invalid: %w", err)
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	cat := &Catalogue{File: f}
	asOf, _ := time.Parse("2006-01-02", f.Catalogue.AsOf)
	cat.Stale = time.Since(asOf) > StaleAge
	return cat, nil
}

// Embedded parses the bundled catalogue (validated by a unit test too, so a
// broken seed fails the build).
func Embedded() (*Catalogue, error) { return Load(embedded) }

// LoadWithOverride returns the embedded catalogue, replaced by the file at
// overridePath when it is set and valid. An invalid override never replaces
// the embedded one — the caller gets the embedded catalogue plus an error to
// log, rather than no prices at all.
func LoadWithOverride(overridePath string) (*Catalogue, error) {
	cat, err := Embedded()
	if err != nil {
		return nil, fmt.Errorf("embedded catalogue is broken: %w", err)
	}
	if overridePath == "" {
		return cat, nil
	}
	data, err := os.ReadFile(overridePath)
	if err != nil {
		return cat, fmt.Errorf("price catalogue override %s not readable, keeping embedded: %w", overridePath, err)
	}
	over, err := Load(data)
	if err != nil {
		return cat, fmt.Errorf("price catalogue override %s is invalid, keeping embedded: %w", overridePath, err)
	}
	over.FromEnv = overridePath
	return over, nil
}

// AsOfDate parses the as_of stamp.
func (c *Catalogue) AsOfDate() time.Time {
	t, _ := time.Parse("2006-01-02", c.File.Catalogue.AsOf)
	return t
}

// Match returns catalogue entries whose model name equals the given base
// model name exactly, compared case-insensitively. No fuzzy matching.
func (c *Catalogue) Match(baseName string) []Rate {
	var out []Rate
	for _, r := range c.File.Catalogue.Rates {
		if strings.EqualFold(r.Model, baseName) {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Provider < out[j].Provider
	})
	return out
}
