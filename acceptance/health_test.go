package acceptance_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"elpulpo/internal/config"
	"elpulpo/internal/health"
	"elpulpo/internal/testutil"
)

func stateFor(h *testutil.Harness, host, server string) (health.StateView, bool) {
	for _, s := range h.App.Mgr.States() {
		if s.HostID == host && s.ServerID == server {
			return s, true
		}
	}
	return health.StateView{}, false
}

// TestAcceptance_16_AddressFallback: only the second address answers; the
// server is up, publishes models, and the dashboard state flags 10.8.0.5-
// style fallback.
func TestAcceptance_16_AddressFallback(t *testing.T) {
	h := testutil.Start(t)
	// A socket bound to 127.0.0.2:P refuses 127.0.0.1:P, modelling "only the
	// second address is reachable" for the same port.
	up := testutil.NewUpstreamOn(t, "127.0.0.2", "ollama", "qwen3.8:27b")
	h.ApplyConfig(&config.Config{Hosts: []config.Host{
		{ID: "m", HostAddresses: []string{"127.0.0.1", "127.0.0.2"}, Servers: []config.Server{
			{ID: "llm", Port: up.Port(), API: "ollama"},
		}},
	}})

	h.WaitForModel("qwen3.8:27b-ollama@m")
	st, ok := stateFor(h, "m", "llm")
	if !ok {
		t.Fatal("server state missing")
	}
	if st.ActiveAddr != "127.0.0.2" {
		t.Fatalf("active address = %q, want 127.0.0.2", st.ActiveAddr)
	}
	if !st.Fallback {
		t.Fatal("fallback flag must be set — the first address never answered")
	}
}

// TestAcceptance_17_NoFailback: the first address becomes reachable again
// while the fallback answers; El Pulpo keeps using the fallback and sends
// the first address no probes.
func TestAcceptance_17_NoFailback(t *testing.T) {
	h := testutil.Start(t)
	up2 := testutil.NewUpstreamOn(t, "127.0.0.2", "ollama", "qwen3.8:27b")
	port2 := up2.Port()
	h.ApplyConfig(&config.Config{Hosts: []config.Host{
		{ID: "m", HostAddresses: []string{"127.0.0.1", "127.0.0.2"}, Servers: []config.Server{
			{ID: "llm", Port: port2, API: "ollama"},
		}},
	}})
	h.WaitForModel("qwen3.8:27b-ollama@m")

	// The first address comes back on the same port.
	up1 := testutil.NewUpstreamOnPort(t, fmt.Sprintf("127.0.0.1:%d", port2), "ollama", "qwen3.8:27b")

	time.Sleep(12 * time.Second) // more than two health intervals

	st, _ := stateFor(h, "m", "llm")
	if st.ActiveAddr != "127.0.0.2" {
		t.Fatalf("fail-back happened: active = %q", st.ActiveAddr)
	}
	if hits := up1.Hits.Load(); hits != 0 {
		t.Fatalf("the preferred address received %d probes; steady state must probe the active address only", hits)
	}
	if up2.Hits.Load() == 0 {
		t.Fatal("the active address stopped being probed")
	}
}

// TestAcceptance_18_ReelectToFirst: when the active fallback stops
// answering while the first address is alive, the server is reached on the
// first address within one interval, never enters down, and the switch is
// logged.
func TestAcceptance_18_ReelectToFirst(t *testing.T) {
	h := testutil.Start(t)
	up2 := testutil.NewUpstreamOn(t, "127.0.0.2", "ollama", "qwen3.8:27b")
	port2 := up2.Port()
	h.ApplyConfig(&config.Config{Hosts: []config.Host{
		{ID: "m", HostAddresses: []string{"127.0.0.1", "127.0.0.2"}, Servers: []config.Server{
			{ID: "llm", Port: port2, API: "ollama"},
		}},
	}})
	h.WaitForModel("qwen3.8:27b-ollama@m")

	// The first address is now alive; the active fallback dies.
	up1 := testutil.NewUpstreamOnPort(t, fmt.Sprintf("127.0.0.1:%d", port2), "ollama", "qwen3.8:27b")
	up2.Close()

	h.Eventually(12*time.Second, "switch back to the first address", func() bool {
		st, ok := stateFor(h, "m", "llm")
		return ok && st.Up && st.ActiveAddr == "127.0.0.1"
	})
	if strings.Contains(h.LogString(), "server is down") {
		t.Fatalf("server must never enter down:\n%s", h.LogString())
	}
	if !strings.Contains(h.LogString(), "switched address") {
		t.Fatalf("switch not logged at INFO:\n%s", h.LogString())
	}
	h.WaitForModel("qwen3.8:27b-ollama@m")
	if st, body := h.Chat("", map[string]any{
		"model":    "qwen3.8:27b-ollama@m",
		"messages": []any{map[string]string{"role": "user", "content": "hi"}},
	}); st != 200 {
		t.Fatalf("request after switch: %d %s", st, body)
	}
	if up1.Hits.Load() == 0 {
		t.Fatal("traffic did not reach the first address")
	}
}

// TestAcceptance_19_ThreeFailuresDown: no address answers → after three
// consecutive probe failures the server is down and its models are gone.
func TestAcceptance_19_ThreeFailuresDown(t *testing.T) {
	h := testutil.Start(t)
	up := testutil.NewUpstreamOn(t, "127.0.0.1", "ollama", "qwen3.8:27b")
	h.ApplyConfig(&config.Config{Hosts: []config.Host{
		{ID: "m", HostAddresses: []string{"127.0.0.1", "127.0.0.2"}, Servers: []config.Server{
			{ID: "llm", Port: up.Port(), API: "ollama"},
		}},
	}})
	h.WaitForModel("qwen3.8:27b-ollama@m")
	up.Close()

	h.Eventually(30*time.Second, "server marked down after three failures", func() bool {
		st, ok := stateFor(h, "m", "llm")
		return ok && !st.Up && st.ConsecFails >= 3
	})
	down := logLine(h.LogString(), "server is down after 3 consecutive probe failures")
	if down == "" {
		t.Fatalf("down transition must be logged:\n%s", h.LogString())
	}
	// A server going dark is a failure, not a warning, and the line says
	// which models clients just lost.
	if !strings.Contains(down, "level=ERROR") {
		t.Fatalf("down transition must be ERROR:\n%s", down)
	}
	if !strings.Contains(down, "models_withdrawn=[qwen3.8:27b]") {
		t.Fatalf("down transition must name the withdrawn models:\n%s", down)
	}
	for _, id := range h.ModelIDs() {
		if strings.HasSuffix(id, "@m") {
			t.Fatalf("down server still publishes %s", id)
		}
	}
}

// TestAcceptance_20_AddressEditReelection: editing host_addresses drops the
// active address and the next probe walks the list from position 0.
func TestAcceptance_20_AddressEditReelection(t *testing.T) {
	h := testutil.Start(t)
	up2 := testutil.NewUpstreamOn(t, "127.0.0.2", "ollama", "qwen3.8:27b")
	h.ApplyConfig(&config.Config{Hosts: []config.Host{
		{ID: "m", HostAddresses: []string{"127.0.0.1", "127.0.0.2"}, Servers: []config.Server{
			{ID: "llm", Port: up2.Port(), API: "ollama"},
		}},
	}})
	h.WaitForModel("qwen3.8:27b-ollama@m")

	// A new server appears at a NEW preferred position (127.0.0.3) with a
	// different model list; edit the list to put it first.
	up3 := testutil.NewUpstreamOn(t, "127.0.0.3", "ollama", "gemma4:31b")
	cfg := h.App.Store.Current().Config
	cfg.Hosts[0].HostAddresses = []string{"127.0.0.3", "127.0.0.2"}
	cfg.Hosts[0].Servers[0].Port = up3.Port()
	h.ApplyConfig(cfg)

	if !strings.Contains(h.LogString(), "dropping the address election") {
		t.Fatalf("address edit must drop the election:\n%s", h.LogString())
	}
	h.WaitForModel("gemma4:31b-ollama@m")
	h.WaitForNoModel("qwen3.8:27b-ollama@m")
	st, _ := stateFor(h, "m", "llm")
	if st.ActiveAddr != "127.0.0.3" {
		t.Fatalf("election must start from position 0: active = %q", st.ActiveAddr)
	}
	if up3.Hits.Load() == 0 {
		t.Fatal("the new first address was never probed")
	}
}

// TestHealthModelListLogging: connecting to a host logs, at INFO, the models
// that host returned; a list that moves while the same address keeps answering
// gets its own INFO line naming what was added and removed. Steady probes say
// nothing at INFO.
func TestHealthModelListLogging(t *testing.T) {
	h := testutil.Start(t)
	up := testutil.NewUpstream(t, "ollama", "alpha:1", "beta:2")
	h.ApplyConfig(&config.Config{Hosts: []config.Host{
		{ID: "m", HostAddresses: []string{"127.0.0.1"}, Servers: []config.Server{
			{ID: "llm", Port: up.Port(), API: "ollama"},
		}},
	}})
	h.WaitForModel("alpha:1-ollama@m")

	connect := ""
	h.Eventually(9*time.Second, "connect line naming the models", func() bool {
		connect = logLine(h.LogString(), "server is up")
		return connect != ""
	})
	for _, want := range []string{"level=INFO", "host=m", "server=llm", "address=127.0.0.1",
		"model_count=2", `models="[alpha:1 beta:2]"`} {
		if !strings.Contains(connect, want) {
			t.Fatalf("connect line lacks %q:\n%s\nfull log:\n%s", want, connect, h.LogString())
		}
	}

	// The same address now answers with a different list.
	up.SetModels("alpha:1", "gamma:3")
	h.WaitForModel("gamma:3-ollama@m")
	h.WaitForNoModel("beta:2-ollama@m")

	changed := ""
	h.Eventually(9*time.Second, "model list change logged", func() bool {
		changed = logLine(h.LogString(), "server model list changed")
		return changed != ""
	})
	for _, want := range []string{"level=INFO", "host=m", "server=llm", "address=127.0.0.1",
		"added=[gamma:3]", "removed=[beta:2]", "model_count=2", `models="[alpha:1 gamma:3]"`} {
		if !strings.Contains(changed, want) {
			t.Fatalf("change line lacks %q:\n%s", want, changed)
		}
	}

	// Steady state stays quiet: only the connect line and the change line
	// name a model list at INFO; routine probes are DEBUG.
	named := 0
	for _, l := range strings.Split(h.LogString(), "\n") {
		if strings.Contains(l, "level=INFO") && strings.Contains(l, " models=") {
			named++
		}
	}
	if named != 2 {
		t.Fatalf("want 2 INFO lines naming a model list, got %d:\n%s", named, h.LogString())
	}
}
