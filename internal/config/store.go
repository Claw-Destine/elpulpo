package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Snapshot is an immutable view of the live configuration. Requests already
// in flight keep the snapshot they started with.
type Snapshot struct {
	Config   *Config
	FileHash string // sha256 (hex) of the file contents this came from; "" if no file
	Source   string // "startup" | "dashboard" | "file"
	Applied  time.Time
}

// Store owns the configuration file: load, validate, canonical save and the
// poll-based watcher that honours hand edits. One mutex serialises saves —
// there is only ever one writer.
type Store struct {
	path string
	log  *slog.Logger

	mu        sync.Mutex // serialises saves and applies
	cur       *Snapshot
	fileHash  string // sha256 of the file contents last observed on disk ("" if none)
	lastErr   string
	attempted string // hash of contents whose load failed; retried only when it changes

	onApplyMu sync.Mutex
	onApply   []func(*Snapshot)
}

// NewStore returns a store for the given path; nothing is read until Load.
func NewStore(path string, log *slog.Logger) *Store {
	return &Store{path: path, log: log, cur: &Snapshot{Config: &Config{}, Source: "startup"}}
}

// SetOnApply registers callbacks fired after every successful apply.
func (s *Store) SetOnApply(fn func(*Snapshot)) {
	s.onApplyMu.Lock()
	s.onApply = append(s.onApply, fn)
	s.onApplyMu.Unlock()
}

// Current returns the live snapshot.
func (s *Store) Current() *Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

// LastErr is the most recent failed load/import message, shown on the
// dashboard; "" when the file is healthy.
func (s *Store) LastErr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// Path is the configuration file path.
func (s *Store) Path() string { return s.path }

func hashBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (s *Store) read() ([]byte, error) {
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return b, err
}

// Load reads the file at startup. A missing file is an empty configuration;
// an invalid one starts El Pulpo with an empty config and records the error
// for the dashboard (the file itself is never touched).
func (s *Store) Load() error {
	b, err := s.read()
	if err != nil {
		return fmt.Errorf("read config %s: %w", s.path, err)
	}
	if b == nil {
		s.mu.Lock()
		s.fileHash = ""
		s.mu.Unlock()
		s.apply(&Snapshot{Config: &Config{}, FileHash: "", Source: "startup", Applied: time.Now()})
		s.log.Info("no config file yet, starting with empty configuration", "path", s.path)
		return nil
	}
	cfg, violations := ParseAndValidate(b)
	s.mu.Lock()
	s.fileHash = hashBytes(b)
	s.mu.Unlock()
	if len(violations) > 0 {
		s.mu.Lock()
		s.lastErr = (&ValidationError{Violations: violations}).Error()
		s.mu.Unlock()
		s.log.Error("config file is invalid, starting with an empty configuration; fix the file or save from the dashboard",
			"path", s.path, "errors", s.lastErr)
		s.apply(&Snapshot{Config: &Config{}, FileHash: hashBytes(b), Source: "startup", Applied: time.Now()})
		return nil
	}
	Normalize(cfg)
	s.apply(&Snapshot{Config: cfg, FileHash: hashBytes(b), Source: "startup", Applied: time.Now()})
	s.log.Info("configuration loaded", "path", s.path, "hosts", len(cfg.Hosts), "routes", len(cfg.Routes()))
	return nil
}

// swapHash refreshes the hash the dashboard's forms carry, without
// touching the live configuration.
func (s *Store) swapHash(h string) {
	s.mu.Lock()
	s.cur = &Snapshot{Config: s.cur.Config, FileHash: h, Source: s.cur.Source, Applied: s.cur.Applied}
	s.mu.Unlock()
}

func (s *Store) apply(snap *Snapshot) {
	s.mu.Lock()
	s.cur = snap
	if snap.Source == "file" || snap.Source == "dashboard" || snap.Source == "startup" {
		s.lastErr = ""
	}
	s.mu.Unlock()
	s.onApplyMu.Lock()
	fns := s.onApply
	s.onApplyMu.Unlock()
	for _, fn := range fns {
		fn(snap)
	}
}

// Save validates cfg, writes it canonically (temp file, chmod 0600, fsync,
// rename) and applies it. It refuses to run when expectHash does not match
// the live configuration's source hash — a hand edit landed after the form
// was rendered.
func (s *Store) Save(cfg *Config, expectHash string) (*Snapshot, error) {
	s.mu.Lock()
	if expectHash != s.fileHash {
		s.mu.Unlock()
		return nil, ErrStaleSave
	}
	canonical := Canonical(cfg)
	if _, violations := ParseAndValidate(canonical); len(violations) > 0 {
		s.mu.Unlock()
		return nil, &ValidationError{Violations: violations}
	}
	if err := s.writeLocked(canonical); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	h := hashBytes(canonical)
	snap := &Snapshot{Config: cfg, FileHash: h, Source: "dashboard", Applied: time.Now()}
	s.cur = snap
	s.fileHash = h
	s.lastErr = ""
	s.attempted = ""
	s.mu.Unlock()
	s.onApplyMu.Lock()
	fns := s.onApply
	s.onApplyMu.Unlock()
	for _, fn := range fns {
		fn(snap)
	}
	return snap, nil
}

func (s *Store) writeLocked(data []byte) error {
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".elpulpo-*.yaml") // created 0600
	if err != nil {
		return fmt.Errorf("config %s is not writable: %w", s.path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("config %s is not writable: %w", s.path, err)
	}
	_ = tmp.Chmod(0o600)
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("fsync config %s: %w", s.path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write config %s: %w", s.path, err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("config %s is not writable: %w", s.path, err)
	}
	if d, derr := os.Open(dir); derr == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// ValidateForImport parses and validates import data without applying it.
func (s *Store) ValidateForImport(data []byte) (*Config, []Violation) {
	return ParseAndValidate(data)
}

// Watch polls the file once a second, comparing a sha256 of its contents;
// a changed file is parsed, validated and applied within the tick. A failed
// validation keeps the live configuration and leaves the file untouched.
// Own saves update the known hash before the watcher looks again, so a save
// is never reported back as a hand edit.
func (s *Store) Watch(stop <-chan struct{}) {
	t := time.NewTicker(time.Second)
	go func() {
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				s.poll()
			}
		}
	}()
}

func (s *Store) poll() {
	b, err := s.read()
	if err != nil {
		s.log.Error("config poll failed to read file", "path", s.path, "err", err)
		return
	}
	h := ""
	if b != nil {
		h = hashBytes(b)
	}
	s.mu.Lock()
	known := s.fileHash
	attempted := s.attempted
	if h == known {
		s.attempted = ""
		s.mu.Unlock()
		return
	}
	// The file changed: from here on, the hash forms are the disk's.
	s.fileHash = h
	s.mu.Unlock()
	if h == attempted {
		s.swapHash(h)
		return // still broken in the same way; do not log every second
	}
	var cfg *Config
	var violations []Violation
	if b == nil {
		cfg = &Config{}
	} else {
		cfg, violations = ParseAndValidate(b)
	}
	if len(violations) > 0 {
		msg := (&ValidationError{Violations: violations}).Error()
		s.mu.Lock()
		s.attempted = h
		s.lastErr = msg
		s.mu.Unlock()
		s.swapHash(h)
		s.log.Error("hand edit of the config file is invalid; live configuration unchanged, file left untouched",
			"path", s.path, "errors", msg)
		return
	}
	Normalize(cfg)
	s.apply(&Snapshot{Config: cfg, FileHash: h, Source: "file", Applied: time.Now()})
	s.log.Info("configuration reloaded from hand edit", "path", s.path, "hosts", len(cfg.Hosts), "routes", len(cfg.Routes()))
}
