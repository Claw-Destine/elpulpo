package usage

import (
	"context"
	"database/sql"
)

// DebugCapture is one recorded request: the context the client sent and the
// response that came back (buffered body, or the raw SSE frames for a
// stream). Only written while debug mode is on.
type DebugCapture struct {
	ID           int64
	TsMs         int64
	HostID       string
	ServerID     string
	Model        string // exactly as the client asked
	Route        string // load-balancer alias, "" for a direct id
	Stream       bool
	Status       string
	HTTPStatus   int
	LatencyMs    int64
	TokensIn     int64
	TokensOut    int64
	RequestJSON  string
	ResponseJSON string // "" for streams or when no body was captured
	HasResponse  bool   // buffered response present (vs. SSE or none)
	ResponseSSE  string // raw stream frames, "" unless Stream
}

// DebugRow is one recording as the list shows it — metadata only, never the
// (potentially huge) context and response bodies.
type DebugRow struct {
	ID         int64  `json:"id"`
	TsMs       int64  `json:"timestamp_ms"`
	HostID     string `json:"host"`
	ServerID   string `json:"server"`
	Model      string `json:"model"`
	Route      string `json:"route"`
	Stream     bool   `json:"stream"`
	Status     string `json:"status"`
	HTTPStatus int    `json:"http_status"`
	LatencyMs  int64  `json:"latency_ms"`
	TokensIn   int64  `json:"tokens_in"`
	TokensOut  int64  `json:"tokens_out"`
	ReqBytes   int64  `json:"request_bytes"`
	RespBytes  int64  `json:"response_bytes"`
}

// InsertDebug stores one recording and returns its id.
func (r *Repo) InsertDebug(ctx context.Context, c *DebugCapture) (int64, error) {
	var respJSON any
	if c.HasResponse {
		respJSON = c.ResponseJSON
	}
	var respSSE any
	if c.Stream {
		respSSE = c.ResponseSSE
	}
	res, err := r.db.ExecContext(ctx, `INSERT INTO debug_requests (
		ts_ms, host_id, server_id, model, route, stream, status, http_status,
		latency_ms, tokens_in, tokens_out, request_json, response_json, response_sse
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.TsMs, c.HostID, c.ServerID, c.Model, c.Route, boolInt(c.Stream),
		c.Status, c.HTTPStatus, c.LatencyMs, c.TokensIn, c.TokensOut,
		c.RequestJSON, respJSON, respSSE)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const debugRowColumns = `id, ts_ms, host_id, server_id, model, route, stream,
	status, http_status, latency_ms, tokens_in, tokens_out,
	length(request_json),
	COALESCE(length(response_json), length(response_sse), 0)`

func scanDebugRow(scan func(dest ...any) error) (DebugRow, error) {
	var d DebugRow
	var stream int
	err := scan(&d.ID, &d.TsMs, &d.HostID, &d.ServerID, &d.Model, &d.Route,
		&stream, &d.Status, &d.HTTPStatus, &d.LatencyMs, &d.TokensIn, &d.TokensOut,
		&d.ReqBytes, &d.RespBytes)
	d.Stream = stream != 0
	return d, err
}

// DebugRows returns the newest recordings, newest first, without loading
// the captured bodies.
func (r *Repo) DebugRows(ctx context.Context, limit, offset int) ([]DebugRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+debugRowColumns+` FROM debug_requests ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DebugRow
	for rows.Next() {
		d, err := scanDebugRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DebugGet loads one recording with its captured bodies.
func (r *Repo) DebugGet(ctx context.Context, id int64) (*DebugCapture, error) {
	row := r.db.QueryRowContext(ctx, `SELECT id, ts_ms, host_id, server_id, model, route,
		stream, status, http_status, latency_ms, tokens_in, tokens_out,
		request_json, response_json, response_sse
		FROM debug_requests WHERE id = ?`, id)
	var c DebugCapture
	var stream int
	var respJSON, respSSE sql.NullString
	err := row.Scan(&c.ID, &c.TsMs, &c.HostID, &c.ServerID, &c.Model, &c.Route,
		&stream, &c.Status, &c.HTTPStatus, &c.LatencyMs, &c.TokensIn, &c.TokensOut,
		&c.RequestJSON, &respJSON, &respSSE)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.Stream = stream != 0
	if respJSON.Valid {
		c.ResponseJSON = respJSON.String
		c.HasResponse = true
	}
	if respSSE.Valid {
		c.ResponseSSE = respSSE.String
	}
	return &c, nil
}

// DebugCount returns how many recordings the table holds.
func (r *Repo) DebugCount(ctx context.Context) (int64, error) {
	var n int64
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM debug_requests`).Scan(&n)
	return n, err
}

// DebugClear deletes every recording and returns how many went away.
func (r *Repo) DebugClear(ctx context.Context) (int64, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM debug_requests`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DebugTrim keeps at most keep of the newest recordings and returns how many
// old ones it deleted. This is the spam guard: the table cannot grow without
// bound while debug mode is left on.
func (r *Repo) DebugTrim(ctx context.Context, keep int64) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM debug_requests WHERE id <= (SELECT MAX(id) FROM debug_requests) - ?`, keep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PruneDebug deletes recordings older than cutoff (unix ms UTC), in batches.
func (r *Repo) PruneDebug(ctx context.Context, cutoffMs int64) (int64, error) {
	var total int64
	for {
		res, err := r.db.ExecContext(ctx,
			`DELETE FROM debug_requests WHERE id IN (SELECT id FROM debug_requests WHERE ts_ms < ? LIMIT 5000)`, cutoffMs)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return total, nil
		}
		total += n
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
	}
}
