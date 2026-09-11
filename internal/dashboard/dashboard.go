// Package dashboard renders the El Pulpo web UI: templ pages under
// /dashboard/*, htmx fragments under /dashboard/part/*, mutations as
// POST /dashboard/action/*, JSON reads under /api/* and CSV/config exports
// under /dashboard/export/.
//
// Behaviour is htmx + hx- attributes only: CSP is default-src 'self' and no
// inline JavaScript is emitted anywhere. The dashboard never sets CORS
// headers — that asymmetry is structural (scenario 44) and owned by app.
package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"

	"elpulpo/internal/balancer"
	"elpulpo/internal/catalog"
	"elpulpo/internal/config"
	"elpulpo/internal/health"
	"elpulpo/internal/usage"
)

// OpenInfo reports the env-only credential state for the dashboard banner.
type OpenInfo struct {
	ProxyOpen  bool
	ProxySet   bool
	DashOpen   bool
	DashUser   string
	DashPassOK bool
}

// Deps is everything the dashboard reads and mutates.
type Deps struct {
	Store     *config.Store
	Settings  *config.SettingsManager
	Mgr       *health.Manager
	Repo      *usage.Repo
	Writer    *usage.Writer
	Catalogue *catalog.Catalogue
	// Balancer compiles the configured routes against live health and
	// in-flight state; Inflight is the counter set it reads.
	Balancer *balancer.Selector
	Inflight *balancer.Registry
	Open     OpenInfo
	// Prune runs the retention prune immediately (wired to App.PruneNow).
	Prune func() (int64, error)
	Log   *slog.Logger
}

// Handler serves the dashboard, its fragments, actions, exports and the
// read-only JSON API.
type Handler struct {
	d *Deps

	mu       sync.Mutex
	csrfKeys map[string]bool // issued double-submit tokens (bookkeeping only)
	imports  map[string]*stagedImport
}

type stagedImport struct {
	cfg  *config.Config
	hash string // config hash the preview was computed against
}

// New builds the dashboard handler.
func New(d *Deps) *Handler {
	return &Handler{
		d:        d,
		csrfKeys: map[string]bool{},
		imports:  map[string]*stagedImport{},
	}
}

// Registered returns the mux covering /dashboard/* and /api/*. Mutations
// are CSRF-guarded; pages set the double-submit cookie.
func (h *Handler) Registered() http.Handler {
	mux := http.NewServeMux()

	// Embedded static assets (same-origin only, no CORS ever).
	mux.HandleFunc("GET /dashboard/static/htmx.min.js", h.serveAsset("assets/htmx.min.js"))
	mux.HandleFunc("GET /dashboard/static/app.css", h.serveAsset("assets/app.css"))

	// Pages (GET issues the CSRF cookie when absent).
	mux.HandleFunc("GET /dashboard", h.redirectHome)
	mux.HandleFunc("GET /dashboard/{$}", h.redirectHome)
	mux.HandleFunc("GET /dashboard/servers", h.page(h.pageServers))
	mux.HandleFunc("GET /dashboard/prices", h.page(h.pagePrices))
	mux.HandleFunc("GET /dashboard/loadbalancer", h.page(h.pageLoadBalancer))
	mux.HandleFunc("GET /dashboard/stats", h.page(h.pageStats))
	mux.HandleFunc("GET /dashboard/settings", h.page(h.pageSettings))

	// htmx fragments.
	mux.HandleFunc("GET /dashboard/part/servers", h.part(h.partServers))
	mux.HandleFunc("GET /dashboard/part/prices", h.part(h.partPrices))
	mux.HandleFunc("GET /dashboard/part/loadbalancer", h.part(h.partLoadBalancer))
	mux.HandleFunc("GET /dashboard/part/stats", h.part(h.partStats))
	mux.HandleFunc("GET /dashboard/part/settings", h.part(h.partSettings))

	// Exports.
	mux.HandleFunc("GET /dashboard/export/config.yaml", h.exportConfigYAML)
	mux.HandleFunc("GET /dashboard/export/rows.csv", h.exportRowsCSV)
	mux.HandleFunc("GET /dashboard/export/summary.csv", h.exportSummaryCSV)

	// Mutating actions: CSRF-guarded, JSON responses.
	for _, action := range []string{
		"config/save", "config/import", "config/import/apply",
		"host/save", "host/delete", "server/save", "server/delete",
		"prices/save", "catalog/apply", "route/save", "route/delete",
		"settings/save", "prune/run",
	} {
		mux.HandleFunc("POST /dashboard/action/"+action, h.routeAction)
	}
	// Any other single-segment /dashboard/action/* name 404s as unknown.
	mux.HandleFunc("POST /dashboard/action/{action}", h.routeAction)

	// Read-only JSON API (no CSRF needed; GETs never mutate).
	mux.HandleFunc("GET /api/health", h.apiHealth)
	mux.HandleFunc("GET /api/config", h.apiConfig)
	mux.HandleFunc("GET /api/settings", h.apiSettings)
	mux.HandleFunc("GET /api/servers", h.apiServers)
	mux.HandleFunc("GET /api/loadbalancer", h.apiLoadBalancer)
	mux.HandleFunc("GET /api/usage/rows", h.apiUsageRows)
	mux.HandleFunc("GET /api/usage/summary", h.apiUsageSummary)
	mux.HandleFunc("GET /api/catalogue", h.apiCatalogue)

	mux.HandleFunc("/", h.notFound)
	return mux
}

func (h *Handler) redirectHome(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/dashboard/servers", http.StatusFound)
}

func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(v)
}
