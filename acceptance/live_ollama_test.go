//go:build live

// Smoke against a real Ollama. Not in the default suite: determinism earns
// more than realism there (implementation spec, Testing). Run manually
// with: go test -tags live ./acceptance/ -run TestLive -v
package acceptance_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"elpulpo/internal/config"
	"elpulpo/internal/testutil"
)

func TestLiveOllamaSmoke(t *testing.T) {
	resp, err := http.Get("http://127.0.0.1:11434/api/tags")
	if err != nil {
		t.Skipf("no live Ollama on 127.0.0.1:11434: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Skipf("Ollama answered %d", resp.StatusCode)
	}

	h := testutil.Start(t)
	h.ApplyConfig(&config.Config{Hosts: []config.Host{
		{ID: "live", HostAddresses: []string{"127.0.0.1"}, Servers: []config.Server{
			{ID: "ollama", Port: 11434, API: "ollama"},
		}},
	}})
	var ids []string
	h.Eventually(10*time.Second, "live models published", func() bool {
		ids = h.ModelIDs()
		return len(ids) > 0
	})
	first := ids[0]
	// Keep it tiny; any served model will do.
	st, body := h.Chat("", map[string]any{
		"model":      first,
		"messages":   []any{map[string]string{"role": "user", "content": "Reply with the single word: ok"}},
		"stream":     false,
		"max_tokens": 8,
		"keep_alive": "60s",
	})
	if st != 200 {
		t.Fatalf("live chat: %d %s", st, body)
	}
	h.Eventually(5*time.Second, "usage row", func() bool {
		rows := h.Rows()
		if len(rows) == 0 {
			return false
		}
		r := rows[0]
		t.Logf("row: model=%s status=%s tokens=%d/%d estimated=%v ttft=%v", r.Model, r.Status, r.TokensIn, r.TokensOut, r.Estimated, r.TTFTMs)
		return r.HostID == "live" && (r.TokensIn > 0 || r.Estimated)
	})
	if !strings.Contains(h.LogString(), "server is up") {
		t.Fatal("probe never reported up")
	}
}
