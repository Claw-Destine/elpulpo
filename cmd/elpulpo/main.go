// Command elpulpo is the El Pulpo proxy: one listener for the
// OpenAI-compatible proxy and the dashboard.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"elpulpo/internal/app"
)

var version = "v1-dev"

func main() {
	var (
		healthFlag  = flag.Bool("health", false, "probe the running instance's /healthz and exit (container healthcheck)")
		checkFlag   = flag.Bool("check-config", false, "validate the configuration file and exit")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	cfgPath := envOr("ELPULPO_CONFIG", "./elpulpo.yaml")
	addr := envOr("ELPULPO_ADDR", ":8080")

	if *showVersion {
		fmt.Println("elpulpo", version)
		return
	}
	if *healthFlag {
		os.Exit(healthProbe(addr))
	}
	if *checkFlag {
		if err := app.CheckConfig(cfgPath); err != nil {
			fmt.Fprintln(os.Stderr, "elpulpo:", err)
			os.Exit(1)
		}
		return
	}

	lvl := app.LevelVar(envOr("ELPULPO_LOG_LEVEL", "info"))
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))

	opts := app.Options{
		Addr:              addr,
		ConfigPath:        cfgPath,
		DataDir:           envOr("ELPULPO_DATA_DIR", "./data"),
		ProxyToken:        os.Getenv("ELPULPO_PROXY_TOKEN"),
		DashboardUser:     envOr("ELPULPO_DASHBOARD_USER", "admin"),
		DashboardPassword: os.Getenv("ELPULPO_DASHBOARD_PASSWORD"),
		CataloguePath:     os.Getenv("ELPULPO_PRICE_CATALOGUE"),
	}

	instance, err := app.New(opts, log)
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := instance.Start(ctx); err != nil {
		log.Error("server stopped with an error", "err", err)
		os.Exit(1)
	}
	log.Info("shutdown complete")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// healthProbe implements the container healthcheck: the binary asks its own
// /healthz, so a shell-less image still gets healthchecks. It reports
// process liveness only — upstream state belongs to the Servers screen.
func healthProbe(addr string) int {
	url := "http://" + addr + "/healthz"
	if len(addr) > 0 && addr[0] == ':' {
		url = "http://127.0.0.1" + addr + "/healthz"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "health probe failed:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "health probe: status %d\n", resp.StatusCode)
		return 1
	}
	return 0
}
