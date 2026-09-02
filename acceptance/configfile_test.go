package acceptance_test

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"elpulpo/internal/app"
	"elpulpo/internal/config"
	"elpulpo/internal/testutil"
	"elpulpo/internal/usage"
)

// TestAcceptance_11_ValidationRejectsSave: duplicate host ids, duplicate
// server ids, colliding name segments and shared addresses are refused
// with field-level errors; the live configuration and /v1/models stay put.
func TestAcceptance_11_ValidationRejectsSave(t *testing.T) {
	h := testutil.Start(t)
	n := startNaming(t, h)
	for _, id := range wantIDs {
		h.WaitForModel(id)
	}
	before := h.ConfigYAML()

	dupHost := n.config()
	dupHost.Hosts = append(dupHost.Hosts, dupHost.Hosts[0])
	mustViolate(t, h, dupHost, "duplicate host id")

	dupServer := n.config()
	dupServer.Hosts[0].Servers = append(dupServer.Hosts[0].Servers, config.Server{ID: "ollama", Port: 20000, API: "openai"})
	mustViolate(t, h, dupServer, "duplicate server id")

	dupSeg := n.config()
	dupSeg.Hosts[0].Servers[1].Postfix = "ollama" // resolves to "ollama" twice on minion1
	mustViolate(t, h, dupSeg, "published model ids would collide")

	sharedAddr := n.config()
	sharedAddr.Hosts[1].HostAddresses = []string{"127.0.0.1"}
	mustViolate(t, h, sharedAddr, "also listed under")

	// The live configuration never moved.
	if got := h.ConfigYAML(); got != before {
		t.Fatal("live config changed after a rejected save")
	}
	if ids := h.ModelIDs(); len(ids) != 5 {
		t.Fatalf("/v1/models changed: %s", testutil.ModelNames(ids))
	}
}

func mustViolate(t *testing.T, h *testutil.Harness, cfg *config.Config, want string) {
	t.Helper()
	_, err := h.App.Store.Save(cfg, h.App.Store.Current().FileHash)
	if err == nil {
		t.Fatalf("save with %q accepted", want)
	}
	ve, ok := err.(*config.ValidationError)
	if !ok {
		t.Fatalf("expected ValidationError for %q, got %T: %v", want, err, err)
	}
	var joined []string
	for _, v := range ve.Violations {
		joined = append(joined, v.Error())
	}
	if !strings.Contains(strings.Join(joined, "\n"), want) {
		t.Fatalf("violations for %q missing %q:\n%s", want, want, strings.Join(joined, "\n"))
	}
}

// TestAcceptance_22_AliasCollisionRejected: an alias equal to another
// entry's model is refused, reporting both offending paths.
func TestAcceptance_22_AliasCollisionRejected(t *testing.T) {
	h := testutil.Start(t)
	startNaming(t, h)
	h.WaitForModel("qwen3.8:27b-ollama@minion1")
	before := h.ConfigYAML()

	f := 1.0
	cfg := nWithPrices(&config.Prices{
		Currency: "USD",
		Models: []config.ModelPrice{
			{Model: "qwen3.8:27b", Aliases: []string{"qwen27"}, Input: &f, Output: &f},
			{Model: "deepseek-v4-flash", Aliases: []string{"qwen3.8:27b"}, Input: &f, Output: &f},
		},
	})
	_, err := h.App.Store.Save(cfg, h.App.Store.Current().FileHash)
	ve, ok := err.(*config.ValidationError)
	if !ok {
		t.Fatalf("expected validation error, got %v", err)
	}
	var paths []string
	for _, v := range ve.Violations {
		paths = append(paths, v.Path)
	}
	joined := strings.Join(paths, " ")
	if !strings.Contains(joined, "prices.models[0].aliases[0]") || !strings.Contains(joined, "prices.models[1].model") {
		t.Fatalf("both offending paths must be reported, got: %v", paths)
	}
	if got := h.ConfigYAML(); got != before {
		t.Fatal("live config changed after rejected save")
	}
}

// TestAcceptance_27_MissingConfigFile: a missing ELPULPO_CONFIG is an
// empty configuration; the catalogue is empty; the first save creates it.
func TestAcceptance_27_MissingConfigFile(t *testing.T) {
	h := testutil.Start(t)
	if _, err := os.Stat(h.App.Opts.ConfigPath); !os.IsNotExist(err) {
		t.Fatalf("config file should not exist yet: %v", err)
	}
	if ids := h.ModelIDs(); len(ids) != 0 {
		t.Fatalf("empty config must publish nothing, got %s", testutil.ModelNames(ids))
	}
	startNaming(t, h)
	h.WaitForModel("gemma4:31b-ollama@minion1")
	if _, err := os.Stat(h.App.Opts.ConfigPath); err != nil {
		t.Fatalf("first save must create the file: %v", err)
	}
}

// TestAcceptance_38_HandEditHonoured: a valid host added by hand on disk
// is noticed, validated and applied within seconds, logged at INFO,
// without a restart.
func TestAcceptance_38_HandEditHonoured(t *testing.T) {
	h := testutil.Start(t)
	up := testutil.NewUpstream(t, "ollama", "handadded-model")
	yaml := "hosts:\n-   host_addresses: [127.0.0.1]\n    id: handhost\n    servers:\n    -   port: " +
		itoa(up.Port()) + "\n        api: ollama\n        id: llm\n"
	if err := os.WriteFile(h.App.Opts.ConfigPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	h.WaitForModel("handadded-model-ollama@handhost")
	if !strings.Contains(h.LogString(), "configuration reloaded from hand edit") {
		t.Fatalf("reload must be logged at INFO:\n%s", h.LogString())
	}
	if !strings.Contains(h.LogString(), "level=INFO") {
		t.Fatal("expected INFO logs present")
	}
}

// TestAcceptance_39_HandEditInvalid: a hand edit that breaks validation
// keeps the live config, logs ERROR, shows on the dashboard state, and
// leaves the file untouched.
func TestAcceptance_39_HandEditInvalid(t *testing.T) {
	h := testutil.Start(t)
	startNaming(t, h)
	h.WaitForModel("gemma4:31b-ollama@minion1")

	original, _ := os.ReadFile(h.App.Opts.ConfigPath)
	broken := append([]byte(nil), original...)
	broken = []byte(strings.Replace(string(broken), "api: ollama", "api: anthropic", 1))
	if err := os.WriteFile(h.App.Opts.ConfigPath, broken, 0o600); err != nil {
		t.Fatal(err)
	}

	h.Eventually(5*time.Second, "the reload error to surface", func() bool {
		return h.App.Store.LastErr() != ""
	})
	if !strings.Contains(h.App.Store.LastErr(), "unsupported adapter") {
		t.Fatalf("LastErr = %q", h.App.Store.LastErr())
	}
	if !strings.Contains(h.LogString(), "level=ERROR") ||
		!strings.Contains(h.LogString(), "hand edit of the config file is invalid") {
		t.Fatalf("must log ERROR:\n%s", h.LogString())
	}
	after, _ := os.ReadFile(h.App.Opts.ConfigPath)
	if string(after) != string(broken) {
		t.Fatal("the file on disk must be left untouched")
	}
	for _, id := range h.ModelIDs() {
		if !strings.Contains(id, "@minion") {
			t.Fatalf("live catalogue changed: %s", id)
		}
	}
	if len(h.ModelIDs()) != 5 {
		t.Fatalf("live catalogue must stay complete, got %s", testutil.ModelNames(h.ModelIDs()))
	}
}

// TestAcceptance_42_WarnOnEveryStart: with the proxy token and dashboard
// password empty, the WARN line is present after every start, not only
// the first.
func TestAcceptance_42_WarnOnEveryStart(t *testing.T) {
	dir := t.TempDir()
	log1 := &testutil.LogSink{}
	a := startApp(t, dir, log1)
	if !strings.Contains(log1.String(), "ELPULPO_PROXY_TOKEN is empty") ||
		!strings.Contains(log1.String(), "ELPULPO_DASHBOARD_PASSWORD is empty") {
		t.Fatalf("first start missing WARN:\n%s", log1.String())
	}
	a.Stop()

	log2 := &testutil.LogSink{}
	a2 := startApp(t, dir, log2)
	defer a2.Stop()
	if !strings.Contains(log2.String(), "ELPULPO_PROXY_TOKEN is empty") ||
		!strings.Contains(log2.String(), "ELPULPO_DASHBOARD_PASSWORD is empty") {
		t.Fatalf("restart missing WARN:\n%s", log2.String())
	}
}

// TestAcceptance_45_HostsOnlyConfig: a hosts-only document saves and
// applies; the export contains no prices key.
func TestAcceptance_45_HostsOnlyConfig(t *testing.T) {
	h := testutil.Start(t)
	startNaming(t, h)
	h.WaitForModel("gemma4:31b-ollama@minion1")
	export := h.ConfigYAML()
	if strings.Contains(export, "prices") {
		t.Fatalf("export of a hosts-only config must omit prices:\n%s", export)
	}
	// Savings are off: no price entries exist.
	ix := usage.NewPriceIndex(h.App.Store.Current().Config.Prices)
	if ix.Enabled() {
		t.Fatal("savings must be off with no prices")
	}
	// A round-trip save of the hosts-only config stays stable.
	cfg := h.App.Store.Current().Config
	if _, err := h.App.Store.Save(cfg, h.App.Store.Current().FileHash); err != nil {
		t.Fatalf("re-save: %v", err)
	}
}

// TestAcceptance_46_SettingsRanges: out-of-range settings are rejected
// with a field-level error naming the allowed range; live values stay.
func TestAcceptance_46_SettingsRanges(t *testing.T) {
	h := testutil.Start(t)
	before := *h.App.Settings.Get()

	v := h.UpdateSettings(map[string]string{"health_interval": "1s"})
	if len(v) == 0 {
		t.Fatal("health_interval 1s must be rejected")
	}
	if !strings.Contains(v[0].Msg, "5s") || !strings.Contains(v[0].Msg, "3600s") {
		t.Fatalf("error must name the allowed range: %s", v[0].Msg)
	}
	v = h.UpdateSettings(map[string]string{"max_request_size": "2GiB"})
	if len(v) == 0 {
		t.Fatal("max_request_size 2GiB must be rejected")
	}
	if !strings.Contains(v[0].Msg, "1GiB") {
		t.Fatalf("error must name 1GiB: %s", v[0].Msg)
	}
	after := *h.App.Settings.Get()
	if after.HealthInterval != before.HealthInterval || after.MaxRequestSize != before.MaxRequestSize {
		t.Fatalf("live settings changed despite rejection: %+v vs %+v", after, before)
	}
}

// TestAcceptance_28_RetentionDefaultOff: a year-old row is kept by
// default; enabling retention_days and pruning removes it, logging the
// count.
func TestAcceptance_28_RetentionDefaultOff(t *testing.T) {
	h := testutil.Start(t)
	old := usage.Row{
		TsMs:   time.Now().AddDate(-1, 0, 0).UnixMilli(),
		HostID: "old", ServerID: "srv", Model: "old-model-openai@old", Endpoint: "chat",
		Status: "ok", HTTPStatus: 200, TokensIn: 5, TokensOut: 7, LatencyMs: 10,
	}
	recent := old
	recent.TsMs = time.Now().UnixMilli()
	if err := h.App.Repo.Insert(context.Background(), []usage.Row{old, recent}); err != nil {
		t.Fatal(err)
	}
	if rows := h.Rows(); len(rows) != 2 {
		t.Fatalf("retention off must keep every row, got %d", len(rows))
	}

	if v := h.UpdateSettings(map[string]string{"retention_days": "30"}); len(v) > 0 {
		t.Fatalf("settings: %v", v)
	}
	n, err := h.App.PruneNow()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pruned %d rows, want 1", n)
	}
	if rows := h.Rows(); len(rows) != 1 || rows[0].TokensIn != recent.TokensIn {
		t.Fatalf("rows after prune: %+v", rows)
	}
	if !strings.Contains(h.LogString(), "usage prune completed") ||
		!strings.Contains(h.LogString(), "removed=1") {
		t.Fatalf("prune count must be logged:\n%s", h.LogString())
	}
}

// --- local helpers --------------------------------------------------------

func itoa(n int) string { return strconv.Itoa(n) }

func nWithPrices(p *config.Prices) *config.Config {
	return &config.Config{Hosts: []config.Host{
		{ID: "m", HostAddresses: []string{"127.0.0.1"}, Servers: []config.Server{
			{ID: "llm", Port: 9, API: "ollama"},
		}},
	}, Prices: p}
}

func startApp(t *testing.T, dir string, sink *testutil.LogSink) *app.App {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))
	a, err := app.New(app.Options{
		Addr:       "127.0.0.1:0",
		ConfigPath: dir + "/elpulpo.yaml",
		DataDir:    dir + "/data",
	}, logger)
	if err != nil {
		t.Fatalf("app.New: %v\n%s", err, sink.String())
	}
	return a
}
