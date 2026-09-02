package dashboard

import (
	"net/http"
	"strconv"

	"elpulpo/internal/config"
	"elpulpo/internal/usage"
)

// Exports. The CSV writers take the same usage.Filter the stats view uses,
// so an export contains exactly what the current filters select and its sums
// equal the displayed totals (scenario 13).

func (h *Handler) exportConfigYAML(w http.ResponseWriter, r *http.Request) {
	data := config.Canonical(h.d.Store.Current().Config)
	w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=elpulpo.yaml")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

func (h *Handler) exportRowsCSV(w http.ResponseWriter, r *http.Request) {
	sq := parseStatsQuery(r)
	h.startCSV(w, "rows.csv")
	if err := h.d.Repo.WriteRowsCSV(r.Context(), w, sq.Filter); err != nil {
		// Headers are long gone; the truncated stream is the only signal.
		h.d.Log.Error("rows CSV export failed", "err", err)
	}
}

func (h *Handler) exportSummaryCSV(w http.ResponseWriter, r *http.Request) {
	sq := parseStatsQuery(r)
	ix := usage.NewPriceIndex(h.d.Store.Current().Config.Prices)
	h.startCSV(w, "summary.csv")
	if err := h.d.Repo.WriteSummaryCSV(r.Context(), w, sq.Filter, ix); err != nil {
		h.d.Log.Error("summary CSV export failed", "err", err)
	}
}

func (h *Handler) startCSV(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename="+name)
	w.WriteHeader(http.StatusOK)
}
