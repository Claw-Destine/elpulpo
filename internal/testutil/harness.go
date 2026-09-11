package testutil

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"gopkg.in/yaml.v3"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"elpulpo/internal/app"
	"elpulpo/internal/config"
	"elpulpo/internal/usage"
)

// LogSink collects slog output for log-assertion scenarios.
type LogSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *LogSink) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *LogSink) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Harness runs a real El Pulpo app behind an httptest listener with its own
// config file, data dir and captured logs.
type Harness struct {
	T     *testing.T
	App   *app.App
	Srv   *httptest.Server
	Logs  *LogSink
	Dir   string
	Jar   *cookiejar.Jar
	CsrfC string // issued CSRF cookie value
}

// Option mutates the app options before construction.
type Option func(*app.Options)

// Start boots a harness. Health checking is sped up (5 s interval) so
// "within one health interval" waits stay tolerable.
func Start(t *testing.T, opts ...Option) *Harness {
	t.Helper()
	dir := t.TempDir()
	logs := &LogSink{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	o := app.Options{
		ConfigPath: dir + "/elpulpo.yaml",
		DataDir:    dir + "/data",
	}
	for _, fn := range opts {
		fn(&o)
	}
	a, err := app.New(o, logger)
	if err != nil {
		t.Fatalf("app.New: %v\nlog:\n%s", err, logs.String())
	}
	srv := httptest.NewServer(a.Handler())
	h := &Harness{T: t, App: a, Srv: srv, Logs: logs, Dir: dir}
	jar, _ := cookiejar.New(nil)
	h.Jar = jar
	// Fast health for tests (still inside the allowed bounds).
	if _, violations := a.Settings.Update(context.Background(), map[string]string{
		"health_interval": "5s", "probe_timeout": "1s",
	}); len(violations) > 0 {
		t.Fatalf("settings update: %v", violations)
	}
	t.Cleanup(func() {
		srv.Close()
		_ = a.Stop()
	})
	return h
}

// URL builds an absolute URL for a path.
func (h *Harness) URL(path string) string { return h.Srv.URL + path }

// Client returns an http.Client with the harness cookie jar.
func (h *Harness) Client() *http.Client {
	return &http.Client{Jar: h.Jar, Timeout: 60 * time.Second}
}

// Do performs a request and returns status + body string.
func (h *Harness) Do(method, path string, headers map[string]string, body io.Reader) (int, string) {
	h.T.Helper()
	req, err := http.NewRequest(method, h.URL(path), body)
	if err != nil {
		h.T.Fatalf("request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.Client().Do(req)
	if err != nil {
		h.T.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// Get issues a GET.
func (h *Harness) Get(path string) (int, string) {
	h.T.Helper()
	return h.Do(http.MethodGet, path, nil, nil)
}

// GetAuth issues a GET with a bearer proxy token.
func (h *Harness) GetAuth(path, token string) (int, string) {
	h.T.Helper()
	return h.Do(http.MethodGet, path, map[string]string{"Authorization": "Bearer " + token}, nil)
}

// Post issues a POST with an optional token.
func (h *Harness) Post(path string, token string, body io.Reader) (int, string) {
	h.T.Helper()
	var hdr map[string]string
	if token != "" {
		hdr = map[string]string{"Authorization": "Bearer " + token}
	}
	return h.Do(http.MethodPost, path, hdr, body)
}

// PostJSON marshals v and POSTs it (no CSRF).
func (h *Harness) PostJSON(path string, v any) (int, string) {
	h.T.Helper()
	b, _ := json.Marshal(v)
	return h.Do(http.MethodPost, path, map[string]string{"Content-Type": "application/json"}, bytes.NewReader(b))
}

// IssueCSRF GETs a dashboard page so the CSRF cookie is set, and returns
// its value.
func (h *Harness) IssueCSRF() string {
	h.T.Helper()
	req, _ := http.NewRequest(http.MethodGet, h.URL("/dashboard/servers"), nil)
	resp, err := h.Client().Do(req)
	if err != nil {
		h.T.Fatalf("csrf page load: %v", err)
	}
	resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == "elpulpo_csrf" {
			h.CsrfC = c.Value
		}
	}
	if h.CsrfC == "" {
		// The cookie may predate this request (dashboard already visited);
		// the jar holds it.
		if u, err := url.Parse(h.Srv.URL); err == nil {
			for _, c := range h.Jar.Cookies(u) {
				if c.Name == "elpulpo_csrf" {
					h.CsrfC = c.Value
				}
			}
		}
	}
	if h.CsrfC == "" {
		h.T.Fatalf("no elpulpo_csrf cookie issued (log:\n%s)", h.Logs.String())
	}
	return h.CsrfC
}

// CSRFPost is a same-origin-looking mutation: CSRF cookie + header.
func (h *Harness) CSRFPost(path string, v any) (int, string) {
	h.T.Helper()
	tok := h.CsrfC
	if tok == "" {
		tok = h.IssueCSRF()
	}
	b, _ := json.Marshal(v)
	return h.Do(http.MethodPost, path, map[string]string{
		"Content-Type": "application/json",
		"X-Csrf-Token": tok,
	}, bytes.NewReader(b))
}

// CSRFPostForm is the mutation the browser actually makes: an
// x-www-form-urlencoded body (repeated keys for the rows of a table form)
// with the CSRF cookie and header.
func (h *Harness) CSRFPostForm(path string, vals url.Values) (int, string) {
	h.T.Helper()
	tok := h.CsrfC
	if tok == "" {
		tok = h.IssueCSRF()
	}
	return h.Do(http.MethodPost, path, map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"X-Csrf-Token": tok,
	}, strings.NewReader(vals.Encode()))
}

// ApplyConfig writes a configuration through the live store (dashboard save
// path), bypassing HTTP — preconditions in most scenarios use this;
// scenarios that test the dashboard itself call SaveConfigHTTP.
func (h *Harness) ApplyConfig(cfg *config.Config) {
	h.T.Helper()
	_, err := h.App.Store.Save(cfg, h.App.Store.Current().FileHash)
	if err != nil {
		h.T.Fatalf("ApplyConfig: %v", err)
	}
}

// SaveConfigHTTP saves via the dashboard action (the "through the
// dashboard" scenarios).
func (h *Harness) SaveConfigHTTP(cfg *config.Config) (int, string) {
	h.T.Helper()
	return h.SaveConfigHTTPHash(cfg, h.App.Store.Current().FileHash)
}

// SaveConfigHTTPHash saves through the dashboard with an explicit hash.
// The Go structs carry only YAML tags, so the document-shaped body is
// produced by re-decoding the canonical form into generic values.
func (h *Harness) SaveConfigHTTPHash(cfg *config.Config, hash string) (int, string) {
	h.T.Helper()
	doc := map[string]any{"hosts": []any{}, "h": hash}
	var round struct {
		Hosts        []config.Host        `yaml:"hosts"`
		Prices       *config.Prices       `yaml:"prices,omitempty"`
		LoadBalancer *config.LoadBalancer `yaml:"loadbalancer,omitempty"`
	}
	round.Hosts, round.Prices, round.LoadBalancer = cfg.Hosts, cfg.Prices, cfg.LoadBalancer
	raw, err := yaml.Marshal(round)
	if err != nil {
		h.T.Fatalf("yaml: %v", err)
	}
	var shape map[string]any
	if err := yaml.Unmarshal(raw, &shape); err != nil {
		h.T.Fatalf("yaml round-trip: %v", err)
	}
	for _, key := range []string{"hosts", "prices", "loadbalancer"} {
		if v, ok := shape[key]; ok {
			doc[key] = v
		}
	}
	return h.CSRFPost("/dashboard/action/config/save", doc)
}

// UpdateSettings patches global settings directly through the manager.
func (h *Harness) UpdateSettings(kv map[string]string) []config.Violation {
	h.T.Helper()
	_, v := h.App.Settings.Update(context.Background(), kv)
	return v
}

// ConfigYAML exports the canonical configuration through the dashboard.
func (h *Harness) ConfigYAML() string {
	h.T.Helper()
	st, body := h.Get("/dashboard/export/config.yaml")
	if st != 200 {
		h.T.Fatalf("export config: status %d: %s", st, body)
	}
	return body
}

// ModelIDs fetches GET /v1/models and returns the ids.
func (h *Harness) ModelIDs(token ...string) []string {
	h.T.Helper()
	st, body := h.GetAuth("/v1/models", h.token(token...))
	if st != 200 {
		h.T.Fatalf("GET /v1/models: %d %s", st, body)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		h.T.Fatalf("models body %q: %v", body, err)
	}
	var ids []string
	for _, d := range out.Data {
		ids = append(ids, d.ID)
	}
	return ids
}

// Chat posts a chat completion and returns status + raw body.
func (h *Harness) Chat(token string, payload map[string]any) (int, string) {
	h.T.Helper()
	b, _ := json.Marshal(payload)
	return h.Post("/v1/chat/completions", h.token(token), bytes.NewReader(b))
}

// token falls back to the configured proxy token when none is given.
func (h *Harness) token(explicit ...string) string {
	if len(explicit) > 0 && explicit[0] != "" {
		return explicit[0]
	}
	return h.App.Opts.ProxyToken
}

// Eventually polls cond until true or timeout.
func (h *Harness) Eventually(d time.Duration, what string, cond func() bool) {
	h.T.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	h.T.Fatalf("timed out waiting for %s\nlog:\n%s", what, h.Logs.String())
}

// WaitForModel waits until the id appears in GET /v1/models.
func (h *Harness) WaitForModel(id string) {
	h.T.Helper()
	h.Eventually(9*time.Second, "model "+id+" published", func() bool {
		for _, m := range h.ModelIDs() {
			if m == id {
				return true
			}
		}
		return false
	})
}

// WaitForNoModel waits until the id disappears from GET /v1/models.
func (h *Harness) WaitForNoModel(id string) {
	h.T.Helper()
	h.Eventually(9*time.Second, "model "+id+" withdrawn", func() bool {
		for _, m := range h.ModelIDs() {
			if m == id {
				return false
			}
		}
		return true
	})
}

// DrainWriter waits for the usage writer to commit everything it holds.
func (h *Harness) DrainWriter() {
	h.T.Helper()
	for i := 0; i < 60 && h.App.Writer.Pending() > 0; i++ {
		time.Sleep(25 * time.Millisecond)
	}
	time.Sleep(120 * time.Millisecond) // the current batch window
}

// Rows returns all usage rows (after draining).
func (h *Harness) Rows() []usage.Row {
	h.T.Helper()
	h.DrainWriter()
	rows, err := h.App.Repo.Rows(context.Background(), usage.Filter{}, "timestamp", false, 100000, 0)
	if err != nil {
		h.T.Fatalf("rows: %v", err)
	}
	return rows
}

// LogString returns everything logged so far.
func (h *Harness) LogString() string { return h.Logs.String() }

// Host is a shorthand config host.
func Host(id string, addrs []string, servers ...config.Server) config.Host {
	return config.Host{ID: id, HostAddresses: addrs, Servers: servers}
}

// Server is a shorthand config server.
func Server(id string, port int, api string) config.Server {
	return config.Server{ID: id, Port: port, API: api}
}

// ModelNames joins ids for readable failure messages.
func ModelNames(ids []string) string { return "[" + strings.Join(ids, " ") + "]" }

// ChatStream posts a streaming chat request and consumes the SSE response
// line by line. onChunk receives every "data:" payload; returning false
// disconnects mid-stream (scenario 8/31). Returns the status, the data
// payloads seen, and any transport error string.
func (h *Harness) ChatStream(token string, payload map[string]any, onChunk func(string) bool) (int, []string, string) {
	h.T.Helper()
	b, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, h.URL("/v1/chat/completions"), bytes.NewReader(b))
	if err != nil {
		h.T.Fatalf("stream req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := h.token(token); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := h.Client().Do(req)
	if err != nil {
		return 0, nil, err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, nil, string(body)
	}
	var seen []string
	br := newBufReader(resp)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "data:") {
				seen = append(seen, strings.TrimSpace(strings.TrimPrefix(t, "data:")))
				if onChunk != nil && !onChunk(seen[len(seen)-1]) {
					return resp.StatusCode, seen, "disconnected"
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return resp.StatusCode, seen, ""
			}
			return resp.StatusCode, seen, err.Error()
		}
	}
}

func newBufReader(resp *http.Response) *bufio.Reader { return bufio.NewReader(resp.Body) }

// ReservePort binds an ephemeral port on host and hands back the port plus
// a release function; the port is dead (connection refused) until released,
// which models "an address that does not answer".
func ReservePort(t testing.TB, host string) (port int, release func()) {
	t.Helper()
	ln, err := net.Listen("tcp", host+":0")
	if err != nil {
		t.Fatalf("reserve %s: %v", host, err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	return p, func() { ln.Close() }
}
