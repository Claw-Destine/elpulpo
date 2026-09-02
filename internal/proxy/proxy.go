// Package proxy is the client-facing OpenAI-compatible proxy: exact-id
// routing, model rewrite in both directions, unbuffered SSE pass-through,
// per-server concurrency slots and usage capture.
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
	"strings"
	"sync/atomic"
	"time"

	"elpulpo/internal/config"
	"elpulpo/internal/health"
	"elpulpo/internal/usage"
)

// Deps wires the proxy to the live state.
type Deps struct {
	Settings *config.SettingsManager
	Health   *health.Manager
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

// Models serves GET /v1/models: published ids of healthy servers only,
// sorted, in OpenAI shape.
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
	for _, id := range p.deps.Health.ModelIDs() {
		out.Data = append(out.Data, model{ID: id, Object: "model", OwnedBy: "elpulpo"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
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

	// 1. Size gate before any parsing or routing: no usage row and no
	// concurrency slot is taken.
	limited := http.MaxBytesReader(w, r.Body, set.MaxRequestSize)
	body, err := io.ReadAll(limited)
	var mbErr *http.MaxBytesError
	if errors.As(err, &mbErr) || (err != nil && !errors.Is(err, context.Canceled)) {
		p.RejTooLarge.Add(1)
		p.deps.Log.Info("rejected oversized request", "limit", set.MaxRequestSize, "remote", r.RemoteAddr, "err", err)
		WriteError(w, http.StatusRequestEntityTooLarge, "request_too_large", "invalid_request_error",
			fmt.Sprintf("request body exceeds the %d byte limit", set.MaxRequestSize), "")
		return
	}
	if err == nil {
		p.route(w, r, start, set, body)
		return
	}
	// context canceled while reading the body: client is gone, nothing was
	// routed, so there is no row.
	_ = err
}

func (p *Proxy) route(w http.ResponseWriter, r *http.Request, start time.Time, set *config.Settings, body []byte) {
	// 2. The body must parse as a JSON object. Managed fields are lifted
	// into a struct and the remainder is kept verbatim, so tool schemas,
	// multimodal payloads and future OpenAI fields survive untouched.
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
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
			WriteError(w, http.StatusBadRequest, "invalid_request_error", "invalid_request_error",
				`"stream" must be a boolean`, "")
			return
		}
	}
	if model == "" {
		WriteError(w, http.StatusBadRequest, "invalid_request_error", "invalid_request_error",
			`"model" is required`, "")
		return
	}

	// 3. Route on exact published id. Routing is exact and single-target:
	// the request vocabulary equals GET /v1/models — bare base names and
	// aliases are unknown ids.
	target, ok := p.deps.Health.Route(model)
	if !ok {
		reason := "not_configured"
		if p.deps.Health.Known(model) {
			reason = "not_available" // known id, server currently down
		} else {
			p.RejUnknown.Add(1)
		}
		WriteError(w, http.StatusNotFound, "model_not_found", "invalid_request_error",
			fmt.Sprintf("model %q is not a published model id (reason: %s)", model, reason), reason)
		return
	}
	st := target.State
	if !st.Up() || st.ActiveAddr() == "" {
		WriteError(w, http.StatusNotFound, "model_not_found", "invalid_request_error",
			fmt.Sprintf("model %q is currently not available (reason: not_available), retry after the next health probe", model),
			"not_available")
		return
	}

	// 4. Concurrency slot: select over the per-server FIFO semaphore, the
	// queue timeout and the client context.
	if !st.AcquireSlot(r.Context(), set.QueueTimeout) {
		switch {
		case r.Context().Err() != nil:
			// Client left while queued; the request was aimed at a server,
			// so it terminates as a cancelled row.
			p.row(start, target, usage.StatusCancelled, 499, 0, 0, 0, 0, false, nil)
		case !st.Up():
			WriteError(w, http.StatusServiceUnavailable, "server_unavailable", "api_error",
				fmt.Sprintf("server %s/%s went down while the request was queued", target.HostID, target.ServerID), "")
		default:
			p.RejBusy.Add(1)
			WriteError(w, http.StatusTooManyRequests, "server_busy", "rate_limit_error",
				fmt.Sprintf("no free slot on %s/%s within the queue timeout", target.HostID, target.ServerID), "")
		}
		return
	}
	defer st.ReleaseSlot() // the slot is held for the whole streamed response
	if !st.Up() {
		WriteError(w, http.StatusServiceUnavailable, "server_unavailable", "api_error",
			fmt.Sprintf("server %s/%s went down while the request was queued", target.HostID, target.ServerID), "")
		return
	}
	st.InFlightAdd(1)
	defer st.InFlightAdd(-1)

	p.dispatch(w, r, start, target, fields, stream, body, set)
}

func (p *Proxy) row(start time.Time, t *health.Target, status string, httpStatus int,
	in, out, cached, reasoning int64, estimated bool, ttft *int64) {
	p.deps.Writer.Submit(usage.Row{
		TsMs: start.UnixMilli(), HostID: t.HostID, ServerID: t.ServerID, Model: t.Published,
		Endpoint: "chat", Status: status, HTTPStatus: httpStatus,
		TokensIn: in, TokensOut: out, TokensCached: cached, TokensReasoning: reasoning,
		Estimated: estimated, LatencyMs: time.Since(start).Milliseconds(), TTFTMs: ttft,
	})
}

func (p *Proxy) dispatch(w http.ResponseWriter, r *http.Request, start time.Time,
	t *health.Target, fields map[string]json.RawMessage, stream bool, rawBody []byte, set *config.Settings) {

	scheme, port, authToken, _ := t.State.Routing()
	addr := t.State.ActiveAddr()
	if addr == "" {
		// Lost the address election after the up-check; failure rules apply.
		WriteError(w, http.StatusServiceUnavailable, "server_unavailable", "api_error",
			fmt.Sprintf("server %s/%s has no live address right now", t.HostID, t.ServerID), "")
		p.row(start, t, usage.StatusUpstreamError, http.StatusServiceUnavailable, 0, 0, 0, 0, false, nil)
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
		p.failUpstream(w, start, t, err, r.Context(), upCtx)
		return
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 500:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		WriteError(w, http.StatusBadGateway, "upstream_error", "api_error",
			fmt.Sprintf("upstream %s/%s returned %d", t.HostID, t.ServerID, resp.StatusCode), "")
		p.row(start, t, usage.StatusUpstreamError, http.StatusBadGateway, 0, 0, 0, 0, false, nil)
		return
	case resp.StatusCode >= 400:
		// Upstream 4xx: status and body passed through unchanged.
		passthrough(w, resp)
		p.row(start, t, usage.StatusOK, resp.StatusCode, 0, 0, 0, 0, false, nil)
		return
	}

	if stream {
		p.pumpStream(w, r, start, t, resp, set, rawBody)
		return
	}
	p.respondBuffered(w, r, start, t, resp, rawBody)
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
func (p *Proxy) failUpstream(w http.ResponseWriter, start time.Time, t *health.Target, err error, reqCtx, upCtx context.Context) {
	switch {
	case reqCtx.Err() != nil && !errors.Is(upCtx.Err(), context.DeadlineExceeded):
		p.row(start, t, usage.StatusCancelled, 499, 0, 0, 0, 0, false, nil)
	case isTimeoutErr(err) || errors.Is(upCtx.Err(), context.DeadlineExceeded):
		WriteError(w, http.StatusGatewayTimeout, "upstream_timeout", "api_error",
			fmt.Sprintf("upstream %s/%s timed out: %v", t.HostID, t.ServerID, err), "")
		p.row(start, t, usage.StatusUpstreamTimeout, http.StatusGatewayTimeout, 0, 0, 0, 0, false, nil)
	default:
		p.deps.Log.Debug("upstream request failed", "host", t.HostID, "server", t.ServerID, "err", err)
		WriteError(w, http.StatusBadGateway, "upstream_error", "api_error",
			fmt.Sprintf("upstream %s/%s failed: %v", t.HostID, t.ServerID, err), "")
		p.row(start, t, usage.StatusUpstreamError, http.StatusBadGateway, 0, 0, 0, 0, false, nil)
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
func (p *Proxy) respondBuffered(w http.ResponseWriter, r *http.Request, start time.Time,
	t *health.Target, resp *http.Response, rawBody []byte) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		switch {
		case r.Context().Err() != nil:
			p.row(start, t, usage.StatusCancelled, 499, 0, 0, 0, 0, false, nil)
		case isTimeoutErr(err):
			WriteError(w, http.StatusGatewayTimeout, "upstream_timeout", "api_error", "upstream body read timed out", "")
			p.row(start, t, usage.StatusUpstreamTimeout, http.StatusGatewayTimeout, 0, 0, 0, 0, false, nil)
		default:
			WriteError(w, http.StatusBadGateway, "upstream_error", "api_error", "upstream body read failed", "")
			p.row(start, t, usage.StatusUpstreamError, http.StatusBadGateway, 0, 0, 0, 0, false, nil)
		}
		return
	}
	var u upstreamUsage
	haveUsage := false
	outChars := 0
	// Response identity: the model field is rewritten back to the
	// published id the client asked for.
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) == nil && doc != nil {
		if _, ok := doc["model"]; ok {
			pub, _ := json.Marshal(t.Published)
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
	p.row(start, t, usage.StatusOK, resp.StatusCode, in, out, cached, reasoning, estimated, nil)
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
func (p *Proxy) pumpStream(w http.ResponseWriter, r *http.Request, start time.Time,
	t *health.Target, resp *http.Response, set *config.Settings, rawBody []byte) {

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
		p.row(start, t, endStatus, httpStatus, in, out, cached, reasoning, estimated, ttft)
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
			frame := rewriteFrame(line, t.Published, &u, &haveUsage, &outChars)
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
				endStatus = usage.StatusUpstreamTimeout
			case errors.Is(rerr, io.EOF):
				endStatus = usage.StatusOK
			default:
				failedBeforeBytes = !headerWritten
				if failedBeforeBytes {
					WriteError(w, http.StatusBadGateway, "upstream_error", "api_error",
						fmt.Sprintf("upstream stream failed: %v", rerr), "")
				}
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
