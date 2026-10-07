package usage_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"elpulpo/internal/usage"
)

func debugCap(ts time.Time, model string) *usage.DebugCapture {
	return &usage.DebugCapture{
		TsMs: ts.UnixMilli(), HostID: "h1", ServerID: "s1", Model: model,
		Status: "ok", HTTPStatus: 200, LatencyMs: 12, TokensIn: 20, TokensOut: 5,
		RequestJSON: `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`,
	}
}

func TestDebugInsertListGet(t *testing.T) {
	r, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()

	buffered := debugCap(time.Now(), "alpha")
	buffered.ResponseJSON = `{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"hello"}}]}`
	buffered.HasResponse = true
	id1, err := r.InsertDebug(ctx, buffered)
	if err != nil {
		t.Fatal(err)
	}

	streamed := debugCap(time.Now(), "beta")
	streamed.Route = "beta-lb"
	streamed.Stream = true
	streamed.ResponseSSE = "data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\ndata: [DONE]\n\n"
	id2, err := r.InsertDebug(ctx, streamed)
	if err != nil {
		t.Fatal(err)
	}

	rows, err := r.DebugRows(ctx, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].ID != id2 || rows[1].ID != id1 {
		t.Fatalf("list not newest-first: %+v", rows)
	}
	if !rows[0].Stream || rows[0].Route != "beta-lb" {
		t.Fatalf("stream row wrong: %+v", rows[0])
	}
	if rows[1].Stream || rows[1].ReqBytes == 0 || rows[1].RespBytes == 0 {
		t.Fatalf("sizes missing on buffered row: %+v", rows[1])
	}

	got, err := r.DebugGet(ctx, id2)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || !got.Stream || got.ResponseSSE == "" || got.HasResponse {
		t.Fatalf("stream capture round trip: %+v", got)
	}
	got, err = r.DebugGet(ctx, id1)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || !got.HasResponse || got.ResponseJSON == "" || got.ResponseSSE != "" {
		t.Fatalf("buffered capture round trip: %+v", got)
	}

	missing, err := r.DebugGet(ctx, 9999)
	if err != nil || missing != nil {
		t.Fatalf("missing id must be (nil,nil), got %+v %v", missing, err)
	}
}

func TestDebugClearTrimCount(t *testing.T) {
	r, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := r.InsertDebug(ctx, debugCap(time.Now(), "m")); err != nil {
			t.Fatal(err)
		}
	}
	n, err := r.DebugCount(ctx)
	if err != nil || n != 5 {
		t.Fatalf("count %d %v", n, err)
	}
	if _, err := r.DebugTrim(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if n, _ := r.DebugCount(ctx); n != 3 {
		t.Fatalf("trim left %d rows", n)
	}
	rows, _ := r.DebugRows(ctx, 10, 0)
	if rows[0].Model != "m" || rows[2].ID > rows[0].ID {
		t.Fatalf("trim removed the wrong end: %+v", rows)
	}
	if n, err := r.DebugClear(ctx); err != nil || n != 3 {
		t.Fatalf("clear removed %d (%v)", n, err)
	}
	if n, _ := r.DebugCount(ctx); n != 0 {
		t.Fatal("clear left rows")
	}
}

// Retention ages the recordings with the usage rows.
func TestDebugPruneAgesRecordings(t *testing.T) {
	r, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()

	old := debugCap(time.Now().AddDate(0, 0, -40), "old")
	fresh := debugCap(time.Now(), "fresh")
	for _, c := range []*usage.DebugCapture{old, fresh} {
		if _, err := r.InsertDebug(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Insert(ctx, []usage.Row{row(time.Now().AddDate(0, 0, -40), "m", "h1", 1, 1, 0, 0, 5)}); err != nil {
		t.Fatal(err)
	}
	n, err := r.PruneOlderThan(ctx, 30)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 { // one usage row + one recording
		t.Fatalf("pruned %d, want 2", n)
	}
	rows, _ := r.DebugRows(ctx, 10, 0)
	if len(rows) != 1 || rows[0].Model != "fresh" {
		t.Fatalf("fresh recording lost or old kept: %+v", rows)
	}
}
