package config

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Violation is one field-level validation failure, with the path of the
// offending entry and the line it was found on.
type Violation struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Msg  string `json:"msg"`
}

func (v Violation) Error() string {
	if v.Line > 0 {
		return fmt.Sprintf("%s: %s (line %d)", v.Path, v.Msg, v.Line)
	}
	return fmt.Sprintf("%s: %s", v.Path, v.Msg)
}

// ValidationError carries every violation found in one pass, never just the
// first one.
type ValidationError struct {
	Violations []Violation
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Violations))
	for _, v := range e.Violations {
		parts = append(parts, v.Error())
	}
	return strings.Join(parts, "; ")
}

// ErrStaleSave is returned when a save's expected hash no longer matches the
// file on disk ("changed on disk, reload first").
var ErrStaleSave = errors.New("config changed on disk, reload first")

type validator struct {
	v      []Violation
	cfg    *Config
	seenHI map[string][]Violation // host id (lowercased) -> first occurrence
	seenAd map[string][]Violation // normalized address -> first occurrence
	names  []nameClaim            // price model/alias claims
}

type nameClaim struct {
	path string
	line int
	name string
}

func (va *validator) addMsg(path string, n *yaml.Node, msg string) {
	line := 0
	if n != nil {
		line = n.Line
	}
	va.v = append(va.v, Violation{Path: path, Line: line, Msg: msg})
}

func (va *validator) add(path string, n *yaml.Node, format string, args ...any) {
	line := 0
	if n != nil {
		line = n.Line
	}
	va.v = append(va.v, Violation{Path: path, Line: line, Msg: fmt.Sprintf(format, args...)})
}

// ParseAndValidate decodes a configuration document and returns every rule
// violation with the offending path and line. It returns a usable Config
// only when there are no violations.
func ParseAndValidate(data []byte) (*Config, []Violation) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		line := 1
		var ln int
		if _, e := fmt.Sscanf(err.Error(), "yaml: line %d:", &ln); e == nil {
			line = ln
		}
		return nil, []Violation{{Path: "$", Line: line, Msg: "invalid YAML: " + err.Error()}}
	}
	if root.Kind == 0 || len(root.Content) == 0 {
		return &Config{}, nil // empty document == empty configuration
	}
	doc := root.Content[0]
	va := &validator{
		cfg:    &Config{},
		seenHI: map[string][]Violation{},
		seenAd: map[string][]Violation{},
	}
	if doc.Kind != yaml.MappingNode {
		va.add("$", doc, "document must be a mapping with keys %q and %q", "hosts", "prices")
		return nil, va.v
	}
	var hostsN, pricesN *yaml.Node
	for i := 0; i+1 < len(doc.Content); i += 2 {
		k, vn := doc.Content[i], doc.Content[i+1]
		switch k.Value {
		case "hosts":
			hostsN = vn
		case "prices":
			pricesN = vn
		default:
			va.add("$", k, "unknown field %q", k.Value)
		}
	}
	if hostsN != nil {
		va.hosts(hostsN)
	}
	if pricesN != nil {
		va.prices(pricesN)
	}
	// Cross-entry symmetry: report both occurrences of a duplicate name.
	for i, a := range va.names {
		for j := i + 1; j < len(va.names); j++ {
			b := va.names[j]
			if a.name == b.name {
				va.v = append(va.v,
					Violation{Path: a.path, Line: a.line, Msg: fmt.Sprintf("duplicate model/alias %q (also at %s)", a.name, b.path)},
					Violation{Path: b.path, Line: b.line, Msg: fmt.Sprintf("duplicate model/alias %q (also at %s)", b.name, a.path)},
				)
			}
		}
	}
	if len(va.v) > 0 {
		return nil, va.v
	}
	return va.cfg, nil
}

func mapEntries(n *yaml.Node, path string, va *validator, allowed map[string]bool) [][2]*yaml.Node {
	var out [][2]*yaml.Node
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, vn := n.Content[i], n.Content[i+1]
		if seen[k.Value] {
			va.add(path, k, "duplicate key %q", k.Value)
			continue
		}
		seen[k.Value] = true
		if allowed != nil && !allowed[k.Value] {
			va.add(path+"."+k.Value, k, "unknown field %q", k.Value)
			continue
		}
		out = append(out, [2]*yaml.Node{k, vn})
	}
	return out
}

func (va *validator) scalar(path string, n *yaml.Node, into any) bool {
	if err := n.Decode(into); err != nil {
		va.add(path, n, "expected a scalar value: %v", err)
		return false
	}
	return true
}

func (va *validator) hosts(n *yaml.Node) {
	const path = "hosts"
	if n.Kind != yaml.SequenceNode {
		va.add(path, n, "expected a list of hosts")
		return
	}
	for i, hn := range n.Content {
		hpath := fmt.Sprintf("hosts[%d]", i)
		if hn.Kind != yaml.MappingNode {
			va.add(hpath, hn, "expected a host mapping")
			continue
		}
		allowed := map[string]bool{"host_addresses": true, "id": true, "description": true, "servers": true}
		var host Host
		var addrN, idN, srvN *yaml.Node
		for _, kv := range mapEntries(hn, hpath, va, allowed) {
			switch kv[0].Value {
			case "host_addresses":
				addrN = kv[1]
			case "id":
				idN = kv[1]
			case "description":
				va.scalar(hpath+".description", kv[1], &host.Description)
			case "servers":
				srvN = kv[1]
			}
		}
		if idN == nil {
			va.add(hpath, hn, "missing required field %q", "id")
		} else {
			va.scalar(hpath+".id", idN, &host.ID)
			if !IDPattern.MatchString(host.ID) {
				va.add(hpath+".id", idN, "must match [a-z0-9][a-z0-9-]{0,31}, got %q", host.ID)
			} else if prev, dup := va.seenHI[strings.ToLower(host.ID)]; dup {
				va.addMsg(hpath+".id", idN, fmt.Sprintf("duplicate host id %q (also at %s)", host.ID, prev[0].Path))
				va.v = append(va.v, Violation{Path: prev[0].Path, Line: prev[0].Line, Msg: fmt.Sprintf("duplicate host id %q (also at %s)", host.ID, hpath+".id")})
			} else {
				va.seenHI[strings.ToLower(host.ID)] = []Violation{{Path: hpath + ".id", Line: idN.Line}}
			}
		}
		if addrN == nil {
			va.add(hpath, hn, "missing required field %q", "host_addresses")
		} else if addrN.Kind != yaml.SequenceNode || len(addrN.Content) == 0 {
			va.add(hpath+".host_addresses", addrN, "must be a non-empty list of addresses")
		} else {
			perHost := map[string]int{}
			for j, an := range addrN.Content {
				apath := fmt.Sprintf("%s.host_addresses[%d]", hpath, j)
				var addr string
				if !va.scalar(apath, an, &addr) {
					continue
				}
				norm := NormalizeAddress(addr)
				if norm == "" {
					va.add(apath, an, "address must not be empty")
					continue
				}
				if _, dup := perHost[norm]; dup {
					va.addMsg(apath, an, fmt.Sprintf("duplicate address %q within this host", addr))
					continue
				}
				perHost[norm] = an.Line
				if prev, dup := va.seenAd[norm]; dup {
					va.addMsg(apath, an, fmt.Sprintf("address %q is also listed under %s", addr, prev[0].Path))
					va.v = append(va.v, Violation{Path: prev[0].Path, Line: prev[0].Line, Msg: fmt.Sprintf("address %q is also listed under %s", addr, apath)})
				} else {
					va.seenAd[norm] = []Violation{{Path: apath, Line: an.Line}}
				}
				host.HostAddresses = append(host.HostAddresses, addr)
			}
		}
		if srvN == nil {
			va.servers(hpath+".servers", srvN, &host, true)
		} else {
			va.servers(hpath+".servers", srvN, &host, false)
		}
		va.cfg.Hosts = append(va.cfg.Hosts, host)
	}
}

func (va *validator) servers(path string, n *yaml.Node, host *Host, missing bool) {
	if n == nil {
		host.Servers = nil
		return
	}
	if n.Kind != yaml.SequenceNode {
		va.add(path, n, "expected a list of servers")
		return
	}
	perID := map[string]int{}
	perPort := map[string]int{}
	for i, sn := range n.Content {
		spath := fmt.Sprintf("%s[%d]", path, i)
		if sn.Kind != yaml.MappingNode {
			va.add(spath, sn, "expected a server mapping")
			continue
		}
		allowed := map[string]bool{"port": true, "api": true, "id": true, "description": true,
			"postfix": true, "scheme": true, "auth_token": true, "max_concurrency": true}
		var srv Server
		var idN, portN, apiN *yaml.Node
		for _, kv := range mapEntries(sn, spath, va, allowed) {
			switch kv[0].Value {
			case "port":
				portN = kv[1]
			case "api":
				apiN = kv[1]
			case "id":
				idN = kv[1]
			case "description":
				va.scalar(spath+".description", kv[1], &srv.Description)
			case "postfix":
				// Removed field: the name segment is now the server id.
				va.add(spath+".postfix", kv[1], "is no longer supported — the name segment in published model ids is the server id; remove this field")
			case "scheme":
				var scheme string
				if va.scalar(spath+".scheme", kv[1], &scheme) {
					if scheme != "http" && scheme != "https" {
						va.add(spath+".scheme", kv[1], "must be %q or %q, got %q", "http", "https", scheme)
					} else {
						srv.Scheme = scheme
					}
				}
			case "auth_token":
				va.scalar(spath+".auth_token", kv[1], &srv.AuthToken)
			case "max_concurrency":
				var mc int
				if va.scalar(spath+".max_concurrency", kv[1], &mc) {
					if mc < 0 {
						va.add(spath+".max_concurrency", kv[1], "must be 0 (unlimited) or a positive integer")
					} else {
						srv.MaxConcurrency = mc
					}
				}
			}
		}
		if idN == nil {
			va.add(spath, sn, "missing required field %q", "id")
		} else {
			va.scalar(spath+".id", idN, &srv.ID)
			if !IDPattern.MatchString(srv.ID) {
				va.add(spath+".id", idN, "must match [a-z0-9][a-z0-9-]{0,31}, got %q", srv.ID)
			}
			if ln, dup := perID[strings.ToLower(srv.ID)]; dup {
				va.addMsg(spath+".id", idN, fmt.Sprintf("duplicate server id %q within host (also at line %d)", srv.ID, ln))
			} else {
				perID[strings.ToLower(srv.ID)] = idN.Line
			}
		}
		if portN == nil {
			va.add(spath, sn, "missing required field %q", "port")
		} else {
			var port int
			if va.scalar(spath+".port", portN, &port) {
				if port < 1 || port > 65535 {
					va.add(spath+".port", portN, "must be between 1 and 65535, got %d", port)
				}
				key := strconv.Itoa(port)
				if ln, dup := perPort[key]; dup {
					va.addMsg(spath+".port", portN, fmt.Sprintf("duplicate port %d within host (also at line %d)", port, ln))
				} else {
					perPort[key] = portN.Line
				}
				srv.Port = port
			}
		}
		if apiN == nil {
			va.add(spath, sn, "missing required field %q", "api")
		} else {
			var api string
			if va.scalar(spath+".api", apiN, &api) {
				if !SupportedAPIs[api] {
					va.add(spath+".api", apiN, "unsupported adapter %q (supported: %s)", api, supportedList())
				} else {
					srv.API = api
				}
			}
		}
		// The name segment is the server id, whose within-host uniqueness is
		// already enforced above, so two servers on one host can never publish
		// colliding model ids.
		host.Servers = append(host.Servers, srv)
	}
	_ = missing
}

func supportedList() string {
	keys := make([]string, 0, len(SupportedAPIs))
	for k := range SupportedAPIs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

func (va *validator) prices(n *yaml.Node) {
	const path = "prices"
	if n.Kind == yaml.ScalarNode && n.Tag == "!!null" {
		return // prices: (explicit null) == absent
	}
	if n.Kind != yaml.MappingNode {
		va.add(path, n, "expected a mapping with keys %q and %q", "currency", "models")
		return
	}
	p := &Prices{}
	var modelsN *yaml.Node
	for _, kv := range mapEntries(n, path, va, map[string]bool{"currency": true, "models": true}) {
		switch kv[0].Value {
		case "currency":
			var cur string
			if va.scalar(path+".currency", kv[1], &cur) {
				if !isISOCurrency(cur) {
					va.add(path+".currency", kv[1], "currency must be a 3-letter ISO 4217 code, got %q", cur)
				} else {
					p.Currency = strings.ToUpper(cur)
				}
			}
		case "models":
			modelsN = kv[1]
		}
	}
	if p.Currency == "" {
		va.add(path, n, "missing required field %q", "currency")
	}
	if modelsN != nil {
		if modelsN.Kind != yaml.SequenceNode {
			va.add(path+".models", modelsN, "expected a list of price entries")
		} else {
			for i, mn := range modelsN.Content {
				mpath := fmt.Sprintf("%s.models[%d]", path, i)
				p.Models = append(p.Models, va.priceEntry(mpath, mn))
			}
		}
	}
	va.cfg.Prices = p
}

func (va *validator) priceEntry(path string, n *yaml.Node) ModelPrice {
	var mp ModelPrice
	if n.Kind != yaml.MappingNode {
		va.add(path, n, "expected a price entry mapping")
		return mp
	}
	allowed := map[string]bool{"model": true, "aliases": true, "input": true, "output": true,
		"cached_input": true, "reasoning_output": true}
	var modelN, aliasesN *yaml.Node
	num := func(fpath string, vn *yaml.Node, into **float64) {
		var f float64
		if va.scalar(fpath, vn, &f) {
			if f < 0 {
				va.add(fpath, vn, "price must be >= 0, got %v", f)
			} else {
				v := f
				*into = &v
			}
		}
	}
	for _, kv := range mapEntries(n, path, va, allowed) {
		switch kv[0].Value {
		case "model":
			modelN = kv[1]
		case "aliases":
			aliasesN = kv[1]
		case "input":
			num(path+".input", kv[1], &mp.Input)
		case "output":
			num(path+".output", kv[1], &mp.Output)
		case "cached_input":
			num(path+".cached_input", kv[1], &mp.CachedInput)
		case "reasoning_output":
			num(path+".reasoning_output", kv[1], &mp.ReasoningOutput)
		}
	}
	if modelN == nil {
		va.add(path, n, "missing required field %q", "model")
	} else {
		va.scalar(path+".model", modelN, &mp.Model)
		if mp.Model == "" {
			va.add(path+".model", modelN, "model name must not be empty")
		} else if strings.Contains(mp.Model, "@") {
			va.add(path+".model", modelN, "model name must not contain %q", "@")
		} else {
			va.names = append(va.names, nameClaim{path: path + ".model", line: modelN.Line, name: mp.Model})
		}
	}
	if mp.Input == nil {
		va.add(path, n, "missing required field %q", "input")
	}
	if mp.Output == nil {
		va.add(path, n, "missing required field %q", "output")
	}
	if aliasesN != nil {
		if aliasesN.Kind != yaml.SequenceNode {
			va.add(path+".aliases", aliasesN, "expected a list of aliases")
		} else {
			for j, an := range aliasesN.Content {
				apath := fmt.Sprintf("%s.aliases[%d]", path, j)
				var a string
				if !va.scalar(apath, an, &a) {
					continue
				}
				if !AliasPattern.MatchString(a) {
					va.add(apath, an, "alias must match [A-Za-z0-9][A-Za-z0-9._:-]{0,63}, got %q", a)
					continue
				}
				seen := false
				for _, existing := range mp.Aliases {
					if existing == a {
						seen = true
					}
				}
				if seen {
					va.add(apath, an, "duplicate alias %q within this entry", a)
					continue
				}
				mp.Aliases = append(mp.Aliases, a)
				va.names = append(va.names, nameClaim{path: apath, line: an.Line, name: a})
			}
		}
	}
	return mp
}

func isISOCurrency(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) != 3 {
		return false
	}
	for _, r := range strings.ToUpper(s) {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}
