// Package proxy is the client-facing OpenAI-compatible proxy: exact-id and
// load-balancer routing, model rewrite in both directions, unbuffered SSE
// pass-through, per-server concurrency slots and usage capture.
package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"elpulpo/internal/balancer"
	"elpulpo/internal/config"
	"elpulpo/internal/health"
	"elpulpo/internal/usage"
)

// Deps wires the proxy to the live state.
type Deps struct {
	Settings *config.SettingsManager
	Store    *config.Store // the live config document: its routes drive balancing
	Health   *health.Manager
	Balancer *balancer.Selector
	Inflight *balancer.Registry
	Writer   *usage.Writer
	Log      *slog.Logger
}

type errorBody struct {
	Error struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Param   *string `json:"param"`
		Code    string  `json:"code"`
		Reason  string  `json:"reason,omitempty"`
	} `json:"error"`
}

// WriteError emits an OpenAI-shaped error document.
func WriteError(w http.ResponseWriter, status int, code, typ, msg, reason string) {
	var b errorBody
	b.Error.Message = msg
	b.Error.Type = typ
	b.Error.Code = code
	b.Error.Reason = reason
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(b)
}

// Proxy serves /v1/models and /v1/chat/completions.
type Proxy struct {
	deps Deps

	// Rejections before routing are counted and logged, not usage rows.
	RejUnknown  atomic.Int64
	RejTooLarge atomic.Int64
	RejBusy     atomic.Int64
}

// New builds the proxy.
func New(deps Deps) *Proxy { return &Proxy{deps: deps} }

// call carries one request from the front door to its terminal log line: what
// the client asked for, where the request was aimed, and what went wrong if
// anything did. Every request that reaches Chat ends in exactly one
// "chat request" line — routed ones via finish, which also writes the usage
// row, rejections via rejected, which writes none.
type call struct {
	start  time.Time
	remote string
	asked  string // model id exactly as the client spelled it
	stream bool
	target *health.Target // nil until routing has picked one
	route  string         // load-balancer alias it came through, "" for a direct id
	reply  string         // the model name the response carries: what was asked
	waitMs int64          // time spent queued for a concurrency slot
	err    error          // underlying failure, when there was one
}

// liveConfig is the config document a request routes against. In-flight
// requests keep the snapshot they started with.
func (p *Proxy) liveConfig() *config.Config {
	if p.deps.Store == nil {
		return &config.Config{}
	}
	return p.deps.Store.Current().Config
}

// resolve turns the asked model into exactly one target. A published id routes
// to its own server as ever; a load-balancer alias routes to the member its
// policy picks. The third result is the 404 reason when nothing resolved:
// not_configured for a name El Pulpo has never heard of, not_available for one
// it knows but cannot serve right now.
func (p *Proxy) resolve(cfg *config.Config, asked string) (*health.Target, string, string) {
	if t, ok := p.deps.Health.Route(asked); ok {
		if !t.State.Up() || t.State.ActiveAddr() == "" {
			return nil, "", "not_available" // known id behind a dark server
		}
		return t, "", ""
	}
	if _, isAlias := cfg.RouteFor(asked); isAlias {
		if p.deps.Balancer == nil {
			return nil, "", "not_available"
		}
		if _, member, ok := p.deps.Balancer.Resolve(cfg, asked); ok {
			return member.Target, asked, ""
		}
		return nil, "", "not_available" // every member is down or unloaded
	}
	// A published id of a server that is currently dark is absent from the
	// route table but known to the fleet: it is configured, just not
	// answerable — the distinction scenario 3 depends on.
	if p.deps.Health.Known(asked) {
		return nil, "", "not_available"
	}
	return nil, "", "not_configured"
}

// rejected reports a request that ends without a usage row: turned away before
// routing, or dropped building the upstream call. Those are counted and logged,
// never written as rows, so this line plus the counters is their only trace.
// A client mistake is WARN; a 5xx is El Pulpo failing at its own job, which is
// ERROR like every other failure.
func (p *Proxy) rejected(c *call, httpStatus int, reason string, extra ...any) {
	args := []any{
		"remote", c.remote, "model", c.asked, "stream", c.stream,
		"status", "rejected", "http_status", httpStatus, "reason", reason,
		"latency_ms", time.Since(c.start).Milliseconds(),
	}
	if httpStatus >= http.StatusInternalServerError {
		p.deps.Log.Error("chat request", append(args, extra...)...)
		return
	}
	p.deps.Log.Warn("chat request", append(args, extra...)...)
}

// Models serves GET /v1/models: published ids of healthy servers plus the
// load-balancer aliases that can answer right now, sorted, in OpenAI shape.
// The list is the complete requestable vocabulary: every name in it is a name
// POST /v1/chat/completions accepts.
func (p *Proxy) Models(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		WriteError(w, http.StatusMethodNotAllowed, "unsupported_endpoint", "invalid_request_error",
			"GET /v1/models only", "")
		return
	}
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	out := struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}{Object: "list", Data: []model{}}
	// A copy: ModelIDs hands out the route table's own slice, and appending to
	// it would write into shared state.
	base, aliases := p.deps.Health.ModelIDs(), p.routeAliases()
	ids := make([]string, 0, len(base)+len(aliases))
	ids = append(ids, base...)
	ids = append(ids, aliases...)
	sort.Strings(ids)
	for _, id := range ids {
		out.Data = append(out.Data, model{ID: id, Object: "model", OwnedBy: "elpulpo"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// routeAliases are the load-balancer names to publish: an alias appears only
// while at least one of its members can answer, so the list never carries a
// name that would only ever answer 404.
func (p *Proxy) routeAliases() []string {
	if p.deps.Balancer == nil || p.deps.Store == nil {
		return nil
	}
	return p.deps.Balancer.Aliases(p.deps.Store.Current().Config)
}

// Chat serves POST /v1/chat/completions.
func (p *Proxy) Chat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteError(w, http.StatusMethodNotAllowed, "unsupported_endpoint", "invalid_request_error",
			"POST /v1/chat/completions only", "")
		return
	}
	start := time.Now()
	set := p.deps.Settings.Get()
	c := &call{start: start, remote: r.RemoteAddr}

	// 1. Size gate before any parsing or routing: no usage row and no
	// concurrency slot is taken.
	limited := http.MaxBytesReader(w, r.Body, set.MaxRequestSize)
	body, err := io.ReadAll(limited)
	var mbErr *http.MaxBytesError
	if errors.As(err, &mbErr) || (err != nil && !errors.Is(err, context.Canceled)) {
		p.RejTooLarge.Add(1)
		p.rejected(c, http.StatusRequestEntityTooLarge, "request_too_large",
			"limit", set.MaxRequestSize, "err", err)
		WriteError(w, http.StatusRequestEntityTooLarge, "request_too_large", "invalid_request_error",
			fmt.Sprintf("request body exceeds the %d byte limit", set.MaxRequestSize), "")
		return
	}
	if err == nil {
		p.route(w, r, c, set, body)
		return
	}
	// context canceled while reading the body: client is gone, nothing was
	// routed, so there is no row.
	p.deps.Log.Debug("client gone while reading the request body", "remote", c.remote, "err", err)
}

func (p *Proxy) route(w http.ResponseWriter, r *http.Request, c *call, set *config.Settings, body []byte) {
	// 2. The body must parse as a JSON object. Managed fields are lifted
	// into a struct and the remainder is kept verbatim, so tool schemas,
	// multimodal payloads and future OpenAI fields survive untouched.
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		p.rejected(c, http.StatusBadRequest, "invalid_json")
		WriteError(w, http.StatusBadRequest, "invalid_request_error", "invalid_request_error",
			"request body must be a JSON object", "")
		return
	}
	var model string
	if raw, ok := fields["model"]; ok {
		_ = json.Unmarshal(raw, &model)
	}
	stream := false
	if raw, ok := fields["stream"]; ok {
		if err := json.Unmarshal(raw, &stream); err != nil {
			c.asked = model
			p.rejected(c, http.StatusBadRequest, "invalid_stream_flag")
			WriteError(w, http.StatusBadRequest, "invalid_request_error", "invalid_request_error",
				`"stream" must be a boolean`, "")
			return
		}
	}
	c.stream = stream
	if model == "" {
		p.rejected(c, http.StatusBadRequest, "model_missing")
		WriteError(w, http.StatusBadRequest, "invalid_request_error", "invalid_request_error",
			`"model" is required`, "")
		return
	}
	c.asked = model

	// 3. Route: an exact published id, or the member a load-balancer alias
	// picks right now. The request vocabulary stays equal to GET /v1/models —
	// the ids it lists and the aliases it lists are exactly the names that
	// resolve here; a bare base model name is neither.
	target, viaRoute, reason := p.resolve(p.liveConfig(), model)
	if target == nil {
		if reason == "not_configured" {
			p.RejUnknown.Add(1)
		}
		p.rejected(c, http.StatusNotFound, reason)
		WriteError(w, http.StatusNotFound, "model_not_found", "invalid_request_error",
			fmt.Sprintf("model %q is not a published model id or a load-balancer alias (reason: %s)", model, reason), reason)
		return
	}
	c.target = target
	c.route = viaRoute
	c.reply = model // the client sees back the name it asked for
	st := target.State
	if viaRoute == "" {
		p.deps.Log.Debug("routing request", "remote", c.remote, "asked", model,
			"host", target.HostID, "server", target.ServerID, "base", target.Base,
			"address", st.ActiveAddr(), "stream", stream)
	}

	// 3b. In-flight accounting for the balancer, taken at admission: the next
	// concurrent request must see this one, or a burst would pile every
	// request onto the same member. A request queued for a concurrency slot
	// therefore counts — it has been aimed at that machine already.
	p.deps.Inflight.Add(target.Published, 1)
	defer p.deps.Inflight.Add(target.Published, -1)
	if viaRoute != "" {
		p.deps.Log.Debug("balancing request", "remote", c.remote, "alias", viaRoute,
			"host", target.HostID, "server", target.ServerID, "base", target.Base,
			"address", st.ActiveAddr(), "model", target.Published, "stream", stream)
	}

	// 4. Concurrency slot: select over the per-server FIFO semaphore, the
	// queue timeout and the client context.
	queued := time.Now()
	if !st.AcquireSlot(r.Context(), set.QueueTimeout) {
		switch {
		case r.Context().Err() != nil:
			// Client left while queued; the request was aimed at a server,
			// so it terminates as a cancelled row.
			p.finish(c, usage.StatusCancelled, 499, 0, 0, 0, 0, false, nil)
		case !st.Up():
			p.rejected(c, http.StatusServiceUnavailable, "server_went_down",
				"host", target.HostID, "server", target.ServerID)
			WriteError(w, http.StatusServiceUnavailable, "server_unavailable", "api_error",
				fmt.Sprintf("server %s/%s went down while the request was queued", target.HostID, target.ServerID), "")
		default:
			p.RejBusy.Add(1)
			p.rejected(c, http.StatusTooManyRequests, "queue_timeout",
				"host", target.HostID, "server", target.ServerID,
				"wait_ms", time.Since(queued).Milliseconds(), "queue_timeout", set.QueueTimeout.String())
			WriteError(w, http.StatusTooManyRequests, "server_busy", "rate_limit_error",
				fmt.Sprintf("no free slot on %s/%s within the queue timeout", target.HostID, target.ServerID), "")
		}
		return
	}
	c.waitMs = time.Since(queued).Milliseconds()
	defer st.ReleaseSlot() // the slot is held for the whole streamed response
	if !st.Up() {
		p.rejected(c, http.StatusServiceUnavailable, "server_went_down",
			"host", target.HostID, "server", target.ServerID)
		WriteError(w, http.StatusServiceUnavailable, "server_unavailable", "api_error",
			fmt.Sprintf("server %s/%s went down while the request was queued", target.HostID, target.ServerID), "")
		return
	}
	st.InFlightAdd(1)
	defer st.InFlightAdd(-1)

	p.dispatch(w, r, c, fields, body, set)
}

// finish is how a routed request ends: one usage row and one log line, so the
// log and the usage table always agree on what reached a server. A request the
// client got a real answer for is INFO; a failure of El Pulpo or of an upstream
// is ERROR, with the underlying error attached when one was captured on the way
// out.
func (p *Proxy) finish(c *call, status string, httpStatus int,
	in, out, cached, reasoning int64, estimated bool, ttft *int64) {
	t := c.target
	lat := time.Since(c.start).Milliseconds()
	p.deps.Writer.Submit(usage.Row{
		TsMs: c.start.UnixMilli(), HostID: t.HostID, ServerID: t.ServerID, Model: t.Published,
		Endpoint: "chat", Status: status, HTTPStatus: httpStatus,
		TokensIn: in, TokensOut: out, TokensCached: cached, TokensReasoning: reasoning,
		Estimated: estimated, LatencyMs: lat, TTFTMs: ttft,
	})

	attrs := []any{
		"remote", c.remote, "host", t.HostID, "server", t.ServerID,
		"model", t.Published, "stream", c.stream,
		"status", status, "http_status", httpStatus,
		"latency_ms", lat, "wait_ms", c.waitMs,
		"tokens_in", in, "tokens_out", out,
		"tokens_cached", cached, "tokens_reasoning", reasoning,
		"estimated", estimated,
	}
	if c.route != "" {
		// Which member served it is the row; which alias asked is the route.
		attrs = append(attrs, "route", c.route)
	}
	if ttft != nil {
		attrs = append(attrs, "ttft_ms", *ttft)
	}
	if c.err != nil {
		attrs = append(attrs, "err", c.err)
	}
	if status == usage.StatusUpstreamError || status == usage.StatusUpstreamTimeout {
		p.deps.Log.Error("chat request", attrs...)
		return
	}
	p.deps.Log.Info("chat request", attrs...)
}

func (p *Proxy) dispatch(w http.ResponseWriter, r *http.Request, c *call,
	fields map[string]json.RawMessage, rawBody []byte, set *config.Settings) {

	t := c.target
	stream := c.stream
	scheme, port, authToken, _ := t.State.Routing()
	addr := t.State.ActiveAddr()
	if addr == "" {
		// Lost the address election after the up-check; failure rules apply.
		WriteError(w, http.StatusServiceUnavailable, "server_unavailable", "api_error",
			fmt.Sprintf("server %s/%s has no live address right now", t.HostID, t.ServerID), "")
		p.finish(c, usage.StatusUpstreamError, http.StatusServiceUnavailable, 0, 0, 0, 0, false, nil)
		return
	}

	// Managed-field rewrite: model becomes the base name; include_usage is
	// added only for streams that did not set stream_options themselves.
	out := make(map[string]json.RawMessage, len(fields))
	for k, v := range fields {
		out[k] = v
	}
	if m, err := json.Marshal(t.Base); err == nil {
		out["model"] = m
	}
	if stream {
		if _, ok := out["stream_options"]; !ok {
			out["stream_options"] = json.RawMessage(`{"include_usage":true}`)
		}
	}
	upBody, err := json.Marshal(out)
	if err != nil {
		p.rejected(c, http.StatusInternalServerError, "upstream_request_encode_failed", "err", err)
		WriteError(w, http.StatusInternalServerError, "internal_error", "api_error", "could not encode upstream request", "")
		return
	}

	upCtx := r.Context()
	var cancel context.CancelFunc
	if set.TotalTimeout > 0 {
		upCtx, cancel = context.WithTimeout(upCtx, set.TotalTimeout)
		defer cancel()
	}
	url := fmt.Sprintf("%s://%s:%d/v1/chat/completions", scheme, addr, port)
	req, err := http.NewRequestWithContext(upCtx, http.MethodPost, url, strings.NewReader(string(upBody)))
	if err != nil {
		p.rejected(c, http.StatusInternalServerError, "upstream_request_build_failed", "err", err)
		WriteError(w, http.StatusInternalServerError, "internal_error", "api_error", "could not build upstream request", "")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream, application/json")
	}
	// The inbound Authorization carried the client's proxy token and is
	// dropped before forwarding; upstream credentials come solely from the
	// server's own auth_token.
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	client := &http.Client{Transport: t.State.TransportFor(set, p.deps.Settings.Gen())}
	resp, err := client.Do(req)
	if err != nil {
		p.failUpstream(w, c, t, err, r.Context(), upCtx)
		return
	}
	defer resp.Body.Close()
	p.deps.Log.Debug("upstream responded", "host", t.HostID, "server", t.ServerID,
		"address", addr, "status", resp.StatusCode, "stream", stream)

	switch {
	case resp.StatusCode >= 500:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		c.err = fmt.Errorf("upstream answered %d", resp.StatusCode)
		WriteError(w, http.StatusBadGateway, "upstream_error", "api_error",
			fmt.Sprintf("upstream %s/%s returned %d", t.HostID, t.ServerID, resp.StatusCode), "")
		p.finish(c, usage.StatusUpstreamError, http.StatusBadGateway, 0, 0, 0, 0, false, nil)
		return
	case resp.StatusCode >= 400:
		// Upstream 4xx: status and body passed through unchanged.
		passthrough(w, resp)
		p.finish(c, usage.StatusOK, resp.StatusCode, 0, 0, 0, 0, false, nil)
		return
	}

	if stream {
		p.pumpStream(w, r, c, t, resp, set, rawBody)
		return
	}
	p.respondBuffered(w, r, c, t, resp, rawBody)
}

func passthrough(w http.ResponseWriter, resp *http.Response) {
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, io.LimitReader(resp.Body, 4<<20))
}

// failUpstream classifies a pre-response upstream failure: timeouts answer
// 504 upstream_timeout, everything else 502 upstream_error, and a client
// that already hung up records a cancelled row.
func (p *Proxy) failUpstream(w http.ResponseWriter, c *call, t *health.Target, err error, reqCtx, upCtx context.Context) {
	switch {
	case reqCtx.Err() != nil && !errors.Is(upCtx.Err(), context.DeadlineExceeded):
		p.finish(c, usage.StatusCancelled, 499, 0, 0, 0, 0, false, nil)
	case isTimeoutErr(err) || errors.Is(upCtx.Err(), context.DeadlineExceeded):
		c.err = err
		WriteError(w, http.StatusGatewayTimeout, "upstream_timeout", "api_error",
			fmt.Sprintf("upstream %s/%s timed out: %v", t.HostID, t.ServerID, err), "")
		p.finish(c, usage.StatusUpstreamTimeout, http.StatusGatewayTimeout, 0, 0, 0, 0, false, nil)
	default:
		c.err = err
		WriteError(w, http.StatusBadGateway, "upstream_error", "api_error",
			fmt.Sprintf("upstream %s/%s failed: %v", t.HostID, t.ServerID, err), "")
		p.finish(c, usage.StatusUpstreamError, http.StatusBadGateway, 0, 0, 0, 0, false, nil)
	}
}

func isTimeoutErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return strings.Contains(err.Error(), "timeout awaiting response headers")
}

// upstreamUsage is the OpenAI usage object including the optional detail
// subsets: cached ⊆ prompt tokens, reasoning ⊆ completion tokens.
type upstreamUsage struct {
	Prompt     int64
	Completion int64
	Cached     int64
	Reasoning  int64
}

func parseUsage(raw json.RawMessage) (upstreamUsage, bool) {
	var u struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		PromptDetails    *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionDetails *struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	}
	if err := json.Unmarshal(raw, &u); err != nil {
		return upstreamUsage{}, false
	}
	out := upstreamUsage{Prompt: u.PromptTokens, Completion: u.CompletionTokens}
	if u.PromptDetails != nil {
		out.Cached = u.PromptDetails.CachedTokens
	}
	if u.CompletionDetails != nil {
		out.Reasoning = u.CompletionDetails.ReasoningTokens
	}
	return out, true
}

// estimateTokens is the character heuristic used when upstream reports no
// usage at all; rows estimated this way carry estimated: true.
func estimateTokens(chars int) int64 {
	return (int64(chars) + 3) / 4
}

// respondBuffered handles non-streaming responses: read fully, rewrite the
// response model to the published id, take usage from the body.
func (p *Proxy) respondBuffered(w http.ResponseWriter, r *http.Request, c *call,
	t *health.Target, resp *http.Response, rawBody []byte) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		switch {
		case r.Context().Err() != nil:
			p.finish(c, usage.StatusCancelled, 499, 0, 0, 0, 0, false, nil)
		case isTimeoutErr(err):
			c.err = err
			WriteError(w, http.StatusGatewayTimeout, "upstream_timeout", "api_error", "upstream body read timed out", "")
			p.finish(c, usage.StatusUpstreamTimeout, http.StatusGatewayTimeout, 0, 0, 0, 0, false, nil)
		default:
			c.err = err
			WriteError(w, http.StatusBadGateway, "upstream_error", "api_error", "upstream body read failed", "")
			p.finish(c, usage.StatusUpstreamError, http.StatusBadGateway, 0, 0, 0, 0, false, nil)
		}
		return
	}
	var u upstreamUsage
	haveUsage := false
	outChars := 0
	// Response identity: the model field is rewritten back to the name the
	// client asked for — its published id, or the alias it balanced through.
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) == nil && doc != nil {
		if _, ok := doc["model"]; ok {
			pub, _ := json.Marshal(c.reply)
			doc["model"] = pub
			if nb, merr := json.Marshal(doc); merr == nil {
				body = nb
			}
		}
		if raw, ok := doc["usage"]; ok {
			u, haveUsage = parseUsage(raw)
		}
		outChars = completionChars(doc)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)

	in, out := u.Prompt, u.Completion
	cached, reasoning := u.Cached, u.Reasoning
	estimated := false
	if !haveUsage {
		in = estimateTokens(len(rawBody))
		out = estimateTokens(outChars)
		estimated = true
	}
	p.finish(c, usage.StatusOK, resp.StatusCode, in, out, cached, reasoning, estimated, nil)
}

// completionChars measures the completion text for the fallback estimate.
func completionChars(doc map[string]json.RawMessage) int {
	raw, ok := doc["choices"]
	if !ok {
		return 0
	}
	var choices []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &choices) != nil {
		return 0
	}
	total := 0
	for _, c := range choices {
		total += len(c.Message.Content) + len(c.Message.ReasoningContent)
	}
	return total
}

// pumpStream proxies SSE chunk by chunk, flushed per chunk, never buffered.
// The model field of data frames carrying it is rewritten; usage comes from
// the last usage-bearing frame; ttft is the first frame written.
func (p *Proxy) pumpStream(w http.ResponseWriter, r *http.Request, c *call,
	t *health.Target, resp *http.Response, set *config.Settings, rawBody []byte) {

	start := c.start
	flusher, _ := w.(http.Flusher)

	// One watchdog guards both liveness edges: it ends the stream when the
	// client disconnects and when the gap between two chunks exceeds
	// stream_idle_timeout. Closing the upstream body is what unblocks the
	// blocked read below.
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixMilli())
	var idleFired atomic.Bool
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		idle := set.StreamIdleTimeout
		period := idle / 4
		if period < 50*time.Millisecond {
			period = 50 * time.Millisecond
		}
		if period > time.Second {
			period = time.Second
		}
		tick := time.NewTicker(period)
		defer tick.Stop()
		idleMs := idle.Milliseconds()
		for {
			select {
			case <-watchDone:
				return
			case <-r.Context().Done():
				resp.Body.Close()
				return
			case <-tick.C:
				if time.Now().UnixMilli()-lastActivity.Load() > idleMs {
					idleFired.Store(true)
					resp.Body.Close()
					return
				}
			}
		}
	}()

	headerWritten := false
	writeHeader := func() {
		if headerWritten {
			return
		}
		headerWritten = true
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		if flusher != nil {
			flusher.Flush()
		}
	}

	br := bufio.NewReaderSize(resp.Body, 64<<10)
	var u upstreamUsage
	haveUsage := false
	var ttft *int64
	outChars := 0

	var endStatus string
	failedBeforeBytes := false
	defer func() {
		in, out := u.Prompt, u.Completion
		cached, reasoning := u.Cached, u.Reasoning
		estimated := false
		if !haveUsage {
			in = estimateTokens(len(rawBody))
			out = estimateTokens(outChars)
			estimated = true
		}
		httpStatus := http.StatusOK
		if endStatus == usage.StatusCancelled {
			httpStatus = 499
		}
		if failedBeforeBytes {
			if endStatus == usage.StatusUpstreamTimeout {
				httpStatus = http.StatusGatewayTimeout
			} else {
				httpStatus = http.StatusBadGateway
			}
		}
		p.finish(c, endStatus, httpStatus, in, out, cached, reasoning, estimated, ttft)
	}()

	for {
		if r.Context().Err() != nil {
			endStatus = usage.StatusCancelled
			return
		}
		line, rerr := br.ReadBytes('\n')
		lastActivity.Store(time.Now().UnixMilli())
		if len(line) > 0 {
			// From here, response bytes have reached the client and a
			// failure can only truncate the stream, not change its status.
			failedBeforeBytes = false
			writeHeader()
			if ttft == nil {
				ms := time.Since(start).Milliseconds()
				if ms < 1 {
					// A first frame did arrive; sub-millisecond is not 0.
					ms = 1
				}
				ttft = &ms
			}
			frame := rewriteFrame(line, c.reply, &u, &haveUsage, &outChars)
			if _, werr := w.Write(frame); werr != nil {
				endStatus = usage.StatusCancelled
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			switch {
			case r.Context().Err() != nil:
				endStatus = usage.StatusCancelled
			case idleFired.Load():
				// Idle gap exceeded. Nothing has reached the client yet:
				// it gets a proper 504; otherwise the stream just ends and
				// the client keeps the chunks it already received.
				failedBeforeBytes = !headerWritten
				if failedBeforeBytes {
					WriteError(w, http.StatusGatewayTimeout, "upstream_timeout", "api_error",
						fmt.Sprintf("upstream %s/%s produced no stream data within the idle timeout", t.HostID, t.ServerID), "")
				}
				c.err = fmt.Errorf("no stream data within %s: %w", set.StreamIdleTimeout, rerr)
				endStatus = usage.StatusUpstreamTimeout
			case errors.Is(rerr, io.EOF):
				endStatus = usage.StatusOK
			default:
				failedBeforeBytes = !headerWritten
				if failedBeforeBytes {
					WriteError(w, http.StatusBadGateway, "upstream_error", "api_error",
						fmt.Sprintf("upstream stream failed: %v", rerr), "")
				}
				c.err = rerr
				endStatus = usage.StatusUpstreamError
			}
			return
		}
	}
}

// rewriteFrame passes SSE syntax through untouched (event:, comments,
// blank lines, data: [DONE]) and rewrites the model field of data frames
// carrying it. Usage and completion text are captured on the way through.
func rewriteFrame(line []byte, published string, u *upstreamUsage, haveUsage *bool, outChars *int) []byte {
	trimmed := strings.TrimRight(string(line), "\r\n")
	if !strings.HasPrefix(trimmed, "data:") {
		return line
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" || payload == "[DONE]" {
		return line
	}
	// Parse cost is confined to frames that can carry interesting keys.
	if !strings.Contains(payload, `"usage"`) && !strings.Contains(payload, `"model"`) &&
		!strings.Contains(payload, `"choices"`) {
		return line
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal([]byte(payload), &doc) != nil || doc == nil {
		return line
	}
	changed := false
	if _, ok := doc["model"]; ok {
		pub, _ := json.Marshal(published)
		doc["model"] = pub
		changed = true
	}
	if raw, ok := doc["usage"]; ok {
		if parsed, ok2 := parseUsage(raw); ok2 {
			*u = parsed
			*haveUsage = true
		}
	}
	if raw, ok := doc["choices"]; ok {
		var choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"delta"`
		}
		if json.Unmarshal(raw, &choices) == nil {
			for _, c := range choices {
				*outChars += len(c.Delta.Content) + len(c.Delta.ReasoningContent)
			}
		}
	}
	if !changed {
		return line
	}
	nb, err := json.Marshal(doc)
	if err != nil {
		return line
	}
	return []byte("data: " + string(nb) + "\n")
}
