// Package acceptance holds the end-to-end acceptance suite: each scenario
// from specs/functional.specs.md is one named test driving the real
// listener against fake upstreams.
package acceptance_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"elpulpo/internal/app"
	"elpulpo/internal/config"
	"elpulpo/internal/testutil"
)

// namingConfig serves the Model-naming example: minion1 (ollama + vllm) and
// minion2 (ollama), with the published ids of scenario 1.
type naming struct {
	Up1, UpVllm, Up2 *testutil.Upstream
}

func (n naming) config() *config.Config {
	return &config.Config{Hosts: []config.Host{
		{ID: "minion1", HostAddresses: []string{"127.0.0.1"}, Servers: []config.Server{
			{ID: "ollama", Port: n.Up1.Port(), API: "ollama"},
			{ID: "vllm", Port: n.UpVllm.Port(), API: "openai"},
		}},
		{ID: "minion2", HostAddresses: []string{"127.0.0.2"}, Servers: []config.Server{
			{ID: "ollama", Port: n.Up2.Port(), API: "ollama"},
		}},
	}}
}

func startNaming(t *testing.T, h *testutil.Harness) naming {
	t.Helper()
	n := naming{
		Up1:    testutil.NewUpstream(t, "ollama", "qwen3.8:27b", "gemma4:31b"),
		UpVllm: testutil.NewUpstream(t, "openai", "deepseek-v4-flash"),
		Up2:    testutil.NewUpstreamOn(t, "127.0.0.2", "ollama", "gpt-oss:120b", "qwen3.8:27b"),
	}
	h.ApplyConfig(n.config())
	return n
}

var wantIDs = []string{
	"deepseek-v4-flash-vllm@minion1",
	"gemma4:31b-ollama@minion1",
	"gpt-oss:120b-ollama@minion2",
	"qwen3.8:27b-ollama@minion1",
	"qwen3.8:27b-ollama@minion2",
}

// TestAcceptance_1_ModelCatalogue: all servers healthy, GET /v1/models
// returns exactly the five published ids, sorted.
func TestAcceptance_1_ModelCatalogue(t *testing.T) {
	h := testutil.Start(t)
	startNaming(t, h)
	for _, id := range wantIDs {
		h.WaitForModel(id)
	}
	got := h.ModelIDs()
	if len(got) != len(wantIDs) {
		t.Fatalf("got %d ids %s, want %d", len(got), testutil.ModelNames(got), len(wantIDs))
	}
	for i := range got {
		if got[i] != wantIDs[i] {
			t.Fatalf("ids not exactly the published set (sorted): %s", testutil.ModelNames(got))
		}
	}
}

// TestAcceptance_3_StoppedServerWithdraws: a stopped server's models
// disappear within one interval; a request answers 404 not_available.
func TestAcceptance_3_StoppedServerWithdraws(t *testing.T) {
	h := testutil.Start(t)
	n := startNaming(t, h)
	h.WaitForModel("deepseek-v4-flash-vllm@minion1")

	n.UpVllm.Close()
	h.WaitForNoModel("deepseek-v4-flash-vllm@minion1")

	st, body := h.Chat("", map[string]any{
		"model":    "deepseek-v4-flash-vllm@minion1",
		"messages": []any{map[string]string{"role": "user", "content": "hi"}},
	})
	if st != 404 {
		t.Fatalf("status %d: %s", st, body)
	}
	var e errorShape
	mustJSON(t, body, &e)
	if e.Error.Code != "model_not_found" || e.Error.Reason != "not_available" {
		t.Fatalf("want model_not_found/not_available, got %+v", e.Error)
	}
}

type errorShape struct {
	Error struct {
		Message string `json:"message"`
		Code    string `json:"code"`
		Reason  string `json:"reason"`
	} `json:"error"`
}

func mustJSON(t *testing.T, body string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("json %q: %v", body, err)
	}
}

// TestAcceptance_4_NonStreamingRouting: upstream sees the base model name,
// the client sees the published id, and the usage row carries host, server,
// status ok and the upstream usage numbers.
func TestAcceptance_4_NonStreamingRouting(t *testing.T) {
	h := testutil.Start(t)
	n := startNaming(t, h)
	h.WaitForModel("deepseek-v4-flash-vllm@minion1")

	st, body := h.Chat("", map[string]any{
		"model":    "deepseek-v4-flash-vllm@minion1",
		"messages": []any{map[string]string{"role": "user", "content": "hi there, how are you?"}},
	})
	if st != 200 {
		t.Fatalf("status %d: %s", st, body)
	}
	var resp struct {
		Model string `json:"model"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	mustJSON(t, body, &resp)
	if resp.Model != "deepseek-v4-flash-vllm@minion1" {
		t.Fatalf("client model = %q, want published id", resp.Model)
	}
	if resp.Usage.PromptTokens != 111 || resp.Usage.CompletionTokens != 42 {
		t.Fatalf("client usage = %+v, want upstream numbers", resp.Usage)
	}
	// The upstream saw the base model name.
	last := n.UpVllm.LastChatBody()
	var seen string
	_ = json.Unmarshal(last["model"], &seen)
	if seen != "deepseek-v4-flash" {
		t.Fatalf("upstream model = %q, want deepseek-v4-flash", seen)
	}
	rows := h.Rows()
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.HostID != "minion1" || r.ServerID != "vllm" || r.Status != "ok" ||
		r.Model != "deepseek-v4-flash-vllm@minion1" || r.TokensIn != 111 || r.TokensOut != 42 ||
		r.HTTPStatus != 200 || r.Endpoint != "chat" {
		t.Fatalf("row = %+v", r)
	}
}

// TestAcceptance_5_StreamingUsage: chunks arrive before generation
// completes, the final chunk carries usage, and the row has non-zero
// tokens plus ttft_ms > 0.
func TestAcceptance_5_StreamingUsage(t *testing.T) {
	h := testutil.Start(t)
	n := startNaming(t, h)
	h.WaitForModel("deepseek-v4-flash-vllm@minion1")
	n.UpVllm.MidStreamStall = 1500 * time.Millisecond

	start := time.Now()
	var firstAt time.Duration
	var usageChunk string
	st, seen, err := h.ChatStream("", map[string]any{
		"model":    "deepseek-v4-flash-vllm@minion1",
		"messages": []any{map[string]string{"role": "user", "content": "hi"}},
		"stream":   true,
	}, func(payload string) bool {
		if firstAt == 0 {
			firstAt = time.Since(start)
		}
		if strings.Contains(payload, "usage") {
			usageChunk = payload
		}
		return true
	})
	if st != 200 || err != "" {
		t.Fatalf("stream status %d err %q", st, err)
	}
	if firstAt == 0 || firstAt > 1*time.Second {
		t.Fatalf("first chunk at %v — must arrive well before generation completes", firstAt)
	}
	if len(seen) < 3 {
		t.Fatalf("only %d data payloads", len(seen))
	}
	if !strings.Contains(usageChunk, `"prompt_tokens":111`) {
		t.Fatalf("final chunk carries no usage: %q (all: %v)", usageChunk, seen)
	}
	if !strings.Contains(usageChunk, "deepseek-v4-flash-vllm@minion1") {
		t.Fatalf("usage chunk model not rewritten to published id: %q", usageChunk)
	}
	rows := h.Rows()
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	r := rows[0]
	if r.TTFTMs == nil || *r.TTFTMs <= 0 || r.TokensIn != 111 || r.TokensOut != 42 || r.Status != "ok" {
		t.Fatalf("row = %+v", r)
	}
}

// TestAcceptance_6_EstimatedFallback: upstream reports no usage, so the
// row is estimated with non-zero counts.
func TestAcceptance_6_EstimatedFallback(t *testing.T) {
	h := testutil.Start(t)
	startNaming(t, h)
	up := testutil.NewUpstream(t, "openai", "nouser-model")
	up.NoUsage = true
	h.ApplyConfig(&config.Config{Hosts: []config.Host{
		{ID: "solo", HostAddresses: []string{"127.0.0.1"}, Servers: []config.Server{
			{ID: "main", Port: up.Port(), API: "openai"},
		}},
	}})
	h.WaitForModel("nouser-model-main@solo")

	st, body := h.Chat("", map[string]any{
		"model":    "nouser-model-main@solo",
		"messages": []any{map[string]string{"role": "user", "content": "a prompt with some characters in it to count"}},
	})
	if st != 200 {
		t.Fatalf("status %d: %s", st, body)
	}
	rows := h.Rows()
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	r := rows[0]
	if !r.Estimated || r.TokensIn == 0 || r.TokensOut == 0 {
		t.Fatalf("row = %+v, want estimated with non-zero counts", r)
	}
}

// TestAcceptance_7_Upstream500: the client gets 502 upstream_error and the
// row records upstream_error with zero tokens.
func TestAcceptance_7_Upstream500(t *testing.T) {
	h := testutil.Start(t)
	n := startNaming(t, h)
	h.WaitForModel("deepseek-v4-flash-vllm@minion1")
	n.UpVllm.Status = 500

	st, body := h.Chat("", map[string]any{
		"model":    "deepseek-v4-flash-vllm@minion1",
		"messages": []any{map[string]string{"role": "user", "content": "hi"}},
	})
	if st != 502 {
		t.Fatalf("status %d: %s", st, body)
	}
	var e errorShape
	mustJSON(t, body, &e)
	if e.Error.Code != "upstream_error" {
		t.Fatalf("code = %q", e.Error.Code)
	}
	rows := h.Rows()
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	r := rows[0]
	if r.Status != "upstream_error" || r.TokensIn != 0 || r.TokensOut != 0 || r.HTTPStatus != 502 {
		t.Fatalf("row = %+v", r)
	}
}

// TestAcceptance_8_ClientDisconnect: disconnecting mid-stream against
// max_concurrency 1 records a cancelled row and the next request is
// accepted immediately.
func TestAcceptance_8_ClientDisconnect(t *testing.T) {
	h := testutil.Start(t)
	up := testutil.NewUpstream(t, "openai", "held-model")
	held := make(chan struct{})
	up.SetHold(held)
	defer close(held)
	h.ApplyConfig(&config.Config{Hosts: []config.Host{
		{ID: "solo", HostAddresses: []string{"127.0.0.1"}, Servers: []config.Server{
			{ID: "main", Port: up.Port(), API: "openai", MaxConcurrency: 1},
		}},
	}})
	h.WaitForModel("held-model-main@solo")

	// Stream in, then hang up while the upstream holds the stream open.
	nChunks := 0
	st, seen, _ := h.ChatStream("", map[string]any{
		"model":    "held-model-main@solo",
		"messages": []any{map[string]string{"role": "user", "content": "hi"}},
		"stream":   true,
	}, func(string) bool {
		nChunks++
		return nChunks < 3 // disconnect mid-stream
	})
	if st != 200 || len(seen) < 3 {
		t.Fatalf("stream st=%d seen=%d", st, len(seen))
	}

	// The follow-up request must be accepted immediately: the cancelled
	// stream released its concurrency slot.
	start := time.Now()
	st2, body := h.Chat("", map[string]any{
		"model":    "held-model-main@solo",
		"messages": []any{map[string]string{"role": "user", "content": "hi again"}},
	})
	if st2 != 200 {
		t.Fatalf("follow-up request failed: %d %s", st2, body)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("follow-up waited %v — slot not released", d)
	}
	h.Eventually(6*time.Second, "cancelled row", func() bool {
		for _, r := range h.Rows() {
			if r.Status == "cancelled" {
				return true
			}
		}
		return false
	})
}

// TestAcceptance_9_QueueAndBusy: with max_concurrency 1 a second request
// is served when the first finishes; past queue_timeout it gets 429.
func TestAcceptance_9_QueueAndBusy(t *testing.T) {
	h := testutil.Start(t)
	up := testutil.NewUpstream(t, "openai", "queued-model")
	h.ApplyConfig(&config.Config{Hosts: []config.Host{
		{ID: "solo", HostAddresses: []string{"127.0.0.1"}, Servers: []config.Server{
			{ID: "main", Port: up.Port(), API: "openai", MaxConcurrency: 1},
		}},
	}})
	h.WaitForModel("queued-model-main@solo")

	// Part A: queued second request is served once the first finishes.
	hold := make(chan struct{})
	up.SetHold(hold)
	firstDone := make(chan struct{})
	go func() {
		h.ChatStream("", map[string]any{
			"model":    "queued-model-main@solo",
			"messages": []any{map[string]string{"role": "user", "content": "hi"}},
			"stream":   true,
		}, func(string) bool { return true })
		close(firstDone)
	}()
	time.Sleep(400 * time.Millisecond) // second request queues behind the first
	second := make(chan int, 1)
	go func() {
		st, _ := h.Chat("", map[string]any{
			"model":    "queued-model-main@solo",
			"messages": []any{map[string]string{"role": "user", "content": "second"}},
		})
		second <- st
	}()
	time.Sleep(400 * time.Millisecond)
	select {
	case st := <-second:
		t.Fatalf("second request finished early with %d while slot held", st)
	default:
	}
	close(hold)
	<-firstDone
	select {
	case st := <-second:
		if st != 200 {
			t.Fatalf("queued second request status %d", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued second request never served")
	}

	// Part B: queue_timeout exceeded answers 429 server_busy.
	if v := h.UpdateSettings(map[string]string{"queue_timeout": "1s"}); len(v) > 0 {
		t.Fatalf("settings: %v", v)
	}
	held2 := make(chan struct{})
	up.SetHold(held2)
	go func() {
		h.ChatStream("", map[string]any{
			"model":    "queued-model-main@solo",
			"messages": []any{map[string]string{"role": "user", "content": "hi"}},
			"stream":   true,
		}, func(string) bool { return true })
	}()
	time.Sleep(300 * time.Millisecond)
	st, body := h.Chat("", map[string]any{
		"model":    "queued-model-main@solo",
		"messages": []any{map[string]string{"role": "user", "content": "busy?"}},
	})
	if st != 429 {
		t.Fatalf("want 429, got %d %s", st, body)
	}
	if !strings.Contains(body, "server_busy") {
		t.Fatalf("body %s", body)
	}
	close(held2)
	up.SetHold(nil)
	h.UpdateSettings(map[string]string{"queue_timeout": "60s"})
}

// TestAcceptance_10_ProxyTokenAuth: a token is required on /v1 when set,
// and its absence logs the WARN line.
func TestAcceptance_10_ProxyTokenAuth(t *testing.T) {
	h := testutil.Start(t, func(o *app.Options) { o.ProxyToken = "s3cret" })
	startNaming(t, h)
	h.WaitForModel("gemma4:31b-ollama@minion1")

	if st, body := h.Get("/v1/models"); st != 401 {
		t.Fatalf("no-token request: %d %s", st, body)
	}
	if ids := h.ModelIDs("s3cret"); len(ids) != 5 {
		t.Fatalf("with token: %s", testutil.ModelNames(ids))
	}
	if st, body := h.PostJSON("/v1/chat/completions", map[string]any{"model": "x"}); st != 401 {
		t.Fatalf("chat without token: %d %s", st, body)
	}

	// Unset token: open access plus the WARN line on every start.
	h2 := testutil.Start(t)
	startNaming(t, h2)
	if st, _ := h2.Get("/v1/models"); st != 200 {
		t.Fatalf("open proxy: %d", st)
	}
	if !strings.Contains(h2.LogString(), "ELPULPO_PROXY_TOKEN is empty") {
		t.Fatalf("missing WARN in log:\n%s", h2.LogString())
	}
}

// TestAcceptance_12_BareAndAliasRejected: only ids listed by /v1/models
// are accepted; bare base names and aliases are not_configured 404s.
func TestAcceptance_12_BareAndAliasRejected(t *testing.T) {
	h := testutil.Start(t)
	startNaming(t, h)
	h.WaitForModel("qwen3.8:27b-ollama@minion1")

	for _, m := range []string{"qwen3.8:27b", "qwen27"} {
		st, body := h.Chat("", map[string]any{
			"model":    m,
			"messages": []any{map[string]string{"role": "user", "content": "hi"}},
		})
		if st != 404 {
			t.Fatalf("model %q: status %d", m, st)
		}
		var e errorShape
		mustJSON(t, body, &e)
		if e.Error.Code != "model_not_found" || e.Error.Reason != "not_configured" {
			t.Fatalf("model %q: want model_not_found/not_configured, got %+v", m, e.Error)
		}
	}
	if n := len(h.Rows()); n != 0 {
		t.Fatalf("unknown-model requests must not write usage rows, got %d", n)
	}
}

// TestAcceptance_15_NoDirectRoutes: /servers/... answers a plain 404.
func TestAcceptance_15_NoDirectRoutes(t *testing.T) {
	h := testutil.Start(t)
	startNaming(t, h)
	st, body := h.Do(http.MethodPost, "/servers/minion1/vllm/v1/chat/completions", nil, strings.NewReader("{}"))
	if st != 404 {
		t.Fatalf("status %d", st)
	}
	if strings.Contains(body, "error") {
		t.Fatalf("expected plain 404, got JSON: %s", body)
	}
}

// TestAcceptance_29_OversizedBody: 413 before routing; no usage row; the
// concurrency slot is untouched.
func TestAcceptance_29_OversizedBody(t *testing.T) {
	h := testutil.Start(t)
	up := testutil.NewUpstream(t, "openai", "small-model")
	h.ApplyConfig(&config.Config{Hosts: []config.Host{
		{ID: "solo", HostAddresses: []string{"127.0.0.1"}, Servers: []config.Server{
			{ID: "main", Port: up.Port(), API: "openai", MaxConcurrency: 1},
		}},
	}})
	h.WaitForModel("small-model-main@solo")
	if v := h.UpdateSettings(map[string]string{"max_request_size": "1KiB"}); len(v) > 0 {
		t.Fatalf("settings: %v", v)
	}

	big := map[string]any{
		"model":    "small-model-main@solo",
		"messages": []any{map[string]string{"role": "user", "content": strings.Repeat("x", 4000)}},
	}
	st, body := h.Chat("", big)
	if st != 413 {
		t.Fatalf("status %d: %s", st, body)
	}
	if !strings.Contains(body, "request_too_large") {
		t.Fatalf("body %s", body)
	}
	if n := len(h.Rows()); n != 0 {
		t.Fatalf("oversized request wrote %d usage rows", n)
	}
	// A normal request still passes (the slot was never taken).
	if v := h.UpdateSettings(map[string]string{"max_request_size": "32MiB"}); len(v) > 0 {
		t.Fatalf("settings: %v", v)
	}
	st, body = h.Chat("", map[string]any{
		"model":    "small-model-main@solo",
		"messages": []any{map[string]string{"role": "user", "content": "fits"}},
	})
	if st != 200 {
		t.Fatalf("normal request after 413: %d %s", st, body)
	}
}

// TestAcceptance_30_FirstByteTimeout: upstream accepts then stalls past
// first_byte_timeout → 504 upstream_timeout and a matching row.
func TestAcceptance_30_FirstByteTimeout(t *testing.T) {
	h := testutil.Start(t)
	up := testutil.NewUpstream(t, "openai", "slow-model")
	up.FirstByteDelay = 4 * time.Second
	h.ApplyConfig(&config.Config{Hosts: []config.Host{
		{ID: "solo", HostAddresses: []string{"127.0.0.1"}, Servers: []config.Server{
			{ID: "main", Port: up.Port(), API: "openai"},
		}},
	}})
	h.WaitForModel("slow-model-main@solo")
	if v := h.UpdateSettings(map[string]string{"first_byte_timeout": "1s"}); len(v) > 0 {
		t.Fatalf("settings: %v", v)
	}
	up.FirstByteDelay = 4 * time.Second

	start := time.Now()
	st, body := h.Chat("", map[string]any{
		"model":    "slow-model-main@solo",
		"messages": []any{map[string]string{"role": "user", "content": "hi"}},
	})
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v; first_byte_timeout not applied", d)
	}
	if st != 504 {
		t.Fatalf("status %d: %s", st, body)
	}
	if !strings.Contains(body, "upstream_timeout") {
		t.Fatalf("body %s", body)
	}
	rows := h.Rows()
	if len(rows) != 1 || rows[0].Status != "upstream_timeout" {
		t.Fatalf("rows %+v", rows)
	}
}

// TestAcceptance_31_StreamIdleTimeout: a mid-stream stall past
// stream_idle_timeout ends the stream; the client keeps the chunks it
// already received; the row is upstream_timeout.
func TestAcceptance_31_StreamIdleTimeout(t *testing.T) {
	h := testutil.Start(t)
	up := testutil.NewUpstream(t, "openai", "stalling-model")
	h.ApplyConfig(&config.Config{Hosts: []config.Host{
		{ID: "solo", HostAddresses: []string{"127.0.0.1"}, Servers: []config.Server{
			{ID: "main", Port: up.Port(), API: "openai"},
		}},
	}})
	h.WaitForModel("stalling-model-main@solo")
	if v := h.UpdateSettings(map[string]string{"stream_idle_timeout": "1s"}); len(v) > 0 {
		t.Fatalf("settings: %v", v)
	}
	up.MidStreamStall = 4 * time.Second

	st, seen, _ := h.ChatStream("", map[string]any{
		"model":    "stalling-model-main@solo",
		"messages": []any{map[string]string{"role": "user", "content": "hi"}},
		"stream":   true,
	}, func(string) bool { return true })
	if st != 200 {
		t.Fatalf("status %d", st)
	}
	if len(seen) < 2 {
		t.Fatalf("client kept %d chunks, want at least 2: %v", len(seen), seen)
	}
	h.Eventually(5*time.Second, "upstream_timeout row", func() bool {
		for _, r := range h.Rows() {
			if r.Status == "upstream_timeout" {
				return true
			}
		}
		return false
	})
}

// TestAcceptance_43_AuthTokenForwarded: the server's own auth_token goes
// upstream and the client's proxy token never does.
func TestAcceptance_43_AuthTokenForwarded(t *testing.T) {
	h := testutil.Start(t, func(o *app.Options) { o.ProxyToken = "client-token" })
	up := testutil.NewUpstream(t, "openai", "secret-model")
	h.ApplyConfig(&config.Config{Hosts: []config.Host{
		{ID: "solo", HostAddresses: []string{"127.0.0.1"}, Servers: []config.Server{
			{ID: "main", Port: up.Port(), API: "openai", AuthToken: "secret-abc"},
		}},
	}})
	h.WaitForModel("secret-model-main@solo")

	st, body := h.Chat("client-token", map[string]any{
		"model":    "secret-model-main@solo",
		"messages": []any{map[string]string{"role": "user", "content": "hi"}},
	})
	if st != 200 {
		t.Fatalf("status %d: %s", st, body)
	}
	if got := up.LastAuth(); got != "Bearer secret-abc" {
		t.Fatalf("upstream Authorization = %q, want Bearer secret-abc", got)
	}
	if strings.Contains(fmt.Sprint(up.LastChatBody()), "client-token") {
		t.Fatal("client proxy token leaked into the upstream body")
	}
}

// TestAcceptance_44_CorsAsymmetry: /v1 answers preflight and answers with
// CORS headers; the dashboard carries none.
func TestAcceptance_44_CorsAsymmetry(t *testing.T) {
	h := testutil.Start(t)
	startNaming(t, h)
	h.WaitForModel("gemma4:31b-ollama@minion1")

	resp, err := http.DefaultClient.Do(mustReq(http.MethodOptions, h.URL("/v1/models"), map[string]string{
		"Origin": "http://elsewhere.example", "Access-Control-Request-Method": "POST",
	}))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("preflight status %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("preflight ACAO = %q", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Authorization") ||
		!strings.Contains(got, "Content-Type") {
		t.Fatalf("preflight allow-headers = %q", got)
	}
	if got := resp.Header.Get("Access-Control-Max-Age"); got != "600" {
		t.Fatalf("max-age = %q", got)
	}

	resp, err = http.DefaultClient.Do(mustReq(http.MethodGet, h.URL("/v1/models"), map[string]string{"Origin": "http://elsewhere.example"}))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("GET /v1/models missing ACAO")
	}

	resp, err = http.DefaultClient.Do(mustReq(http.MethodGet, h.URL("/dashboard/servers"), map[string]string{"Origin": "http://elsewhere.example"}))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("dashboard must not carry CORS headers")
	}
	if resp.Header.Get("Access-Control-Allow-Methods") != "" {
		t.Fatalf("dashboard must not carry CORS method headers")
	}
}

// logLine returns the first captured line carrying the given slog message.
func logLine(log, msg string) string {
	want := `msg="` + msg + `"`
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, want) {
			return l
		}
	}
	return ""
}

// requestLines picks the proxy's per-request log lines out of everything the
// harness captured.
func requestLines(log string) []string {
	var out []string
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, `msg="chat request"`) {
			out = append(out, l)
		}
	}
	return out
}

// TestProxyRequestLogLines: every request that reaches the proxy ends in
// exactly one "chat request" line. Routed ones are INFO carrying the server,
// the queue wait and the token counts; a rejection is WARN carrying a reason
// and writes no usage row.
func TestProxyRequestLogLines(t *testing.T) {
	h := testutil.Start(t)
	up := testutil.NewUpstream(t, "openai", "log-model")
	h.ApplyConfig(&config.Config{Hosts: []config.Host{
		{ID: "solo", HostAddresses: []string{"127.0.0.1"}, Servers: []config.Server{
			{ID: "main", Port: up.Port(), API: "openai"},
		}},
	}})
	h.WaitForModel("log-model-main@solo")

	chat := func(model string, stream bool) {
		t.Helper()
		payload := map[string]any{
			"model":    model,
			"messages": []any{map[string]string{"role": "user", "content": "hi"}},
		}
		if stream {
			payload["stream"] = true
			st, _, err := h.ChatStream("", payload, nil)
			if st != 200 || err != "" {
				t.Fatalf("stream: status %d err %q", st, err)
			}
			return
		}
		if st, body := h.Chat("", payload); st != 200 {
			t.Fatalf("chat: status %d %s", st, body)
		}
	}

	chat("log-model-main@solo", false)
	chat("log-model-main@solo", true)
	// Unknown model: 404, no usage row.
	if st, _ := chatStatus(t, h, "nope-not-here"); st != 404 {
		t.Fatalf("unknown model: status %d", st)
	}
	// Upstream failure: a real failure, so ERROR.
	up.Status = 500
	if st, _ := chatStatus(t, h, "log-model-main@solo"); st != 502 {
		t.Fatalf("upstream 500: status %d", st)
	}

	h.DrainWriter()
	h.Eventually(3*time.Second, "exactly four chat request lines", func() bool {
		return len(requestLines(h.LogString())) == 4
	})
	lines := requestLines(h.LogString())

	// Routed requests: one line each, saying where it went and what it cost.
	for i, want := range [][]string{
		{"level=INFO", "host=solo", "server=main", "model=log-model-main@solo",
			"stream=false", "status=ok", "http_status=200", "wait_ms=", "tokens_in=", "estimated=false"},
		{"level=INFO", "host=solo", "server=main", "stream=true",
			"status=ok", "http_status=200", "ttft_ms="},
		{"level=WARN", "reason=not_configured", "status=rejected", "http_status=404"},
		{"level=ERROR", "host=solo", "server=main", "status=upstream_error",
			"http_status=502", `err="upstream answered 500"`},
	} {
		for _, attr := range want {
			if !strings.Contains(lines[i], attr) {
				t.Fatalf("line %d lacks %q:\n%s", i+1, attr, lines[i])
			}
		}
	}
	// The rejection leaves no row; the three routed requests leave three.
	if rows := h.Rows(); len(rows) != 3 {
		t.Fatalf("routed requests must write 3 usage rows, got %d: %+v", len(rows), rows)
	}
}

// chatStatus fires one chat request and reports only its status.
func chatStatus(t *testing.T, h *testutil.Harness, model string) (int, string) {
	t.Helper()
	return h.Chat("", map[string]any{
		"model":    model,
		"messages": []any{map[string]string{"role": "user", "content": "hi"}},
	})
}

func mustReq(method, url string, hdr map[string]string) *http.Request {
	r, _ := http.NewRequest(method, url, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}
