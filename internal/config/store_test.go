package config_test

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"elpulpo/internal/config"
)

// lockedBuf: slog writes come from the watcher goroutine too.
type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newStore(t *testing.T) (*config.Store, string, *lockedBuf) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "elpulpo.yaml")
	buf := &lockedBuf{}
	st := config.NewStore(path, slog.New(slog.NewTextHandler(buf, nil)))
	if err := st.Load(); err != nil {
		t.Fatal(err)
	}
	return st, path, buf
}

func minimal() *config.Config {
	return &config.Config{Hosts: []config.Host{
		{ID: "h", HostAddresses: []string{"127.0.0.1"}, Servers: []config.Server{
			{ID: "s", Port: 11434, API: "ollama"},
		}},
	}}
}

// Own writes are not external edits, and a save creates the file with
// canonical contents (scenarios 27, 40's precondition).
func TestSaveCreatesFileAndStaysSilentForWatcher(t *testing.T) {
	st, path, buf := newStore(t)
	snap, err := st.Save(minimal(), st.Current().FileHash)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if snap.FileHash == "" || !strings.Contains(string(b), "id: h") {
		t.Fatalf("file not written: %q", b)
	}
	// A hand edit of the same bytes must not be reported back to us —
	// the watcher compares against the hash we now know.
	st.Watch(neverStop())
	time.Sleep(2200 * time.Millisecond)
	if strings.Contains(buf.String(), "hand edit") {
		t.Fatalf("own save reported as hand edit:\n%s", buf.String())
	}
}

// A valid hand edit is honoured within seconds and logged at INFO
// (scenario 38); an invalid one keeps the live config (scenario 39).
func TestWatcherHonoursAndRefuses(t *testing.T) {
	st, path, buf := newStore(t)
	if _, err := st.Save(minimal(), st.Current().FileHash); err != nil {
		t.Fatal(err)
	}
	st.Watch(neverStop())

	// valid edit: add a second host
	good := &config.Config{Hosts: append([]config.Host{
		{ID: "b", HostAddresses: []string{"10.0.0.2"}, Servers: []config.Server{
			{ID: "s", Port: 8000, API: "openai"},
		}},
	}, minimal().Hosts...)}
	data := config.Canonical(good)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(st.Current().Config.Hosts) == 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(st.Current().Config.Hosts) != 2 {
		t.Fatalf("hand edit not applied:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "configuration reloaded from hand edit") {
		t.Fatalf("INFO reload missing:\n%s", buf.String())
	}

	// invalid edit: unsupported adapter
	bad := strings.Replace(string(data), "api: openai", "api: anthropic", 1)
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st.LastErr() != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if st.LastErr() == "" || !strings.Contains(st.LastErr(), "unsupported adapter") {
		t.Fatalf("LastErr = %q", st.LastErr())
	}
	if len(st.Current().Config.Hosts) != 2 {
		t.Fatal("live config changed after invalid edit")
	}
	onDisk, _ := os.ReadFile(path)
	if string(onDisk) != bad {
		t.Fatal("file must be left untouched")
	}
	if !strings.Contains(buf.String(), "level=ERROR") {
		t.Fatalf("ERROR missing:\n%s", buf.String())
	}

	// A stale save is refused (scenario 40).
	if _, err := st.Save(minimal(), "stale-hash"); err != config.ErrStaleSave {
		t.Fatalf("want ErrStaleSave, got %v", err)
	}
}

func neverStop() chan struct{} { return make(chan struct{}) }

func TestMissingFileIsEmpty(t *testing.T) {
	st, path, _ := newStore(t)
	if !st.Current().Config.Empty() {
		t.Fatal("missing file must be empty config")
	}
	if st.Current().FileHash != "" {
		t.Fatal("empty hash for missing file")
	}
	_ = path
}
