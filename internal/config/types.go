// Package config holds the El Pulpo configuration document: the hosts and
// prices sections, its validation, its canonical YAML form and the on-disk
// store that watches the file.
package config

import (
	"fmt"
	"regexp"
	"strings"
)

// Config is the whole configuration document: one YAML file is both the
// store and the export format.
type Config struct {
	Hosts        []Host        `yaml:"hosts"`
	Prices       *Prices       `yaml:"prices,omitempty"`
	LoadBalancer *LoadBalancer `yaml:"loadbalancer,omitempty"`
}

// Host is one machine running one or more LLM servers, reachable under one
// or more addresses in preference order.
type Host struct {
	HostAddresses []string `yaml:"host_addresses"`
	ID            string   `yaml:"id"`
	Description   string   `yaml:"description,omitempty"`
	Servers       []Server `yaml:"servers"`
}

// Server is one LLM endpoint: a host address + port + API adapter.
type Server struct {
	Port           int    `yaml:"port"`
	API            string `yaml:"api"`
	ID             string `yaml:"id"`
	Description    string `yaml:"description,omitempty"`
	Scheme         string `yaml:"scheme,omitempty"`
	AuthToken      string `yaml:"auth_token,omitempty"`
	MaxConcurrency int    `yaml:"max_concurrency"`
}

// NameSegment is the segment that appears in published model ids. It is by
// definition the server id, so the same value both names the server on the
// dashboard/usage rows and appears in the id clients address.
func (s Server) NameSegment() string {
	return s.ID
}

// SchemeOrDefault returns the configured scheme or http.
func (s Server) SchemeOrDefault() string {
	if s.Scheme != "" {
		return s.Scheme
	}
	return "http"
}

// LoadBalancer is the load-balancer section: the routes whose alias clients
// address instead of a published model id. The whole section is optional.
type LoadBalancer struct {
	Routes []Route `yaml:"routes"`
}

// Route is one load-balanced alias: a name in GET /v1/models that resolves to
// a ranked list of published model ids. Member order is the preference order —
// the first member is the one that gets the request when nothing is in flight.
type Route struct {
	Alias       string        `yaml:"alias"`
	Description string        `yaml:"description,omitempty"`
	Members     []RouteMember `yaml:"models"`
}

// RouteMember is one published model id inside a route, with the cost one
// in-flight connection to it is charged. Faster machines carry a smaller
// cost, slower ones a larger one, so equalising the in-flight cost across the
// members sends proportionally more traffic to the faster one.
type RouteMember struct {
	Model string   `yaml:"model"` // published id: <base>-<server-id>@<host-id>
	Cost  *float64 `yaml:"cost,omitempty"`
}

// DefaultMemberCost is what a member with no explicit cost is charged: one
// unit per in-flight connection, the same as every other member.
const DefaultMemberCost = 1.0

// CostOrDefault is the member's cost, defaulting to 1 when unset.
func (m RouteMember) CostOrDefault() float64 {
	if m.Cost == nil || *m.Cost <= 0 {
		return DefaultMemberCost
	}
	return *m.Cost
}

// Prices is the reference-price section. The whole section is optional.
type Prices struct {
	Currency string       `yaml:"currency"`
	Models   []ModelPrice `yaml:"models"`
}

// ModelPrice is the reference price of one base model name, per 1M tokens.
type ModelPrice struct {
	Model           string   `yaml:"model"`
	Aliases         []string `yaml:"aliases,omitempty"`
	Input           *float64 `yaml:"input"`
	Output          *float64 `yaml:"output"`
	CachedInput     *float64 `yaml:"cached_input,omitempty"`
	ReasoningOutput *float64 `yaml:"reasoning_output,omitempty"`
}

// IDPattern is the shape host ids and server ids must match so they stay
// URL- and model-id-safe.
var IDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// AliasPattern is the shape price aliases must match (no '@').
var AliasPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

// SupportedAPIs lists the adapters v1 ships. An adapter must define a model
// list endpoint, a chat endpoint and a usage source (see functional spec).
var SupportedAPIs = map[string]bool{"openai": true, "ollama": true}

// PublishID builds the published model id for a base model name on a server.
func PublishID(baseModel, segment, hostID string) string {
	return fmt.Sprintf("%s-%s@%s", baseModel, segment, hostID)
}

// SplitPublishedID parses a published model id right-anchored: the last '@'
// starts the host id, the last '-' before it starts the name segment.
func SplitPublishedID(id string) (base, segment, hostID string, ok bool) {
	at := strings.LastIndexByte(id, '@')
	if at < 0 || at == len(id)-1 {
		return "", "", "", false
	}
	head, hostID := id[:at], id[at+1:]
	dash := strings.LastIndexByte(head, '-')
	if dash <= 0 {
		return "", "", "", false
	}
	return head[:dash], head[dash+1:], hostID, true
}

// BaseModelOf returns the base model name behind a published id; if the id
// does not parse, the input is returned unchanged.
func BaseModelOf(publishedID string) string {
	if base, _, _, ok := SplitPublishedID(publishedID); ok {
		return base
	}
	return publishedID
}

// NormalizeAddress lowercases and strips a trailing dot, the comparison
// form for duplicate-address detection.
func NormalizeAddress(a string) string {
	a = strings.TrimSpace(a)
	a = strings.ToLower(a)
	return strings.TrimSuffix(a, ".")
}

// Empty reports whether the document carries nothing at all.
func (c *Config) Empty() bool {
	return len(c.Hosts) == 0 &&
		(c.Prices == nil || len(c.Prices.Models) == 0) &&
		(c.LoadBalancer == nil || len(c.LoadBalancer.Routes) == 0)
}

// Routes returns the configured load-balancer routes, nil-safe.
func (c *Config) Routes() []Route {
	if c == nil || c.LoadBalancer == nil {
		return nil
	}
	return c.LoadBalancer.Routes
}

// RouteFor finds the route whose alias equals the asked name. Lookup is exact:
// the alias is a published name, and model ids are case-sensitive.
func (c *Config) RouteFor(alias string) (*Route, bool) {
	for i := range c.Routes() {
		if c.LoadBalancer.Routes[i].Alias == alias {
			return &c.LoadBalancer.Routes[i], true
		}
	}
	return nil, false
}
