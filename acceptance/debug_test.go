package acceptance_test

// Debug-mode acceptance scenarios (specs/functional.specs.md, "Request
// debugging"): the switch is off by default and records nothing; when on it
// stores the context and the answer for routed requests, optionally only for
// one model; streams are stored as their frames and read back merged; the
// dashboard Debug screen drives it all — toggle, list, messages and raw panes.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"elpulpo/internal/config"
	"elpulpo/internal/testutil"
)

// --- helpers ---------------------------------------------------------------

// debugFleet starts one fake upstream behind host h1/server s1 publishing
// "alpha" (published id alpha-s1@h1) and waits until it is routable.
func debugFleet(t *testing.T, h *testutil.Harness) *testutil.Upstream {
	t.Helper()
	up := testutil.NewUpstream(t, "openai", "alpha")
	h.ApplyConfig(&config.Config{Hosts: []config.Host{
		{ID: "h1", HostAddresses: []string{"127.0.0.1"}, Servers: []config.Server{
			{ID: "s1", Port: up.Port(), API: "openai"},
		}},
	}})
	h.WaitForModel("alpha-s1@h1")
	return up
}

// debugSet flips the switch (and optionally the model filter) the way the
// Debug screen's form does.
func debugSet(t *testing.T, h *testutil.Harness, enabled bool, model string) {
	t.Helper()
	dashAdoptCSRF(t, h)
	v := "false"
	if enabled {
		v = "true"
	}
	st, body := h.CSRFPost("/dashboard/action/debug/save", map[string]any{
		"debug_enabled": v, "debug_model": model,
	})
	if st != 200 {
		t.Fatalf("debug/save: %d %s", st, body)
	}
}

func debugDrain(h *testutil.Harness) {
	h.T.Helper()
	h.DrainWriter()
	for i := 0; i < 80 && h.App.Debug.Pending() > 0; i++ {
		time.Sleep(25 * time.Millisecond)
	}
	time.Sleep(60 * time.Millisecond) // the current insert
}

type debugMeta struct {
	ID         int64  `json:"id"`
	Model      string `json:"model"`
	Route      string `json:"route"`
	Stream     bool   `json:"stream"`
	Status     string `json:"status"`
	HTTPStatus int    `json:"http_status"`
}

func debugList(t *testing.T, h *testutil.Harness) (bool, string, []debugMeta) {
	t.Helper()
	st, body := h.Get("/api/debug/records")
	if st != 200 {
		t.Fatalf("GET /api/debug/records: %d %s", st, body)
	}
	var out struct {
		Enabled bool        `json:"enabled"`
		Model   string      `json:"model"`
		Records []debugMeta `json:"records"`
	}
	mustJSON(t, body, &out)
	return out.Enabled, out.Model, out.Records
}

func debugGet(t *testing.T, h *testutil.Harness, id int64) map[string]any {
	t.Helper()
	st, body := h.Get("/api/debug/record?id=" + itoa(int(id)))
	if st != 200 {
		t.Fatalf("GET /api/debug/record?id=%d: %d %s", id, st, body)
	}
	var out map[string]any
	mustJSON(t, body, &out)
	return out
}

func debugChat(t *testing.T, h *testutil.Harness, model string) {
	t.Helper()
	st, body := h.Chat("", map[string]any{
		"model": model,
		"messages": []any{
			map[string]string{"role": "system", "content": "Be brief."},
			map[string]string{"role": "user", "content": "hi"},
		},
	})
	if st != 200 {
		t.Fatalf("chat: %d %s", st, body)
	}
}

// --- scenarios ----------------------------------------------------------------

// Off by default: chat traffic writes no recordings.
func TestDebug_OffByDefaultRecordsNothing(t *testing.T) {
	h := testutil.Start(t)
	debugFleet(t, h)

	debugChat(t, h, "alpha-s1@h1")
	sst, chunks, serr := h.ChatStream("", map[string]any{
		"model":    "alpha-s1@h1",
		"stream":   true,
		"messages": []any{map[string]string{"role": "user", "content": "hi"}},
	}, nil)
	if sst != 200 || len(chunks) == 0 || serr != "" {
		t.Fatalf("stream chat: %d %v %v", sst, chunks, serr)
	}
	debugDrain(h)

	enabled, model, records := debugList(t, h)
	if enabled {
		t.Fatal("debug mode must default to off")
	}
	if model != "" {
		t.Fatalf("the model filter must default to every model, got %q", model)
	}
	if len(records) != 0 {
		t.Fatalf("recordings with debug off: %+v", records)
	}

	// The screen says so too.
	st, html := h.Get("/dashboard/debug")
	if st != 200 {
		t.Fatalf("GET /dashboard/debug: %d", st)
	}
	if !strings.Contains(html, "Debug recording is off") {
		t.Fatalf("the page does not say recording is off:\n%s", html)
	}
	if !strings.Contains(html, "<h1>Debug</h1>") {
		t.Fatalf("no Debug page:\n%s", html)
	}
}

// On with no filter: a buffered chat is stored with its context and its
// answer, and both render as messages and raw.
func TestDebug_OnRecordsContextAndResponse(t *testing.T) {
	h := testutil.Start(t)
	up := debugFleet(t, h)
	up.ChatFunc = func(req map[string]json.RawMessage, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cmpl-1","object":"chat.completion","model":"alpha","choices":[{"index":0,"message":{"role":"assistant","content":"**bold** reply"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":3}}`))
	}

	debugSet(t, h, true, "")

	// A multimodal context with a tool call round trip is the full shape:
	// text + system prompt + image data URL + tool_calls + tool result.
	st, body := h.Chat("", map[string]any{
		"model": "alpha-s1@h1",
		"messages": []any{
			map[string]string{"role": "system", "content": "You are terse."},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "what is in this picture?"},
				map[string]any{"type": "image_url", "image_url": map[string]string{
					"url": "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==",
				}},
			}},
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function", "function": map[string]string{
					"name": "get_weather", "arguments": `{"city":"Rome"}`,
				}},
			}},
			map[string]string{"role": "tool", "tool_call_id": "call_1", "content": "14C, clear"},
		},
	})
	if st != 200 {
		t.Fatalf("chat: %d %s", st, body)
	}
	debugDrain(h)

	_, _, records := debugList(t, h)
	if len(records) != 1 {
		t.Fatalf("want one recording, got %+v", records)
	}
	rec := records[0]
	if rec.Model != "alpha-s1@h1" || rec.Stream || rec.Status != "ok" || rec.HTTPStatus != 200 {
		t.Fatalf("recording meta wrong: %+v", rec)
	}

	full := debugGet(t, h, rec.ID)
	reqDoc, _ := full["request"].(map[string]any)
	msgs, _ := reqDoc["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("stored context lost messages: %v", reqDoc["messages"])
	}
	resp, _ := full["response"].(map[string]any)
	ch, _ := resp["choices"].([]any)
	if len(ch) != 1 {
		t.Fatalf("stored response lost the choice: %v", resp)
	}

	// Messages view (the default): roles, the image, the tool call, and the
	// answer rendered from markdown.
	st, html := h.Get("/dashboard/debug?sel=" + itoa(int(rec.ID)))
	if st != 200 {
		t.Fatalf("page with selection: %d", st)
	}
	for _, want := range []string{
		"dbg-role-system", "dbg-role-user", "dbg-role-tool",
		`<img class="dbg-img"`, "You are terse.", "what is in this picture?",
		"get_weather", "city", "Rome",
		"<strong>bold</strong> reply",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("messages view misses %q", want)
		}
	}
	// The raw view carries the request and response JSON text.
	st, html = h.Get("/dashboard/debug?sel=" + itoa(int(rec.ID)) + "&view=raw")
	if st != 200 {
		t.Fatalf("raw page: %d", st)
	}
	for _, want := range []string{"chat.completion", "image_url", "prompt_tokens"} {
		if !strings.Contains(html, want) {
			t.Fatalf("raw view misses %q", want)
		}
	}

	// Turning it off stops the recording (same switch path, form-shaped).
	debugSet(t, h, false, "")
	debugChat(t, h, "alpha-s1@h1")
	debugDrain(h)
	if _, _, again := debugList(t, h); len(again) != 1 {
		t.Fatalf("turning debug off did not stop recording: %+v", again)
	}
}

// A stream is stored as its SSE frames and reads back as the merged answer.
func TestDebug_StreamFramesAreRecordedAndMerged(t *testing.T) {
	h := testutil.Start(t)
	debugFleet(t, h)
	debugSet(t, h, true, "")

	sst, chunks, serr := h.ChatStream("", map[string]any{
		"model":    "alpha-s1@h1",
		"stream":   true,
		"messages": []any{map[string]string{"role": "user", "content": "hi"}},
	}, nil)
	if sst != 200 || len(chunks) == 0 || serr != "" {
		t.Fatalf("stream chat: %d %v %v", sst, chunks, serr)
	}
	debugDrain(h)

	_, _, records := debugList(t, h)
	if len(records) != 1 || !records[0].Stream {
		t.Fatalf("want one stream recording, got %+v", records)
	}
	full := debugGet(t, h, records[0].ID)
	sse, _ := full["response_sse"].(string)
	if !strings.Contains(sse, "data:") || !strings.Contains(sse, "[DONE]") {
		t.Fatalf("stored SSE looks wrong:\n%s", sse)
	}

	st, html := h.Get("/dashboard/debug?sel=" + itoa(int(records[0].ID)))
	if st != 200 {
		t.Fatalf("page: %d", st)
	}
	if !strings.Contains(html, "Hello world") {
		t.Fatalf("the messages view must show the merged deltas:\n%s", html)
	}
	if !strings.Contains(html, "reconstructed from") {
		t.Fatalf("the merge must be disclosed:\n%s", html)
	}
	st, html = h.Get("/dashboard/debug?sel=" + itoa(int(records[0].ID)) + "&view=raw")
	if !strings.Contains(html, "data:") {
		t.Fatalf("the raw view must show the frames:\n%s", html)
	}
}

// The model filter narrows recordings to the selected name.
func TestDebug_ModelFilterSelectsWhatIsRecorded(t *testing.T) {
	h := testutil.Start(t)
	upA := testutil.NewUpstream(t, "openai", "alpha")
	upB := testutil.NewUpstream(t, "openai", "beta")
	h.ApplyConfig(&config.Config{Hosts: []config.Host{
		{ID: "h1", HostAddresses: []string{"127.0.0.1"}, Servers: []config.Server{
			{ID: "s1", Port: upA.Port(), API: "openai"},
			{ID: "s2", Port: upB.Port(), API: "openai"},
		}},
	}})
	h.WaitForModel("alpha-s1@h1")
	h.WaitForModel("beta-s2@h1")

	debugSet(t, h, true, "beta-s2@h1")
	debugChat(t, h, "alpha-s1@h1")
	debugChat(t, h, "beta-s2@h1")
	debugDrain(h)

	_, filter, records := debugList(t, h)
	if filter != "beta-s2@h1" {
		t.Fatalf("filter %q not reported", filter)
	}
	if len(records) != 1 || records[0].Model != "beta-s2@h1" {
		t.Fatalf("only the filtered model may be recorded: %+v", records)
	}

	// A load-balancer alias routes to the member; the filter names the
	// member's published id, so the request is recorded under the alias.
	cfg := &config.Config{Hosts: []config.Host{
		{ID: "h1", HostAddresses: []string{"127.0.0.1"}, Servers: []config.Server{
			{ID: "s1", Port: upA.Port(), API: "openai"},
			{ID: "s2", Port: upB.Port(), API: "openai"},
		}},
	}, LoadBalancer: &config.LoadBalancer{Routes: []config.Route{
		{Alias: "beta-lb", Members: []config.RouteMember{
			{Model: "beta-s2@h1", Cost: fptr(1)},
		}},
	}}}
	st, body := h.SaveConfigHTTP(cfg)
	if st != 200 {
		t.Fatalf("route save: %d %s", st, body)
	}
	h.WaitForModel("beta-lb")
	h.Chat("", map[string]any{"model": "beta-lb", "messages": []any{map[string]string{"role": "user", "content": "hi"}}})
	debugDrain(h)
	_, _, records = debugList(t, h)
	if len(records) != 2 || records[0].Model != "beta-lb" || records[0].Route != "beta-lb" {
		t.Fatalf("routed request not recorded: %+v", records)
	}
}

func fptr(v float64) *float64 { return &v }

// Clear empties the recordings; the switch itself is untouched.
func TestDebug_ClearAndCSRF(t *testing.T) {
	h := testutil.Start(t)
	debugFleet(t, h)
	debugSet(t, h, true, "")
	debugChat(t, h, "alpha-s1@h1")
	debugDrain(h)
	_, _, records := debugList(t, h)
	if len(records) != 1 {
		t.Fatalf("precondition: %+v", records)
	}

	// A mutation without the CSRF marker changes nothing (scenario 41).
	st, body := h.PostJSON("/dashboard/action/debug/clear", nil)
	if st != 403 {
		t.Fatalf("clear without CSRF marker: %d %s", st, body)
	}
	_, _, still := debugList(t, h)
	if len(still) != 1 {
		t.Fatalf("the markerless POST cleared: %+v", still)
	}

	dashAdoptCSRF(t, h)
	st, body = h.CSRFPost("/dashboard/action/debug/clear", map[string]any{})
	if st != 200 {
		t.Fatalf("debug/clear: %d %s", st, body)
	}
	_, _, after := debugList(t, h)
	if len(after) != 0 {
		t.Fatalf("clear removed nothing: %+v", after)
	}
	if en, _, _ := debugList(t, h); !en {
		t.Fatal("clear must not change the recording switch")
	}
}

// debugUntilMs reads the live window deadline from the records API.
func debugUntilMs(t *testing.T, h *testutil.Harness) int64 {
	t.Helper()
	st, body := h.Get("/api/debug/records")
	if st != 200 {
		t.Fatalf("GET /api/debug/records: %d %s", st, body)
	}
	var out struct {
		UntilMs int64 `json:"until_ms"`
	}
	mustJSON(t, body, &out)
	return out.UntilMs
}

// TestDebug_TimeLimitClosesTheWindow — recording runs inside a bounded
// window: enabling without naming a duration gets the shortest one, a named
// duration is honoured and restarts on Apply, and past the deadline nothing
// is recorded even before the app resets the switch itself.
func TestDebug_TimeLimitClosesTheWindow(t *testing.T) {
	h := testutil.Start(t)
	debugFleet(t, h)

	// A duration the form does not offer is refused with its field name,
	// and nothing changes.
	dashAdoptCSRF(t, h)
	if st, body := h.CSRFPost("/dashboard/action/debug/save", map[string]any{
		"debug_enabled": "true", "debug_for": "2d",
	}); st != 422 || !strings.Contains(body, "debug_for") {
		t.Fatalf("unknown duration: %d %s", st, body)
	}
	if en, _, _ := debugList(t, h); en {
		t.Fatal("a refused save must not switch recording on")
	}

	// Turning it on without a duration gets the default window (15m).
	if st, body := h.CSRFPost("/dashboard/action/debug/save", map[string]any{
		"debug_enabled": "true",
	}); st != 200 {
		t.Fatalf("enable default: %d %s", st, body)
	}
	if d := time.Until(time.UnixMilli(debugUntilMs(t, h))); d < 13*time.Minute || d > 17*time.Minute {
		t.Fatalf("default window should end about 15m out, got %v", d)
	}

	// An explicit Apply restarts the window at its own length.
	if st, body := h.CSRFPost("/dashboard/action/debug/save", map[string]any{
		"debug_enabled": "true", "debug_for": "1h",
	}); st != 200 {
		t.Fatalf("enable 1h: %d %s", st, body)
	}
	until := debugUntilMs(t, h)
	if d := time.Until(time.UnixMilli(until)); d < 55*time.Minute || d > 61*time.Minute {
		t.Fatalf("window should end about an hour out, got %v", d)
	}
	st, html := h.Get("/dashboard/debug")
	if st != 200 || !strings.Contains(html, "Debug recording is ON") || !strings.Contains(html, "It stops at") {
		t.Fatalf("the status line should name the window:\n%s", html)
	}
	// The form offers the windows, with the live one pre-selected.
	for _, want := range []string{`name="debug_for"`, ">15 minutes<", ">1 hour<", ">1 day<", ">no time limit<"} {
		if !strings.Contains(html, want) {
			t.Fatalf("the duration select should offer %q:\n%s", want, html)
		}
	}
	if !strings.Contains(html, `value="1h" selected`) {
		t.Fatalf("a one-hour window should pre-select its option:\n%s", html)
	}

	// Inside the window requests are recorded.
	debugChat(t, h, "alpha-s1@h1")
	debugDrain(h)
	_, _, records := debugList(t, h)
	if len(records) != 1 {
		t.Fatalf("want one recording inside the window, got %+v", records)
	}

	// Past the deadline: no recording, and the screen reads off — even
	// while the stored switch has not been reset yet. Expire the window
	// directly; the wall clock is not a test fixture.
	if _, v := h.App.Settings.Update(context.Background(), map[string]string{
		"debug_until": itoa(int(time.Now().Add(-time.Second).UnixMilli())),
	}); len(v) > 0 {
		t.Fatalf("expire window: %v", v)
	}
	debugChat(t, h, "alpha-s1@h1")
	debugDrain(h)
	en, _, records := debugList(t, h)
	if len(records) != 1 {
		t.Fatalf("an expired window must record nothing, got %+v", records)
	}
	if en {
		t.Fatal("the API must report an expired window as off")
	}
	if st, html := h.Get("/dashboard/debug"); st != 200 || !strings.Contains(html, "Debug recording is off") {
		t.Fatalf("the page must read off after the deadline:\n%s", html)
	}

	// And the app flips the switch back by itself within seconds.
	deadline := time.Now().Add(15 * time.Second)
	for h.App.Settings.Get().DebugEnabled && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if h.App.Settings.Get().DebugEnabled {
		t.Fatal("the closed window did not reset its switch")
	}
	if u := h.App.Settings.Get().DebugUntil; u != 0 {
		t.Fatalf("the reset must clear the deadline too, got %d", u)
	}
}
