package usage

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"elpulpo/internal/config"
)

// Filter selects the rows a view (table, summary, CSV export) shows. All
// criteria combine with AND; list filters are exact match, ModelContains is
// a substring match.
type Filter struct {
	FromMs        int64 // inclusive, 0 = unbounded
	ToMs          int64 // exclusive, 0 = unbounded
	Hosts         []string
	Servers       []string
	Endpoints     []string
	Statuses      []string
	ModelContains string
}

func (f Filter) where() (string, []any) {
	var parts []string
	var args []any
	if f.FromMs > 0 {
		parts = append(parts, "ts_ms >= ?")
		args = append(args, f.FromMs)
	}
	if f.ToMs > 0 {
		parts = append(parts, "ts_ms < ?")
		args = append(args, f.ToMs)
	}
	in := func(col string, vals []string) {
		if len(vals) == 0 {
			return
		}
		parts = append(parts, col+" IN ("+strings.TrimSuffix(strings.Repeat("?,", len(vals)), ",")+")")
		for _, v := range vals {
			args = append(args, v)
		}
	}
	in("host_id", f.Hosts)
	in("server_id", f.Servers)
	in("endpoint", f.Endpoints)
	in("status", f.Statuses)
	if f.ModelContains != "" {
		esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(f.ModelContains)
		parts = append(parts, `model LIKE ? ESCAPE '\'`)
		args = append(args, "%"+esc+"%")
	}
	if len(parts) == 0 {
		return "1=1", args
	}
	return strings.Join(parts, " AND "), args
}

var sortColumns = map[string]string{
	"timestamp": "ts_ms", "id": "id", "host": "host_id", "server": "server_id",
	"model": "model", "endpoint": "endpoint", "status": "status",
	"http_status": "http_status", "tokens_in": "tokens_in", "tokens_out": "tokens_out",
	"tokens_cached": "tokens_cached", "tokens_reasoning": "tokens_reasoning",
	"estimated": "estimated", "latency_ms": "latency_ms", "ttft_ms": "ttft_ms",
}

const rowColumns = `id, ts_ms, host_id, server_id, model, endpoint, status, http_status,
	tokens_in, tokens_out, tokens_cached, tokens_reasoning, estimated, latency_ms, ttft_ms`

func scanRow(scan func(dest ...any) error) (Row, error) {
	var r Row
	var est int
	var ttft sql.NullInt64
	err := scan(&r.ID, &r.TsMs, &r.HostID, &r.ServerID, &r.Model, &r.Endpoint, &r.Status,
		&r.HTTPStatus, &r.TokensIn, &r.TokensOut, &r.TokensCached, &r.TokensReasoning,
		&est, &r.LatencyMs, &ttft)
	r.Estimated = est != 0
	if ttft.Valid {
		v := ttft.Int64
		r.TTFTMs = &v
	}
	return r, err
}

// Count returns how many rows the filter selects.
func (r *Repo) Count(ctx context.Context, f Filter) (int64, error) {
	where, args := f.where()
	var n int64
	err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM requests WHERE "+where, args...).Scan(&n)
	return n, err
}

// Rows returns one page of filtered rows. sortKey is one of the table
// columns (default ts_ms desc); pagination is limit/offset.
func (r *Repo) Rows(ctx context.Context, f Filter, sortKey string, desc bool, limit, offset int) ([]Row, error) {
	col, ok := sortColumns[sortKey]
	if !ok {
		col = "ts_ms"
	}
	order := "ASC"
	if desc {
		order = "DESC"
	}
	where, args := f.where()
	q := fmt.Sprintf("SELECT %s FROM requests WHERE %s ORDER BY %s %s, id %s LIMIT ? OFFSET ?",
		rowColumns, where, col, order, order)
	args = append(args, limit, offset)
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		row, err := scanRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// Totals is the table's totals row plus percentile helpers.
type Totals struct {
	Requests        int64 `json:"requests"`
	TokensIn        int64 `json:"tokens_in"`
	TokensOut       int64 `json:"tokens_out"`
	TokensCached    int64 `json:"tokens_cached"`
	TokensReasoning int64 `json:"tokens_reasoning"`
	EstimatedRows   int64 `json:"estimated_rows"`
	LatencyP50Ms    int64 `json:"latency_p50_ms"`
	LatencyP95Ms    int64 `json:"latency_p95_ms"`
}

// Totals computes sums and exact percentiles over the filtered set.
// Percentiles are exact: ORDER BY latency_ms LIMIT 1 OFFSET n/2 (p50) and
// OFFSET 19n/20 (p95), as the implementation spec pins them.
func (r *Repo) Totals(ctx context.Context, f Filter) (Totals, error) {
	var t Totals
	where, args := f.where()
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0),
		COALESCE(SUM(tokens_cached),0), COALESCE(SUM(tokens_reasoning),0), COALESCE(SUM(estimated),0)
		FROM requests WHERE `+where, args...).
		Scan(&t.Requests, &t.TokensIn, &t.TokensOut, &t.TokensCached, &t.TokensReasoning, &t.EstimatedRows)
	if err != nil {
		return t, err
	}
	if t.Requests > 0 {
		if t.LatencyP50Ms, err = r.percentile(ctx, where, args, t.Requests/2); err != nil {
			return t, err
		}
		if t.LatencyP95Ms, err = r.percentile(ctx, where, args, t.Requests*19/20); err != nil {
			return t, err
		}
	}
	return t, nil
}

func (r *Repo) percentile(ctx context.Context, where string, args []any, offset int64) (int64, error) {
	q := "SELECT latency_ms FROM requests WHERE " + where + " ORDER BY latency_ms LIMIT 1 OFFSET ?"
	qargs := append(append([]any{}, args...), offset)
	var v int64
	err := r.db.QueryRowContext(ctx, q, qargs...).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return v, err
}

// Percentiles computes p50/p95 for an extra predicate over the filter.
func (r *Repo) Percentiles(ctx context.Context, f Filter, extraWhere string, extraArgs []any) (int64, int64, error) {
	where, args := f.where()
	if extraWhere != "" {
		where = "(" + where + ") AND (" + extraWhere + ")"
		args = append(args, extraArgs...)
	}
	var n int64
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM requests WHERE "+where, args...).Scan(&n); err != nil {
		return 0, 0, err
	}
	if n == 0 {
		return 0, 0, nil
	}
	p50, err := r.percentile(ctx, where, args, n/2)
	if err != nil {
		return 0, 0, err
	}
	p95, err := r.percentile(ctx, where, args, n*19/20)
	if err != nil {
		return 0, 0, err
	}
	return p50, p95, nil
}

// Distinct lists filter options (host_id, server_id, model, endpoint,
// status) over the whole table.
func (r *Repo) Distinct(ctx context.Context, column string) ([]string, error) {
	allowed := map[string]bool{"host_id": true, "server_id": true, "model": true, "endpoint": true, "status": true}
	if !allowed[column] {
		return nil, fmt.Errorf("distinct not allowed on %q", column)
	}
	rows, err := r.db.QueryContext(ctx, "SELECT DISTINCT "+column+" FROM requests ORDER BY 1")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// --- Savings -------------------------------------------------------------

// PriceIndex resolves base model names (and aliases) to price entries.
// Matching is exact and case-sensitive: an alias exists to cover the second
// spelling, not the second language.
type PriceIndex struct {
	currency string
	byName   map[string]*config.ModelPrice
}

// NewPriceIndex indexes a Prices section; a nil section yields an index with
// no prices (savings off).
func NewPriceIndex(p *config.Prices) *PriceIndex {
	ix := &PriceIndex{byName: map[string]*config.ModelPrice{}}
	if p == nil {
		return ix
	}
	ix.currency = p.Currency
	for i := range p.Models {
		e := &p.Models[i]
		ix.byName[e.Model] = e
		for _, a := range e.Aliases {
			ix.byName[a] = e
		}
	}
	return ix
}

// Currency is the document currency ("" when prices are absent).
func (p *PriceIndex) Currency() string { return p.currency }

// Enabled reports whether any price entry exists.
func (p *PriceIndex) Enabled() bool { return len(p.byName) > 0 }

// Lookup finds the entry whose model equals name or whose aliases contain
// it. Returns nil when no price is set.
func (p *PriceIndex) Lookup(name string) *config.ModelPrice { return p.byName[name] }

// AmountTokens computes the reference amount for token counts at entry e,
// per 1M tokens. cached is a subset of in, reasoning a subset of out.
func AmountTokens(in, out, cached, reasoning int64, e *config.ModelPrice) float64 {
	if e == nil || e.Input == nil || e.Output == nil {
		return 0
	}
	pIn, pOut := *e.Input, *e.Output
	pCached, pReasoning := pIn, pOut
	if e.CachedInput != nil {
		pCached = *e.CachedInput
	}
	if e.ReasoningOutput != nil {
		pReasoning = *e.ReasoningOutput
	}
	if cached > in {
		cached = in
	}
	if reasoning > out {
		reasoning = out
	}
	inEff, outEff := in-cached, out-reasoning
	return (float64(inEff)*pIn + float64(cached)*pCached + float64(outEff)*pOut + float64(reasoning)*pReasoning) / 1e6
}

// AmountForRow prices one usage row, or false when no entry matches.
func AmountForRow(r Row, ix *PriceIndex) (float64, bool) {
	e := ix.Lookup(config.BaseModelOf(r.Model))
	if e == nil {
		return 0, false
	}
	return AmountTokens(r.TokensIn, r.TokensOut, r.TokensCached, r.TokensReasoning, e), true
}

// --- Summary -------------------------------------------------------------

// SummaryGroup is one aggregated group (by model, host, server or day).
type SummaryGroup struct {
	Key             string   `json:"key"`
	Members         []string `json:"members,omitempty"` // published ids behind the group
	Requests        int64    `json:"requests"`
	TokensIn        int64    `json:"tokens_in"`
	TokensOut       int64    `json:"tokens_out"`
	TokensCached    int64    `json:"tokens_cached"`
	TokensReasoning int64    `json:"tokens_reasoning"`
	EstimatedRows   int64    `json:"estimated_rows"`
	P50Ms           int64    `json:"p50_ms"`
	P95Ms           int64    `json:"p95_ms"`
	Amount          float64  `json:"amount"`
	Priced          bool     `json:"priced"`
	NoPrice         bool     `json:"no_price"`
}

// Summary is a grouped aggregation with grand totals and the names that
// carry no price.
type Summary struct {
	GroupBy      string         `json:"group_by"`
	Currency     string         `json:"currency"`
	SavingsOn    bool           `json:"savings_on"`
	Groups       []SummaryGroup `json:"groups"`
	GrandTotal   SummaryGroup   `json:"grand_total"`
	NoPriceNames []string       `json:"no_price_models"`
}

func groupExpr(groupBy string) (string, error) {
	switch groupBy {
	case "model":
		return "model", nil
	case "host":
		return "host_id", nil
	case "server":
		return "host_id || '/' || server_id", nil
	case "day":
		return "strftime('%Y-%m-%d', ts_ms/1000, 'unixepoch', 'localtime')", nil
	}
	return "", fmt.Errorf("unknown group_by %q (model, host, server, day)", groupBy)
}

// dayBounds converts a local calendar day key to a [start,end) ms window.
func dayBounds(key string) (int64, int64, error) {
	d, err := time.ParseInLocation("2006-01-02", key, time.Local)
	if err != nil {
		return 0, 0, err
	}
	start := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.Local)
	end := start.AddDate(0, 0, 1)
	return start.UnixMilli(), end.UnixMilli(), nil
}

// groupPredicate restricts a percentile query to one group's rows.
func groupPredicate(groupBy, key string, members []string) (string, []any) {
	switch groupBy {
	case "model":
		ph := strings.TrimSuffix(strings.Repeat("?,", len(members)), ",")
		args := make([]any, len(members))
		for i, m := range members {
			args[i] = m
		}
		return "model IN (" + ph + ")", args
	case "host":
		return "host_id = ?", []any{key}
	case "server":
		host, srv, _ := strings.Cut(key, "/")
		return "host_id = ? AND server_id = ?", []any{host, srv}
	case "day":
		s, e, err := dayBounds(key)
		if err != nil {
			return "0", nil
		}
		return "ts_ms >= ? AND ts_ms < ?", []any{s, e}
	}
	return "0", nil
}

// Summary aggregates over the filtered period. Model grouping keys on the
// *price* model name, so usage reported under an alias is grouped with the
// entry's model name and priced by it.
func (r *Repo) Summary(ctx context.Context, f Filter, groupBy string, ix *PriceIndex) (*Summary, error) {
	expr, err := groupExpr(groupBy)
	if err != nil {
		return nil, err
	}
	where, args := f.where()
	q := fmt.Sprintf(`SELECT %s, model, COUNT(*), SUM(tokens_in), SUM(tokens_out), SUM(tokens_cached),
		SUM(tokens_reasoning), SUM(estimated) FROM requests WHERE %s GROUP BY 1, 2`, expr, where)
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type agg struct {
		req, in, out, cached, reasoning, est int64
	}
	byGroup := map[string]*SummaryGroup{}
	groupOrder := []string{}
	members := map[string]map[string]bool{}
	noPriceSet := map[string]bool{}
	grandAmount := 0.0
	grandPriced, grandNoPrice := false, false

	for rows.Next() {
		var key, model string
		var a agg
		if err := rows.Scan(&key, &model, &a.req, &a.in, &a.out, &a.cached, &a.reasoning, &a.est); err != nil {
			return nil, err
		}
		base := config.BaseModelOf(model)
		entry := ix.Lookup(base)
		amount := 0.0
		if entry != nil {
			amount = AmountTokens(a.in, a.out, a.cached, a.reasoning, entry)
			grandAmount += amount
			grandPriced = true
		} else {
			noPriceSet[base] = true
			grandNoPrice = true
		}
		// Model grouping rolls up under the price entry's name, so aliases
		// and originals share one group and one amount.
		finalKey := key
		if groupBy == "model" {
			if entry != nil {
				finalKey = entry.Model
			} else {
				finalKey = base
			}
		}
		g, ok := byGroup[finalKey]
		if !ok {
			g = &SummaryGroup{Key: finalKey, Amount: 0}
			byGroup[finalKey] = g
			groupOrder = append(groupOrder, finalKey)
			members[finalKey] = map[string]bool{}
		}
		g.Requests += a.req
		g.TokensIn += a.in
		g.TokensOut += a.out
		g.TokensCached += a.cached
		g.TokensReasoning += a.reasoning
		g.EstimatedRows += a.est
		if entry != nil {
			g.Amount += amount
			g.Priced = true
		} else {
			g.NoPrice = true
		}
		members[finalKey][model] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := &Summary{GroupBy: groupBy, Currency: ix.Currency(), SavingsOn: ix.Enabled()}
	sort.Strings(groupOrder)
	for _, k := range groupOrder {
		g := byGroup[k]
		mem := make([]string, 0, len(members[k]))
		for m := range members[k] {
			mem = append(mem, m)
		}
		sort.Strings(mem)
		g.Members = mem
		extraW, extraA := groupPredicate(groupBy, k, mem)
		p50, p95, err := r.Percentiles(ctx, f, extraW, extraA)
		if err != nil {
			return nil, err
		}
		g.P50Ms, g.P95Ms = p50, p95
		out.Groups = append(out.Groups, *g)
	}
	tot, err := r.Totals(ctx, f)
	if err != nil {
		return nil, err
	}
	out.GrandTotal = SummaryGroup{
		Key: "TOTAL", Requests: tot.Requests, TokensIn: tot.TokensIn, TokensOut: tot.TokensOut,
		TokensCached: tot.TokensCached, TokensReasoning: tot.TokensReasoning, EstimatedRows: tot.EstimatedRows,
		P50Ms: tot.LatencyP50Ms, P95Ms: tot.LatencyP95Ms,
		Amount: grandAmount, Priced: grandPriced, NoPrice: grandNoPrice,
	}
	for n := range noPriceSet {
		out.NoPriceNames = append(out.NoPriceNames, n)
	}
	sort.Strings(out.NoPriceNames)
	return out, nil
}

// --- Period presets (server timezone) -------------------------------------

// TodayBounds returns [local midnight, next local midnight) as unix ms.
func TodayBounds(now time.Time) (int64, int64) {
	y, m, d := now.In(time.Local).Date()
	start := time.Date(y, m, d, 0, 0, 0, 0, time.Local)
	end := start.AddDate(0, 0, 1)
	return start.UnixMilli(), end.UnixMilli()
}

// MonthBounds returns [first of local month, first of next) as unix ms.
func MonthBounds(now time.Time) (int64, int64) {
	t := now.In(time.Local)
	y, m, _ := t.Date()
	start := time.Date(y, m, 1, 0, 0, 0, 0, time.Local)
	end := start.AddDate(0, 1, 0)
	return start.UnixMilli(), end.UnixMilli()
}
