-- Debug recordings: one row per captured chat request, written only while
-- debug mode is on (off by default, so this table stays empty unless asked).
CREATE TABLE debug_requests (
  id            INTEGER PRIMARY KEY,
  ts_ms         INTEGER NOT NULL,             -- received, unix ms UTC
  host_id       TEXT NOT NULL DEFAULT '',     -- routing result ("" = not routed)
  server_id     TEXT NOT NULL DEFAULT '',
  model         TEXT NOT NULL,                -- model id exactly as the client asked
  route         TEXT NOT NULL DEFAULT '',     -- load-balancer alias ("" = direct id)
  stream        INTEGER NOT NULL DEFAULT 0,
  status        TEXT NOT NULL DEFAULT '',     -- same statuses as requests.status
  http_status   INTEGER NOT NULL DEFAULT 0,
  latency_ms    INTEGER NOT NULL DEFAULT 0,
  tokens_in     INTEGER NOT NULL DEFAULT 0,
  tokens_out    INTEGER NOT NULL DEFAULT 0,
  request_json  TEXT NOT NULL,                -- client request body, verbatim
  response_json TEXT,                         -- buffered response body (NULL for streams)
  response_sse  TEXT                          -- raw SSE frames (NULL for buffered)
);
CREATE INDEX ix_debug_ts ON debug_requests (ts_ms);
