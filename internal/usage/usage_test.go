package usage_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"elpulpo/internal/config"
	"elpulpo/internal/usage"
)

func repo(t *testing.T) *usage.Repo {
	t.Helper()
	r, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func row(ts time.Time, model, host string, in, out, cached, reasoning int64, lat int64) usage.Row {
	return usage.Row{
		TsMs: ts.UnixMilli(), HostID: host, ServerID: "srv", Model: model, Endpoint: "chat",
		Status: "ok", HTTPStatus: 200, TokensIn: in, TokensOut: out,
		TokensCached: cached, TokensReasoning: reasoning, LatencyMs: lat,
	}
}

func f(v float64) *float64 { return &v }

func index() *usage.PriceIndex {
	return usage.NewPriceIndex(&config.Prices{
		Currency: "USD",
		Models: []config.ModelPrice{{
			Model: "qwen3.8:27b", Aliases: []string{"qwen27"},
			Input: f(0.25), Output: f(1.00), CachedInput: f(0.10), ReasoningOutput: f(1.00),
		}},
	})
}

// Scenario 21: alias and original price identically; model summary merges
// the spellings. Amount = in_eff*in + cached*pcached + out_eff*out +
// reasoning*preasoning, per 1M.
func TestSavingsAndAliasGrouping(t *testing.T) {
	r := repo(t)
	now := time.Now()
	rows := []usage.Row{
		row(now, "qwen3.8:27b-ollama@minion1", "minion1", 1000, 500, 400, 100, 100),
		row(now, "qwen27-vllm@minion2", "minion2", 2000, 1000, 0, 0, 200),
	}
	if err := r.Insert(context.Background(), rows); err != nil {
		t.Fatal(err)
	}
	s, err := r.Summary(context.Background(), usage.Filter{}, "model", index())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Groups) != 1 {
		t.Fatalf("both spellings must group under the price entry's model: %+v", s.Groups)
	}
	g := s.Groups[0]
	if g.Key != "qwen3.8:27b" || !g.Priced || g.NoPrice {
		t.Fatalf("group %+v", g)
	}
	want := (600*0.25 + 400*0.10 + 400*1.00 + 100*1.00 + 2000*0.25 + 1000*1.00) / 1e6
	if diff := g.Amount - want; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("amount %v want %v", g.Amount, want)
	}
}

func TestNoPriceSet(t *testing.T) {
	r := repo(t)
	r.Insert(context.Background(), []usage.Row{
		row(time.Now(), "mystery-model-openai@h", "h", 10, 10, 0, 0, 5),
	})
	s, err := r.Summary(context.Background(), usage.Filter{}, "model", index())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.NoPriceNames) != 1 || s.NoPriceNames[0] != "mystery-model" {
		t.Fatalf("no-price list: %+v", s.NoPriceNames)
	}
	if s.Groups[0].Priced || !s.Groups[0].NoPrice || s.Groups[0].Amount != 0 {
		t.Fatalf("unpriced group must not fake an amount: %+v", s.Groups[0])
	}
}

// Percentiles are exact on the filtered set (spec's OFFSET definition).
func TestPercentiles(t *testing.T) {
	r := repo(t)
	var rows []usage.Row
	for i := 1; i <= 20; i++ {
		rows = append(rows, row(time.Now(), "m-openai@h", "h", 1, 1, 0, 0, int64(i*10)))
	}
	r.Insert(context.Background(), rows)
	tot, err := r.Totals(context.Background(), usage.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if tot.Requests != 20 {
		t.Fatalf("requests %d", tot.Requests)
	}
	// sorted latencies 10..200; p50 = OFFSET 10 -> 110; p95 = OFFSET 19 -> 200
	if tot.LatencyP50Ms != 110 || tot.LatencyP95Ms != 200 {
		t.Fatalf("p50=%d p95=%d", tot.LatencyP50Ms, tot.LatencyP95Ms)
	}
}

// The displayed totals must equal the sums recomputed from the rows CSV
// (scenarios 13, 32): one filter path, two consumers.
func TestCSVRoundTrip(t *testing.T) {
	r := repo(t)
	now := time.Now()
	rows := []usage.Row{
		row(now, "qwen3.8:27b-ollama@minion1", "minion1", 1000, 500, 400, 100, 100),
		row(now, "qwen27-vllm@minion2", "minion2", 2000, 1000, 0, 0, 200),
		row(now, "unknown-x-openai@h2", "h2", 7, 3, 0, 0, 50),
	}
	rows[2].Estimated = true
	r.Insert(context.Background(), rows)

	var buf bytes.Buffer
	if err := r.WriteRowsCSV(context.Background(), &buf, usage.Filter{}); err != nil {
		t.Fatal(err)
	}
	recs, err := csv.NewReader(strings.NewReader(buf.String())).ReadAll()
	if err != nil {
		t.Fatalf("csv parse: %v\n%s", err, buf.String())
	}
	if len(recs) != 4 { // header + 3
		t.Fatalf("csv rows: %d", len(recs))
	}
	var in, out, latSum int64
	estCount := 0
	for _, rec := range recs[1:] {
		i, _ := strconv.ParseInt(rec[7], 10, 64)
		o, _ := strconv.ParseInt(rec[8], 10, 64)
		lat, _ := strconv.ParseInt(rec[12], 10, 64)
		in += i
		out += o
		latSum += lat
		if rec[11] == "true" {
			estCount++
		}
	}
	tot, _ := r.Totals(context.Background(), usage.Filter{})
	if tot.TokensIn != in || tot.TokensOut != out || tot.EstimatedRows != int64(estCount) {
		t.Fatalf("totals %v vs CSV in=%d out=%d est=%d", tot, in, out, estCount)
	}

	// Summary CSV: per-model amounts match the reference prices.
	buf.Reset()
	if err := r.WriteSummaryCSV(context.Background(), &buf, usage.Filter{}, index()); err != nil {
		t.Fatal(err)
	}
	recs, err = csv.NewReader(strings.NewReader(buf.String())).ReadAll()
	if err != nil {
		t.Fatalf("summary csv parse: %v\n%s", err, buf.String())
	}
	var amountQwen string
	for _, rec := range recs[1:] {
		if rec[0] == "model" && rec[1] == "qwen3.8:27b" {
			amountQwen = rec[10]
		}
		if rec[0] == "model" && rec[1] == "unknown-x" && rec[10] != "" {
			t.Fatal("unpriced group must not carry an amount")
		}
	}
	want := (600*0.25 + 400*0.10 + 400*1.00 + 100*1.00 + 2000*0.25 + 1000*1.00) / 1e6
	got, err := strconv.ParseFloat(amountQwen, 64)
	if err != nil {
		t.Fatalf("amount %q: %v", amountQwen, err)
	}
	if diff := got - want; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("summary csv amount %v want %v", got, want)
	}

	// Empty selection still writes the header.
	buf.Reset()
	r.WriteRowsCSV(context.Background(), &buf, usage.Filter{ModelContains: "nothing-matches-xyz"})
	if !strings.HasPrefix(buf.String(), "timestamp,host,") {
		t.Fatalf("empty CSV must keep the header: %q", buf.String())
	}
}

func TestPrune(t *testing.T) {
	r := repo(t)
	var rows []usage.Row
	old := time.Now().AddDate(0, 0, -40).UnixMilli()
	for i := 0; i < 6001; i++ { // crosses the 5000-row batch boundary
		rw := row(time.UnixMilli(old), "m-openai@h", "h", 1, 1, 0, 0, 1)
		rw.TsMs = old - int64(i)
		rows = append(rows, rw)
	}
	recent := row(time.Now(), "m-openai@h", "h", 1, 1, 0, 0, 1)
	rows = append(rows, recent)
	if err := r.Insert(context.Background(), rows); err != nil {
		t.Fatal(err)
	}
	n, err := r.PruneOlderThan(context.Background(), 30)
	if err != nil {
		t.Fatal(err)
	}
	if n != 6001 {
		t.Fatalf("pruned %d, want 6001", n)
	}
	rest, _ := r.Rows(context.Background(), usage.Filter{}, "timestamp", false, 10, 0)
	if len(rest) != 1 {
		t.Fatalf("kept %d rows", len(rest))
	}
}
