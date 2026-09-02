# El Pulpo functional specs

Covers **v1** only. Deferred items are listed in [Out of scope](#out-of-scope-v1).

## Terminology

| term | meaning |
| --- | --- |
| host | a machine running one or more LLM servers; it may be reachable under several addresses (LAN, VPN), listed in preference order |
| host_addresses | the ordered addresses of a host; position 0 is preferred, the rest are fallbacks |
| server | one LLM endpoint: a host address + port + API adapter |
| api | the adapter El Pulpo uses to talk to a server (model-list endpoint, chat endpoint, usage parsing) |
| postfix | name segment used in published model ids, defaults to `api` |
| base model name | the model name as reported by the server, e.g. `qwen3.8:27b` |
| published model id | `<base-model>-<postfix ?? api>@<host-id>` — the name clients address |
| alias | an alternative name for a base model name, host-independent |
| price entry | the reference prices of one base model name, with its aliases |

## Proxy

### Configuration

All configuration is editable through the web dashboard and takes effect without a restart. It is one
document with two top-level sections, `hosts` and `prices`, and it lives in one YAML file at
`ELPULPO_CONFIG` — that file is both the store and the export format. A missing file is an empty
configuration: El Pulpo starts, serves an empty catalogue and prompts for the first host; the first
save creates the file.

```yaml
hosts:
-   host_addresses:            # required — non-empty, in preference order
    - 192.168.1.101            #   tried in this order; see "Address selection"
    - 10.8.0.5
    id: minion1               # required — short host name
    description: "Rack box 1"  # optional
    servers:
    -   port: 11434           # required
        api: ollama           # required — must be a supported adapter
        id: ollama            # required — unique within the host
        description: "GPU inference"   # optional
        postfix: ollama       # optional — defaults to api
        scheme: http          # optional — default http; https, see "Upstream TLS"
        auth_token: ""        # optional — sent upstream as Authorization: Bearer
        max_concurrency: 0    # optional — 0 (default) means unlimited
```

Reference prices live in the same document, keyed by model name:

```yaml
prices:
    currency: USD              # required — ISO 4217, labels every amount below
    models:
    -   model: qwen3.8:27b     # base model name, as reported by the servers
        aliases: [qwen27]      # optional — other names this price also covers
        input: 0.25            # per 1M tokens
        output: 1.00
        cached_input: 0.10     # optional — falls back to input
        reasoning_output: 1.00 # optional — falls back to output
```

**Validation** — a save that violates any rule below is rejected with a field-level error and the
currently live configuration stays in place:

- host `id` unique across hosts (case-insensitive).
- `host_addresses` non-empty, entries unique within the host, and an address may not appear in the
  list of two different hosts — El Pulpo could not tell them apart. Addresses are compared as
  normalised strings (lowercased, trailing dot stripped); an IP and a DNS name for the same
  interface are therefore *not* detected as a duplicate.
- server `id` unique within a host; `port` unique within a host.
- the resolved name segment (`postfix ?? api`) unique within a host — this is what guarantees that
  two servers of the same `api` on one host cannot publish colliding model ids.
- `api` names a supported adapter.
- `id` and `host-id` match `[a-z0-9][a-z0-9-]{0,31}` so they are URL- and model-id-safe.
- every published model id the configuration produces is globally unique, and its base model name
  contains no `@` (published ids are parsed right-anchored: last `@` starts the host id, the last
  `-` before it starts the name segment).
- price entries — **the whole `prices` section is optional**: a configuration with only `hosts` is
  valid, savings stay off until it is filled in, and the export omits an empty `prices` rather than
  writing an empty one. When present: `currency` is a 3-letter ISO 4217 code; `model` unique across
  entries; each alias unique across the whole document and not equal to any other entry's `model` or
  alias, so price lookup is never ambiguous; aliases match `[A-Za-z0-9][A-Za-z0-9._:-]{0,63}` (no
  `@`); all four prices are numbers ≥ 0.

Config changes apply within one health-check interval; requests already in flight finish against the
configuration they started with.

### Configuration export and import

The dashboard exports the live configuration as YAML in exactly the documented shape, and imports the
same shape back. The config document is the hosts section and the prices section with the currency;
runtime state (health, active address, usage rows) and the global settings are not part of it.

- **Export** materialises every field, including the ones left at their default, and orders hosts by
  `id`, servers by `id` within a host and price entries by `model`, so two exports of the same config
  are byte-identical and diffs between them are meaningful.
- **Import replaces the whole configuration** — there is no merge. It is validated against every rule
  in the list above before anything is applied.
- A validation failure applies nothing and reports *all* violations, each with the path of the
  offending entry (`hosts[1].servers[0].api: unsupported adapter "anthropic"`).
- A valid import is previewed as added / removed / changed hosts and servers, and only applied on
  confirmation. It then takes effect like any other change: within one health interval, in-flight
  requests untouched.
- The file is **machine-owned**: a save rewrites it canonically, so comments an operator added by
  hand do not survive it — they are dropped, not corrupted. Everything else about the file is
  operator-friendly:
  - a **hand edit on disk is honoured**: El Pulpo notices the change, validates it and applies it
    within a couple of seconds, logging the reload at `INFO`. No restart.
  - a hand edit that **fails validation** is refused: the live configuration stays as it was, the
    error is logged at `ERROR` and shown on the dashboard, and the file on disk is left untouched.
  - a dashboard save **races no hand edit**: if the file changed on disk after the dashboard loaded
    it, the save is refused with "changed on disk, reload first" rather than overwriting it.

### Model naming

For this configuration:

```yaml
hosts:
-   host_addresses: [192.168.1.101]
    id: minion1
    servers:
    -   port: 11434
        api: ollama
        id: ollama
    -   port: 8000
        api: openai
        id: vllm
        postfix: vllm
-   host_addresses: [192.168.1.102]
    id: minion2
    servers:
    -   port: 11434
        api: ollama
        id: ollama
```

serving `qwen3.8:27b` + `gemma4:31b` on `minion1:11434`, `deepseek-v4-flash` on `minion1:8000`, and
`gpt-oss:120b` + `qwen3.8:27b` on `minion2:11434`, `GET /v1/models` publishes:

- `qwen3.8:27b-ollama@minion1`
- `gemma4:31b-ollama@minion1`
- `deepseek-v4-flash-vllm@minion1`
- `gpt-oss:120b-ollama@minion2`
- `qwen3.8:27b-ollama@minion2`

The suffix comes from `postfix ?? api`, never from the server `id`; the id only names the server for
the dashboard and the `server` column of the usage rows. The same model on two hosts is **two
distinct ids** and the client chooses one — there is no balancing between them.

### Adapters

Supported in v1:

| api | model list endpoint | chat endpoint | usage source |
| --- | --- | --- | --- |
| `openai` | `GET /v1/models` (`data[].id`) | `POST /v1/chat/completions` | `usage.prompt_tokens` / `usage.completion_tokens` |
| `ollama` | `GET /api/tags` (`models[].name`) | `POST /v1/chat/completions` (Ollama's OpenAI-compat layer) | as above |

An adapter must define all three columns. Unsupported or omitted `usage` is handled by the token
fallback in [Usage recording](#usage-recording).

### Upstream TLS

A server with `scheme: https` is reached over TLS, but in v1 **its certificate is not verified**.
Self-signed certificates are the norm on homelab and VLAN inference boxes, and a proxy that refuses
them is a proxy that does not get used.

The consequence is stated rather than hidden: anyone positioned between El Pulpo and the host can
read and modify that traffic, and sees the `auth_token` El Pulpo forwards. v1 accepts this on the
assumption that the fleet lives on a network the operator controls. Verification and pinning are
deferred; until then this is the honest trade, not an oversight.

### Cross-origin access

`/v1` answers preflight and carries permissive CORS headers — `Access-Control-Allow-Origin: *`, with
`Authorization` and `Content-Type` allowed on `GET`, `POST` and `OPTIONS`, preflight cached 10
minutes. Browser clients and SDKs are expected to call the proxy directly.

This works because `/v1` is token-only and cookieless: a wildcard origin cannot ride a session that
does not exist. The dashboard and `/api/*` get **no CORS headers at all** and remain same-origin plus
the CSRF marker — a web page may talk to the proxy, never to the configuration.

### Client-facing API

One HTTP listener serves the proxy and the dashboard.

| route | behaviour |
| --- | --- |
| `GET /v1/models` | OpenAI-shaped list of `published model id`s, from healthy servers only, sorted by id |
| `POST /v1/chat/completions` | routed on exact match of `model` against a published id; forwarded to that one server with `model` replaced by the base model name |
| anything else | `404`, with `unsupported_endpoint` for valid OpenAI paths that v1 does not proxy (completions, embeddings, …) |

There is no way to address a host or a server directly: the published model id is the only handle
clients get, and every other path — including anything under `/servers/` — is a plain `404`.

**Routing is exact and single-target.** In v1 there is no load balancing and no name resolution
across hosts: a published id maps to exactly one server, and the bare base model name
(`qwen3.8:27b`) is **not** accepted even when only one server offers it. **Aliases are not accepted
as `model` values either** — they exist only to attach prices to names (see
[Savings](#savings)), and a request for `qwen27` is an unknown id. Keeping the request vocabulary
equal to `GET /v1/models` means a client can always discover every name that will work. A request for
an id that is unknown returns `404 model_not_found` (`reason: not_configured`) and for a known id
whose server is currently down `404 model_not_found` (`reason: not_available`, retry after the next
health probe).

**Response identity.** The `model` field of a routed response — in the JSON body and in every SSE
chunk — is rewritten back to the published id the client asked for, so clients see what they
requested and a client-side cache keyed on the model name stays coherent.

### Streaming

Streaming responses (`stream: true`) are proxied through chunk by chunk, flushed per chunk, never
buffered. When a client requests a stream without setting `stream_options.include_usage`, El Pulpo
sets it to `true` for both adapters so the final chunk carries usage, and forwards that chunk
untouched. A client's explicit `stream_options` always wins.

### Request limits and timeouts

| knob | default | why this value |
| --- | --- | --- |
| `max_request_size` | 32 MiB | a 128k-token text prompt is ~1 MiB; multimodal base64 payloads dominate. Comfortably above the 1 MiB default of common reverse proxies, so El Pulpo is never the tighter limit |
| `connect_timeout` | 5s | TCP + TLS setup only; a host that cannot connect in 5s is not coming back soon |
| `first_byte_timeout` | 60s | must survive a cold model load on Ollama / llama.cpp, but a client should not hang blind for longer |
| `stream_idle_timeout` | 120s | the gap allowed between two SSE chunks — covers long tool-call pauses and slow GPUs |
| `total_timeout` | off (`0`) | no overall deadline: a stream over a large context legitimately runs for many minutes, so liveness is judged per chunk instead |

- An oversized body is rejected with `413 request_too_large` before routing, so no usage row and no
  concurrency slot are taken.
- Any of `connect_timeout`, `first_byte_timeout` or `stream_idle_timeout` firing returns
  `504 upstream_timeout` and records the row as `upstream_timeout`. Once response bytes have reached
  the client the response can only be truncated, so an idle timeout mid-stream ends the stream
  without changing the status the client already saw.
- `max_request_size` applies to proxy routes; the dashboard's own endpoints are exempt.

### Upstream and client errors

- Upstream `4xx`: status and body passed through unchanged.
- Upstream `5xx` or connection failure: `502 upstream_error`.
- Upstream timeout, per the table above: `504 upstream_timeout`.
- Server went down while the request was queued: `503 server_unavailable`.
- Queue wait exceeded: `429 server_busy`.
- Oversized body: `413 request_too_large`.
- Client disconnect: the upstream request is cancelled, its concurrency slot released, the usage row
  recorded as `cancelled`.
- No retries in v1 — a request is attempted against exactly one server.

### Health and availability

- Every server is probed every `health_interval` with a `GET` on its model-list endpoint, bounded by
  `probe_timeout`, against the address selection described below.
- `200` with a parseable payload is up; three consecutive probe failures mark it down (anti-flap); the
  first success marks it up again.
- Every successful probe refreshes that server's model list, so models pulled or deleted on the
  backend become visible or disappear within one interval. Reaching a server is an `INFO` event that
  names the address and the models that host returned ("server is up", "server switched address",
  "server re-elected an address"), and a list that moves while the same address keeps answering is
  its own `INFO` line naming the ids added and removed. A routine probe with nothing new to report
  says nothing at `INFO` — it is visible at `debug`.
- A down server publishes no models, refuses routed requests as above, and shows its last error in
  the dashboard. Going down is `ERROR`, naming the models clients just lost; so is losing the active
  address, even when a fallback keeps the server up.
- On startup El Pulpo probes immediately: the dashboard serves at once, `GET /v1/models` is empty
  until the first probes complete.

### Address selection

A host's `host_addresses` is an ordered preference list, not a set — the same machine may be
reachable on the LAN and under a different VPN address. Health is tracked **per server**, never per
address: each server independently remembers the one address it last reached, its `active_address`.

- **Probe with an active address.** Probe that address only. Success keeps everything as it is.
- **No active address, or the active one failed.** Walk `host_addresses` from position 0 and take the
  first address that answers; it becomes the `active_address`, the server is up, and its model list
  comes from that response. A switch from one address to another is logged at `INFO` with both
  addresses.
- **Nothing answered.** That is one consecutive probe failure for the server; three in a row mark it
  down, exactly as in [Health and availability](#health-and-availability).
- **No fail-back.** While the `active_address` answers, the addresses before it are never re-tried —
  a VPN address that comes back does not steal traffic on its own. Re-election from position 0
  happens only when the active address stops answering, when the host's `host_addresses` is edited,
  or on restart.
- **Requests never probe or switch.** A routed request uses whatever `active_address` the server has
  at that moment; if the address dies mid-request the failure rules apply and the next probe re-elects
  — the request itself is not retried.
- **Cost.** One probe per server per interval in steady state; the extra addresses are only tried in
  the interval where the active one stops answering.
- The dashboard shows the `active_address` of every server, and marks clearly when it is a fallback
  rather than the first configured address.

### Concurrency

`max_concurrency` caps in-flight requests per server (a slot is held for the whole streamed
response). Excess requests queue FIFO and wait up to `queue_timeout` before `429`.
`max_concurrency: 0` means unlimited, which is the default.

### Authentication

Credentials come from the environment only; the dashboard cannot change them.

| variable | effect |
| --- | --- |
| `ELPULPO_PROXY_TOKEN` | clients must send `Authorization: Bearer <token>` on all `/v1` routes |
| `ELPULPO_DASHBOARD_USER` / `ELPULPO_DASHBOARD_PASSWORD` | HTTP Basic auth on `/dashboard` and `/api/*` |

If either is empty, that surface is left unauthenticated and El Pulpo logs a `WARN` at startup and
shows a persistent banner in the dashboard — on every start, not just the first. The client's proxy
token is never forwarded upstream; upstream credentials come from each server's `auth_token`.
Dashboard Basic auth over plain HTTP must be discouraged in that warning, since config editing is
remote control of the whole fleet.

Because a browser sends Basic auth credentials without any consent step, every route that changes
state (config save, import, settings write) additionally requires a same-origin marker that a
cross-site form cannot set — otherwise any page the operator's browser visits could rewrite the
fleet. A request without it is rejected before the change is applied (`403`).

### Global settings

Every value below is editable in the dashboard, applies immediately, and **survives a restart**. The
defaults are authoritative here, not in the prose around them. Two things are deliberately not
settings: credentials, which are environment only, and the
[configuration](#configuration) document (`hosts`, `prices`), which is edited as one unit and exports
as YAML.

| setting | default | allowed | meaning |
| --- | --- | --- | --- |
| `health_interval` | 30s | 5s – 3600s | model-list probe period per server |
| `probe_timeout` | 5s | 1s – 300s | per-probe timeout |
| `connect_timeout` | 5s | 1s – 300s | upstream TCP + TLS setup |
| `first_byte_timeout` | 60s | 1s – 3600s | upstream response headers / first SSE chunk |
| `stream_idle_timeout` | 120s | 1s – 3600s | allowed gap between two SSE chunks |
| `total_timeout` | off (`0`) | off, or 1s – 86400s | optional overall deadline per request |
| `max_request_size` | 32 MiB | 1 KiB – 1 GiB | accepted proxy request body |
| `queue_timeout` | 60s | 1s – 3600s | wait for a free `max_concurrency` slot |
| `retention_days` | off (`0`) | off, or any positive integer | prune usage rows older than N days |

A value outside its range is rejected with a field-level error and the live value stays in force, as
for a configuration save. Per-server `max_concurrency` takes `0` (unlimited) or any positive integer.

**Time is the server's, everywhere.** Timestamps are stored in UTC, but presets (`today`, `this
month`), custom ranges and all display happen in the timezone El Pulpo runs in. Containers default to
UTC, so an operator wanting local-day statistics sets the container's `TZ`. Exported CSV stays UTC —
a file is not a display.

## Token usage stats

### Usage recording

Every request that is routed to a server writes one row when it terminates — success, upstream
error, upstream timeout or cancellation. Requests rejected before routing (unknown model, `413`,
`429`) are not usage rows; they are counted and logged as their own `chat request` line at `WARN`,
with a `reason`. Every routed request emits exactly one `chat request` line as well — `INFO` when
the client got an answer, `ERROR` on an upstream failure — carrying host, server, published model,
`stream`, status, HTTP status, latency, queue wait, token counts and `ttft_ms` on streams, so the
log and the usage table say the same thing about what reached a server. Failure means failure: an
`ERROR` line is something El Pulpo or its fleet got wrong, never a client asking for a model that is
not published or hitting a concurrency limit — the two startup warnings about unset credentials are
`WARN` for the same reason. Credentials never appear in either log or table.

| field | notes |
| --- | --- |
| `timestamp` | when the request was received (UTC; displayed in the server timezone) |
| `host`, `server` | the ids it was routed to |
| `model` | published model id |
| `endpoint` | `chat` in v1 |
| `status` | `ok` \| `upstream_error` \| `upstream_timeout` \| `cancelled` |
| `http_status` | status returned to the client |
| `tokens_in`, `tokens_out` | prompt and completion tokens |
| `tokens_cached` | subset of `tokens_in`, when reported |
| `tokens_reasoning` | subset of `tokens_out`, when reported |
| `estimated` | `true` when El Pulpo had to count tokens itself |
| `latency_ms`, `ttft_ms` | total duration; time to first chunk, streams only |

**Token fallback:** when the upstream reports no `usage`, El Pulpo records a local estimate
(character heuristic by default), sets `estimated: true`, and the dashboard marks those rows as
approximate. `tokens_out` always includes `tokens_reasoning`; `tokens_in` always includes
`tokens_cached`.

### Stats view

The dashboard's statistics tab is a table of those rows, filterable and sortable.

- Filters: multi-select exact match on `host`, `server`, `endpoint`, `status`; substring match on
  `model`; combined with AND.
- Grouped summary above the table, over the filtered period, grouped by `model`, `host`, `server` or
  `day`: token sums, row count, latency p50/p95 and — where a price exists — the amount.
- Period: presets `today` and `this month`, plus a custom date range, evaluated in the server timezone.
- Sorting on any column, pagination (default 50 rows).
- A totals row for the filtered period: `tokens_in`, `tokens_out`, `tokens_cached`,
  `tokens_reasoning`, plus `latency` p50/p95.
- Rows with `estimated: true` are visually distinguished in the table and flagged in totals.

**CSV export.** Both dashboard data views export their filtered set:

- **Rows** — one line per usage row, every column of the table, `estimated` as `true`/`false`,
  `timestamp` in ISO 8601 UTC. UTF-8 with a header line and comma delimiter; text fields quoted so
  model names with commas or quotes survive a spreadsheet round trip. The export always uses
  `YYYY-MM-DD HH:MM:SS`-parseable UTC, never the display timezone, so re-importing or scripting over
  the file is unambiguous.
- **Summary** — the aggregated block (per model, per host, per day) with its token sums and amounts.
- Exports contain exactly what the current filters and period select, no more and no less; the
  displayed totals must equal the sums over the exported lines. An export of an empty selection is a
  header-only file rather than no file at all.
- Large sets are streamed rather than buffered into memory.

### Usage retention

`retention_days` prunes usage rows older than N days, evaluated against `timestamp` in UTC. Default is
**off** (`0`): every row is kept forever, and nothing is deleted unless the operator asks.

- When set, pruning runs at startup and once a day, and logs how many rows it removed.
- Pruning is irreversible and never touches reference prices, the configuration, or the exported CSVs.
- Aggregations and savings only ever see retained rows, so when retention is on the dashboard shows a
  note that history beyond N days is not available — older usage must not silently read as zero.
- Row-level rollups are out of scope for v1: outside the window there is no data at all, not a daily
  summary.

### Savings

Reference prices are the `prices` section of the configuration: one entry per base model name, so a
single entry covers that model on every host, expressed per 1M tokens in `currency`:

| price | applies to | fallback |
| --- | --- | --- |
| `input` | `tokens_in − tokens_cached` | — (required to enable savings) |
| `output` | `tokens_out − tokens_reasoning` | — |
| `cached_input` | `tokens_cached` | `input` |
| `reasoning_output` | `tokens_reasoning` | `output` |

**Matching.** A usage row's base model name is the published id with its `-<segment>@<host-id>`
suffix removed; the row is priced by the entry whose `model` equals it, or whose `aliases` contain
it. Matching is exact and case-sensitive — the whole point of an alias is to cover the second
spelling (`Qwen3-27B` on vLLM, `qwen3.8:27b` on Ollama) without duplicating the price. Aliases affect
pricing only: they never change a published model id and are not accepted as `model` values in
requests.

Savings for a row are
`(in_eff × price_in + cached × price_cached + out_eff × price_out + reasoning × price_reasoning) / 1e6`,
shown next to the token sums in the grouped summary and in both CSV exports, and as a grand total.

Base model names seen in usage that match no entry are excluded from the total and listed under
`no price set` — never silently counted as zero. The Prices screen lists them, so the operator learns
which names the servers actually report.

### Price catalogue

To spare the operator typing cloud rates by hand, El Pulpo ships a read-only catalogue of current
provider prices, embedded in the binary: provider, cloud model name, the four per-1M rates, currency
(`USD`) and an `as_of` date, with a catalogue version and overall `as_of` stamp shown in the UI.

It is a **hand-curated seed list** — roughly the two dozen models people actually compare against,
not a mirror of every provider catalogue. Refreshing it is part of cutting a release, and a supplied
override file has the same shape. Nobody is pretending a static file tracks a market; the `as_of`
stamp and the `stale` label exist so nobody forgets that.

- **Offline by construction.** Beyond the hosts in its own configuration, El Pulpo talks to nothing:
  no update check, no telemetry, no rate fetch. Rates change when the binary is upgraded, or when the
  operator points `ELPULPO_PRICE_CATALOGUE` at a file they supply —
  the only way to refresh an air-gapped install, and the same env-var pattern as the config seed.
- **Never applied on its own.** The Prices screen offers catalogue entries for base model names seen
  in usage that have no price entry, matched on the exact name or alias — no fuzzy guessing. Applying
  an entry is an explicit act that writes the numbers into the `prices` section of the configuration,
  where they stay editable and are exported as YAML like anything else.
- **Existing prices are never overwritten.** Applying over a live entry needs an explicit overwrite
  confirmation.
- **No currency conversion.** A `USD` catalogue rate is not turned into another currency by El
  Pulpo: if the document currency is not `USD`, the rate is displayed for reference and the operator
  enters the number themselves. Inventing an exchange rate would put a fake precision into savings.
- **Staleness is visible.** A catalogue older than 180 days is labelled `stale`, and savings computed
  from catalogue-derived prices are presented as estimates of the cloud bill avoided, not as an
  invoice.

## Dashboard

v1 screens, all acting on the live configuration without a restart:

- **Servers** — hosts and servers with state, the `active_address` (flagged when it is a fallback
  rather than the first configured address), model count, in-flight requests, last probe and last
  error; add / edit / delete forms, including the ordered `host_addresses` list; validation errors
  reported per field; YAML export download and import upload with the added/removed/changed preview.
- **Prices** — the `prices` section of the same configuration: currency, entries per base model name
  with their aliases, the model names seen in usage that match no entry, and the bundled
  [price catalogue](#price-catalogue) offered against those names.
- **Statistics** — the table, grouped summary and both CSV exports described above.
- **Settings** — every knob from [Global settings](#global-settings); read-only display of the
  env-only credentials with the open-access warning when unset, and a note stating that retention
  pruning is irreversible while it is on.

## Acceptance criteria (v1)

The configuration from the [Model naming](#model-naming) example (hosts `minion1`, `minion2`) is
used throughout.

| # | given | expect |
| --- | --- | --- |
| 1 | all servers healthy | `GET /v1/models` returns exactly the 5 published ids of the Model naming example |
| 2 | `minion2` deleted via the dashboard | within one health interval `GET /v1/models` returns 3 ids; no restart happened |
| 3 | vllm server stopped | `deepseek-v4-flash-vllm@minion1` disappears within one interval; a request for it returns `404 model_not_found` with `reason: not_available` |
| 4 | `POST /v1/chat/completions`, `model: deepseek-v4-flash-vllm@minion1`, non-streaming | the upstream sees `model: deepseek-v4-flash`; the client's response carries `model: deepseek-v4-flash-vllm@minion1`; a row exists with `host: minion1, server: vllm, status: ok` and the upstream usage numbers |
| 5 | same request with `stream: true` | chunks arrive before generation completes; final chunk carries usage; row has non-zero `tokens_in`/`tokens_out` and `ttft_ms > 0` |
| 6 | upstream returns no `usage` at all | the row exists with `estimated: true` and non-zero estimated counts |
| 7 | upstream answers `500` | client receives `502 upstream_error`; row has `status: upstream_error` and zero tokens |
| 8 | client disconnects mid-stream against `max_concurrency: 1` | row `status: cancelled`; a subsequent request is accepted immediately |
| 9 | `max_concurrency: 1`, two simultaneous requests | the second is served once the first finishes; with `queue_timeout` exceeded it gets `429 server_busy` |
| 10 | `ELPULPO_PROXY_TOKEN` set | a request without the token gets `401`; with the token it succeeds; unset variable produces the `WARN` log line |
| 11 | config with duplicate host id, duplicate server id in a host, duplicate name segment, or an address also listed under another host | save rejected with a field-level error; live config and `GET /v1/models` unchanged |
| 12 | `bare model name` request (`model: qwen3.8:27b`), or an alias (`model: qwen27`) | `404 model_not_found` with `reason: not_configured` — only ids listed by `GET /v1/models` are accepted |
| 13 | period `this month` + `status: ok` filter, then CSV export of rows | every line matches both criteria; totals equal the sums over the exported lines; re-parsing the file gives the same token counts; an export with no matching rows is a header-only file |
| 14 | price set for one model only | grand total covers that model alone; other models display `no price set` |
| 15 | `POST /servers/minion1/vllm/v1/chat/completions` | plain `404` — no direct per-server route exists |
| 16 | `minion1` configured as `[192.168.1.101, 10.8.0.5]`, only the second reachable | the server is `up`, its models are published, the dashboard shows `10.8.0.5` as active and flags it as a fallback |
| 17 | the first address becomes reachable again while the second still answers | El Pulpo keeps using the second address (no fail-back) and sends it no probes |
| 18 | the active second address stops answering, the first is alive | within one interval the server is reached on the first address and never enters `down`; the switch is in the log |
| 19 | no address of a host answers | after three consecutive probe failures the server is `down` and its models are withdrawn |
| 20 | `host_addresses` of a live host edited | the `active_address` is dropped and the next probe walks the list from position 0 |
| 21 | price entry `model: qwen3.8:27b, aliases: [qwen27]`, usage rows reported under both names | both rows get the same amount and are grouped under one model in the summary |
| 22 | two price entries where one's alias equals the other's `model` | save rejected, both offending paths reported, live config unchanged |
| 23 | a model in usage that matches no entry or alias | `no price set`, excluded from the grand total, listed on the Prices screen |
| 24 | export config, import it back unchanged | import preview reports no changes, `GET /v1/models` unchanged, and a second export is byte-identical to the first |
| 25 | import YAML with an unsupported `api`, a duplicate host id and a duplicate name segment | all three violations reported with their paths; live config and `GET /v1/models` unchanged |
| 26 | import YAML that drops `minion2` | preview lists `minion2` as removed; after confirming, behaviour matches scenario 2 |
| 27 | `ELPULPO_CONFIG` pointing at a file that does not exist | El Pulpo starts with an empty configuration and an empty `GET /v1/models`; the first save through the dashboard creates the file |
| 28 | a row dated a year ago, `retention_days` at its default | the row is still listed and counted; after setting `retention_days: 30` and running the prune, it is gone, the removal count is logged and the dashboard states history is limited to 30 days |
| 29 | body larger than `max_request_size` | `413 request_too_large`; no usage row exists and a server at `max_concurrency: 1` still accepts a normal request |
| 30 | upstream accepts the connection then stalls past `first_byte_timeout` | client gets `504 upstream_timeout`; row has `status: upstream_timeout` |
| 31 | stream stalls between chunks past `stream_idle_timeout` | the stream ends; the client keeps the chunks it already received; row has `status: upstream_timeout` |
| 32 | summary grouped by `model` over a period, exported as summary CSV | per-model token sums equal the sums computed over that model's rows, and amounts match the reference prices in force |
| 33 | model in usage with no entry, catalogue has the exact name | the Prices screen offers that entry; applying it writes the rates into the config, the total updates and the next export contains them |
| 34 | model in usage with no catalogue match | no suggestion is made and the model stays `no price set` |
| 35 | apply a catalogue entry over an existing price entry | refused until overwrite is confirmed; without confirmation the config is unchanged |
| 36 | document currency is `EUR` | no catalogue rate is written automatically; the `USD` value is reference only |
| 37 | El Pulpo with no route to anything but its configured hosts | everything above still works; the catalogue version and `as_of` date are displayed, and a catalogue older than 180 days is marked `stale` |
| 38 | a valid host added by hand directly in the config file | within a couple of seconds the host is probed and its models appear, with the reload logged at `INFO`; no restart |
| 39 | a hand edit that breaks validation (unknown `api`) | the live configuration is unchanged, the error is logged at `ERROR` and shown on the dashboard, the file on disk is untouched |
| 40 | dashboard form opened, then the file edited on disk, then the form saved | the save is refused with "changed on disk, reload first" and nothing is written |
| 41 | a cross-site `POST` to a config-mutating dashboard route without the required header | `403`, configuration unchanged |
| 42 | `ELPULPO_PROXY_TOKEN` or the dashboard password left empty, then restart | both the `WARN` log line and the dashboard banner are present after every restart, not only the first |
| 43 | server with `auth_token: secret-abc`, request carrying a different proxy token | the upstream receives `Authorization: Bearer secret-abc` and never the client's token |
| 44 | browser preflight and call to `/v1/models` from another origin; same against `/dashboard` | `/v1` answers the preflight and the response carries `Access-Control-Allow-Origin: *`; `/dashboard` carries no CORS headers |
| 45 | config containing only `hosts`, no `prices` section | it saves and applies, savings read as disabled, and the export contains no `prices` key |
| 46 | `health_interval: 1s`, or `max_request_size: 2GiB` | rejected with a field-level error naming the allowed range; the live values are unchanged |

## Out of scope (v1)

**No load balancing and no pools — v1 routing is exact and single-target.** No failover across
duplicate models, no named groups of published ids behind one requestable name ·
direct per-server routes and any other host/server passthrough · per-client API keys and
per-client attribution in stats · embeddings endpoints · Ollama native `/api/chat`, `/api/generate`,
`/api/embeddings` · Anthropic `/v1/messages` · response caching · multi-instance or HA deployments ·
TLS termination at El Pulpo (reverse proxy assumed) · **any request to a host outside the configured
fleet**, price catalogue updates included · upstream certificate verification (see
[Upstream TLS](#upstream-tls)) · currency conversion of catalogue rates · backup and restore of the usage
store (export the CSVs; config is backed up by exporting the YAML) · daily rollups that survive
retention pruning · import/export of the global settings · audit log of configuration changes.

**v1 has no open questions.**

## Post-v1 design notes

Not requirements, and not v1. Kept here so the next round does not start from zero.

**Pools** (named groups of published ids behind one requestable name) are where load balancing
eventually lives. Four decisions to settle first: the naming grammar that keeps a pool id from
colliding with a generated published id, and whether members stay individually requestable; selection
policy among healthy members, and where failover retries stop — retrying is only legal before the
first byte reaches the client; how a usage row records both the pool requested and the member that
served it, since savings depend on the member's price; and whether `max_concurrency` stays per member
or also applies pool-wide. A grouping of *different* models ("fast", "smart") is a separate feature
needing its own ranking rules.

Whatever gets built must not disturb three v1 invariants: `GET /v1/models` stays the complete
requestable vocabulary, so a pool must appear there; savings keep keying off the *serving* member's
model name; and `host_addresses` election stays per server, below any pool — a pool never chooses an
address. Aliases remain pricing-only, so a pool is the only routing indirection that can exist.

## Interpretations taken during implementation (v1, as built)

These are the points where the v1 text left room for choice; the implementation resolved them as
follows, and the acceptance suite encodes these readings.

1. **Publishing is gated on a live address.** A server publishes its models only while `up` *and*
   holding an active address. This is what makes scenario 3's "withdraw within one interval" true
   without contradicting scenario 19's three-round down-marking: between an active-address failure
   and the re-election, the server could not answer, so it must not be listed.
2. **Re-election after an active-address failure walks the list within the same round** (scenario
   18): the failed probe is followed by attempts at the other addresses in preference order,
   immediately. `consec_failures` counts rounds in which *no* address answered, not probes.
3. **Price entries require `input` and `output`**; `cached_input`/`reasoning_output` are optional
   and fall back to them. A price entry with only fallbacks would make savings meaningless.
4. **`postfix` obeys the id grammar** (`[a-z0-9][a-z0-9-]{0,31}`), as do host and server ids, so
   published ids never take arbitrary shapes. Alias names follow the model-name pattern and must be
   globally unique across every model name *and* every alias — the symmetric collision of scenario
   22 is therefore rejected whichever path carries it, and both offending paths are reported.
5. **A 4xx from the upstream is a faithfully forwarded answer**: the client receives the upstream
   status and body as-is, and the usage row records `status: ok` with that `http_status`. Only 5xx,
   timeout, upstream error, and cancellation are non-`ok` statuses.
6. **A request cancelled before the first byte upstream** records `status: cancelled` with
   `http_status: 499` (the client itself is gone; whatever we answered is moot).
7. **Unpriced groups carry no amount**: the summary flags them, the grand total excludes them, the
   CSV leaves the amount cell empty. There is no conversion and no estimate.
8. **Day groups and the today/month presets use the server's local timezone** (single clock for
   screens and exports).
9. **Summary CSV shape**: `group_by,group,requests,tokens_in,tokens_out,tokens_cached,
   tokens_reasoning,estimated_rows,p50_ms,p95_ms,amount,currency`, written as a `model` block, a
   `host` block, a `day` block, and one `total` row.
10. **The canonical export uses the Go YAML encoder's block style** (sequence items indented under
    their key); the document examples use the compact-dash style. Import accepts both shapes — only
    the live configuration is canonicalised. Byte stability and materialised defaults are pinned by
    a golden file.
11. **Settings range errors print bounds in seconds** (`5s to 3600s`), matching the input form.
12. **Duration settings have a 1s floor**; `total_timeout` and `retention_days` additionally accept
    `0`/`off` as "disabled". Acceptance timeouts therefore run at 1s settings against ≥2.5s stalls.
13. **CSRF uses cookie `elpulpo_csrf` and header `X-CSRF-Token`** on every dashboard mutation.
14. **Unknown-but-known-shaped OpenAI paths under `/v1`** (e.g. `/v1/embeddings`) answer
    `404 unsupported_endpoint`; everything else is a plain 404.
15. **`ttft_ms` is never 0 once a first frame reached the client**: a sub-millisecond first byte is
    recorded as 1, so "streaming ⇒ `ttft_ms > 0`" holds without a timer artifact.
