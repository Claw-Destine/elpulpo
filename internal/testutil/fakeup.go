// Package testutil provides the fake upstreams and the end-to-end harness
// used by the acceptance suite: fakes are built on httptest and record the
// headers they received, which is what makes the credential-forwarding
// assertion (scenario 43) possible.
package testutil

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Upstream is a fake LLM server shaped either like OpenAI (`api:
// "openai"` → /v1/models) or like Ollama (`api: "ollama"` → /api/tags,
// plus the OpenAI-compatible /v1/chat/completions). Knobs cover fixed model
// lists, first-byte delay, mid-stream stall, missing usage, 500s and
// held-open streams for disconnect tests. Every request header is recorded.
type Upstream struct {
	srv      *httptest.Server
	API      string // "openai" (default) or "ollama"
	Models   []string
	ChatFunc func(req map[string]json.RawMessage, w http.ResponseWriter, r *http.Request)

	// knobs
	Status         int           // chat status override (0 = 200)
	Body           string        // body override for non-2xx
	NoUsage        bool          // omit usage entirely (fallback estimate)
	FirstByteDelay time.Duration // delay before the first response byte
	MidStreamStall time.Duration // pause between SSE chunk 2 and 3
	holdOpen       chan struct{} // if non-nil: stream stalls here until closed

	UsageIn       int64
	UsageOut      int64
	UsageCached   int64
	UsageReasoned int64
	ChatCount     int64
	Hits          atomic.Int64 // every request, any endpoint

	mu       sync.Mutex
	lastHdr  http.Header
	lastBody map[string]json.RawMessage
	bodies   []map[string]json.RawMessage
}

// NewUpstream starts a fake upstream on 127.0.0.1 (or host, if given as
// "127.0.0.2" style prefix in host) and returns it with Port set.
func NewUpstream(t testing.TB, api string, models ...string) *Upstream {
	t.Helper()
	u := &Upstream{API: api, Models: models, UsageIn: 111, UsageOut: 42}
	if u.API == "" {
		u.API = "openai"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", u.handleModels)
	mux.HandleFunc("/api/tags", u.handleModels)
	mux.HandleFunc("/v1/chat/completions", u.handleChat)
	srv := httptest.NewServer(mux)
	u.srv = srv
	t.Cleanup(srv.Close)
	return u
}

// NewUpstreamOn binds the fake upstream to a specific loopback address
// (127.0.0.1 and 127.0.0.2 — the whole /8 is local on Linux — so address
// failover is exercised for real).
func NewUpstreamOn(t testing.TB, host, api string, models ...string) *Upstream {
	t.Helper()
	u := NewUpstream(t, api, models...)
	u.srv.Close()
	ln, err := net.Listen("tcp", host+":0")
	if err != nil {
		t.Fatalf("bind %s: %v", host, err)
	}
	u.srv = &httptest.Server{Listener: ln, Config: &http.Server{Handler: u.handler()}}
	u.srv.Start()
	t.Cleanup(u.srv.Close)
	return u
}

func (u *Upstream) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", u.handleModels)
	mux.HandleFunc("/api/tags", u.handleModels)
	mux.HandleFunc("/v1/chat/completions", u.handleChat)
	return mux
}

// URL is the fake's base URL.
func (u *Upstream) URL() string { return u.srv.URL }

// Port is the TCP port it listens on.
func (u *Upstream) Port() int {
	hostport := u.srv.URL[strings.LastIndexByte(u.srv.URL, ':')+1:]
	if i := strings.IndexByte(hostport, '/'); i >= 0 {
		hostport = hostport[:i]
	}
	n, _ := strconv.Atoi(hostport)
	return n
}

// SetHold installs (nil clears) a gate: streaming responses stall at the
// end until the gate channel is closed or receives.
func (u *Upstream) SetHold(ch chan struct{}) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.holdOpen = ch
}

// Hold returns the current gate.
func (u *Upstream) Hold() chan struct{} {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.holdOpen
}

// Close stops the fake early (scenario: upstream stopped).
func (u *Upstream) Close() { u.srv.Close() }

// LastAuth returns the Authorization header of the last chat request.
func (u *Upstream) LastAuth() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.lastHdr == nil {
		return ""
	}
	return u.lastHdr.Get("Authorization")
}

// LastInboundHeader returns any header of the last chat request.
func (u *Upstream) LastInboundHeader(k string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.lastHdr == nil {
		return ""
	}
	return u.lastHdr.Get(k)
}

// LastChatBody returns the last parsed chat request body.
func (u *Upstream) LastChatBody() map[string]json.RawMessage {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.lastBody
}

// ChatBodies returns every parsed chat body in arrival order.
func (u *Upstream) ChatBodies() []map[string]json.RawMessage {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]map[string]json.RawMessage(nil), u.bodies...)
}

func (u *Upstream) handleModels(w http.ResponseWriter, r *http.Request) {
	u.Hits.Add(1)
	u.mu.Lock()
	u.lastHdr = r.Header.Clone()
	models := append([]string(nil), u.Models...)
	u.mu.Unlock()
	out := map[string]any{}
	if u.API == "ollama" {
		var ms []map[string]string
		for _, m := range models {
			ms = append(ms, map[string]string{"name": m})
		}
		out["models"] = ms
	} else {
		var ms []map[string]string
		for _, m := range models {
			ms = append(ms, map[string]string{"id": m, "object": "model"})
		}
		out["data"] = ms
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// SetModels changes what the upstream lists, which is how a test drives a
// model-list change on a server that stayed reachable.
func (u *Upstream) SetModels(models ...string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.Models = models
}

func (u *Upstream) handleChat(w http.ResponseWriter, r *http.Request) {
	u.Hits.Add(1)
	u.mu.Lock()
	u.lastHdr = r.Header.Clone()
	u.ChatCount++
	u.mu.Unlock()
	var body map[string]json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&body)
	u.mu.Lock()
	u.lastBody = body
	u.bodies = append(u.bodies, body)
	u.mu.Unlock()

	if u.ChatFunc != nil {
		u.ChatFunc(body, w, r)
		return
	}
	if u.Status >= 400 {
		if u.FirstByteDelay > 0 {
			time.Sleep(u.FirstByteDelay)
		}
		w.WriteHeader(u.Status)
		b := u.Body
		if b == "" {
			b = `{"error":{"message":"boom"}}`
		}
		_, _ = w.Write([]byte(b))
		return
	}
	var model string
	_ = json.Unmarshal(body["model"], &model)
	var stream bool
	if raw, ok := body["stream"]; ok {
		_ = json.Unmarshal(raw, &stream)
	}
	var so map[string]any
	if raw, ok := body["stream_options"]; ok {
		_ = json.Unmarshal(raw, &so)
	}
	includeUsage := true
	if so != nil {
		if v, ok := so["include_usage"].(bool); ok {
			includeUsage = v
		}
	}

	if !stream {
		if u.FirstByteDelay > 0 {
			time.Sleep(u.FirstByteDelay)
		}
		resp := map[string]any{
			"id": "cmpl-fake", "object": "chat.completion", "created": 1, "model": model,
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]string{"role": "assistant", "content": "Hello from the fake upstream — a nice long reply to estimate."},
				"finish_reason": "stop",
			}},
		}
		if !u.NoUsage {
			usage := map[string]any{"prompt_tokens": u.UsageIn, "completion_tokens": u.UsageOut}
			if u.UsageCached > 0 {
				usage["prompt_tokens_details"] = map[string]any{"cached_tokens": u.UsageCached}
			}
			if u.UsageReasoned > 0 {
				usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": u.UsageReasoned}
			}
			resp["usage"] = usage
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	// SSE.
	if u.FirstByteDelay > 0 {
		time.Sleep(u.FirstByteDelay)
	}
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	chunk := func(payload any) {
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	deltas := []string{"Hel", "lo ", "wor", "ld"}
	for i, d := range deltas {
		chunk(map[string]any{
			"id": "cmpl-fake", "object": "chat.completion.chunk", "created": 1, "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": d}}},
		})
		if i == 1 && u.MidStreamStall > 0 {
			time.Sleep(u.MidStreamStall)
		}
	}
	if hold := u.Hold(); hold != nil {
		select {
		case <-hold:
		case <-time.After(30 * time.Second):
		}
		return
	}
	if !u.NoUsage && includeUsage {
		chunk(map[string]any{
			"id": "cmpl-fake", "object": "chat.completion.chunk", "created": 1, "model": model,
			"choices": []any{},
			"usage":   map[string]any{"prompt_tokens": u.UsageIn, "completion_tokens": u.UsageOut},
		})
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// NewUpstreamOnPort binds the fake upstream to host:port exactly — for
// scenarios where several addresses share one port and liveness flips.
func NewUpstreamOnPort(t testing.TB, hostport, api string, models ...string) *Upstream {
	t.Helper()
	u := &Upstream{API: api, Models: models, UsageIn: 111, UsageOut: 42}
	if u.API == "" {
		u.API = "openai"
	}
	ln, err := net.Listen("tcp", hostport)
	if err != nil {
		t.Fatalf("bind %s: %v", hostport, err)
	}
	u.srv = &httptest.Server{Listener: ln, Config: &http.Server{Handler: u.handler()}}
	u.srv.Start()
	t.Cleanup(u.srv.Close)
	return u
}
