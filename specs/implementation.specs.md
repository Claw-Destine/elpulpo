# El Pulpo implementation specs

Everything here serves [functional.specs.md](functional.specs.md); where the two disagree the
functional spec wins and this file is wrong.

## Decisions

| topic | choice | why it wins here |
| --- | --- | --- |
| backend | **Go**, latest stable (only stdlib features available since 1.22) | `FlushInterval: -1` gives an unbuffered SSE pass-through; one goroutine per long-lived stream; channels make the per-server queue trivial; `go:embed` carries the price catalogue; `CGO_ENABLED=0` yields the single static binary and a shell-less image |
| config store | **one YAML file** (`ELPULPO_CONFIG`) | the file is the store *and* the export format, so byte-stable export (scenario 24) is structural rather than a serialization exercise; hand-editable and diffable |
| usage store | **embedded SQLite**, CGO-free (`modernc.org/sqlite`), WAL | insert-per-request with concurrent dashboard readers, no server dependency, prune is one statement; Postgres stays possible behind the repository boundary below, if it is ever wanted |
| frontend | **SSR from the Go binary** (`templ` + htmx + no-build CSS) | five screens of tables and forms; no node build, no asset pipeline, no CORS, one Basic-auth middleware covers pages and fragments alike |
| settings store | SQLite `settings` table | global settings are explicitly *not* in the config document but must survive a restart |

No web framework, no ORM, no code generator beyond `templ`. `database/sql` with hand-written SQL.

## Dependencies

| import | used for |
| --- | --- |
| `net/http`, `net/http/httputil` | listener, reverse proxy |
| `log/slog` | structured logging, `ELPULPO_LOG_LEVEL` |
| `gopkg.in/yaml.v3` | config decode/encode; `yaml.Node` line numbers for validation errors |
| `modernc.org/sqlite` | usage store and settings, no CGO |
| `a-h/templ`, vendored `htmx.min.js`, one small CSS | dashboard |
| `database/sql`, `crypto/subtle`, `context`, `time` | plumbing |

## Layout

```
cmd/elpulpo/            main: flags, env, wiring, signals
internal/config/        config document: load, validate, canonical save, file watch
internal/proxy/         routing, rewrite, SSE pass-through, concurrency slot
internal/health/        probe loop, address election, server state
internal/usage/         repository, writer queue, queries, prune, CSV
internal/catalog/       embedded price catalogue + file override
internal/dashboard/     templ pages, htmx fragments, exports
```

One seam worth keeping honest: `usage.Repo` is the only thing that speaks SQL. A Postgres driver stays
additive as long as nothing above that interface emits SQL.

## Configuration file

- **Path** `ELPULPO_CONFIG` (default `./elpulpo.yaml`). A missing file is an empty configuration; the
  first save creates it.
- **Canonical write.** Struct-driven marshal (never a `map[string]any`, whose key order drifts),
  4-space indent, defaults materialised (`max_concurrency: 0`, `scheme: http` written out), hosts
  sorted by `id`, servers by `id`, price entries by `model`. Then: temp file in the same directory →
  `chmod 0600` → `fsync` → `os.Rename`. A crash mid-save leaves either the old file or the new one.
- **Own writes are not external edits.** The known hash is updated before the watcher looks again, so
  a save is never reported back as a hand edit.
- **Watch** by polling `stat` every second and comparing a sha256 of the contents — no `fsnotify`
  dependency, and identical behaviour on bind mounts and overlay filesystems where inotify is
  unreliable. On change: parse → validate → apply (the same code path as a dashboard save, so
  in-flight requests are untouched) and log `INFO`; on failure keep the live config, log `ERROR`, show
  the error on the dashboard, never touch the file.
- **Stale-save guard.** Each rendered form carries the config hash it came from; a save whose hash
  differs from the file's current hash is refused before validation (scenario 40). One mutex
  serialises saves — there is only ever one writer.
- **Validation errors** quote the path *and* the line from `yaml.Node` (`hosts[1].servers[0].api`,
  line 23), all violations in one pass rather than one at a time.

## Request path

1. `http.MaxBytesReader` at `max_request_size` — `413` before any parsing or routing, so no usage row
   and no concurrency slot (scenario 29).
2. The body must parse as a JSON object; anything else is `400` before routing. Managed fields
   (`model`, `stream`, `stream_options`) are lifted into a struct and the remainder kept as
   `map[string]json.RawMessage`, re-emitted verbatim — tool schemas, multimodal payloads and any
   future OpenAI field survive untouched.
3. Route on exact published id from the catalogue the health loop maintains; otherwise
   `404 model_not_found` with the matching `reason`.
4. Rewrite `model` to the base model name; set `stream_options.include_usage` only when
   `stream: true` and the client omitted it.
5. **Concurrency slot**: `select` over the per-server semaphore, `queue_timeout` and
   `r.Context().Done()`; a server that went down while queued yields `503`. The slot is released by a
   `defer` spanning the whole streamed body, which is what makes scenario 8 pass.
6. The response `model` is rewritten to the published id — in the JSON body and in every SSE `data:`
   frame containing `"model"`. Frames are read line-delimited; `event:`, comment lines and
   `data: [DONE]` pass through untouched; each frame is flushed as written. Nothing is buffered and
   the parse cost is confined to frames carrying that key.
7. Usage comes from the last usage-bearing frame or body; absent usage means an estimate plus the
   `estimated` flag. `ttft_ms` is recorded at the first frame written.
8. The row is handed to the writer queue, not to SQLite, from the request goroutine.

A per-server `http.Transport` keeps the idle pool and error surface from being shared across servers,
with `DisableCompression: true` so bytes arrive as the upstream sent them.

### Timeouts, per knob

| functional knob | mechanism |
| --- | --- |
| `connect_timeout` | `DialContext` timeout on the transport |
| `first_byte_timeout` | `Transport.ResponseHeaderTimeout` |
| `stream_idle_timeout` | `http.ResponseController.SetReadDeadline(now + idle)` before each read of the stream, reset on success |
| `total_timeout` | optional `context.WithTimeout` on the upstream request; off by default |
| `probe_timeout` | a separate `http.Client` for probes, so probe latency never consumes request budget |
| client disconnect | the handler context is the parent; cancellation reaches the upstream body and releases the slot |

## Health and address election

One goroutine per server on a `time.Ticker(health_interval)`, re-created when the setting changes.
Server state — up/down, consecutive failures, `active_address`, last error, model list — lives in a
mutex-guarded struct read by both the router and the dashboard. Election follows
[Address selection](functional.specs.md#address-selection) exactly: probe the active address, on failure walk
`host_addresses` from index 0, three consecutive all-fail rounds mark down, no fail-back, reset on a
config change. The catalogue behind `/v1/models` is rebuilt from these states, which is why a server
going down withdraws its models within one interval without extra machinery.

## Usage store

`ELPULPO_DATA_DIR/usage.db` with `journal_mode=WAL`, `synchronous=NORMAL`, `busy_timeout=5000`.

```sql
CREATE TABLE requests (
  id               INTEGER PRIMARY KEY,
  ts_ms            INTEGER NOT NULL,          -- received, unix ms UTC
  host_id          TEXT NOT NULL,
  server_id        TEXT NOT NULL,
  address          TEXT NOT NULL,             -- which address served it
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
CREATE TABLE meta (version INTEGER NOT NULL);
```

- **Write path.** The request goroutine pushes to a buffered channel (1024); one writer commits in
  batches of 64 rows or 50 ms. The proxy path never waits on the database: when the channel is full
  the row is dropped with an `ERROR` line, which beats stalling a response. The channel is drained
  during shutdown grace. Rows for in-flight requests die with a hard kill — accepted, and stated in
  the README rather than papered over.
- **Percentiles.** Exact, through `ORDER BY latency_ms LIMIT 1 OFFSET n/2` (and `OFFSET 19n/20`) over
  the filtered set. No SQLite percentile extension and no reading a whole period into Go; if a period
  ever outgrows that, `retention_days` is the intended lever.
- **Prune.** Batched `DELETE ... WHERE id IN (SELECT id ... WHERE ts_ms < ? LIMIT 5000)` in a loop, so
  the daily prune never holds one long write lock. Count logged. Runs at startup and on a daily tick.
- **CSV.** Streamed `text/csv` with `Content-Disposition`, quoted per RFC 4180, written from inside
  the `rows.Scan` loop — no materialisation.
- **Migrations.** Numbered SQL files under `go:embed`, applied in order in a transaction, version in
  `meta`. No migration framework.
- **If Postgres ever comes.** It stays a second `Repo` implementation provided: nothing outside
  `internal/usage` writes SQL, only `id` is referenced above that boundary, time math stays in Go on
  `ts_ms` integers, and no SQLite-only function is used. The batched-delete query is the one place
  that knows SQLite syntax and it is one function.

## Dashboard

`templ` pages under `/dashboard/*`, htmx fragments under `/dashboard/part/*`, mutations as
`POST /dashboard/action/*`, exports at `/dashboard/export/{rows,summary}.csv`.

- `CSP: default-src 'self'` — htmx drives behaviour through `hx-` attributes, so `unsafe-inline`
  appears nowhere.
- **CSRF.** Basic auth is sent by the browser without a consent step, so mutating routes additionally
  require a header htmx sets globally plus a double-submit token in a `SameSite=Strict` cookie. No
  state ever changes on a `GET` (scenario 41).
- Credentials compared with `crypto/subtle.ConstantTimeCompare`; tokens and `auth_token` values never
  reach the logs.
- The Servers screen refreshes by htmx polling every 5s, the stats table on the same mechanism. No
  websocket: one connection model is worth more than the latency saved, and nobody leaves this
  dashboard open for hours.

## Runtime and deployment

| env | default | effect |
| --- | --- | --- |
| `ELPULPO_ADDR` | `:8080` | single listener: proxy + dashboard |
| `ELPULPO_CONFIG` | `./elpulpo.yaml` | config store; must be writable for saves |
| `ELPULPO_DATA_DIR` | `./data` | `usage.db`; must be writable |
| `ELPULPO_LOG_LEVEL` | `info` | slog level |
| `ELPULPO_PROXY_TOKEN` | empty | empty = open proxy + `WARN` + banner |
| `ELPULPO_DASHBOARD_USER` | `admin` | used only when a password is set |
| `ELPULPO_DASHBOARD_PASSWORD` | empty | empty = open dashboard + `WARN` + banner |
| `ELPULPO_PRICE_CATALOGUE` | embedded | path override |

- **Build** `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w"`.
- **Image** multi-stage, final `gcr.io/distroless/static-debian12:nonroot`, non-root, no shell.
  `HEALTHCHECK CMD ["/elpulpo","--health"]` — the binary probes its own `/healthz`, so a shell-less
  image still gets healthchecks. `/healthz` reports process liveness only; upstream state belongs to
  the Servers screen, never to a container healthcheck.
- **Shutdown** on `SIGTERM`/`SIGINT`: stop listening, let in-flight requests finish within the 30s
  grace, cancel streams, drain the usage channel inside that window, checkpoint and close. A prune
  already running is not restarted.
- A read-only config mount is a supported read-only mode: reloads and hand edits keep working, saves
  fail with an error naming the path.

## Testing

Each acceptance scenario is one named test — `TestAcceptance_16_AddressFallback` and so on — driving
the real listener against fake upstreams built on `httptest`:

- Ollama-shaped (`/api/tags` + `/v1/chat/completions`) and OpenAI-shaped (`/v1/models`) fakes with
  knobs for a fixed model list, first-byte delay, mid-stream stall, missing `usage`, `500`, and
  held-open streams for disconnects.
- Two-address tests bind `127.0.0.1` and `127.0.0.2` — the whole `/8` is local on Linux, so
  `host_addresses` failover and stickiness (16–20) are exercised for real rather than mocked.
- Timeout scenarios (30, 31) run with injected sub-second settings, so the suite stays fast.
- Golden files pin the canonical config export (24) and the validation error text (11, 17, 25); CSV
  round-trip tests re-parse the export and recompute the totals (13, 32).
- `go test -race` throughout. A real-Ollama smoke test sits behind `-tags live` and is not in the
  default suite: determinism earns more than realism here.

## Deliberately absent

Web framework, ORM, template engine beyond `templ`, plugin system, Prometheus endpoint, clustering,
encryption of the config at rest, and any outbound HTTP call. Each is a separate decision later;
paying for them now would buy nothing the functional spec asks for.

## Deliverable note

`README.md` must cover: quick start for binary and Docker, the env table above, the config file
reference including what a dashboard save does to hand-written comments, and how to back up —
`VACUUM INTO` for the database, a plain copy of the YAML taken after a save.
