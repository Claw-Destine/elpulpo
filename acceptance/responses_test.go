package acceptance_test

// The OpenAI Responses endpoint: POST /v1/responses routes through exactly
// the same machinery as chat — published id or alias, slots, limits, usage —
// and differs only in the dialect: `input` passes through untouched, usage
// arrives as input_tokens/output_tokens (in the terminal event on streams),
// and the response identity lives inside the response object, which streams
// carry as snapshots. El Pulpo rewrites the model wherever it appears and
// is otherwise a transparent pipe: it does not store responses, so
// retrieving one by id is not proxied.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"elpulpo/internal/app"
	"elpulpo/internal/config"
	"elpulpo/internal/testutil"
)

// respStart serves the naming-example fleet; the vllm box answers both
// endpoints.
func respStart(t *testing.T, h *testutil.Harness) naming {
	t.Helper()
	return startNaming(t, h)
}

// TestAcceptance_59_ResponsesRoutingNonStreaming: the upstream sees the base
// model name and the untouched input fields; the client's response carries
// the published id; the usage row names endpoint responses with the
// upstream's input_tokens/output_tokens.
func TestAcceptance_59_ResponsesRoutingNonStreaming(t *testing.T) {
	h := testutil.Start(t)
	n := respStart(t, h)
	h.WaitForModel("deepseek-v4-flash-vllm@minion1")

	st, body := h.Responses("", map[string]any{
		"model":                "deepseek-v4-flash-vllm@minion1",
		"input":                "hello",
		"previous_response_id": "resp-earlier-123",
		"temperature":          0.5,
	})
	if st != 200 {
		t.Fatalf("status %d: %s\nlog:\n%s", st, body, h.LogString())
	}

	// The upstream sees the base model — and the fields El Pulpo does not
	// manage, verbatim: a responses body may chain previous_response_id or
	// carry tools, and the proxy must not drop what it does not understand.
	up := n.UpVllm.LastResponsesBody()
	var gotModel string
	_ = json.Unmarshal(up["model"], &gotModel)
	if gotModel != "deepseek-v4-flash" {
		t.Fatalf("upstream must see the base model, saw %q", gotModel)
	}
	var prev string
	_ = json.Unmarshal(up["previous_response_id"], &prev)
	if prev != "resp-earlier-123" {
		t.Fatalf("previous_response_id must pass through untouched, saw %q", prev)
	}
	if _, ok := up["temperature"]; !ok {
		t.Fatalf("temperature must pass through untouched: %v", up)
	}
	// Non-streaming gets no usage flag injected: the buffered answer always
	// carries usage.
	if _, ok := up["stream_options"]; ok {
		t.Fatalf("stream_options must not appear on a non-streaming request")
	}

	// Response identity: the response object carries the name asked for.
	var doc map[string]json.RawMessage
	mustJSON(t, body, &doc)
	var echoed string
	_ = json.Unmarshal(doc["model"], &echoed)
	if echoed != "deepseek-v4-flash-vllm@minion1" {
		t.Fatalf("response must carry the published id, got %q in %s", echoed, body)
	}
	var object string
	_ = json.Unmarshal(doc["object"], &object)
	if object != "response" {
		t.Fatalf("the dialect of the body must survive the rewrite, object=%q", object)
	}

	h.DrainWriter()
	rows := h.Rows()
	if len(rows) == 0 {
		t.Fatal("no usage row")
	}
	r := rows[len(rows)-1]
	if r.Endpoint != "responses" || r.HostID != "minion1" || r.ServerID != "vllm" ||
		r.Model != "deepseek-v4-flash-vllm@minion1" || r.Status != "ok" {
		t.Fatalf("row = %+v, want endpoint=responses minion1/vllm ok", r)
	}
	if r.TokensIn != 111 || r.TokensOut != 42 {
		t.Fatalf("row must carry the Responses usage counts (input_tokens/output_tokens): %+v", r)
	}
	if r.Estimated {
		t.Fatalf("usage was reported, the row must not be estimated: %+v", r)
	}

	// The log names the endpoint, so a grep can tell the dialects apart.
	if !strings.Contains(h.LogString(), `msg="responses request"`) ||
		!strings.Contains(h.LogString(), "endpoint=responses") {
		t.Fatalf("responses requests must log as their own line naming the endpoint:\n%s", h.LogString())
	}
}

// TestAcceptance_60_ResponsesStream: the event stream is piped through
// untouched, the model is rewritten wherever it sits (inside the response
// snapshots too), and usage comes from the terminal response.completed
// event, which carries it unconditionally — the proxy injects nothing, and a
// client's own stream_options passes through verbatim.
func TestAcceptance_60_ResponsesStream(t *testing.T) {
	h := testutil.Start(t)
	n := respStart(t, h)
	h.WaitForModel("deepseek-v4-flash-vllm@minion1")

	_, events, terr := h.ResponsesStream("", map[string]any{
		"model":  "deepseek-v4-flash-vllm@minion1",
		"input":  "hello",
		"stream": true,
	}, nil)
	if terr != "" {
		t.Fatalf("stream: %s\nlog:\n%s", terr, h.LogString())
	}
	if len(events) < 4 {
		t.Fatalf("the event stream must arrive whole, got %d frames", len(events))
	}

	// The dialect survives: typed events, and the stream ends with
	// response.completed — not with chat's data: [DONE].
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, `"type":"response.completed"`) {
		t.Fatalf("the terminal event must pass through: %s", joined)
	}
	if strings.Contains(joined, "[DONE]") {
		t.Fatalf("Responses streams must not gain a [DONE] sentinel")
	}

	// Response identity inside the snapshots: every response model the
	// stream carries is the name the client asked for — top level or nested.
	seenModel := 0
	for _, e := range events {
		var doc map[string]json.RawMessage
		if json.Unmarshal([]byte(e), &doc) != nil {
			continue
		}
		if raw, ok := doc["model"]; ok {
			var m string
			if json.Unmarshal(raw, &m) == nil && m != "" {
				seenModel++
				if m != "deepseek-v4-flash-vllm@minion1" {
					t.Fatalf("envelope model not rewritten: %q", m)
				}
			}
		}
		if raw, ok := doc["response"]; ok {
			var snap struct {
				Model string `json:"model"`
			}
			if json.Unmarshal(raw, &snap) == nil && snap.Model != "" {
				seenModel++
				if snap.Model != "deepseek-v4-flash-vllm@minion1" {
					t.Fatalf("response snapshot model not rewritten: %q", snap.Model)
				}
			}
		}
	}
	if seenModel == 0 {
		t.Fatal("no model field was seen in the stream to check the rewrite")
	}

	// A Responses stream needs no injected usage flag: the terminal event
	// carries usage unconditionally, so the proxy adds no stream_options of
	// its own — chat's include_usage habit is a dialect difference, not a
	// proxy rule.
	upBody := n.UpVllm.LastResponsesBody()
	if _, ok := upBody["stream_options"]; ok {
		t.Fatalf("the proxy must inject nothing into a Responses body: %s", upBody["stream_options"])
	}

	h.DrainWriter()
	r := h.Rows()[len(h.Rows())-1]
	if r.Endpoint != "responses" || r.Status != "ok" {
		t.Fatalf("row = %+v", r)
	}
	if r.TokensIn != 111 || r.TokensOut != 42 {
		t.Fatalf("usage must come from the terminal event: %+v", r)
	}
	if r.TTFTMs == nil || *r.TTFTMs <= 0 {
		t.Fatalf("a stream records ttft_ms > 0: %+v", r)
	}

	// A client's own stream_options is forwarded verbatim — with
	// include_token_usage false the fake reports no usage, and the row falls
	// back to the estimate.
	_, _, terr = h.ResponsesStream("", map[string]any{
		"model":          "deepseek-v4-flash-vllm@minion1",
		"input":          "hello",
		"stream":         true,
		"stream_options": map[string]any{"include_token_usage": false},
	}, nil)
	if terr != "" {
		t.Fatalf("second stream: %s", terr)
	}
	upBody = n.UpVllm.LastResponsesBody()
	if string(upBody["stream_options"]) != `{"include_token_usage":false}` {
		t.Fatalf("the client's stream_options must survive untouched: %s", upBody["stream_options"])
	}
	h.DrainWriter()
	rows := h.Rows()
	r = rows[len(rows)-1]
	if !r.Estimated || r.TokensIn == 0 || r.TokensOut == 0 {
		t.Fatalf("no usage reported ⇒ estimated row with non-zero counts: %+v", r)
	}
}

// TestAcceptance_61_ResponsesViaRouteAlias: a route alias is a name in the
// Responses vocabulary too — the member serves, the alias is answered, the
// row names the member.
func TestAcceptance_61_ResponsesViaRouteAlias(t *testing.T) {
	h := testutil.Start(t)
	f := lbStart(t, h, []config.RouteMember{
		{Model: "qwen3.8:27b-ollama@minion1", Cost: lbCost(1)},
		{Model: "qwen3.8:27b-ollama@minion2", Cost: lbCost(1)},
	})
	h.Eventually(9*time.Second, "alias qwen published", func() bool { return lbHasModel(h, "qwen") })

	st, body := h.Responses("", map[string]any{
		"model": "qwen",
		"input": "hello",
	})
	if st != 200 {
		t.Fatalf("responses via alias: %d %s\nlog:\n%s", st, body, h.LogString())
	}
	var doc map[string]json.RawMessage
	mustJSON(t, body, &doc)
	var echoed string
	_ = json.Unmarshal(doc["model"], &echoed)
	if echoed != "qwen" {
		t.Fatalf("the alias must be what the client sees back: %s", body)
	}
	if n := len(f.fast.ResponsesBodies()); n != 1 {
		t.Fatalf("an idle route takes the first member, fast got %d", n)
	}
	h.DrainWriter()
	r := h.Rows()[len(h.Rows())-1]
	if r.Model != f.idFast || r.Endpoint != "responses" {
		t.Fatalf("the row names the serving member: %+v", r)
	}
	if !strings.Contains(h.LogString(), `msg="responses request"`) ||
		!strings.Contains(h.LogString(), "route=qwen") {
		t.Fatalf("the log line names the route like chat's does:\n%s", h.LogString())
	}
}

// TestAcceptance_62_ResponsesErrors: the errors that are not the dialect —
// an unknown model, a raw 4xx passthrough, retrieval by id not proxied,
// the wrong method, and the upstream's own credentials on the forwarded
// request.
func TestAcceptance_62_ResponsesErrors(t *testing.T) {
	h := testutil.Start(t)
	n := respStart(t, h)
	h.WaitForModel("deepseek-v4-flash-vllm@minion1")

	// Unknown model: the same 404 and reason as chat.
	st, body := h.Responses("", map[string]any{"model": "nope-not-here", "input": "x"})
	if st != 404 {
		t.Fatalf("unknown model: %d %s", st, body)
	}
	var e errorShape
	mustJSON(t, body, &e)
	if e.Error.Code != "model_not_found" || e.Error.Reason != "not_configured" {
		t.Fatalf("want model_not_found/not_configured, got %+v", e.Error)
	}

	// Upstream 4xx: status and body passed through unchanged, recorded ok.
	n.UpVllm.Status = 404
	n.UpVllm.Body = `{"error":{"message":"model not loaded on the server","type":"invalid_request_error"}}`
	st, body = h.Responses("", map[string]any{
		"model": "deepseek-v4-flash-vllm@minion1", "input": "x",
	})
	if st != 404 || !strings.Contains(body, "model not loaded") {
		t.Fatalf("upstream 4xx must pass through: %d %s", st, body)
	}
	n.UpVllm.Status = 0
	n.UpVllm.Body = ""
	h.DrainWriter()
	r := h.Rows()[len(h.Rows())-1]
	if r.Status != "ok" || r.HTTPStatus != 404 || r.Endpoint != "responses" {
		t.Fatalf("an upstream 4xx is the upstream's answer, recorded ok: %+v", r)
	}

	// Retrieval by id is not proxied: El Pulpo is not a response store, so
	// the OpenAI-shaped subpath answers unsupported_endpoint.
	if st, body = h.GetAuth("/v1/responses/resp-abc123", ""); st != 404 {
		t.Fatalf("GET /v1/responses/{id}: %d %s", st, body)
	}
	if !strings.Contains(body, "unsupported_endpoint") {
		t.Fatalf("want unsupported_endpoint, got %s", body)
	}

	// A model missing from the body is the same 400 as chat.
	if st, body = h.Responses("", map[string]any{"input": "x"}); st != 400 {
		t.Fatalf("missing model: %d %s", st, body)
	}

	// Credentials: the upstream sees the server's auth_token, never the
	// client's proxy token — the same rule on this endpoint.
	ht := testutil.Start(t, func(o *app.Options) { o.ProxyToken = "proxy-secret" })
	up := testutil.NewUpstream(t, "openai", "gemma4:31b")
	ht.ApplyConfig(&config.Config{Hosts: []config.Host{
		{ID: "minion1", HostAddresses: []string{"127.0.0.1"}, Servers: []config.Server{
			{ID: "vllm", Port: up.Port(), API: "openai", AuthToken: "server-token-xyz"},
		}},
	}})
	ht.WaitForModel("gemma4:31b-vllm@minion1")
	if st, body = ht.Responses("proxy-secret", map[string]any{
		"model": "gemma4:31b-vllm@minion1", "input": "x",
	}); st != 200 {
		t.Fatalf("authorized responses request: %d %s", st, body)
	}
	if got := up.LastAuth(); got != "Bearer server-token-xyz" {
		t.Fatalf("upstream must see its own token, got %q", got)
	}
	if st, body := ht.PostJSON("/v1/responses", map[string]any{
		"model": "gemma4:31b-vllm@minion1", "input": "x",
	}); st != 401 || !strings.Contains(body, "unauthorized") {
		t.Fatalf("a request without the proxy token is refused: %d %s", st, body)
	}
}
