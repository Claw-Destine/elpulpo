// Package app wires everything: one listener for the proxy and the
// dashboard, the auth/CORS/CSRF middleware, and the start/stop lifecycle.
package app

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"elpulpo/internal/catalog"
	"elpulpo/internal/config"
	"elpulpo/internal/dashboard"
	"elpulpo/internal/health"
	"elpulpo/internal/proxy"
	"elpulpo/internal/usage"
)

// Options come from the environment; credentials are env-only and the
// dashboard cannot change them.
type Options struct {
	Addr              string // default ":8080"
	ConfigPath        string // default "./elpulpo.yaml"
	DataDir           string // default "./data"
	LogLevel          string // default "info"
	ProxyToken        string // empty = open proxy (+ WARN + banner)
	DashboardUser     string // used only when a password is set
	DashboardPassword string // empty = open dashboard (+ WARN + banner)
	CataloguePath     string // empty = embedded catalogue
}

// Defaults applied when Options fields are empty.
func (o *Options) applyDefaults() {
	if o.Addr == "" {
		o.Addr = ":8080"
	}
	if o.ConfigPath == "" {
		o.ConfigPath = "./elpulpo.yaml"
	}
	if o.DataDir == "" {
		o.DataDir = "./data"
	}
	if o.LogLevel == "" {
		o.LogLevel = "info"
	}
	if o.DashboardUser == "" {
		o.DashboardUser = "admin"
	}
}

// App is the assembled application.
type App struct {
	Opts      Options
	Log       *slog.Logger
	Store     *config.Store
	Settings  *config.SettingsManager
	Repo      *usage.Repo
	Writer    *usage.Writer
	Mgr       *health.Manager
	Proxy     *proxy.Proxy
	Catalogue *catalog.Catalogue

	writerCancel context.CancelFunc
	writerDone   chan struct{}
	httpSrv      *http.Server
	shutdownOnce chan struct{}
}

// New builds the app but does not listen. Everything that must exist before
// the dashboard serves — the usage DB, settings, the configuration, the
// catalogue — is loaded here.
func New(opts Options, log *slog.Logger) (*App, error) {
	opts.applyDefaults()
	a := &App{Opts: opts, Log: log, writerDone: make(chan struct{}), shutdownOnce: make(chan struct{})}

	repo, err := usage.Open(filepath.Join(opts.DataDir, "usage.db"))
	if err != nil {
		return nil, fmt.Errorf("open usage store: %w", err)
	}
	a.Repo = repo
	a.Settings = config.NewSettingsManager(repo, log)
	if err := a.Settings.Load(context.Background()); err != nil {
		return nil, fmt.Errorf("load settings: %w", err)
	}

	cat, err := catalog.LoadWithOverride(opts.CataloguePath)
	if err != nil {
		// A broken override keeps the embedded catalogue; say so loudly.
		log.Error("price catalogue: falling back to the embedded catalogue", "err", err)
		a.Catalogue, err = catalog.Embedded()
		if err != nil {
			return nil, err
		}
	} else {
		a.Catalogue = cat
	}

	a.Store = config.NewStore(opts.ConfigPath, log)
	a.Mgr = health.NewManager(a.Settings, log)
	a.Mgr.Start()
	a.Store.SetOnApply(a.Mgr.Apply)
	if err := a.Store.Load(); err != nil {
		return nil, err
	}
	a.Store.Watch(a.ctxDone())

	a.Writer = usage.NewWriter(repo, log)
	ctx, cancel := context.WithCancel(context.Background())
	a.writerCancel = cancel
	go func() { a.Writer.Run(ctx); close(a.writerDone) }()

	a.Proxy = proxy.New(proxy.Deps{Settings: a.Settings, Health: a.Mgr, Writer: a.Writer, Log: log})

	// Retention: pruning runs at startup and once a day when enabled.
	go a.pruneLoop()

	a.openAccessWarn()
	return a, nil
}

func (a *App) ctxDone() chan struct{} { return a.shutdownOnce }

func levelVar(l *slog.LevelVar, name string) {
	switch strings.ToLower(name) {
	case "debug":
		l.Set(slog.LevelDebug)
	case "warn", "warning":
		l.Set(slog.LevelWarn)
	case "error":
		l.Set(slog.LevelError)
	default:
		l.Set(slog.LevelInfo)
	}
}

// LevelVar applies ELPULPO_LOG_LEVEL.
func LevelVar(name string) *slog.LevelVar {
	l := new(slog.LevelVar)
	levelVar(l, name)
	return l
}

// openAccessWarn logs the startup warnings for unauthenticated surfaces.
// Credentials are empty-state facts of every start, not a first-run hint.
func (a *App) openAccessWarn() {
	if a.Opts.ProxyToken == "" {
		a.Log.Warn("ELPULPO_PROXY_TOKEN is empty: the /v1 proxy is open to anyone who can reach it")
	}
	if a.Opts.DashboardPassword == "" {
		a.Log.Warn("ELPULPO_DASHBOARD_PASSWORD is empty: the dashboard (remote control of the whole fleet) is unauthenticated; " +
			"if you enable Basic auth over plain HTTP, put it behind TLS — config editing is remote control")
	}
}

// OpenAccess is the dashboard banner state: present on every start, not
// only the first.
type OpenAccess struct {
	ProxyOpen  bool   `json:"proxy_open"`
	Dashboard  bool   `json:"dashboard_open"`
	User       string `json:"user"`
	PasswordOK bool   `json:"password_set"`
}

// OpenAccess reports the env-only credential state.
func (a *App) OpenAccess() OpenAccess {
	return OpenAccess{
		ProxyOpen:  a.Opts.ProxyToken == "",
		Dashboard:  a.Opts.DashboardPassword == "",
		User:       a.Opts.DashboardUser,
		PasswordOK: a.Opts.DashboardPassword != "",
	}
}

// --- middleware ---------------------------------------------------------------

// cors applies to the /v1 mux only. The dashboard and /api/* never see it,
// so the absence of CORS headers there is structural. /v1 answers preflight
// without an auth check — it is token-only and cookieless, so a wildcard
// origin cannot ride a session that does not exist.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) proxyAuth(next http.Handler) http.Handler {
	if a.Opts.ProxyToken == "" {
		return next
	}
	want := a.Opts.ProxyToken
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		got = strings.TrimSpace(got)
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			proxy.WriteError(w, http.StatusUnauthorized, "unauthorized", "authentication_error",
				"missing or invalid proxy token (Authorization: Bearer <token>)", "")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) basicAuth(next http.Handler) http.Handler {
	if a.Opts.DashboardPassword == "" {
		return next
	}
	user, pass := a.Opts.DashboardUser, a.Opts.DashboardPassword
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, ok := r.BasicAuth()
		uOK := subtle.ConstantTimeCompare([]byte(gotUser), []byte(user)) == 1
		pOK := subtle.ConstantTimeCompare([]byte(gotPass), []byte(pass)) == 1
		if !ok || !(uOK && pOK) {
			w.Header().Set("WWW-Authenticate", `Basic realm="El Pulpo", charset="UTF-8"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- routes ---------------------------------------------------------------

// knownOpenAIPath answers /v1/* paths that exist in the OpenAI API but are
// not proxied in v1 (completions, embeddings, …) with unsupported_endpoint;
// everything else under /v1 is a plain 404.
func v1NotFound(w http.ResponseWriter, r *http.Request) {
	p := strings.Trim(r.URL.Path, "/")
	seg, _, _ := strings.Cut(strings.TrimPrefix(p, "v1/"), "/")
	switch seg {
	case "completions", "embeddings", "responses", "messages", "images", "audio",
		"files", "fine_tuning", "fine-tuning", "moderations", "assistants",
		"threads", "runs", "batches", "uploads", "realtime", "vector_stores",
		"models": // /v1/models/{id} — the bare listing is handled above
		proxy.WriteError(w, http.StatusNotFound, "unsupported_endpoint", "invalid_request_error",
			fmt.Sprintf("endpoint %q is not proxied in v1", r.URL.Path), "")
		return
	}
	http.NotFound(w, r)
}

// Handler builds the single listener's routing: proxy + dashboard + health.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("HEAD /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// The /v1 mux carries CORS and the proxy token check. Nothing else.
	v1 := http.NewServeMux()
	v1.HandleFunc("GET /v1/models", a.Proxy.Models)
	v1.HandleFunc("POST /v1/chat/completions", a.Proxy.Chat)
	v1.HandleFunc("/", v1NotFound)
	mux.Handle("/v1/", a.proxyAuth(cors(v1)))

	// Dashboard + JSON API: same-origin only, Basic auth, CSRF on
	// mutations, CSP on pages.
	d := dashboard.New(&dashboard.Deps{
		Store:     a.Store,
		Settings:  a.Settings,
		Mgr:       a.Mgr,
		Repo:      a.Repo,
		Writer:    a.Writer,
		Catalogue: a.Catalogue,
		Open: dashboard.OpenInfo{
			ProxyOpen:  a.Opts.ProxyToken == "",
			ProxySet:   a.Opts.ProxyToken != "",
			DashOpen:   a.Opts.DashboardPassword == "",
			DashUser:   a.Opts.DashboardUser,
			DashPassOK: a.Opts.DashboardPassword != "",
		},
		Prune: a.PruneNow,
		Log:   a.Log,
	})
	dh := a.basicAuth(csp(d.Registered()))
	mux.Handle("/dashboard/", dh)
	mux.Handle("/dashboard", dh)
	mux.Handle("/api/", a.basicAuth(d.Registered()))

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard/", http.StatusFound)
	})

	// Anything else — including anything under /servers/ — is a plain 404.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	return mux
}

// csp adds the dashboard Content-Security-Policy; htmx drives behaviour
// through hx- attributes, so unsafe-inline appears nowhere.
func csp(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// --- lifecycle ---------------------------------------------------------------

func (a *App) pruneLoop() {
	run := func(why string) {
		days := a.Settings.Get().RetentionDays
		if days <= 0 {
			return
		}
		n, err := a.Repo.PruneOlderThan(context.Background(), days)
		if err != nil {
			a.Log.Error("usage prune failed", "err", err)
			return
		}
		a.Log.Info("usage prune completed", "removed", n, "retention_days", days, "trigger", why)
	}
	run("startup")
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-a.shutdownOnce:
			return
		case <-t.C:
			run("daily")
		}
	}
}

// PruneNow runs the retention prune immediately (dashboard action).
func (a *App) PruneNow() (int64, error) {
	days := a.Settings.Get().RetentionDays
	if days <= 0 {
		return 0, errors.New("retention_days is off; nothing to prune")
	}
	n, err := a.Repo.PruneOlderThan(context.Background(), days)
	a.Log.Info("usage prune completed", "removed", n, "retention_days", days, "trigger", "dashboard")
	return n, err
}

// Start serves until ctx is cancelled or Stop is called.
func (a *App) Start(ctx context.Context) error {
	srv := &http.Server{
		Addr:              a.Opts.Addr,
		Handler:           a.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	a.httpSrv = srv
	errs := make(chan error, 1)
	go func() {
		a.Log.Info("listening", "addr", a.Opts.Addr, "config", a.Opts.ConfigPath, "data", a.Opts.DataDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		return a.Stop()
	}
}

// Stop stops listening, finishes in-flight requests within a 30 s grace,
// cancels streams, drains the usage channel, checkpoints and closes.
func (a *App) Stop() error {
	select {
	case <-a.shutdownOnce:
		return nil // already stopping
	default:
		close(a.shutdownOnce)
	}
	var firstErr error
	if a.httpSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := a.httpSrv.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	a.Mgr.Stop()
	a.writerCancel()
	select {
	case <-a.writerDone:
	case <-time.After(30 * time.Second):
		a.Log.Error("usage writer did not drain within the shutdown grace; rows were dropped")
	}
	if err := a.Repo.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// CheckConfig validates the configuration file and exits via return code.
func CheckConfig(path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "%s: file does not exist (that is a valid empty configuration)\n", path)
		return nil
	}
	if err != nil {
		return err
	}
	if _, violations := config.ParseAndValidate(data); len(violations) > 0 {
		for _, v := range violations {
			fmt.Fprintln(os.Stderr, v.Error())
		}
		return errors.New("configuration is invalid")
	}
	fmt.Println("configuration is valid")
	return nil
}
