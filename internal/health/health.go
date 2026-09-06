// Package health probes every configured server, tracks per-server state
// (up/down, active address, model list, last error) and maintains the route
// catalogue that /v1 and the dashboard read.
package health

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"elpulpo/internal/config"
)

// ServerState is the mutable, mutex-guarded state of one server, read by
// both the router and the dashboard. The model list, active address and
// up/down flag are the properties that matter to routing.
type ServerState struct {
	HostID, ServerID string // immutable

	inFlight atomic.Int64

	mu         sync.Mutex
	up         bool
	consecFail int
	active     string // last address that answered
	lastErr    string
	lastProbe  time.Time
	models     []string
	hostAddrs  []string
	cfg        config.Server

	sem         *fairSem
	transport   *http.Transport
	transGen    int64
	transScheme string
}

func newServerState(hostID string, addrs []string, srv config.Server) *ServerState {
	return &ServerState{
		HostID: hostID, ServerID: srv.ID,
		hostAddrs: addrs, cfg: srv,
		sem: newFairSem(srv.MaxConcurrency),
	}
}

// InFlight reports concurrent in-flight requests (slot held for the whole
// streamed response).
func (s *ServerState) InFlight() int64 { return s.inFlight.Load() }

// InFlightAdd tracks a request for the dashboard's in-flight column.
func (s *ServerState) InFlightAdd(delta int64) { s.inFlight.Add(delta) }

func (s *ServerState) snapshot() (up bool, consec int, active, lastErr string, lastProbe time.Time, models []string, addrs []string, cfg config.Server) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := append([]string(nil), s.models...)
	a := append([]string(nil), s.hostAddrs...)
	return s.up, s.consecFail, s.active, s.lastErr, s.lastProbe, m, a, s.cfg
}

// Up reports whether the server is currently considered up.
func (s *ServerState) Up() bool { up, _, _, _, _, _, _, _ := s.snapshot(); return up }

// ActiveAddr is the address routed requests go to; "" while un-elected.
func (s *ServerState) ActiveAddr() string { _, _, a, _, _, _, _, _ := s.snapshot(); return a }

func (s *ServerState) setConfig(addrs []string, srv config.Server) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	addrsChanged := strings.Join(s.hostAddrs, ",") != strings.Join(addrs, ",")
	s.hostAddrs = addrs
	if addrsChanged {
		// No fail-back and no stale stickiness: an edited address list
		// drops the election and the next probe walks from position 0.
		s.active = ""
	}
	s.cfg = srv
	s.sem.setCapacity(srv.MaxConcurrency)
	return addrsChanged
}

// Routing returns the dispatch parameters a routed request needs.
func (s *ServerState) Routing() (scheme string, port int, authToken string, maxConcurrency int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.SchemeOrDefault(), s.cfg.Port, s.cfg.AuthToken, s.cfg.MaxConcurrency
}

// AcquireSlot takes a concurrency slot, queueing FIFO up to timeout; ctx
// cancellation (client disconnect) leaves the queue. max_concurrency 0 is
// unlimited and never queues.
func (s *ServerState) AcquireSlot(ctx context.Context, timeout time.Duration) bool {
	w, immediate := s.sem.Acquire()
	if immediate {
		return true
	}
	return w.Wait(ctx, timeout)
}

// ReleaseSlot frees the slot for the whole streamed response's worth of
// holding; a defer spanning the request body is what releases it on client
// disconnects too.
func (s *ServerState) ReleaseSlot() { s.sem.Release() }

// transportFor returns the per-server transport, rebuilt when timing
// settings or the scheme changed. DisableCompression keeps upstream bytes
// exactly as sent.
func (s *ServerState) TransportFor(set *config.Settings, gen int64) *http.Transport {
	s.mu.Lock()
	defer s.mu.Unlock()
	scheme := s.cfg.SchemeOrDefault()
	if s.transport == nil || s.transGen != gen || s.transScheme != scheme {
		t := &http.Transport{
			DisableCompression:    true,
			DialContext:           (&net.Dialer{Timeout: set.ConnectTimeout}).DialContext,
			ResponseHeaderTimeout: set.FirstByteTimeout,
			MaxIdleConnsPerHost:   64,
		}
		if scheme == "https" {
			// v1 trade, stated in the functional spec: homelab inference
			// boxes run self-signed certificates. No verification, no
			// pinning. #nosec G402 -- deliberate, documented.
			t.TLSClientConfig = &tlsConfigInsecure
		}
		s.transport, s.transGen, s.transScheme = t, gen, scheme
	}
	return s.transport
}

// --- probe URLs and parsing -------------------------------------------------

func modelListPath(api string) string {
	if api == "ollama" {
		return "/api/tags"
	}
	return "/v1/models"
}

func parseModels(api string, body []byte) ([]string, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("unparseable probe payload: %w", err)
	}
	key := "data"
	nameField := "id"
	if api == "ollama" {
		key, nameField = "models", "name"
	}
	raw, ok := doc[key]
	if !ok {
		return nil, fmt.Errorf("probe payload missing %q", key)
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("probe payload %q is not a list: %w", key, err)
	}
	var out []string
	for _, e := range entries {
		var v string
		if r, ok := e[nameField]; ok && json.Unmarshal(r, &v) == nil && v != "" {
			out = append(out, v)
			continue
		}
		if r, ok := e["model"]; ok && json.Unmarshal(r, &v) == nil && v != "" {
			out = append(out, v) // ollama sometimes answers with "model"
		}
	}
	return out, nil
}

// --- routes ------------------------------------------------------------------

// Target is one published model id mapped to exactly one server.
type Target struct {
	Published string // what clients address
	Base      string // what the upstream sees
	HostID    string
	ServerID  string
	State     *ServerState
}

type routeTable struct {
	routes map[string]*Target
	ids    []string // published ids from healthy servers, sorted
}

// Manager runs one probe goroutine per server and owns the state structs.
type Manager struct {
	settings *config.SettingsManager
	log      *slog.Logger

	mu      sync.Mutex
	servers map[string]*entry // key: host + "\x00" + server
	table   atomic.Pointer[routeTable]

	reelect   chan struct{} // closed-value ping: health_interval changed
	baseCtx   context.Context
	cancelAll context.CancelFunc
	stopped   bool

	rebuild atomic.Bool // debounce for table rebuilds
}

type entry struct {
	state    *ServerState
	stop     chan struct{}
	cancel   context.CancelFunc
	probeNow chan struct{}
}

func key(host, server string) string { return host + "\x00" + server }

// NewManager wires health to the settings; Start begins probing on Apply.
func NewManager(settings *config.SettingsManager, log *slog.Logger) *Manager {
	m := &Manager{settings: settings, log: log, servers: map[string]*entry{}, reelect: make(chan struct{}, 1)}
	m.table.Store(&routeTable{routes: map[string]*Target{}})
	return m
}

// Start gives the manager its lifetime context.
func (m *Manager) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	m.baseCtx = ctx
	m.cancelAll = cancel
	m.settings.OnChange(func(*config.Settings) {
		select {
		case m.reelect <- struct{}{}:
		default:
		}
	})
}

// Stop ends all probe goroutines.
func (m *Manager) Stop() {
	m.mu.Lock()
	m.stopped = true
	for _, e := range m.servers {
		close(e.stop)
		e.cancel()
	}
	m.mu.Unlock()
	if m.cancelAll != nil {
		m.cancelAll()
	}
}

// Apply reconciles the live configuration: every server gets a probe
// goroutine (created immediately, probed immediately), removed servers stop,
// and host-address edits drop the address election. In-flight requests are
// untouched — they hold the Target they started with.
func (m *Manager) Apply(snap *config.Snapshot) {
	desired := map[string]struct {
		addrs []string
		srv   config.Server
	}{}
	for _, h := range snap.Config.Hosts {
		for _, s := range h.Servers {
			desired[key(h.ID, s.ID)] = struct {
				addrs []string
				srv   config.Server
			}{append([]string(nil), h.HostAddresses...), s}
		}
	}
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	// Stop vanished servers.
	for k, e := range m.servers {
		if _, ok := desired[k]; !ok {
			close(e.stop)
			e.cancel()
			delete(m.servers, k)
		}
	}
	// Update existing, start new.
	for k, d := range desired {
		hostID, serverID, _ := strings.Cut(k, "\x00")
		if e, ok := m.servers[k]; ok {
			if e.state.setConfig(d.addrs, d.srv) {
				m.log.Info("host addresses edited, dropping the address election; next probe walks from position 0",
					"host", hostID, "server", serverID)
				select {
				case e.probeNow <- struct{}{}:
				default:
				}
			}
			continue
		}
		st := newServerState(hostID, d.addrs, d.srv)
		ctx, cancel := context.WithCancel(m.baseCtx)
		e := &entry{state: st, stop: make(chan struct{}), cancel: cancel, probeNow: make(chan struct{}, 1)}
		m.servers[k] = e
		go m.loop(ctx, e)
	}
	m.mu.Unlock()
	m.RebuildTable()
}

// RebuildTable recomputes the published-id catalogue from live server
// state. Healthy servers publish their models; down servers publish none.
func (m *Manager) RebuildTable() {
	t := &routeTable{routes: map[string]*Target{}}
	m.mu.Lock()
	entries := make([]*ServerState, 0, len(m.servers))
	for _, e := range m.servers {
		entries = append(entries, e.state)
	}
	m.mu.Unlock()
	for _, st := range entries {
		up, _, active, _, _, models, _, cfg := st.snapshot()
		// A server without a live address cannot answer requests, so it
		// publishes nothing — models return within the interval the
		// election is restored.
		if !up || active == "" {
			continue
		}
		for _, base := range models {
			pub := config.PublishID(base, cfg.NameSegment(), st.HostID)
			// Published ids are globally unique by validation (host ids
			// unique, segments unique per host); first-writer is enough.
			if _, dup := t.routes[pub]; !dup {
				t.routes[pub] = &Target{Published: pub, Base: base, HostID: st.HostID, ServerID: st.ServerID, State: st}
			}
		}
	}
	t.ids = make([]string, 0, len(t.routes))
	for id := range t.routes {
		t.ids = append(t.ids, id)
	}
	sort.Strings(t.ids)
	m.table.Store(t)
}

// Route looks up a published model id. The caller must still check the
// target's state: a known id behind a down server answers not_available.
func (m *Manager) Route(published string) (*Target, bool) {
	t := m.table.Load()
	tg, ok := t.routes[published]
	return tg, ok
}

// Known reports whether the id is configured at all (for the 404 reason):
// a down server keeps its last model list, so configured-but-unreachable
// ids are distinguished from ids that exist nowhere.
func (m *Manager) Known(published string) bool {
	if _, ok := m.Route(published); ok {
		return true
	}
	_, _, hostID, ok := config.SplitPublishedID(published)
	if !ok {
		return false
	}
	m.mu.Lock()
	entries := make([]*ServerState, 0, len(m.servers))
	for _, e := range m.servers {
		entries = append(entries, e.state)
	}
	m.mu.Unlock()
	for _, st := range entries {
		if st.HostID != hostID {
			continue
		}
		_, _, _, _, _, models, _, cfg := st.snapshot()
		for _, mod := range models {
			if config.PublishID(mod, cfg.NameSegment(), st.HostID) == published {
				return true
			}
		}
	}
	return false
}

// ModelIDs returns the sorted published ids from healthy servers.
func (m *Manager) ModelIDs() []string {
	return m.table.Load().ids
}

// --- probe loop ---------------------------------------------------------------

func (m *Manager) loop(ctx context.Context, e *entry) {
	stop := e.stop
	st := e.state
	m.probeServer(ctx, st) // probe immediately on startup and on add
	for {
		interval := m.settings.Get().HealthInterval
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-stop:
			timer.Stop()
			return
		case <-m.reelect:
			timer.Stop() // interval setting changed; restart the timer
			continue
		case <-e.probeNow:
			timer.Stop()
			m.probeServer(ctx, st)
		case <-timer.C:
			m.probeServer(ctx, st)
		}
	}
}

func (m *Manager) probeServer(ctx context.Context, st *ServerState) {
	set := m.settings.Get()
	up, _, active, _, _, _, addrs, _ := st.snapshot()
	if len(addrs) == 0 {
		return
	}
	// Probe the active address only while it answers — no fail-back: while
	// it answers, the addresses before it are never re-tried. If it fails
	// (or none exists), walk host_addresses from position 0 in this same
	// round: the first address that answers is elected.
	candidates := make([]string, 0, len(addrs)+1)
	if active != "" {
		candidates = append(candidates, active)
	}
	for _, a := range addrs {
		if a != active {
			candidates = append(candidates, a)
		}
	}
	var lastErr string
	for _, addr := range candidates {
		models, err := m.probeAddr(ctx, st, set, addr)
		if err == nil {
			st.mu.Lock()
			prev, prevModels := st.active, st.models
			switch {
			case prev != "" && prev != addr:
				m.log.Info("server switched address", "host", st.HostID, "server", st.ServerID,
					"from", prev, "to", addr, "model_count", len(models), "models", models)
			case prev == "" && up:
				m.log.Info("server re-elected an address", "host", st.HostID, "server", st.ServerID,
					"address", addr, "model_count", len(models), "models", models)
			case !up:
				m.log.Info("server is up", "host", st.HostID, "server", st.ServerID,
					"address", addr, "model_count", len(models), "models", models)
			default:
				// Steady state: the connect line already named the models.
				m.log.Debug("probe ok", "host", st.HostID, "server", st.ServerID,
					"address", addr, "model_count", len(models))
			}
			// A list that moved while the same address kept answering is the
			// interesting event: models loaded, unloaded or pulled.
			if up && prev == addr && !sameModels(prevModels, models) {
				added, removed := diffModels(prevModels, models)
				m.log.Info("server model list changed", "host", st.HostID, "server", st.ServerID,
					"address", addr, "added", added, "removed", removed,
					"model_count", len(models), "models", models)
			}
			st.up, st.consecFail, st.active, st.models = true, 0, addr, models
			st.lastErr, st.lastProbe = "", time.Now()
			st.mu.Unlock()
			m.RebuildTable()
			return
		}
		m.log.Debug("probe failed", "host", st.HostID, "server", st.ServerID, "address", addr, "err", err)
		lastErr = err.Error()
	}
	// The active address (if any) failed: re-elect from position 0 next
	// round. Nothing answered — one consecutive failure for the server.
	st.mu.Lock()
	if active != "" {
		m.log.Error("active address failed, re-electing from the first address",
			"host", st.HostID, "server", st.ServerID, "address", active, "err", lastErr)
		st.active = ""
	}
	st.consecFail++
	st.lastErr, st.lastProbe = lastErr, time.Now()
	wentDown := false
	if st.consecFail >= 3 && st.up {
		st.up = false
		wentDown = true
	}
	st.mu.Unlock()
	if wentDown {
		m.log.Error("server is down after 3 consecutive probe failures",
			"host", st.HostID, "server", st.ServerID, "last_error", lastErr,
			"models_withdrawn", modelsOf(st))
	}
	m.RebuildTable()
}

// modelsOf snapshots the model list a server last published, so the line
// announcing it went dark also says what clients just lost.
func modelsOf(st *ServerState) []string {
	_, _, _, _, _, models, _, _ := st.snapshot()
	return models
}

// sameModels compares two model lists as sets: hosts are free to list their
// models in any order, and a reorder is not a change.
func sameModels(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, m := range a {
		seen[m]++
	}
	for _, m := range b {
		if seen[m]--; seen[m] < 0 {
			return false
		}
	}
	return true
}

// diffModels names what a server stopped and started publishing.
func diffModels(prev, now []string) (added, removed []string) {
	inPrev := make(map[string]bool, len(prev))
	for _, m := range prev {
		inPrev[m] = true
	}
	inNow := make(map[string]bool, len(now))
	for _, m := range now {
		inNow[m] = true
	}
	for _, m := range now {
		if !inPrev[m] {
			added = append(added, m)
		}
	}
	for _, m := range prev {
		if !inNow[m] {
			removed = append(removed, m)
		}
	}
	return added, removed
}

func (m *Manager) probeAddr(ctx context.Context, st *ServerState, set *config.Settings, addr string) ([]string, error) {
	url := fmt.Sprintf("%s://%s:%d%s", st.cfg.SchemeOrDefault(), addr, st.cfg.Port, modelListPath(st.cfg.API))
	ctx, cancel := context.WithTimeout(ctx, set.ProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if st.cfg.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+st.cfg.AuthToken)
	}
	client := &http.Client{Transport: st.TransportFor(set, m.settings.Gen())}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: status %d", url, resp.StatusCode)
	}
	models, err := parseModels(st.cfg.API, body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	return models, nil
}

// --- dashboard view ------------------------------------------------------------

// StateView is one server's dashboard row.
type StateView struct {
	HostID         string    `json:"host"`
	ServerID       string    `json:"server"`
	API            string    `json:"api"`
	Port           int       `json:"port"`
	Scheme         string    `json:"scheme"`
	Up             bool      `json:"up"`
	ActiveAddr     string    `json:"active_address"`
	Fallback       bool      `json:"fallback"` // active != first configured address
	Addresses      []string  `json:"addresses"`
	ConsecFails    int       `json:"consecutive_failures"`
	LastError      string    `json:"last_error"`
	LastProbe      time.Time `json:"last_probe"`
	Models         []string  `json:"models"`
	ModelCount     int       `json:"model_count"`
	InFlight       int64     `json:"in_flight"`
	MaxConcurrency int       `json:"max_concurrency"`
	Published      []string  `json:"published"`
}

// States lists every live server state, ordered by host and server id.
func (m *Manager) States() []StateView {
	m.mu.Lock()
	entries := make([]*ServerState, 0, len(m.servers))
	for _, e := range m.servers {
		entries = append(entries, e.state)
	}
	m.mu.Unlock()
	var out []StateView
	for _, st := range entries {
		up, cf, active, lerr, lp, models, addrs, cfg := st.snapshot()
		v := StateView{
			HostID: st.HostID, ServerID: st.ServerID, API: cfg.API, Port: cfg.Port,
			Scheme: cfg.SchemeOrDefault(), Up: up, ActiveAddr: active,
			Addresses: addrs, ConsecFails: cf, LastError: lerr, LastProbe: lp,
			Models: models, ModelCount: len(models), InFlight: st.InFlight(),
			MaxConcurrency: cfg.MaxConcurrency,
		}
		if len(addrs) > 0 && active != "" && active != addrs[0] {
			v.Fallback = true
		}
		for _, mod := range models {
			v.Published = append(v.Published, config.PublishID(mod, cfg.NameSegment(), st.HostID))
		}
		sort.Strings(v.Published)
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].HostID != out[j].HostID {
			return out[i].HostID < out[j].HostID
		}
		return out[i].ServerID < out[j].ServerID
	})
	return out
}

// ModelView is one published id as the dashboard's model list shows it:
// what clients address, what the upstream sees, and whether the route table
// answers for it right now.
type ModelView struct {
	Published string `json:"published"`
	Base      string `json:"base"`
	HostID    string `json:"host"`
	ServerID  string `json:"server"`
	API       string `json:"api"`
	Upstream  string `json:"upstream"` // scheme://address:port requests go to, "" when withdrawn
	Routable  bool   `json:"routable"` // the id is in the route table: /v1 answers it
}

// Models lists every published id the fleet is known to publish. The route
// table's ids — exactly what GET /v1/models answers — come first; the ids of
// servers that are down or have no live address follow as withdrawn, so the
// dashboard explains a 404 not_available instead of hiding the name. Ordered
// by published id within each group.
func (m *Manager) Models() []ModelView {
	t := m.table.Load()
	out := make([]ModelView, 0, len(t.routes))
	seen := make(map[string]bool, len(t.routes))
	for _, id := range t.ids {
		tg := t.routes[id]
		_, _, active, _, _, _, _, cfg := tg.State.snapshot()
		out = append(out, ModelView{
			Published: id, Base: tg.Base, HostID: tg.HostID, ServerID: tg.ServerID,
			API: cfg.API, Upstream: endpoint(cfg, active), Routable: true,
		})
		seen[id] = true
	}

	m.mu.Lock()
	entries := make([]*ServerState, 0, len(m.servers))
	for _, e := range m.servers {
		entries = append(entries, e.state)
	}
	m.mu.Unlock()
	var withdrawn []ModelView
	for _, st := range entries {
		_, _, _, _, _, models, _, cfg := st.snapshot()
		seg := cfg.NameSegment()
		for _, mod := range models {
			pub := config.PublishID(mod, seg, st.HostID)
			if seen[pub] {
				continue // routable already, or an earlier server published it
			}
			seen[pub] = true
			// No upstream: nothing routes this id while its server is dark.
			withdrawn = append(withdrawn, ModelView{
				Published: pub, Base: mod, HostID: st.HostID, ServerID: st.ServerID, API: cfg.API,
			})
		}
	}
	sort.Slice(withdrawn, func(i, j int) bool { return withdrawn[i].Published < withdrawn[j].Published })
	return append(out, withdrawn...)
}

// endpoint names the upstream address requests for a model go to.
func endpoint(cfg config.Server, addr string) string {
	if addr == "" {
		return ""
	}
	return fmt.Sprintf("%s://%s:%d", cfg.SchemeOrDefault(), addr, cfg.Port)
}
