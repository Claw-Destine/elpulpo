package dashboard

import (
	"encoding/json"
	"net/http"

	"gopkg.in/yaml.v3"

	"elpulpo/internal/catalog"
	"elpulpo/internal/config"
	"elpulpo/internal/health"
	"elpulpo/internal/usage"
)

// Read-only JSON API under /api/*. GET only, no CSRF needed, and never any
// CORS headers — the asymmetry against /v1 is structural (scenario 44).

func (h *Handler) apiHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": "v1"})
}

func (h *Handler) apiConfig(w http.ResponseWriter, r *http.Request) {
	snap := h.d.Store.Current()
	var doc any
	if err := yaml.Unmarshal(config.Canonical(snap.Config), &doc); err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"config": doc, "hash": snap.FileHash})
}

func (h *Handler) apiSettings(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{}
	b, err := json.Marshal(h.d.Settings.Get())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody(err))
		return
	}
	if err := json.Unmarshal(b, &out); err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody(err))
		return
	}
	out["open"] = map[string]any{
		"proxy_open":     h.d.Open.ProxyOpen,
		"proxy_set":      h.d.Open.ProxySet,
		"dashboard_open": h.d.Open.DashOpen,
		"user":           h.d.Open.DashUser,
		"password_set":   h.d.Open.DashPassOK,
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) apiServers(w http.ResponseWriter, r *http.Request) {
	states := h.d.Mgr.States()
	if states == nil {
		states = []health.StateView{}
	}
	writeJSON(w, http.StatusOK, states)
}

func (h *Handler) apiUsageRows(w http.ResponseWriter, r *http.Request) {
	sq := parseStatsQuery(r)
	ctx := r.Context()
	total, err := h.d.Repo.Count(ctx, sq.Filter)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody(err))
		return
	}
	rows, err := h.d.Repo.Rows(ctx, sq.Filter, sq.Sort, sq.Desc, sq.PageSize, (sq.Page-1)*sq.PageSize)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody(err))
		return
	}
	if rows == nil {
		rows = []usage.Row{}
	}
	totals, err := h.d.Repo.Totals(ctx, sq.Filter)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rows": rows, "total": total, "page": sq.Page, "page_size": sq.PageSize, "totals": totals,
	})
}

func (h *Handler) apiUsageSummary(w http.ResponseWriter, r *http.Request) {
	sq := parseStatsQuery(r)
	ix := usage.NewPriceIndex(h.d.Store.Current().Config.Prices)
	sum, err := h.d.Repo.Summary(r.Context(), sq.Filter, sq.GroupBy, ix)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody(err))
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

func (h *Handler) apiCatalogue(w http.ResponseWriter, r *http.Request) {
	c := h.d.Catalogue.File.Catalogue
	rates := c.Rates
	if rates == nil {
		rates = []catalog.Rate{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":  c.Version,
		"as_of":    c.AsOf,
		"currency": c.Currency,
		"stale":    h.d.Catalogue.Stale,
		"rates":    rates,
	})
}
