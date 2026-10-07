package dashboard

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/yuin/goldmark"
	gast "github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	gutil "github.com/yuin/goldmark/util"

	"elpulpo/internal/config"
	"elpulpo/internal/usage"
)

// The Debug screen: an on/off switch (with an optional model filter) over a
// list of recorded requests, and a right pane that shows the selected
// recording either as its messages, rendered, or as raw JSON/SSE text.

// --- view models -------------------------------------------------------------

// DebugMessage is one message as the messages view shows it: a role and an
// ordered set of parts (text, structured JSON, images), plus tool calls and
// any reasoning the server attached.
type DebugMessage struct {
	Role      string
	Name      string
	Parts     []DebugPart
	ToolCalls []DebugToolCall
	Reasoning string
	Refusal   string
	HasParts  bool
}

// DebugPart is one piece of a message's content.
type DebugPart struct {
	Kind   string // "text" | "json" | "image" | "file"
	Text   string // markdown source (text)
	JSON   string // pretty JSON (json / file)
	URL    string // image source
	MIME   string // image media type for the caption
	Bytes  int64  // decoded size of a base64 image
	Inline bool   // URL is a data: URL the browser can render
	Title  string
}

// DebugToolCall is one tool call with its arguments pretty-printed.
type DebugToolCall struct {
	ID, Name, Args string
}

// DebugChoice is one response choice.
type DebugChoice struct {
	Index  string
	Msg    DebugMessage
	Finish string
}

// DebugDetail is one selected recording, parsed for either view mode.
type DebugDetail struct {
	Cap     *usage.DebugCapture // nil when the id is gone
	Missing bool
	Mode    string // "messages" | "raw"

	Model       string // request-side model id from the body
	Messages    []DebugMessage
	ReqOther    string // the rest of the request body, pretty JSON
	Choices     []DebugChoice
	Usage       string
	FramesSeen  int
	FrameNote   string // "" or a reconstruction note
	RawRequest  string
	RawResponse string
	ResponseAs  string // "JSON" | "SSE stream" | "nothing"
	Err         string
}

// DebugView is the whole Debug screen.
type DebugView struct {
	Enabled   bool     // effective: switched on and inside its time window
	Model     string   // the filter, "" = every model
	Until     int64    // window deadline, unix ms (0 = no limit)
	UntilText string   // what the status line says about the window
	For       string   // which duration the form's select pre-fills
	Models    []string // filter options: published ids + aliases
	Records   []usage.DebugRow
	Sel       int64 // selected recording (0 = none)
	Mode      string
	Detail    *DebugDetail
	Q         debugQuery
}

type debugQuery struct {
	Sel  int64
	Mode string
	Poll bool
}

func parseDebugQuery(r *http.Request) debugQuery {
	q := r.URL.Query()
	dq := debugQuery{Sel: atoi64(q.Get("sel")), Mode: q.Get("view"), Poll: q.Get("poll") != ""}
	if dq.Mode != "raw" {
		dq.Mode = "messages"
	}
	return dq
}

// canon re-encodes the screen state for fragment polling.
func (dq debugQuery) canon() string {
	v := url.Values{}
	if dq.Sel > 0 {
		v.Set("sel", strconv.FormatInt(dq.Sel, 10))
	}
	if dq.Mode != "messages" {
		v.Set("view", dq.Mode)
	}
	return v.Encode()
}

func (dq debugQuery) pageURL() string {
	s := dq.canon()
	if s == "" {
		return "/dashboard/debug"
	}
	return "/dashboard/debug?" + s
}

func (dq debugQuery) pollURL() string {
	s := dq.canon()
	if s != "" {
		s = "poll=1&" + s
	} else {
		s = "poll=1"
	}
	return "/dashboard/part/debug?" + s
}

func (dq debugQuery) with(k, v string) string {
	q, _ := url.ParseQuery(dq.canon())
	q.Set(k, v)
	return q.Encode()
}

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// debugDurations are the time limits the form offers; Apply restarts the
// window, so the switch cannot be forgotten open.
var debugDurations = map[string]time.Duration{
	"15m": 15 * time.Minute,
	"1h":  time.Hour,
	"1d":  24 * time.Hour,
}

// debugDeadline maps a form choice to the stored absolute deadline
// ("0" = the window stays open until someone closes it).
func debugDeadline(choice string, now time.Time) string {
	if d, ok := debugDurations[choice]; ok {
		return strconv.FormatInt(now.Add(d).UnixMilli(), 10)
	}
	return "0"
}

// debugWindow renders what the live deadline means: the sentence the status
// line adds, and the duration the form's select should pre-fill with.
func debugWindow(s *config.Settings, now time.Time) (text, choice string) {
	if !s.DebugOn(now) {
		return "", "15m"
	}
	if s.DebugUntil == 0 {
		return "It runs until switched off.", "never"
	}
	until := time.UnixMilli(s.DebugUntil)
	rem := until.Sub(now)
	layout := "15:04:05"
	if !sameLocalDay(until, now) {
		layout = "Jan 2 15:04"
	}
	switch {
	case rem > time.Hour:
		choice = "1d"
	case rem > 15*time.Minute:
		choice = "1h"
	default:
		choice = "15m"
	}
	return "It stops at " + until.In(time.Local).Format(layout) + " (" + dbgLeft(rem) + " left).", choice
}

func sameLocalDay(a, b time.Time) bool {
	ay, am, ad := a.In(time.Local).Date()
	by, bm, bd := b.In(time.Local).Date()
	return ay == by && am == bm && ad == bd
}

// dbgLeft shortens a duration to one honest phrase: 42m, 3h15m, 9s.
func dbgLeft(d time.Duration) string {
	if d < 0 {
		return "moments"
	}
	if d < time.Minute {
		return strconv.Itoa(int(d.Round(time.Second)/time.Second)) + "s"
	}
	d = d.Round(time.Minute) // round first: 23h59m59s must read 24h, not 23h60m
	h := d / time.Hour
	m := (d % time.Hour) / time.Minute
	if m == 0 {
		return strconv.Itoa(int(h)) + "h"
	}
	return strconv.Itoa(int(h)) + "h" + strconv.Itoa(int(m)) + "m"
}

// --- handlers ----------------------------------------------------------------

func (h *Handler) pageDebug(w http.ResponseWriter, r *http.Request, csrf string) {
	h.render(w, r, DebugPage(h.d, csrf, h.debugView(r)))
}

func (h *Handler) partDebug(w http.ResponseWriter, r *http.Request, csrf string) {
	v := h.debugView(r)
	if !v.Q.Poll {
		// Address bar shows the page with the current selection, never the
		// fragment endpoint; a self-refresh must not add history entries.
		w.Header().Set("HX-Push-Url", v.Q.pageURL())
	}
	h.render(w, r, DebugLiveFragment(v))
}

func (h *Handler) debugView(r *http.Request) DebugView {
	dq := parseDebugQuery(r)
	s := h.d.Settings.Get()
	now := time.Now()
	v := DebugView{Enabled: s.DebugOn(now), Model: s.DebugModel, Until: s.DebugUntil,
		Mode: dq.Mode, Q: dq, Sel: dq.Sel}
	v.UntilText, v.For = debugWindow(s, now)
	v.Models = h.debugModelOptions()

	records, err := h.d.Repo.DebugRows(r.Context(), 100, 0)
	if err != nil {
		v.Detail = &DebugDetail{Err: "could not read recordings: " + err.Error(), Mode: dq.Mode}
		return v
	}
	v.Records = records

	if dq.Sel > 0 {
		rec, err := h.d.Repo.DebugGet(r.Context(), dq.Sel)
		if err != nil {
			v.Detail = &DebugDetail{Err: "could not read recording: " + err.Error(), Mode: dq.Mode}
			return v
		}
		if rec == nil {
			v.Detail = &DebugDetail{Missing: true, Mode: dq.Mode}
			return v
		}
		v.Detail = h.debugDetail(rec, dq.Mode)
	}
	return v
}

// debugModelOptions lists what the filter can name: every published id plus
// every load-balancer alias, sorted; "" (all models) is the default option.
func (h *Handler) debugModelOptions() []string {
	out := []string{}
	seen := map[string]bool{}
	for _, m := range h.d.Mgr.Models() {
		if !seen[m.Published] {
			seen[m.Published] = true
			out = append(out, m.Published)
		}
	}
	for _, rt := range h.d.Store.Current().Config.Routes() {
		if !seen[rt.Alias] {
			seen[rt.Alias] = true
			out = append(out, rt.Alias)
		}
	}
	sort.Strings(out)
	return out
}

// --- actions -------------------------------------------------------------------

func (h *Handler) actionDebugSave(w http.ResponseWriter, r *http.Request) {
	fields, err := bodyFields(r)
	if err != nil {
		badRequest(w, fmt.Errorf("invalid body: %w", err))
		return
	}
	patch := map[string]string{}
	if raw, ok := fields["debug_enabled"]; ok {
		patch["debug_enabled"] = unquote(raw)
	}
	if raw, ok := fields["debug_model"]; ok {
		patch["debug_model"] = unquote(raw) // "" is a value: every model
	}
	// The time limit: any request that turns recording on without naming a
	// window gets the shortest one, so the switch cannot be left open by
	// accident. Turning it off always closes the window.
	_, hasEnabled := fields["debug_enabled"]
	_, hasFor := fields["debug_for"]
	forChoice := ""
	if hasFor {
		forChoice = strings.ToLower(strings.TrimSpace(unquote(fields["debug_for"])))
		if forChoice == "" {
			forChoice = "15m"
		}
	} else if hasEnabled {
		forChoice = "15m"
	}
	if forChoice != "" && forChoice != "never" {
		if _, ok := debugDurations[forChoice]; !ok {
			violationsJSON(w, []config.Violation{{Path: "debug_for",
				Msg: `debug_for must be "15m", "1h", "1d" or "never"`}})
			return
		}
	}
	if hasEnabled || hasFor {
		on := h.d.Settings.Get().DebugOn(time.Now())
		if raw, ok := fields["debug_enabled"]; ok {
			switch strings.ToLower(unquote(raw)) {
			case "true", "on", "1", "yes":
				on = true
			case "false", "off", "0", "no":
				on = false
			}
		}
		if on {
			patch["debug_until"] = debugDeadline(forChoice, time.Now())
		} else {
			patch["debug_until"] = "0"
		}
	}
	if len(patch) == 0 {
		badRequest(w, fmt.Errorf(`body must carry "debug_enabled", "debug_model" or "debug_for"`))
		return
	}
	if _, violations := h.d.Settings.Update(r.Context(), patch); len(violations) > 0 {
		violationsJSON(w, violations)
		return
	}
	w.Header().Set("HX-Trigger", "elpulpo-changed")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) actionDebugClear(w http.ResponseWriter, r *http.Request) {
	n, err := h.d.Repo.DebugClear(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody(err))
		return
	}
	w.Header().Set("HX-Trigger", "elpulpo-changed")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": n})
}

// --- JSON API ------------------------------------------------------------------

func (h *Handler) apiDebugRecords(w http.ResponseWriter, r *http.Request) {
	s := h.d.Settings.Get()
	limit := 100
	if v := atoi(r.URL.Query().Get("limit")); v > 0 && v <= 1000 {
		limit = v
	}
	records, err := h.d.Repo.DebugRows(r.Context(), limit, int(atoi64(r.URL.Query().Get("offset"))))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody(err))
		return
	}
	if records == nil {
		records = []usage.DebugRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": s.DebugOn(time.Now()), "model": s.DebugModel,
		"until_ms": s.DebugUntil, "records": records,
	})
}

func (h *Handler) apiDebugRecord(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.URL.Query().Get("id"))
	c, err := h.d.Repo.DebugGet(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody(err))
		return
	}
	if c == nil {
		writeJSON(w, http.StatusNotFound, errMsg("no such recording"))
		return
	}
	out := map[string]any{
		"id": c.ID, "timestamp_ms": c.TsMs, "host": c.HostID, "server": c.ServerID,
		"model": c.Model, "route": c.Route, "stream": c.Stream, "status": c.Status,
		"http_status": c.HTTPStatus, "latency_ms": c.LatencyMs,
		"tokens_in": c.TokensIn, "tokens_out": c.TokensOut,
		"request": jsonValue(c.RequestJSON),
	}
	if c.HasResponse {
		out["response"] = jsonValue(c.ResponseJSON)
	} else if c.ResponseSSE != "" {
		out["response_sse"] = c.ResponseSSE
	}
	writeJSON(w, http.StatusOK, out)
}

// --- recording parsing -----------------------------------------------------------

// debugDetail turns one capture into the detail pane for the requested mode.
func (h *Handler) debugDetail(c *usage.DebugCapture, mode string) *DebugDetail {
	d := &DebugDetail{Cap: c, Mode: mode}

	// The request side: the context as the client sent it.
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(c.RequestJSON), &doc); err != nil {
		d.Err = "request body is not valid JSON: " + err.Error()
		d.Model = c.Model
	} else {
		if raw, ok := doc["model"]; ok {
			_ = json.Unmarshal(raw, &d.Model)
		}
		if raw, ok := doc["messages"]; ok {
			d.Messages = debugParseMessages(raw)
		}
		rest := map[string]json.RawMessage{}
		for k, v := range doc {
			if k != "model" && k != "messages" {
				rest[k] = v
			}
		}
		if len(rest) > 0 {
			d.ReqOther = debugPretty(rest)
		}
	}
	d.RawRequest = debugPrettyRaw(c.RequestJSON)

	// The response side: a buffered body, or SSE frames to reconstruct.
	switch {
	case c.HasResponse:
		d.ResponseAs = "JSON"
		d.RawResponse = debugPrettyRaw(c.ResponseJSON)
		var rdoc map[string]json.RawMessage
		if json.Unmarshal([]byte(c.ResponseJSON), &rdoc) == nil {
			if raw, ok := rdoc["choices"]; ok {
				d.Choices = debugParseChoices(raw)
			}
			if raw, ok := rdoc["usage"]; ok {
				d.Usage = debugUsageLine(raw)
			}
			if _, ok := rdoc["error"]; ok {
				d.Usage = "" // an error document carries no usage
			}
		}
	case c.Stream && c.ResponseSSE != "":
		d.ResponseAs = "SSE stream"
		d.RawResponse = c.ResponseSSE
		var frames int
		d.Choices, d.Usage, frames = debugReconstructStream(c.ResponseSSE)
		d.FramesSeen = frames
		d.FrameNote = fmt.Sprintf(
			"reconstructed from %d streamed frames; raw SSE in the raw view", frames)
	default:
		d.ResponseAs = "nothing"
	}
	return d
}

// debugParseMessages reads a messages array: strings, content-part arrays,
// tool calls, names, refusal and reasoning all land in the display model.
func debugParseMessages(raw json.RawMessage) []DebugMessage {
	var msgs []json.RawMessage
	if json.Unmarshal(raw, &msgs) != nil {
		return nil
	}
	out := make([]DebugMessage, 0, len(msgs))
	for _, m := range msgs {
		var obj map[string]json.RawMessage
		if json.Unmarshal(m, &obj) != nil {
			out = append(out, DebugMessage{Role: "message", Parts: []DebugPart{{Kind: "json", JSON: debugPrettyRaw(string(m))}}, HasParts: true})
			continue
		}
		out = append(out, debugParseMessage(obj))
	}
	return out
}

func debugParseMessage(obj map[string]json.RawMessage) DebugMessage {
	msg := DebugMessage{}
	_ = json.Unmarshal(obj["role"], &msg.Role)
	if msg.Role == "" {
		msg.Role = "unknown"
	}
	_ = json.Unmarshal(obj["name"], &msg.Name)
	_ = json.Unmarshal(obj["reasoning_content"], &msg.Reasoning)
	_ = json.Unmarshal(obj["refusal"], &msg.Refusal)

	if raw, ok := obj["content"]; ok {
		msg.Parts = debugParseContent(raw)
		msg.HasParts = len(msg.Parts) > 0
	}
	if raw, ok := obj["tool_calls"]; ok {
		var tcs []map[string]json.RawMessage
		if json.Unmarshal(raw, &tcs) == nil {
			for _, tc := range tcs {
				var fn struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				}
				var id string
				_ = json.Unmarshal(tc["id"], &id)
				if f, ok := tc["function"]; ok {
					_ = json.Unmarshal(f, &fn)
				}
				args := debugPrettyRaw(string(fn.Arguments))
				msg.ToolCalls = append(msg.ToolCalls, DebugToolCall{ID: id, Name: fn.Name, Args: args})
			}
		}
	}
	return msg
}

// debugParseContent reads one message's content: a plain string, a parts
// array (text / image_url / anything structured), null, or a bare value.
func debugParseContent(raw json.RawMessage) []DebugPart {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []DebugPart{{Kind: "text", Text: s}}
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) == nil {
		out := []DebugPart{}
		for _, p := range parts {
			out = append(out, debugParsePart(p))
		}
		return out
	}
	// Neither string nor array: structured content shown as JSON.
	return []DebugPart{{Kind: "json", JSON: debugPretty(raw)}}
}

func debugParsePart(raw json.RawMessage) DebugPart {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return DebugPart{Kind: "json", JSON: debugPretty(raw)}
	}
	var kind string
	_ = json.Unmarshal(obj["type"], &kind)
	switch kind {
	case "text":
		var t string
		_ = json.Unmarshal(obj["text"], &t)
		return DebugPart{Kind: "text", Text: t}
	case "image_url":
		u := ""
		if raw, ok := obj["image_url"]; ok {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				u = s
			} else {
				var iu struct {
					URL string `json:"url"`
				}
				_ = json.Unmarshal(raw, &iu)
				u = iu.URL
			}
		}
		p := DebugPart{Kind: "image", URL: u, Inline: strings.HasPrefix(u, "data:")}
		if p.Inline {
			p.MIME, p.Bytes = debugDataImageInfo(u)
		}
		return p
	default:
		return DebugPart{Kind: "json", JSON: debugPretty(raw), Title: kind}
	}
}

// debugDataImageInfo decodes just enough of a data: URL to caption it.
func debugDataImageInfo(u string) (string, int64) {
	mime := "image"
	body := u
	if !strings.HasPrefix(u, "data:") {
		return mime, 0
	}
	head, rest, _ := strings.Cut(u[len("data:"):], ",")
	body = rest
	m, _, _ := strings.Cut(head, ";")
	if m != "" {
		mime = m
	}
	if dec, err := base64.StdEncoding.DecodeString(body); err == nil {
		return mime, int64(len(dec))
	}
	return mime, int64(len(body))
}

func debugParseChoices(raw json.RawMessage) []DebugChoice {
	var choices []map[string]json.RawMessage
	if json.Unmarshal(raw, &choices) != nil {
		return nil
	}
	out := []DebugChoice{}
	for i, ch := range choices {
		var msg json.RawMessage
		if m, ok := ch["message"]; ok {
			msg = m
		} else if m, ok := ch["delta"]; ok {
			msg = m // streams reconstructed elsewhere; tolerate here
		}
		var finish string
		_ = json.Unmarshal(ch["finish_reason"], &finish)
		c := DebugChoice{Index: strconv.Itoa(i), Finish: finish}
		var obj map[string]json.RawMessage
		if json.Unmarshal(msg, &obj) == nil && obj != nil {
			if _, ok := obj["role"]; !ok {
				obj["role"] = json.RawMessage(`"assistant"`)
			}
			c.Msg = debugParseMessage(obj)
		}
		out = append(out, c)
	}
	return out
}

// debugReconstructStream merges SSE data frames back into choices: the deltas
// concatenated, tool-call arguments stitched per index, the last usage seen.
func debugReconstructStream(sse string) (choices []DebugChoice, usageLine string, frames int) {
	type acc struct {
		role, content, reasoning, refusal, finish string
		names                                     map[int]string
		ids                                       map[int]string
		args                                      map[int]*strings.Builder
		order                                     []int
	}
	byIndex := map[int]*acc{}
	var idxOrder []int

	for _, line := range strings.Split(sse, "\n") {
		trimmed := strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		frames++
		var doc map[string]json.RawMessage
		if json.Unmarshal([]byte(payload), &doc) != nil {
			continue
		}
		if raw, ok := doc["usage"]; ok && usageLine == "" {
			usageLine = debugUsageLine(raw)
		}
		if raw, ok := doc["choices"]; ok {
			var chs []map[string]json.RawMessage
			if json.Unmarshal(raw, &chs) == nil {
				for _, ch := range chs {
					var idx float64
					_ = json.Unmarshal(ch["index"], &idx)
					i := int(idx)
					a, ok := byIndex[i]
					if !ok {
						a = &acc{names: map[int]string{}, ids: map[int]string{}, args: map[int]*strings.Builder{}}
						byIndex[i] = a
						idxOrder = append(idxOrder, i)
					}
					var fr string
					if json.Unmarshal(ch["finish_reason"], &fr) == nil && fr != "" {
						a.finish = fr
					}
					var delta map[string]json.RawMessage
					if d, ok := ch["delta"]; ok && json.Unmarshal(d, &delta) == nil {
						var s string
						if x, ok := delta["role"]; ok && json.Unmarshal(x, &s) == nil && s != "" {
							a.role = s
						}
						if x, ok := delta["content"]; ok && json.Unmarshal(x, &s) == nil {
							a.content += s
						}
						if x, ok := delta["reasoning_content"]; ok && json.Unmarshal(x, &s) == nil {
							a.reasoning += s
						}
						if x, ok := delta["refusal"]; ok && json.Unmarshal(x, &s) == nil {
							a.refusal += s
						}
						if x, ok := delta["tool_calls"]; ok {
							var tcs []map[string]json.RawMessage
							if json.Unmarshal(x, &tcs) == nil {
								for _, tc := range tcs {
									var ti float64
									_ = json.Unmarshal(tc["index"], &ti)
									j := int(ti)
									if _, seen := a.args[j]; !seen {
										a.args[j] = &strings.Builder{}
										a.order = append(a.order, j)
									}
									var s string
									if f, ok := tc["function"]; ok {
										var fn map[string]json.RawMessage
										if json.Unmarshal(f, &fn) == nil {
											if x, ok := fn["name"]; ok && json.Unmarshal(x, &s) == nil && s != "" {
												a.names[j] += s
											}
											if x, ok := fn["arguments"]; ok && json.Unmarshal(x, &s) == nil {
												a.args[j].WriteString(s)
											}
										}
									}
									if x, ok := tc["id"]; ok && json.Unmarshal(x, &s) == nil && s != "" {
										a.ids[j] = s
									}
								}
							}
						}
					}
				}
			}
		}
	}

	sort.Ints(idxOrder)
	for _, i := range idxOrder {
		a := byIndex[i]
		role := a.role
		if role == "" {
			role = "assistant"
		}
		msg := DebugMessage{Role: role, Reasoning: a.reasoning, Refusal: a.refusal}
		if a.content != "" {
			msg.Parts = append(msg.Parts, DebugPart{Kind: "text", Text: a.content})
		}
		for _, j := range a.order {
			msg.ToolCalls = append(msg.ToolCalls, DebugToolCall{
				ID: a.ids[j], Name: a.names[j], Args: debugPrettyRaw(a.args[j].String()),
			})
		}
		msg.HasParts = len(msg.Parts) > 0
		choices = append(choices, DebugChoice{Index: strconv.Itoa(i), Msg: msg, Finish: a.finish})
	}
	return choices, usageLine, frames
}

func debugUsageLine(raw json.RawMessage) string {
	var u struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
		PromptDetails    *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionDetails *struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	}
	if json.Unmarshal(raw, &u) != nil {
		return ""
	}
	parts := []string{
		fmt.Sprintf("prompt %d", u.PromptTokens),
		fmt.Sprintf("completion %d", u.CompletionTokens),
	}
	if u.PromptDetails != nil && u.PromptDetails.CachedTokens > 0 {
		parts = append(parts, fmt.Sprintf("cached %d", u.PromptDetails.CachedTokens))
	}
	if u.CompletionDetails != nil && u.CompletionDetails.ReasoningTokens > 0 {
		parts = append(parts, fmt.Sprintf("reasoning %d", u.CompletionDetails.ReasoningTokens))
	}
	if u.TotalTokens > 0 {
		parts = append(parts, fmt.Sprintf("total %d", u.TotalTokens))
	}
	return strings.Join(parts, " · ")
}

// --- JSON helpers ----------------------------------------------------------------

func debugPretty(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}

// debugPrettyRaw re-indents a JSON text, or returns it unchanged when it is
// not valid JSON (a truncated capture still shows what was recorded).
func debugPrettyRaw(s string) string {
	t := strings.TrimSpace(s)
	if t == "" {
		return s
	}
	var v any
	dec := json.NewDecoder(strings.NewReader(t))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return s
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return s
	}
	return string(b)
}

// --- display helpers -----------------------------------------------------------

func dbgRowModel(r usage.DebugRow) string {
	if r.Route != "" {
		return r.Route
	}
	return r.Model
}

func dbgRowTitle(r usage.DebugRow) string {
	if r.Route != "" {
		return r.Route + " → " + r.Model
	}
	return r.Model
}

func dbgStatusText(r usage.DebugRow) string {
	if r.Status == usage.StatusOK || r.Status == "" {
		return strconv.Itoa(r.HTTPStatus)
	}
	return r.Status + " " + strconv.Itoa(r.HTTPStatus)
}

func dbgStatusClass(r usage.DebugRow) string {
	if r.Status == usage.StatusOK || r.Status == "" {
		return "up"
	}
	return "down"
}

func dbgCapStatusText(c *usage.DebugCapture) string {
	if c.Status == usage.StatusOK || c.Status == "" {
		return "ok"
	}
	return c.Status
}

func dbgCapStatusClass(c *usage.DebugCapture) string {
	if c.Status == usage.StatusOK || c.Status == "" {
		return "up"
	}
	return "down"
}

// dbgRoleKey maps a message role onto a CSS class suffix; anything unknown
// (including an empty role) reads as the neutral "other".
func dbgRoleKey(role string) string {
	switch strings.ToLower(role) {
	case "system", "developer", "user", "assistant", "tool":
		return strings.ToLower(role)
	}
	return "other"
}

func dbgMsgClass(role string) string {
	return "dbg-msg dbg-role-" + dbgRoleKey(role)
}

func dbgRoleLabel(m DebugMessage) string {
	if m.Name != "" {
		return m.Role + " · " + m.Name
	}
	return m.Role
}

// dbgTrunc shortens a long URL for display without pretending it is safe.
func dbgTrunc(s string) string {
	if len(s) <= 120 {
		return s
	}
	return s[:117] + "…"
}

// --- markdown ---------------------------------------------------------------------

// dbgMarkdown renders message text: CommonMark with the default (safe)
// renderer, so raw HTML inside a message is escaped, never emitted, and a
// link or image destination only survives with a harmless scheme.
var dbgMarkdown = goldmark.New(
	goldmark.WithParserOptions(parser.WithASTTransformers(
		gutil.Prioritized(dbgLinkGuard{}, 1000),
	)),
)

type dbgLinkGuard struct{}

func (dbgLinkGuard) Transform(n *gast.Document, reader text.Reader, pc parser.Context) {
	_ = gast.Walk(n, func(node gast.Node, entering bool) (gast.WalkStatus, error) {
		if !entering {
			return gast.WalkContinue, nil
		}
		switch v := node.(type) {
		case *gast.Link:
			if !dbgSafeHref(string(v.Destination)) {
				v.Destination = []byte("#")
			}
		case *gast.Image:
			if !dbgSafeImage(string(v.Destination)) {
				v.Destination = []byte("about:blank")
				v.Title = []byte("blocked")
			}
		}
		return gast.WalkContinue, nil
	})
}

func dbgSafeHref(s string) bool {
	if s == "" || strings.HasPrefix(s, "#") || strings.HasPrefix(s, "/") {
		return true
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "mailto", "":
		return true
	}
	return false
}

func dbgSafeImage(s string) bool {
	if strings.HasPrefix(s, "data:image/") {
		return true
	}
	return dbgSafeHref(s) && !strings.HasPrefix(s, "#") && !strings.HasPrefix(s, "/")
}

// dbgMD renders one markdown source into a trusted-HTML component.
func dbgMD(src string) templ.Component {
	var buf bytes.Buffer
	if err := dbgMarkdown.Convert([]byte(src), &buf); err != nil {
		return templ.Raw("<span class=\"err\">markdown render failed: " + html.EscapeString(err.Error()) + "</span>")
	}
	return templ.Raw(buf.String())
}
