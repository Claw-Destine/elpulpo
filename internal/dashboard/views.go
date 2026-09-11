package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"elpulpo/internal/catalog"
	"elpulpo/internal/config"
	"elpulpo/internal/health"
	"elpulpo/internal/usage"
)

// pageContent is a deferred template render, used as the children slot of
// Layout. (The type alias lives in Go so templates need no templ import.)
type pageContent = func() templ.Component

// --- page wrappers -----------------------------------------------------------

// page renders a full page (nav, banners, htmx bootstrap) after ensuring the
// CSRF cookie — GETs never mutate anything.
func (h *Handler) page(fn func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := h.ensureCSRF(w, r)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fn(w, r, tok)
	}
}

// part renders an htmx fragment; the cookie is (re-)issued so long-lived
// tabs keep a valid marker.
func (h *Handler) part(fn func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return h.page(fn)
}

func (h *Handler) render(w http.ResponseWriter, r *http.Request, c templ.Component) {
	if err := c.Render(r.Context(), w); err != nil {
		h.d.Log.Error("template render failed", "err", err)
	}
}

// --- servers ------------------------------------------------------------------

type ServersView struct {
	States []health.StateView
	Models []health.ModelView
	Hosts  []FormHost
	Hash   string
}

type FormHost struct {
	ID          string
	Description string
	Addresses   string // one per line, preference order
	Servers     []FormServer
}

type FormServer struct {
	HostID, ID, API, Description, Scheme, AuthToken string
	Port, MaxConcurrency                            int
	Models                                          []string // observed models (probed)
}

func (h *Handler) serversView() ServersView {
	snap := h.d.Store.Current()
	states := h.d.Mgr.States()
	seen := map[string][]string{}
	for _, st := range states {
		seen[st.HostID+"\x00"+st.ServerID] = st.Models
	}
	v := ServersView{States: states, Models: h.d.Mgr.Models(), Hash: snap.FileHash}
	for _, host := range snap.Config.Hosts {
		fh := FormHost{ID: host.ID, Description: host.Description, Addresses: strings.Join(host.HostAddresses, "\n")}
		for _, srv := range host.Servers {
			fh.Servers = append(fh.Servers, FormServer{
				HostID: host.ID, ID: srv.ID, API: srv.API, Description: srv.Description,
				Scheme: srv.SchemeOrDefault(), AuthToken: srv.AuthToken,
				Port: srv.Port, MaxConcurrency: srv.MaxConcurrency,
				Models: seen[host.ID+"\x00"+srv.ID],
			})
		}
		v.Hosts = append(v.Hosts, fh)
	}
	if v.States == nil {
		v.States = []health.StateView{}
	}
	if v.Models == nil {
		v.Models = []health.ModelView{}
	}
	return v
}

func (h *Handler) pageServers(w http.ResponseWriter, r *http.Request, csrf string) {
	h.render(w, r, ServersPage(h.d, csrf, h.serversView()))
}

func (h *Handler) partServers(w http.ResponseWriter, r *http.Request, csrf string) {
	v := h.serversView()
	switch r.URL.Query().Get("scope") {
	case "forms":
		h.render(w, r, ServersFormsFragment(v))
	case "models":
		h.render(w, r, ServersModelsFragment(v.Models))
	default:
		h.render(w, r, ServersStateFragment(v.States))
	}
}

// --- prices --------------------------------------------------------------------

type PricesView struct {
	Hash     string
	Currency string
	Rows     []PriceRow
	NoPrice  []OfferRow
	Catalog  CatalogView
	Err      string
}

type PriceRow struct {
	Model, Aliases    string
	Input, Output     string
	Cached, Reasoning string
}

type OfferRow struct {
	Name  string
	Match *catalog.Rate // exact catalogue match, nil when none exists
}

type CatalogView struct {
	Version, AsOf, Currency string
	Stale                   bool
	Rates                   []catalog.Rate
}

func fmtPrice(f *float64) string {
	if f == nil {
		return ""
	}
	return strconv.FormatFloat(*f, 'f', -1, 64)
}

func (h *Handler) pricesView() PricesView {
	snap := h.d.Store.Current()
	v := PricesView{Hash: snap.FileHash}
	if p := snap.Config.Prices; p != nil {
		v.Currency = p.Currency
		for _, m := range p.Models {
			v.Rows = append(v.Rows, PriceRow{
				Model: m.Model, Aliases: strings.Join(m.Aliases, ", "),
				Input: fmtPrice(m.Input), Output: fmtPrice(m.Output),
				Cached: fmtPrice(m.CachedInput), Reasoning: fmtPrice(m.ReasoningOutput),
			})
		}
	}
	// Base model names seen in usage over all time that match no entry.
	ix := usage.NewPriceIndex(snap.Config.Prices)
	sum, err := h.d.Repo.Summary(context.Background(), usage.Filter{}, "model", ix)
	if err != nil {
		v.Err = err.Error()
	} else if sum != nil {
		for _, name := range sum.NoPriceNames {
			var match *catalog.Rate
			if ms := h.d.Catalogue.Match(name); len(ms) > 0 {
				m := ms[0]
				match = &m
			}
			v.NoPrice = append(v.NoPrice, OfferRow{Name: name, Match: match})
		}
	}
	c := h.d.Catalogue.File.Catalogue
	v.Catalog = CatalogView{Version: c.Version, AsOf: c.AsOf, Currency: c.Currency,
		Stale: h.d.Catalogue.Stale, Rates: c.Rates}
	return v
}

func (h *Handler) pagePrices(w http.ResponseWriter, r *http.Request, csrf string) {
	h.render(w, r, PricesPage(h.d, csrf, h.pricesView()))
}

func (h *Handler) partPrices(w http.ResponseWriter, r *http.Request, csrf string) {
	h.render(w, r, PricesFragment(h.pricesView()))
}

// --- load balancer -------------------------------------------------------------

// LBView is the Load Balancer screen: every configured route with its live
// in-flight load, plus the published ids the member picker offers.
type LBView struct {
	Hash      string
	Routes    []LBRouteView
	Published []health.ModelView
}

type LBRouteView struct {
	Alias       string         `json:"alias"`
	Description string         `json:"description"`
	Serving     bool           `json:"serving"` // at least one member can answer: the alias is in /v1/models
	Members     []LBMemberView `json:"members"`
}

type LBMemberView struct {
	Model     string `json:"model"`     // published id, with its postfix
	Base      string `json:"base"`      // base model name, as the server knows it
	HostID    string `json:"host"`      //
	ServerID  string `json:"server"`    //
	Cost      string `json:"cost"`      //
	InFlight  int64  `json:"in_flight"` //
	Load      string `json:"load"`      // cost × in flight — what the policy equalises
	Available bool   `json:"available"`
	Preferred bool   `json:"preferred"` // the member a request would go to if one arrived now
	Upstream  string `json:"upstream"`  // where a request to this member goes right now
}

func fmtCost(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// rowNumber labels a member row for its own buttons. They address the row by
// position rather than by model id: htmx submits the edited inputs too, so a
// button carrying the id the row used to hold would aim at a model that is no
// longer there, and the click would silently do nothing.
func rowNumber(i int) string { return strconv.Itoa(i) }

func (h *Handler) lbView() LBView {
	snap := h.d.Store.Current()
	v := LBView{Hash: snap.FileHash, Published: h.d.Mgr.Models()}
	// Both slices stay non-nil so /api/loadbalancer answers with [] rather than
	// null when there is nothing to show — including when there is no balancer
	// at all, which is why the guards sit before that early return.
	if v.Published == nil {
		v.Published = []health.ModelView{}
	}
	v.Routes = []LBRouteView{}
	if h.d.Balancer == nil {
		return v
	}
	for _, rt := range h.d.Balancer.View(snap.Config) {
		rv := LBRouteView{Alias: rt.Alias, Description: rt.Description, Serving: rt.Serving()}
		for _, m := range rt.Members {
			mv := LBMemberView{
				Model: m.Model, Cost: fmtCost(m.Cost), InFlight: m.InFlight,
				Load: fmtCost(m.Load), Available: m.Available(), Preferred: m.Preferred,
			}
			if base, seg, hostID, ok := config.SplitPublishedID(m.Model); ok {
				mv.Base, mv.ServerID, mv.HostID = base, seg, hostID
			}
			if m.Target != nil {
				mv.Base, mv.HostID, mv.ServerID = m.Target.Base, m.Target.HostID, m.Target.ServerID
				scheme, port, _, _ := m.Target.State.Routing()
				if addr := m.Target.State.ActiveAddr(); addr != "" {
					mv.Upstream = fmt.Sprintf("%s://%s:%d", scheme, addr, port)
				}
			}
			rv.Members = append(rv.Members, mv)
		}
		v.Routes = append(v.Routes, rv)
	}
	return v
}

func (h *Handler) pageLoadBalancer(w http.ResponseWriter, r *http.Request, csrf string) {
	h.render(w, r, LoadBalancerPage(h.d, csrf, h.lbView()))
}

func (h *Handler) partLoadBalancer(w http.ResponseWriter, r *http.Request, csrf string) {
	v := h.lbView()
	switch r.URL.Query().Get("scope") {
	case "forms":
		h.render(w, r, LoadBalancerFormsFragment(v))
	default:
		h.render(w, r, LoadBalancerLiveFragment(v))
	}
}

// --- stats ---------------------------------------------------------------------

type StatsView struct {
	Q             statsQuery
	Summary       *usage.Summary
	Rows          []usage.Row
	Total         int64
	Pages         int
	Err           string
	RetentionDays int
	Options       StatsOptions
}

type StatsOptions struct {
	Hosts, Servers, Endpoints, Statuses []string
}

func (h *Handler) statsView(r *http.Request) StatsView {
	sq := parseStatsQuery(r)
	v := StatsView{Q: sq, RetentionDays: h.d.Settings.Get().RetentionDays}
	ix := usage.NewPriceIndex(h.d.Store.Current().Config.Prices)
	ctx := r.Context()
	if opts, err := h.d.Repo.Distinct(ctx, "host_id"); err == nil {
		v.Options.Hosts = opts
	}
	if opts, err := h.d.Repo.Distinct(ctx, "server_id"); err == nil {
		v.Options.Servers = opts
	}
	if opts, err := h.d.Repo.Distinct(ctx, "endpoint"); err == nil {
		v.Options.Endpoints = opts
	}
	if opts, err := h.d.Repo.Distinct(ctx, "status"); err == nil {
		v.Options.Statuses = opts
	}
	sum, err := h.d.Repo.Summary(ctx, sq.Filter, sq.GroupBy, ix)
	if err != nil {
		v.Err = err.Error()
		return v
	}
	v.Summary = sum
	total, err := h.d.Repo.Count(ctx, sq.Filter)
	if err != nil {
		v.Err = err.Error()
		return v
	}
	v.Total = total
	v.Pages = int((total + int64(sq.PageSize) - 1) / int64(sq.PageSize))
	rows, err := h.d.Repo.Rows(ctx, sq.Filter, sq.Sort, sq.Desc, sq.PageSize, (sq.Page-1)*sq.PageSize)
	if err != nil {
		v.Err = err.Error()
		return v
	}
	v.Rows = rows
	if v.Rows == nil {
		v.Rows = []usage.Row{}
	}
	return v
}

func (h *Handler) pageStats(w http.ResponseWriter, r *http.Request, csrf string) {
	h.render(w, r, StatsPage(h.d, csrf, h.statsView(r)))
}

func (h *Handler) partStats(w http.ResponseWriter, r *http.Request, csrf string) {
	v := h.statsView(r)
	// htmx takes the address-bar URL from this header — it wins over
	// hx-push-url, whose raw request URL would show the fragment endpoint.
	// The header therefore names the page with the canonical query, and only
	// for user-initiated refreshes: a self-refresh (marked with the poll
	// param) must not add history entries.
	if r.URL.Query().Get(statsPollParam) == "" {
		w.Header().Set("HX-Push-Url", v.Q.pageURL())
	}
	h.render(w, r, StatsFragment(v))
}

// --- settings --------------------------------------------------------------------

type SettingsView struct {
	Fields []SettingField
	Open   OpenInfo
	Hash   string
}

type SettingField struct {
	Key, Label, Value, Hint string
}

func (h *Handler) settingsView() SettingsView {
	s := h.d.Settings.Get()
	v := SettingsView{Open: h.d.Open, Hash: h.d.Store.Current().FileHash}
	v.Fields = []SettingField{
		{"health_interval", "Health interval", fmtDur(s.HealthInterval), "5s – 3600s, e.g. 30s"},
		{"probe_timeout", "Probe timeout", fmtDur(s.ProbeTimeout), "1s – 300s"},
		{"connect_timeout", "Connect timeout", fmtDur(s.ConnectTimeout), "1s – 300s"},
		{"first_byte_timeout", "First byte timeout", fmtDur(s.FirstByteTimeout), "1s – 3600s"},
		{"stream_idle_timeout", "Stream idle timeout", fmtDur(s.StreamIdleTimeout), "1s – 3600s"},
		{"total_timeout", "Total timeout", fmtDur(s.TotalTimeout), "0 = off, or 1s – 86400s"},
		{"max_request_size", "Max request size", fmtSize(s.MaxRequestSize), "1KiB – 1GiB, e.g. 32MiB"},
		{"queue_timeout", "Queue timeout", fmtDur(s.QueueTimeout), "1s – 3600s"},
		{"retention_days", "Retention days", strconv.Itoa(s.RetentionDays), "0 = keep forever"},
	}
	return v
}

func (h *Handler) pageSettings(w http.ResponseWriter, r *http.Request, csrf string) {
	h.render(w, r, SettingsPage(h.d, csrf, h.settingsView()))
}

func (h *Handler) partSettings(w http.ResponseWriter, r *http.Request, csrf string) {
	h.render(w, r, SettingsFragment(h.settingsView()))
}

// --- formatting ------------------------------------------------------------------

func fmtDur(d time.Duration) string {
	if d == 0 {
		return "0"
	}
	return d.String()
}

func fmtSize(b int64) string {
	switch {
	case b >= 1<<30 && b%(1<<30) == 0:
		return strconv.FormatInt(b/(1<<30), 10) + "GiB"
	case b >= 1<<20 && b%(1<<20) == 0:
		return strconv.FormatInt(b/(1<<20), 10) + "MiB"
	case b >= 1<<10 && b%(1<<10) == 0:
		return strconv.FormatInt(b/(1<<10), 10) + "KiB"
	}
	return strconv.FormatInt(b, 10)
}

func fmtClock(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func fmtTs(ms int64) string {
	return time.UnixMilli(ms).In(time.Local).Format("2006-01-02 15:04:05")
}

func fmtI(v int64) string { return strconv.FormatInt(v, 10) }

func fmtAmt(f float64) string { return strconv.FormatFloat(f, 'f', 4, 64) }

func selected(cur []string, v string) bool {
	for _, c := range cur {
		if c == v {
			return true
		}
	}
	return false
}

func contains(vs []string, v string) bool {
	for _, s := range vs {
		if s == v {
			return true
		}
	}
	return false
}
