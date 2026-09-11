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
internal/balancer/      in-flight accounting and the route pick (cost-weighted least load)
internal/proxy/         routing, rewrite, SSE pass-through, concurrency slot
internal/health/        probe loop, address election, server state
internal/usage/         repository, writer queue, queries, prune, CSV
internal/catalog/       embedded price catalogue + file override
internal/dashboard/     templ pages, htmx fragments, exports
```

One seam worth keeping honest: `usage.Repo` is the only thing that speaks SQL. A Postgres driver stays
additive as long as nothing above that interface emits SQL.

The other seam is `balancer.Selector`: it reads the route catalogue `health` maintains and the
in-flight registry, and answers one question — which member of this alias takes the next request. The
proxy asks it, the dashboard renders what it says, and neither reimplements the policy.

## Configuration file

- **Path** `ELPULPO_CONFIG` (default `./elpulpo.yaml`). A missing file is an empty configuration; the
  first save creates it.
- **Canonical write.** Struct-driven marshal (never a `map[string]any`, whose key order drifts),
  4-space indent, defaults materialised (`max_concurrency: 0`, `scheme: http`, a route member's
  `cost: 1` written out), hosts sorted by `id`, servers by `id`, price entries by `model`, routes by
  `alias` — but **route members keep their order**, because it is the preference order the balancer
  reads. Then: temp file in the same directory → `chmod 0600` → `fsync` → `os.Rename`. A crash
  mid-save leaves either the old file or the new one.
- **Own writes are not external edits.** The known hash is updated before the watcher looks again, so
  a save is never reported back as a hand edit.
- **An empty `prices` or `loadbalancer` section is omitted**, not written as `prices: {}`, so a
  hosts-only config stays readable (scenario 45).
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
3. Resolve the asked name: exact published id from the catalogue the health loop maintains, or — when
   the name is a route alias of the live config document — the member `balancer.Selector` picks.
   Otherwise `404 model_not_found` with the matching `reason` (`not_configured` for a name that is
   neither, `not_available` for a known published id behind a dark server or an alias whose every
   member is down). The published-id lookup goes first: a published id always carries `@` and an
   alias never does, so the order needs no arbitration.
3b. The chosen published id's in-flight counter is raised **at admission**, before the concurrency
   slot, and released by the `defer` that spans the request: the next concurrent request must see
   this one, or a burst would pile every request onto the same member.
4. Rewrite `model` to the base model name; set `stream_options.include_usage` only when
   `stream: true` and the client omitted it. The outgoing `Authorization` header is the server's
   `auth_token`, never the client's proxy token — the inbound header is dropped before forwarding,
   which is what scenario 43 asserts.
5. **Concurrency slot**: `select` over the per-server semaphore, `queue_timeout` and
   `r.Context().Done()`; a server that went down while queued yields `503`. The slot is released by a
   `defer` spanning the whole streamed body, which is what makes scenario 8 pass.
6. The response `model` is rewritten to the name the client asked for — its published id, or the
   alias it balanced through — in the JSON body and in every SSE `data:` frame containing `"model"`.
   Frames are read line-delimited; `event:`, comment lines and
   `data: [DONE]` pass through untouched; each frame is flushed as written. Nothing is buffered and
   the parse cost is confined to frames carrying that key.
7. Usage comes from the last usage-bearing frame or body; absent usage means an estimate plus the
   `estimated` flag. `ttft_ms` is recorded at the first frame written.
8. The row is handed to the writer queue, not to SQLite, from the request goroutine.
9. The same terminal point logs exactly one `chat request` line: the row's host/server/model/status
   plus latency, `wait_ms` (time queued for a slot), token counts, `estimated`, and `ttft_ms` on
   streams; a request that came through a route adds `route=<alias>`, so the row and the log name the
   member that served and the name it was asked for. `ERROR` on `upstream_error`/`upstream_timeout`,
   `INFO` otherwise. A request turned away
   before routing logs the same `msg` at `WARN` with `status=rejected` and a `reason` and writes no
   row (a 5xx among them is `ERROR` — that one is our failure), so one line always equals one
   row-or-rejection. `DEBUG` adds the routing decision (id → host/server/address, base name sent
   upstream; for an alias, the alias and the member it chose), the upstream's own response status,
   and each probe with its model count.

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

**Upstream TLS.** `scheme: https` builds a transport with `TLSClientConfig.InsecureSkipVerify: true`
— the functional spec's v1 choice, so the field name says what it does and `gosec` is silenced with a
comment rather than a lint waiver nobody reads. No verification, no pinning, no CA field.

**Settings bounds** live in one table in `internal/config`, validated before applying: `health_interval`
5s–3600s, `probe_timeout`/`connect_timeout` 1s–300s, `first_byte_timeout`/`stream_idle_timeout`/
`queue_timeout` 1s–3600s, `total_timeout` off or 1s–86400s, `max_request_size` 1 KiB–1 GiB,
`retention_days` 0 or positive. Out of range is a field error with the range echoed (scenario 46), and
the value in force never changes.

**Time.** Period boundaries are computed with `time.Local`, so the container's `TZ` decides what
"today" means; rows and CSV stay UTC throughout. Nothing in the schema stores an offset.

**CORS.** One middleware on the `/v1` mux: `OPTIONS` answers `204` with the allow headers and no auth
check, every other `/v1` response carries `Access-Control-Allow-Origin: *`. It is attached to that mux
only — the dashboard and `/api/*` muxes never see it, so the absence of CORS headers on the dashboard
(scenario 44) is structural rather than a configuration choice.

## Load balancing

Two pieces, both small enough to read at once:

- **`balancer.Registry`** — `map[published model id]int64` under a mutex, `Add(id, ±1)` and
  `InFlight(id)`. An id at zero is deleted rather than kept, so the map holds only what is busy and
  `Counts()` is cheap for the dashboard and `/api/loadbalancer`.
- **`balancer.Selector`** — compiles one configured route against live state: for each member, its
  cost, the registry's count, `Load = cost × count`, and its `*health.Target` when the route
  catalogue has it *and* the server is `up` with an active address. `pick()` walks the members in
  order and keeps the first strictly-lowest load, so an unavailable member is skipped, an idle route
  lands on the first member (every load is 0, the tie-break is the order), and ties always go to the
  earliest row.

The selector reads `health.Manager.Route` rather than owning a catalogue: the route table is the
health loop's, rebuilt under its own mutex on every probe, and duplicating it would put a second
source of truth between a request and the address it goes to. `View(cfg)` compiles every route for
the dashboard and marks the member `pick()` would choose; `Aliases(cfg)` is the same computation
reduced to the serving routes, which is what `GET /v1/models` appends to the published ids.

Nothing is precomputed per config apply: a route table is a handful of members, and compiling it per
request (or per 5 s poll) keeps the numbers it decides on current instead of adding a cache to
invalidate when health, config or in-flight state moves.

## Health and address election

One goroutine per server on a `time.Ticker(health_interval)`, re-created when the setting changes.
Server state — up/down, consecutive failures, `active_address`, last error, model list — lives in a
mutex-guarded struct read by both the router and the dashboard. Election follows
[Address selection](functional.specs.md#address-selection) exactly: probe the active address, on failure walk
`host_addresses` from index 0, three consecutive all-fail rounds mark down, no fail-back, reset on a
config change. The catalogue behind `/v1/models` is rebuilt from these states, which is why a server
going down withdraws its models within one interval without extra machinery.

The connect line names the address and the model list returned ("server is up" / "switched address"
/ "re-elected an address", `INFO`); a list that differs from the last one under the same active
address is a separate `INFO` line with `added`/`removed` computed as sets, so a host reordering its
models logs nothing. An unchanged probe logs at `DEBUG` only. A failed address walk (`ERROR`, once
per round — the active address is cleared, so it cannot repeat) and the down transition (`ERROR`,
with the models withdrawn) are the two failure lines.

## Usage store

`ELPULPO_DATA_DIR/usage.db` with `journal_mode=WAL`, `synchronous=NORMAL`, `busy_timeout=5000`.

```sql
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

## Price catalogue

`internal/catalog/prices.yaml`, embedded with `go:embed`; `ELPULPO_PRICE_CATALOGUE` replaces it at
runtime with a file of the identical schema.

```yaml
catalogue:
    version: "2026.09"
    as_of: 2026-09-01          # drives the 180-day stale label
    currency: USD
    rates:
    -   provider: anthropic
        model: claude-sonnet-4-5
        input: 3.00
        output: 15.00
        cached_input: 0.30     # omit when not applicable
        reasoning_output: 15.00
```

- Matching is exact on `model` against the base model name or an alias, compared case-insensitively.
  No fuzzy matching, no similarity threshold — cloud names rarely equal Ollama tags, which is exactly
  why the Prices screen also lists entries for manual picking.
- A unit test fails the build on any entry missing provider/model/input/output, on a currency other
  than `USD`, or on an unparseable `as_of`. A supplied override file runs through the same validation
  at startup; if it fails, El Pulpo keeps the embedded catalogue and says so rather than starting with
  no prices at all.
- Refresh is a human editing the file when cutting a release. No generator, no provider API calls.

## Performance envelope (v1 design targets)

Design targets, not benchmark guarantees — a release that misses one is a conversation, not a
rollback. Sized for one instance on one box, where the inference upstreams are the real bottleneck.

| target | value | how it is checked |
| --- | --- | --- |
| concurrent in-flight streams | 200 sustained | opt-in `-scale` test against fake upstreams that hold streams open |
| request rate | 50 new req/s peak | same harness, non-streaming |
| memory | ≤ 150 MB RSS at 200 streams | RSS sampled during that test; bodies are never buffered, only the current frame |
| added latency | ≤ 10 ms p95 per request, ≤ 1 ms per SSE frame | measured against a direct-to-upstream baseline |
| usage rows retained | designed for 10 M (≈50k/day × 200 days) | opt-in `-scale` test: filtered 30-day query and percentiles ≤ 1s p95 |
| prune | 1 M rows in ≤ 60s | times the batched delete on a generated 1 M-row database |
| CSV export | 100k rows in ≤ 5s within the RSS cap | streamed export timed against a seeded database |
| startup | `/healthz` listening ≤ 1s | plain test, no tag needed |
| artifacts | binary ≤ 35 MB, image ≤ 60 MB | checked in CI; `modernc.org/sqlite` is most of the weight |

The two `-scale` tests exist so a regression in the writer batching or the percentile query shows up on
demand without taxing the default suite; everything else is either asserted cheaply or left as a
stated expectation.

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
- Each screen is a stack of independently refreshing regions, and **every fragment renders its own
  polling container**: the swap is `outerHTML`, so a region that fetched a bare child would lose its
  `hx-get`/`hx-trigger` on the first refresh and stop refreshing. The Servers screen is
  `#servers-state` (the table) → `#servers-models` (published ids, from the route table the proxy
  routes with) → `#servers-config` (the forms and the YAML panel); the first two poll every 5s and
  the forms re-read after any mutation, which also re-seats the config hash the next save is guarded
  with. `GET /dashboard/part/servers` serves them by `?scope=`. The Load balancer screen is the same
  shape with two regions — `#lb-live` (per-member cost, in-flight count, resulting load, and where
  the next request would go, on the same 5 s clock) and `#lb-config` (`?scope=forms`) — and its
  member picker is a `<datalist>` of the published ids: a native dropdown of what the fleet offers
  that still accepts a typed name, which is what keeps the screen JavaScript-free under
  `default-src 'self'`.
- A mutation answers `HX-Trigger: elpulpo-changed`. htmx dispatches it on the submitting form and the
  event bubbles, so the regions listen `elpulpo-changed from:body` — that is what reloads the table
  and the model list the moment a host, a server or an import is applied, in every open tab. The
  stats table polls on the same 5s mechanism. No websocket: one connection model is worth more than
  the latency saved, and nobody leaves this dashboard open for hours.

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
  held-open streams for disconnects. Every fake records the headers it received: scenario 43 is the
  credential-forwarding assertion, and it is the test that catches the one bug class that matters here
  — leaking the client's proxy token upstream, or failing to send the server's own.
- Scenario 44 asserts the CORS asymmetry in both directions; 45 and 46 cover the hosts-only config and
  the settings ranges.
- Two-address tests bind `127.0.0.1` and `127.0.0.2` — the whole `/8` is local on Linux, so
  `host_addresses` failover and stickiness (16–20) are exercised for real rather than mocked.
- Timeout scenarios (30, 31) run with injected sub-second settings, so the suite stays fast.
- **Deliberately untested in v1:** `total_timeout` and TLS upstreams. There is no certificate
  verification to exercise because it is off, and `total_timeout` is off by default and awkward to
  reach in a fast suite. Recorded here so their absence is not mistaken for coverage.
- Golden files pin the canonical config export (24) and the validation error text (11, 17, 25); CSV
  round-trip tests re-parse the export and recompute the totals (13, 32).
- `go test -race` throughout. A real-Ollama smoke test sits behind `-tags live` and is not in the
  default suite: determinism earns more than realism here.

## Deliberately absent

Web framework, ORM, template engine beyond `templ`, plugin system, Prometheus endpoint, clustering,
encryption of the config at rest, and any HTTP call to a host outside the configured fleet. Each is a
separate decision later; paying for them now would buy nothing the functional spec asks for.

## Deliverable note

`README.md` must cover: quick start for binary and Docker, the env table above, the config file
reference including what a dashboard save does to hand-written comments, and how to back up —
`VACUUM INTO` for the database, a plain copy of the YAML taken after a save.

## As-built notes (v1)

- **Test layout**: the acceptance suite lives in `acceptance/` (package `acceptance_test`) on a
  shared harness, `internal/testutil` — `Start(t)` boots the real app on an `httptest` listener
  with `health_interval=5s`/`probe_timeout=1s`, fake upstreams carry the knobs the Testing section
  lists, and `IssueCSRF/CSRFPost` drive dashboard mutations. Golden files: the canonical export is
  pinned in `internal/config/testdata/canonical.golden` (routes included: route sort by lowercased
  alias, member order verbatim, a materialised `cost: 1`); validation text is asserted per violation
  path. The 27/45/46 dashboard-route variants live beside the store-level tests. The two new seams
  have their own level: `internal/balancer` tests the policy on hand-built routes and the compiling
  of a real route against a running `health.Manager` over two fake upstreams, and
  `internal/dashboard/route_form_test.go` pins the flat route form — rows zipped by position, and a
  row action landing on the row that was clicked even after that row was renamed.
- **Injected settings**: duration settings are floored at 1s (see functional spec, interpretation
  12), so timeout scenarios run 1s settings against 2.5–4s fake stalls. The suite stays linear.
- **`-tags live` smoke**: `acceptance/live_ollama_test.go` runs one chat against a real Ollama on
  `127.0.0.1:11434` and skips when none answers; it is never in the default suite.
- **templ versions**: runtime `github.com/a-h/templ v0.3.1020` (go.mod); generated with CLI
  v0.3.1001 and confirmed compiling against either. Regenerate with `make templ`.
- **Dashboard deviations from the original build contract (all additive)**: `config/import/apply`
  also accepts `{"yaml","h"}` (re-posting the textarea) beside the staged `import_id` flow, because
  the CSP forbids the inline JS that would move an id from preview to confirm; `prices/save` also
  accepts flat form fields beside `{"prices":…|null}` — one repeated field per column, **one value
  per table row**, zipped by position, so every row on screen is saved, never only the first;
  `config/save` also takes a `yaml` form field and the `loadbalancer` section beside `hosts` and
  `prices`; `route/save` takes either `{"route":{…}}` or the flat route form (`alias`,
  `description`, one `model` and one `cost` per row, plus the clicked button's
  `delete_member`/`move_up`/`move_down` carrying the **row's position** (never the model id: htmx
  submits the edited inputs too, so an id-named action would aim at a row the operator has already
  renamed and quietly do nothing), and `original_alias` to rename an existing route); mutations answer an `HX-Trigger` header for htmx
  refresh. CSRF, hash-guard, violation and stale-save outcomes are exactly as contracted.
- **Route ordering is data, not display order**: the dashboard renders members in the configured
  order because that order *is* the preference order, and `Normalize` sorts routes by alias while
  deliberately leaving their members alone.
