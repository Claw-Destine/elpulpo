package config

import (
	"bytes"
	"sort"

	"gopkg.in/yaml.v3"
)

// Canonical forms are struct-driven (never a map, whose key order drifts) and
// materialise defaults, so two exports of the same config are byte-identical.

type hostOut struct {
	HostAddresses []string    `yaml:"host_addresses"`
	ID            string      `yaml:"id"`
	Description   string      `yaml:"description,omitempty"`
	Servers       []serverOut `yaml:"servers"`
}

type serverOut struct {
	Port           int    `yaml:"port"`
	API            string `yaml:"api"`
	ID             string `yaml:"id"`
	Description    string `yaml:"description,omitempty"`
	Scheme         string `yaml:"scheme"`
	AuthToken      string `yaml:"auth_token"`
	MaxConcurrency int    `yaml:"max_concurrency"`
}

type pricesOut struct {
	Currency string     `yaml:"currency"`
	Models   []priceOut `yaml:"models"`
}

type priceOut struct {
	Model           string   `yaml:"model"`
	Aliases         []string `yaml:"aliases,omitempty"`
	Input           float64  `yaml:"input"`
	Output          float64  `yaml:"output"`
	CachedInput     *float64 `yaml:"cached_input,omitempty"`
	ReasoningOutput *float64 `yaml:"reasoning_output,omitempty"`
}

type docOut struct {
	Hosts  []hostOut  `yaml:"hosts"`
	Prices *pricesOut `yaml:"prices,omitempty"`
}

// Normalize sorts hosts by id, servers by id within a host, price entries by
// model and aliases alphabetically, so store and export agree on order.
func Normalize(c *Config) {
	sort.SliceStable(c.Hosts, func(i, j int) bool { return c.Hosts[i].ID < c.Hosts[j].ID })
	for i := range c.Hosts {
		hs := c.Hosts[i].Servers
		sort.SliceStable(hs, func(a, b int) bool { return hs[a].ID < hs[b].ID })
	}
	if c.Prices != nil {
		ms := c.Prices.Models
		sort.SliceStable(ms, func(a, b int) bool { return ms[a].Model < ms[b].Model })
		for i := range ms {
			sort.Strings(ms[i].Aliases)
		}
	}
}

// Canonical renders the document in exactly the documented export shape:
// 4-space indent, defaults materialised, empty prices omitted.
func Canonical(c *Config) []byte {
	Normalize(c)
	out := docOut{}
	for _, h := range c.Hosts {
		ho := hostOut{HostAddresses: h.HostAddresses, ID: h.ID, Description: h.Description}
		if ho.HostAddresses == nil {
			ho.HostAddresses = []string{}
		}
		for _, s := range h.Servers {
			ho.Servers = append(ho.Servers, serverOut{
				Port: s.Port, API: s.API, ID: s.ID, Description: s.Description,
				Scheme: s.SchemeOrDefault(), AuthToken: s.AuthToken,
				MaxConcurrency: s.MaxConcurrency,
			})
		}
		if ho.Servers == nil {
			ho.Servers = []serverOut{}
		}
		out.Hosts = append(out.Hosts, ho)
	}
	if out.Hosts == nil {
		out.Hosts = []hostOut{}
	}
	if c.Prices != nil && len(c.Prices.Models) > 0 {
		p := &pricesOut{Currency: c.Prices.Currency}
		for _, m := range c.Prices.Models {
			in, outp := 0.0, 0.0
			if m.Input != nil {
				in = *m.Input
			}
			if m.Output != nil {
				outp = *m.Output
			}
			p.Models = append(p.Models, priceOut{
				Model: m.Model, Aliases: m.Aliases, Input: in, Output: outp,
				CachedInput: m.CachedInput, ReasoningOutput: m.ReasoningOutput,
			})
		}
		out.Prices = p
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(4)
	if err := enc.Encode(out); err != nil {
		// Encoding a struct of scalars cannot fail; keep the API simple.
		panic("config: canonical encode: " + err.Error())
	}
	enc.Close()
	return buf.Bytes()
}
