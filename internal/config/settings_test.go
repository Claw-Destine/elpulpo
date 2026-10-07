package config_test

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"elpulpo/internal/config"
	"elpulpo/internal/usage"
)

func settingsMgr(t *testing.T) (*config.SettingsManager, *usage.Repo) {
	t.Helper()
	r, err := usage.Open(filepath.Join(t.TempDir(), "u.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	m := config.NewSettingsManager(r, nil)
	if err := m.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	return m, r
}

func TestDefaultsMaterialised(t *testing.T) {
	m, _ := settingsMgr(t)
	g := m.Get()
	d := config.DefaultSettings()
	if *g != d {
		t.Fatalf("defaults: %+v vs %+v", g, d)
	}
	if g.HealthInterval != 30*time.Second || g.QueueTimeout != 60*time.Second ||
		g.MaxRequestSize != 32<<20 || g.RetentionDays != 0 || g.TotalTimeout != 0 {
		t.Fatalf("unexpected defaults %+v", g)
	}
}

func TestParseDurAndSize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"30s", "30s"}, {"2m", "2m0s"}, {"1h", "1h0m0s"}, {"45", "45s"},
	}
	m, _ := settingsMgr(t)
	for _, c := range cases {
		_, v := m.Update(context.Background(), map[string]string{"total_timeout": c.in})
		if len(v) > 0 {
			t.Fatalf("%q: %v", c.in, v)
		}
		if got := m.Get().TotalTimeout.String(); got != c.want {
			t.Fatalf("%q -> %q want %q", c.in, got, c.want)
		}
	}
	// total_timeout accepts 0 = disabled.
	if _, v := m.Update(context.Background(), map[string]string{"total_timeout": "0"}); len(v) > 0 {
		t.Fatalf("0: %v", v)
	}
	if m.Get().TotalTimeout != 0 {
		t.Fatal("0 must disable the total timeout")
	}

	sizes := []struct {
		in   string
		want int64
	}{{"32MiB", 32 << 20}, {"1KiB", 1024}, {"1GiB", 1 << 30}, {"1048576", 1048576},
		{"512KB", 512_000}, {"1MB", 1_000_000}}
	for _, c := range sizes {
		_, v := m.Update(context.Background(), map[string]string{"max_request_size": c.in})
		if len(v) > 0 {
			t.Fatalf("%q: %v", c.in, v)
		}
		if m.Get().MaxRequestSize != c.want {
			t.Fatalf("%q -> %d want %d", c.in, m.Get().MaxRequestSize, c.want)
		}
	}
}

// Scenario 46: out-of-range values are rejected per field naming the
// allowed range, and nothing is applied.
func TestRangeRejections(t *testing.T) {
	m, r := settingsMgr(t)
	before := *m.Get()
	cases := map[string]string{
		"health_interval":     "1s",   // below 5s
		"probe_timeout":       "10m",  // above 60s
		"queue_timeout":       "0s",   // 0 not allowed here
		"max_request_size":    "2GiB", // above 1GiB
		"retention_days":      "-3",
		"connect_timeout":     "1s1",
		"total_timeout":       "48h",
		"stream_idle_timeout": "12h",
	}
	for k, v := range cases {
		_, viol := m.Update(context.Background(), map[string]string{k: v})
		if len(viol) == 0 {
			t.Fatalf("%s=%s must be rejected", k, v)
		}
	}
	after := *m.Get()
	if after != before {
		t.Fatalf("live settings moved despite rejections: %+v vs %+v", after, before)
	}
	// Persisted values too.
	reloaded := config.NewSettingsManager(r, nil)
	if err := reloaded.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *reloaded.Get() != before {
		t.Fatal("rejected values must not be persisted")
	}
	// The violation names the allowed range.
	_, viol := m.Update(context.Background(), map[string]string{"health_interval": "1s"})
	if len(viol) == 0 || viol[0].Error() == "" {
		t.Fatal("violation must carry a message")
	}
}

// Generation bumps only on real changes — the proxy rebinds transports on
// it.
func TestGenerationBumps(t *testing.T) {
	m, _ := settingsMgr(t)
	g0 := m.Gen()
	m.Update(context.Background(), map[string]string{"health_interval": "10s"})
	if m.Gen() == g0 {
		t.Fatal("gen must bump on change")
	}
	g1 := m.Gen()
	m.Update(context.Background(), map[string]string{"health_interval": "10s"})
	if m.Gen() != g1 {
		t.Fatal("no-op save must not bump the generation")
	}
	m.Update(context.Background(), map[string]string{"nope": "1"})
	if m.Gen() != g1 {
		t.Fatal("unknown keys must not bump the generation")
	}
}

func TestDebugSettings(t *testing.T) {
	m, r := settingsMgr(t)
	g := m.Get()
	if g.DebugEnabled || g.DebugModel != "" {
		t.Fatalf("debug mode must default off with no filter: %+v", g)
	}

	if _, v := m.Update(context.Background(), map[string]string{"debug_enabled": "true"}); len(v) > 0 {
		t.Fatalf("on: %v", v)
	}
	if !m.Get().DebugEnabled {
		t.Fatal("debug_enabled true not applied")
	}
	if _, v := m.Update(context.Background(), map[string]string{"debug_enabled": "banana"}); len(v) == 0 {
		t.Fatal("debug_enabled must reject non-boolean text")
	}
	if _, v := m.Update(context.Background(), map[string]string{"debug_enabled": "off"}); len(v) > 0 {
		t.Fatalf("off: %v", v)
	}
	if m.Get().DebugEnabled {
		t.Fatal("off must disable")
	}

	// The filter names a model, and "" is a value: every model.
	if _, v := m.Update(context.Background(), map[string]string{"debug_model": "alpha-s1@h1"}); len(v) > 0 {
		t.Fatalf("filter: %v", v)
	}
	if m.Get().DebugModel != "alpha-s1@h1" {
		t.Fatalf("filter not stored: %q", m.Get().DebugModel)
	}
	if _, v := m.Update(context.Background(), map[string]string{"debug_model": ""}); len(v) > 0 {
		t.Fatalf("clearing the filter: %v", v)
	}
	if m.Get().DebugModel != "" {
		t.Fatalf("empty must clear the filter, got %q", m.Get().DebugModel)
	}

	// Both survive a restart.
	if _, v := m.Update(context.Background(), map[string]string{"debug_enabled": "true", "debug_model": "beta"}); len(v) > 0 {
		t.Fatalf("pair: %v", v)
	}
	m2 := config.NewSettingsManager(r, nil)
	if err := m2.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !m2.Get().DebugEnabled || m2.Get().DebugModel != "beta" {
		t.Fatalf("debug settings did not survive reload: %+v", m2.Get())
	}
}

func TestDebugOnHonoursWindow(t *testing.T) {
	m, _ := settingsMgr(t)
	now := time.Now()

	// A window in the future keeps recording on; past it, off — no matter
	// that the stored switch still says true.
	if _, v := m.Update(context.Background(), map[string]string{
		"debug_enabled": "true",
		"debug_until":   itoa64(now.Add(15 * time.Minute).UnixMilli()),
	}); len(v) > 0 {
		t.Fatalf("open window: %v", v)
	}
	if !m.Get().DebugOn(now) {
		t.Fatal("recording must be active inside its window")
	}
	if m.Get().DebugOn(now.Add(20 * time.Minute)) {
		t.Fatal("recording must stop at the deadline")
	}

	// No deadline means it runs until switched off.
	if _, v := m.Update(context.Background(), map[string]string{"debug_until": "0"}); len(v) > 0 {
		t.Fatalf("no limit: %v", v)
	}
	if !m.Get().DebugOn(now.Add(365 * 24 * time.Hour)) {
		t.Fatal("an unlimited window stays open")
	}
	if _, v := m.Update(context.Background(), map[string]string{"debug_enabled": "false"}); len(v) > 0 {
		t.Fatalf("switch off: %v", v)
	}
	if m.Get().DebugOn(now) {
		t.Fatal("off is off")
	}

	// The knob itself validates: non-negative integers only.
	if _, v := m.Update(context.Background(), map[string]string{"debug_until": "-5"}); len(v) == 0 {
		t.Fatal("a negative deadline must be refused")
	}
	if _, v := m.Update(context.Background(), map[string]string{"debug_until": "soon"}); len(v) == 0 {
		t.Fatal("non-numeric deadline must be refused")
	}
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }
