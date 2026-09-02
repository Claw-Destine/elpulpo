package usage

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Writer keeps the proxy path off the database: request goroutines push rows
// into a buffered channel and one writer commits in batches of 64 rows or
// every 50 ms. When the channel is full the row is dropped with an ERROR —
// stalling a response would be worse.
type Writer struct {
	repo *Repo
	log  *slog.Logger
	ch   chan Row

	closeOnce sync.Once
	done      chan struct{}
}

// NewWriter builds a writer over the repository.
func NewWriter(repo *Repo, log *slog.Logger) *Writer {
	return &Writer{repo: repo, log: log, ch: make(chan Row, 1024), done: make(chan struct{})}
}

// Run consumes and commits until ctx is cancelled, then drains what is left
// in the channel — that is the shutdown grace.
func (w *Writer) Run(ctx context.Context) {
	defer close(w.done)
	batch := make([]Row, 0, 64)
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	flush := func() {
		if len(batch) == 0 {
			return
		}
		fctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := w.repo.Insert(fctx, batch); err != nil {
			w.log.Error("usage writer: batch insert failed, rows dropped", "rows", len(batch), "err", err)
		}
		batch = batch[:0]
	}
	for {
		select {
		case row := <-w.ch:
			batch = append(batch, row)
			if len(batch) >= 64 {
				flush()
			}
		case <-t.C:
			flush()
		case <-ctx.Done():
			flush()
			for { // drain the buffer inside the shutdown grace
				select {
				case row := <-w.ch:
					batch = append(batch, row)
					if len(batch) >= 64 {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

// Submit hands a row to the writer; never blocks the request path.
func (w *Writer) Submit(row Row) {
	select {
	case w.ch <- row:
	default:
		w.log.Error("usage writer queue full, dropping usage row", "model", row.Model, "host", row.HostID, "server", row.ServerID)
	}
}

// Pending estimates how many rows are buffered but not yet committed.
func (w *Writer) Pending() int { return len(w.ch) }

// Wait blocks until the writer goroutine has finished draining.
func (w *Writer) Wait() { <-w.done }
