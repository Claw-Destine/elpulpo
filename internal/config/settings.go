package config

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Settings are the global, dashboard-editable knobs. They survive a restart
// (stored in SQLite) but are deliberately not part of the config document.
type Settings struct {
	HealthInterval    time.Duration `json:"health_interval"`
	ProbeTimeout      time.Duration `json:"probe_timeout"`
	ConnectTimeout    time.Duration `json:"connect_timeout"`
	FirstByteTimeout  time.Duration `json:"first_byte_timeout"`
	StreamIdleTimeout time.Duration `json:"stream_idle_timeout"`
	TotalTimeout      time.Duration `json:"total_timeout"` // 0 == off
	MaxRequestSize    int64         `json:"max_request_size"`
	QueueTimeout      time.Duration `json:"queue_timeout"`
	RetentionDays     int           `json:"retention_days"` // 0 == off
}

// DefaultSettings holds the authoritative v1 defaults.
func DefaultSettings() Settings {
	return Settings{
		HealthInterval:    30 * time.Second,
		ProbeTimeout:      5 * time.Second,
		ConnectTimeout:    5 * time.Second,
		FirstByteTimeout:  60 * time.Second,
		StreamIdleTimeout: 120 * time.Second,
		TotalTimeout:      0,
		MaxRequestSize:    32 << 20,
		QueueTimeout:      60 * time.Second,
		RetentionDays:     0,
	}
}

// SettingKV persists setting key/value pairs (implemented by the usage DB).
type SettingKV interface {
	GetSetting(ctx context.Context, key string) (string, bool, error)
	SetSettings(ctx context.Context, kv map[string]string) error
}

// SettingsManager keeps the live settings, validates every write against the
// bounds table below, persists it and notifies listeners.
type SettingsManager struct {
	kv       SettingKV
	log      *slog.Logger
	cur      atomic.Pointer[Settings]
	gen      atomic.Int64
	onChange []func(*Settings)
}

func NewSettingsManager(kv SettingKV, log *slog.Logger) *SettingsManager {
	m := &SettingsManager{kv: kv, log: log}
	d := DefaultSettings()
	m.cur.Store(&d)
	return m
}

// OnChange registers a callback fired after every accepted update.
func (m *SettingsManager) OnChange(fn func(*Settings)) { m.onChange = append(m.onChange, fn) }

// Get returns the live settings snapshot.
func (m *SettingsManager) Get() *Settings { return m.cur.Load() }

// Gen bumps whenever a timing-related setting changes, so caches keyed on
// transport parameters (dial timeouts, response-header timeouts) can drop.
func (m *SettingsManager) Gen() int64 { return m.gen.Load() }

var settingKeys = []string{
	"health_interval", "probe_timeout", "connect_timeout", "first_byte_timeout",
	"stream_idle_timeout", "total_timeout", "max_request_size", "queue_timeout",
	"retention_days",
}

// Load reads persisted values over the defaults.
func (m *SettingsManager) Load(ctx context.Context) error {
	s := DefaultSettings()
	for _, k := range settingKeys {
		v, ok, err := m.kv.GetSetting(ctx, k)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if viol := applySettingField(&s, k, v); viol != nil {
			m.log.Warn("ignoring persisted setting that no longer validates", "key", k, "value", v, "err", viol.Msg)
		}
	}
	m.cur.Store(&s)
	m.gen.Add(1)
	return nil
}

// Update validates a patch (field -> raw string), persists it and swaps it
// in. A field outside its range is rejected with a field-level error naming
// the allowed range; the live value stays in force.
func (m *SettingsManager) Update(ctx context.Context, patch map[string]string) (*Settings, []Violation) {
	cur := *m.cur.Load()
	candidate := cur
	var violations []Violation
	written := map[string]string{}
	for _, k := range settingKeys {
		raw, ok := patch[k]
		if !ok || strings.TrimSpace(raw) == "" {
			continue
		}
		if viol := applySettingField(&candidate, k, raw); viol != nil {
			violations = append(violations, *viol)
			continue
		}
		written[k] = raw
	}
	// Also reject unknown fields so typos never silently do nothing.
	for k := range patch {
		known := false
		for _, sk := range settingKeys {
			if sk == k {
				known = true
			}
		}
		if !known {
			violations = append(violations, Violation{Path: k, Msg: "unknown setting"})
		}
	}
	if len(violations) > 0 {
		return &cur, violations
	}
	if err := m.kv.SetSettings(ctx, written); err != nil {
		return &cur, []Violation{{Path: "$", Msg: "could not persist settings: " + err.Error()}}
	}
	changed := candidate != cur
	m.cur.Store(&candidate)
	if changed {
		m.gen.Add(1)
		for _, fn := range m.onChange {
			fn(&candidate)
		}
	}
	return &candidate, nil
}

var sizeRe = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*(B|KB|MB|GB|KiB|MiB|GiB)?$`)

// parseSize accepts a plain byte count or a suffixed size (32MiB, 512KB).
func parseSize(raw string) (int64, error) {
	m := sizeRe.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return 0, fmt.Errorf("not a size: %q", raw)
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, err
	}
	mult := int64(1)
	switch m[2] {
	case "", "B":
	case "KB":
		mult = 1000
	case "MB":
		mult = 1000 * 1000
	case "GB":
		mult = 1000 * 1000 * 1000
	case "KiB":
		mult = 1 << 10
	case "MiB":
		mult = 1 << 20
	case "GiB":
		mult = 1 << 30
	}
	return int64(n * float64(mult)), nil
}

// parseDur accepts "90" (seconds) or a Go duration string ("30s", "5m").
func parseDur(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("empty")
	}
	if n, err := strconv.ParseFloat(raw, 64); err == nil {
		return time.Duration(n * float64(time.Second)), nil
	}
	return time.ParseDuration(raw)
}

func outOfRange(field string, v time.Duration, min, max time.Duration) *Violation {
	return &Violation{Path: field, Msg: fmt.Sprintf("value %s is out of the allowed range %.0fs to %.0fs (0 = off where allowed)", v, min.Seconds(), max.Seconds())}
}

// applySettingField decodes and range-checks one field into dst. Returns a
// violation instead of applying when the value is outside the bounds.
func applySettingField(dst *Settings, key, raw string) *Violation {
	raw = strings.TrimSpace(raw)
	switch key {
	case "health_interval":
		v, err := parseDur(raw)
		if err != nil {
			return &Violation{Path: key, Msg: fmt.Sprintf("not a duration: %q (allowed 5s to 3600s)", raw)}
		}
		if v < 5*time.Second || v > 3600*time.Second {
			return outOfRange(key, v, 5*time.Second, 3600*time.Second)
		}
		dst.HealthInterval = v
	case "probe_timeout":
		v, err := parseDur(raw)
		if err != nil {
			return &Violation{Path: key, Msg: fmt.Sprintf("not a duration: %q (allowed 1s to 300s)", raw)}
		}
		if v < time.Second || v > 300*time.Second {
			return outOfRange(key, v, time.Second, 300*time.Second)
		}
		dst.ProbeTimeout = v
	case "connect_timeout":
		v, err := parseDur(raw)
		if err != nil {
			return &Violation{Path: key, Msg: fmt.Sprintf("not a duration: %q (allowed 1s to 300s)", raw)}
		}
		if v < time.Second || v > 300*time.Second {
			return outOfRange(key, v, time.Second, 300*time.Second)
		}
		dst.ConnectTimeout = v
	case "first_byte_timeout":
		v, err := parseDur(raw)
		if err != nil {
			return &Violation{Path: key, Msg: fmt.Sprintf("not a duration: %q (allowed 1s to 3600s)", raw)}
		}
		if v < time.Second || v > 3600*time.Second {
			return outOfRange(key, v, time.Second, 3600*time.Second)
		}
		dst.FirstByteTimeout = v
	case "stream_idle_timeout":
		v, err := parseDur(raw)
		if err != nil {
			return &Violation{Path: key, Msg: fmt.Sprintf("not a duration: %q (allowed 1s to 3600s)", raw)}
		}
		if v < time.Second || v > 3600*time.Second {
			return outOfRange(key, v, time.Second, 3600*time.Second)
		}
		dst.StreamIdleTimeout = v
	case "total_timeout":
		if raw == "0" || raw == "off" {
			dst.TotalTimeout = 0
			return nil
		}
		v, err := parseDur(raw)
		if err != nil {
			return &Violation{Path: key, Msg: fmt.Sprintf("not a duration: %q (allowed off, or 1s to 86400s)", raw)}
		}
		if v != 0 && (v < time.Second || v > 86400*time.Second) {
			return &Violation{Path: key, Msg: fmt.Sprintf("value %s is out of the allowed range (off, or 1s to 86400s)", v)}
		}
		dst.TotalTimeout = v
	case "max_request_size":
		v, err := parseSize(raw)
		if err != nil {
			return &Violation{Path: key, Msg: fmt.Sprintf("not a size: %q (allowed 1KiB to 1GiB)", raw)}
		}
		if v < 1<<10 || v > 1<<30 {
			return &Violation{Path: key, Msg: fmt.Sprintf("value %d is out of the allowed range 1KiB to 1GiB", v)}
		}
		dst.MaxRequestSize = v
	case "queue_timeout":
		v, err := parseDur(raw)
		if err != nil {
			return &Violation{Path: key, Msg: fmt.Sprintf("not a duration: %q (allowed 1s to 3600s)", raw)}
		}
		if v < time.Second || v > 3600*time.Second {
			return outOfRange(key, v, time.Second, 3600*time.Second)
		}
		dst.QueueTimeout = v
	case "retention_days":
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return &Violation{Path: key, Msg: fmt.Sprintf("retention_days must be 0 (off) or a positive integer, got %q", raw)}
		}
		dst.RetentionDays = n
	default:
		return &Violation{Path: key, Msg: "unknown setting"}
	}
	return nil
}
