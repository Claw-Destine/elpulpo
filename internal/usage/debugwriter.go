package usage

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// DebugKeepRows is the spam guard: while debug mode is on, at most this many
// of the newest recordings are kept (the usage retention prune deletes them
// earlier when it is enabled).
const DebugKeepRows int64 = 1000

// DebugWriter keeps recordings off the request path: the proxy pushes a
// capture into a buffered channel and one goroutine commits them. Captures
// can be large (a whole context plus response), so the buffer is smaller
// than the usage writer's and a full queue drops the capture with an ERROR —
// stalling a response would be worse.
type DebugWriter struct {
	repo *Repo
	log  *slog.Logger
	ch   chan *DebugCapture

	closeOnce sync.Once
	done      chan struct{}
}

// NewDebugWriter builds a debug writer over the repository.
func NewDebugWriter(repo *Repo, log *slog.Logger) *DebugWriter {
	return &DebugWriter{repo: repo, log: log, ch: make(chan *DebugCapture, 128), done: make(chan struct{})}
}

// Run commits captures until ctx is cancelled, then drains what is left —
// that is the shutdown grace.
func (w *DebugWriter) Run(ctx context.Context) {
	defer close(w.done)
	n := 0
	for {
		select {
		case c := <-w.ch:
			w.commit(c)
			if n++; n >= 10 {
				n = 0
				w.trim(ctx)
			}
		case <-ctx.Done():
			w.trim(context.Background())
			for { // drain the buffer inside the shutdown grace
				select {
				case c := <-w.ch:
					w.commit(c)
				default:
					return
				}
			}
		}
	}
}

func (w *DebugWriter) commit(c *DebugCapture) {
	fctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := w.repo.InsertDebug(fctx, c); err != nil {
		w.log.Error("debug writer: insert failed, capture dropped", "model", c.Model, "err", err)
	}
}

func (w *DebugWriter) trim(ctx context.Context) {
	fctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	n, err := w.repo.DebugTrim(fctx, DebugKeepRows)
	if err != nil {
		w.log.Error("debug writer: trim failed", "err", err)
		return
	}
	if n > 0 {
		w.log.Info("debug writer: trimmed old recordings", "removed", n, "keep", DebugKeepRows)
	}
}

// Submit hands a capture to the writer; never blocks the request path.
func (w *DebugWriter) Submit(c *DebugCapture) {
	select {
	case w.ch <- c:
	default:
		w.log.Error("debug writer queue full, dropping debug capture", "model", c.Model)
	}
}

// Pending estimates how many captures are buffered but not yet committed.
func (w *DebugWriter) Pending() int { return len(w.ch) }
