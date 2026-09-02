CREATE TABLE requests (
  id               INTEGER PRIMARY KEY,
  ts_ms            INTEGER NOT NULL,          -- received, unix ms UTC
  host_id          TEXT NOT NULL,
  server_id        TEXT NOT NULL,
  model            TEXT NOT NULL,             -- published id
  endpoint         TEXT NOT NULL,
  status           TEXT NOT NULL,
  http_status      INTEGER NOT NULL,
  tokens_in        INTEGER NOT NULL,
  tokens_out       INTEGER NOT NULL,
  tokens_cached    INTEGER NOT NULL DEFAULT 0,
  tokens_reasoning INTEGER NOT NULL DEFAULT 0,
  estimated        INTEGER NOT NULL DEFAULT 0,
  latency_ms       INTEGER NOT NULL,
  ttft_ms          INTEGER
);
CREATE INDEX ix_ts ON requests (ts_ms);
CREATE INDEX ix_host_server_ts ON requests (host_id, server_id, ts_ms);
CREATE INDEX ix_model_ts ON requests (model, ts_ms);
CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
