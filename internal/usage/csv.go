package usage

import (
	"context"
	"encoding/csv"
	"io"
	"strconv"
	"time"
)

// CSV timestamps are always parseable UTC, never the display timezone.
func csvTime(tsMs int64) string {
	return time.UnixMilli(tsMs).UTC().Format(time.RFC3339Nano)
}

func csvOpt(v *int64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatInt(*v, 10)
}

// WriteRowsCSV streams the filtered rows as text/csv, written from inside
// the scan loop — nothing is materialised. An empty selection still writes
// the header line.
func (r *Repo) WriteRowsCSV(ctx context.Context, w io.Writer, f Filter) error {
	cw := csv.NewWriter(w)
	header := []string{"timestamp", "host", "server", "model", "endpoint", "status", "http_status",
		"tokens_in", "tokens_out", "tokens_cached", "tokens_reasoning", "estimated",
		"latency_ms", "ttft_ms"}
	if err := cw.Write(header); err != nil {
		return err
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return err
	}
	where, args := f.where()
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+rowColumns+" FROM requests WHERE "+where+" ORDER BY ts_ms, id", args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		row, err := scanRow(rows.Scan)
		if err != nil {
			return err
		}
		rec := []string{
			csvTime(row.TsMs), row.HostID, row.ServerID, row.Model, row.Endpoint, row.Status,
			strconv.Itoa(row.HTTPStatus),
			strconv.FormatInt(row.TokensIn, 10), strconv.FormatInt(row.TokensOut, 10),
			strconv.FormatInt(row.TokensCached, 10), strconv.FormatInt(row.TokensReasoning, 10),
			strconv.FormatBool(row.Estimated),
			strconv.FormatInt(row.LatencyMs, 10), csvOpt(row.TTFTMs),
		}
		if err := cw.Write(rec); err != nil {
			return err
		}
		cw.Flush() // stream: keep at most one record buffered
		if err := cw.Error(); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}

func amountCSV(g SummaryGroup) string {
	if !g.Priced {
		return "" // no price set is never silently written as zero
	}
	return strconv.FormatFloat(g.Amount, 'f', 6, 64)
}

// WriteSummaryCSV streams the aggregated blocks — per model, per host and
// per day — with token sums and amounts, plus a TOTAL line.
func (r *Repo) WriteSummaryCSV(ctx context.Context, w io.Writer, f Filter, ix *PriceIndex) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()
	if err := cw.Write([]string{"group_by", "group", "requests", "tokens_in", "tokens_out",
		"tokens_cached", "tokens_reasoning", "estimated_rows", "p50_ms", "p95_ms", "amount", "currency"}); err != nil {
		return err
	}
	currency := ix.Currency()
	for _, gb := range []string{"model", "host", "day"} {
		s, err := r.Summary(ctx, f, gb, ix)
		if err != nil {
			return err
		}
		for _, g := range s.Groups {
			rec := []string{gb, g.Key,
				strconv.FormatInt(g.Requests, 10),
				strconv.FormatInt(g.TokensIn, 10), strconv.FormatInt(g.TokensOut, 10),
				strconv.FormatInt(g.TokensCached, 10), strconv.FormatInt(g.TokensReasoning, 10),
				strconv.FormatInt(g.EstimatedRows, 10),
				strconv.FormatInt(g.P50Ms, 10), strconv.FormatInt(g.P95Ms, 10),
				amountCSV(g), currency}
			if err := cw.Write(rec); err != nil {
				return err
			}
			cw.Flush()
			if err := cw.Error(); err != nil {
				return err
			}
		}
	}
	gt := func() error {
		s, err := r.Summary(ctx, f, "model", ix)
		if err != nil {
			return err
		}
		g := s.GrandTotal
		return cw.Write([]string{"total", "TOTAL",
			strconv.FormatInt(g.Requests, 10),
			strconv.FormatInt(g.TokensIn, 10), strconv.FormatInt(g.TokensOut, 10),
			strconv.FormatInt(g.TokensCached, 10), strconv.FormatInt(g.TokensReasoning, 10),
			strconv.FormatInt(g.EstimatedRows, 10),
			strconv.FormatInt(g.P50Ms, 10), strconv.FormatInt(g.P95Ms, 10),
			amountCSV(g), currency})
	}()
	if gt != nil {
		return gt
	}
	cw.Flush()
	return cw.Error()
}
