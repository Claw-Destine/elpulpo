package dashboard

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"elpulpo/internal/config"
)

// bodyFields normalises an action body — application/json or form-encoded —
// into one flat map of raw field values. JSON values are kept as their raw
// JSON text (objects and arrays intact), so a form field carrying JSON and a
// JSON body field look identical to the actions. A form field the browser
// repeats (the rows of a table form) is likewise kept as a JSON array, never
// reduced to its first value.
func bodyFields(r *http.Request) (map[string]string, error) {
	fields := map[string]string{}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var raw map[string]json.RawMessage
		if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&raw); err != nil {
			return nil, err
		}
		for k, v := range raw {
			fields[k] = string(v)
		}
		return fields, nil
	}
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	for k, vals := range r.PostForm {
		switch len(vals) {
		case 0:
			continue
		case 1:
			fields[k] = vals[0]
		default:
			// A repeated form field — the rows of a table form — keeps every
			// value, in submitted order, as JSON array text: the shape
			// fieldAll already reads out of a JSON body, so both encodings
			// look the same to the actions. Collapsing these to the first
			// value would silently drop every row but the first.
			b, err := json.Marshal(vals)
			if err != nil {
				return nil, err
			}
			fields[k] = string(b)
		}
	}
	return fields, nil
}

// fieldStr returns a field as a plain string: a JSON body's quoted string is
// unquoted; numbers/bools keep their raw text; form values pass through.
func fieldStr(fields map[string]string, key string) string {
	raw, ok := fields[key]
	if !ok {
		return ""
	}
	return unquote(raw)
}

// unquote removes JSON string quoting when present, passing raw text through.
func unquote(raw string) string {
	t := strings.TrimSpace(raw)
	if strings.HasPrefix(t, `"`) {
		var s string
		if err := json.Unmarshal([]byte(t), &s); err == nil {
			return s
		}
	}
	return raw
}

// fieldAll returns every value of a field (multi-valued forms and JSON
// arrays both flatten to a []string; JSON numbers become their text).
func fieldAll(fields map[string]string, key string) []string {
	raw, ok := fields[key]
	if !ok {
		return nil
	}
	t := strings.TrimSpace(raw)
	if strings.HasPrefix(t, "[") {
		var vals []any
		if err := json.Unmarshal([]byte(t), &vals); err == nil {
			out := make([]string, 0, len(vals))
			for _, v := range vals {
				switch tv := v.(type) {
				case string:
					out = append(out, tv)
				case float64:
					out = append(out, strconv.FormatFloat(tv, 'f', -1, 64))
				case nil:
					out = append(out, "")
				default:
					b, _ := json.Marshal(tv)
					out = append(out, string(b))
				}
			}
			return out
		}
	}
	return []string{fieldStr(fields, key)}
}

func fieldBool(fields map[string]string, key string) bool {
	v := strings.ToLower(fieldStr(fields, key))
	return v == "true" || v == "on" || v == "1" || v == "yes"
}

// splitLinesList parses a list field: a JSON array or a newline- (or
// comma-) separated string.
func splitLinesList(raw string) []string {
	t := strings.TrimSpace(raw)
	if t == "" {
		return nil
	}
	if strings.HasPrefix(t, "[") {
		var out []string
		if err := json.Unmarshal([]byte(t), &out); err == nil {
			return out
		}
	}
	var out []string
	for _, line := range strings.FieldsFunc(t, func(r rune) bool { return r == '\n' || r == ',' }) {
		if s := strings.TrimSpace(line); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// decodeDocJSON decodes a document-shaped JSON value (the YAML field names)
// into out by round-tripping through YAML, since config types carry yaml
// tags only. Integral JSON numbers become ints so YAML scalars stay clean.
func decodeDocJSON(raw string, out any) error {
	var v any
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return err
	}
	b, err := yaml.Marshal(normJSON(v))
	if err != nil {
		return err
	}
	return yaml.Unmarshal(b, out)
}

func normJSON(v any) any {
	switch tv := v.(type) {
	case map[string]any:
		for k, ev := range tv {
			tv[k] = normJSON(ev)
		}
		return tv
	case []any:
		for i, ev := range tv {
			tv[i] = normJSON(ev)
		}
		return tv
	case json.Number:
		s := tv.String()
		if !strings.ContainsAny(s, ".eE") {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				return n
			}
		}
		if f, err := tv.Float64(); err == nil {
			return f
		}
		return s
	}
	return v
}

// cloneConfig deep-copies the live configuration so a mutation never touches
// the snapshot in-flight requests still hold.
func cloneConfig(c *config.Config) *config.Config {
	out := &config.Config{}
	for _, h := range c.Hosts {
		ch := config.Host{ID: h.ID, Description: h.Description}
		if h.HostAddresses != nil {
			ch.HostAddresses = append([]string(nil), h.HostAddresses...)
		}
		if h.Servers != nil { // Server holds only scalars — a copy is deep
			ch.Servers = append([]config.Server(nil), h.Servers...)
		}
		out.Hosts = append(out.Hosts, ch)
	}
	if c.Prices != nil {
		p := &config.Prices{Currency: c.Prices.Currency}
		for _, m := range c.Prices.Models {
			cm := config.ModelPrice{Model: m.Model, Input: copyFloat(m.Input), Output: copyFloat(m.Output),
				CachedInput: copyFloat(m.CachedInput), ReasoningOutput: copyFloat(m.ReasoningOutput)}
			if m.Aliases != nil {
				cm.Aliases = append([]string(nil), m.Aliases...)
			}
			p.Models = append(p.Models, cm)
		}
		out.Prices = p
	}
	return out
}

func copyFloat(f *float64) *float64 {
	if f == nil {
		return nil
	}
	v := *f
	return &v
}

// parsePrice reads a rate field: ok=false means the text is not a number at
// all (which must be reported, not silently zeroed).
func parsePrice(s string) (v *float64, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, true
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, false
	}
	return &f, true
}
