// Package usage is the request-usage store: one row per routed request,
// plus the settings key/value table. It is the only package that speaks SQL.
package usage

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

// Row is one usage record. Timestamps are unix milliseconds UTC.
type Row struct {
	ID              int64  `json:"id,omitempty"`
	TsMs            int64  `json:"timestamp_ms"`
	HostID          string `json:"host"`
	ServerID        string `json:"server"`
	Model           string `json:"model"` // published id
	Endpoint        string `json:"endpoint"`
	Status          string `json:"status"`
	HTTPStatus      int    `json:"http_status"`
	TokensIn        int64  `json:"tokens_in"`
	TokensOut       int64  `json:"tokens_out"`
	TokensCached    int64  `json:"tokens_cached"`
	TokensReasoning int64  `json:"tokens_reasoning"`
	Estimated       bool   `json:"estimated"`
	LatencyMs       int64  `json:"latency_ms"`
	TTFTMs          *int64 `json:"ttft_ms"`
}

// Row statuses.
const (
	StatusOK              = "ok"
	StatusUpstreamError   = "upstream_error"
	StatusUpstreamTimeout = "upstream_timeout"
	StatusCancelled       = "cancelled"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Repo is the SQLite-backed usage repository.
type Repo struct {
	db *sql.DB
}

// Open prepares the database at path with WAL and runs migrations.
func Open(path string) (*Repo, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.Clean(path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// modernc/sqlite tolerates one writer plus concurrent readers; keep the
	// pool small so writer batching gets the connection it needs.
	db.SetMaxOpenConns(8)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	r := &Repo{db: db}
	if err := r.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return r, nil
}

// DB exposes the pool for shutdown checkpointing.
func (r *Repo) DB() *sql.DB { return r.db }

func (r *Repo) migrate() error {
	if _, err := r.db.Exec(`CREATE TABLE IF NOT EXISTS meta (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	cur := 0
	row := r.db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM meta`)
	if err := row.Scan(&cur); err != nil {
		return err
	}
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		num, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: bad number", name)
		}
		if num <= cur {
			continue
		}
		stmt, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := r.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(stmt)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO meta (version) VALUES (?)`, num); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Insert stores one row (used by the writer and by tests).
func (r *Repo) Insert(ctx context.Context, rows []Row) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO requests (
		ts_ms, host_id, server_id, model, endpoint, status, http_status,
		tokens_in, tokens_out, tokens_cached, tokens_reasoning, estimated, latency_ms, ttft_ms
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, row := range rows {
		if _, err := stmt.ExecContext(ctx, row.TsMs, row.HostID, row.ServerID, row.Model,
			row.Endpoint, row.Status, row.HTTPStatus, row.TokensIn, row.TokensOut,
			row.TokensCached, row.TokensReasoning, boolInt(row.Estimated), row.LatencyMs, row.TTFTMs); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// GetSetting reads one settings row.
func (r *Repo) GetSetting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := r.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// SetSettings upserts settings rows.
func (r *Repo) SetSettings(ctx context.Context, kv map[string]string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range kv {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO settings (key, value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Close checkpoints WAL and closes the pool.
func (r *Repo) Close() error {
	r.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return r.db.Close()
}
