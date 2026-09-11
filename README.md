# 🐙 El Pulpo

El Pulpo is a proxy for self-hosted LLM inference. It sits between your
clients and your own inference boxes and does two jobs:

- registers the usage of every request and serves a web dashboard to
  monitor and analyse token usage and savings against cloud reference
  prices;
- keeps the configuration of many LLM servers in one place, with one
  model catalogue to name in your clients — including load-balanced
  aliases that spread one model across several machines.

Clients address El Pulpo with the OpenAI chat API; El Pulpo rewrites the
model field to the base name the serving process actually understands,
forwards the request, and streams the answer through unbuffered.

## Quick start — binary

```sh
make build            # -> ./elpulpo  (CGO not required)
./elpulpo             # listens on :8080, reads ./elpulpo.yaml, writes ./data/
```

Open `http://localhost:8080/dashboard/`, add a host, done. A missing
`elpulpo.yaml` is a valid empty configuration; the first save creates it.

## Quick start — Docker

```sh
docker build -t elpulpo .
docker run -d --name elpulpo -p 8080:8080 \
  -v $PWD/config:/etc/elpulpo \
  -v elpulpo-data:/var/lib/elpulpo \
  -e ELPULPO_PROXY_TOKEN=$(openssl rand -hex 16) \
  -e ELPULPO_DASHBOARD_PASSWORD=$(openssl rand -hex 16) \
  elpulpo
```

The image is shell-less (`scratch`); its healthcheck runs
`/elpulpo --health`, which probes the process's own `/healthz`.

The service runs as uid 65534 and must be able to write both mount points
(SQLite in WAL mode writes into the *directory*). Docker initialises fresh
named volumes from the image, which pre-creates `/etc/elpulpo` and
`/var/lib/elpulpo` owned by that uid, so the volumes in the example above
work out of the box. Two cases still need a host-side fix, and startup
names whichever you hit:

- **bind mounts** (`-v /opt/elpulpo/data:/var/lib/elpulpo`): Docker cannot
  change host ownership, so run
  `sudo chown -R 65534:65534 /opt/elpulpo` first;
- **named volumes created by an older image**, which are root-owned — fix
  once, e.g. with `docker run --rm -v elpulpo-data:/var/lib/elpulpo alpine
  chown -R 65534:65534 /var/lib/elpulpo`.

## Configuration

Everything lives in one YAML file (`ELPULPO_CONFIG`). The dashboard edits
the same document it exports: one write path, one file format. A hand
edit is honoured within a couple of seconds (content-hash poll, no
fsnotify): valid edits apply like a dashboard save, invalid ones are
rejected with the file left untouched and the error shown on every page.
A save whose form was rendered before the last change on disk is refused
with `changed on disk, reload first`.

```yaml
hosts:
-   host_addresses:            # required, non-empty, preference order
    - 192.168.1.101
    - 10.8.0.5
    id: minion1               # required, [a-z0-9][a-z0-9-]{0,31}, unique
    description: "Rack box 1"
    servers:
    -   port: 11434            # required, unique within the host
        api: ollama            # required, ollama|openai
        id: ollama             # required, unique within the host; also the
                               # id segment of this server's published model ids
        scheme: http           # http (default) | https (NOT verified — see limits)
        auth_token: ""         # forwarded as Authorization: Bearer …
        max_concurrency: 0     # 0 = unlimited, else FIFO queue
prices:
    currency: USD              # required when the section exists
    models:
    -   model: qwen3.8:27b     # base model name as servers report it
        aliases: [qwen27]      # other spellings this price also covers
        input: 0.25            # per 1M tokens
        output: 1.00
        cached_input: 0.10     # optional, falls back to input
        reasoning_output: 1.00 # optional, falls back to output
loadbalancer:
    routes:                  # optional section; an empty one is omitted
    -   alias: qwen          # the id clients name in /v1/models
        description: "Qwen on whichever box is freeest"
        models:              # required, non-empty, in preference order
        -   model: qwen3.8:27b-ollama@minion1   # published id, postfix and all
            cost: 1          # what one in-flight connection here is worth
        -   model: qwen3.8:27b-ollama@minion2
            cost: 3          # the slow box is worth 3, so it takes a third as much
```

Clients address servers by *published model id*
(`<base-model>-<server-id>@<host-id>`), or by a route *alias* if you
defined one; `GET /v1/models` lists exactly what you may request — the
published ids of healthy servers plus the alias of every route that can
answer. Names come from the servers, not the config: a model is
published because a healthy server reports it.

**Saving rewrites the file.** Every save — dashboard or import —
serialises the live configuration canonically: sorted hosts and price
entries, defaults materialised. Your hand-written comments and custom
key order do not survive it. The file is the export, not the editing
surface; keep hand-edits to the live moment or edit through the
dashboard.

## Environment

| variable | default | effect |
| --- | --- | --- |
| `ELPULPO_ADDR` | `:8080` | listener address |
| `ELPULPO_CONFIG` | `./elpulpo.yaml` | configuration document |
| `ELPULPO_DATA_DIR` | `./data` | `usage.db` lives here |
| `ELPULPO_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `ELPULPO_PROXY_TOKEN` | empty | required `Authorization: Bearer …` on `/v1`; empty = open + warning |
| `ELPULPO_DASHBOARD_USER` | `admin` | Basic-auth user (only when a password is set) |
| `ELPULPO_DASHBOARD_PASSWORD` | empty | empty = unauthenticated dashboard + warning; set = Basic auth on the whole dashboard and `/api/*` |
| `ELPULPO_PRICE_CATALOGUE` | empty | replaces the embedded reference-price catalogue when set |

Credentials are environment-only: the dashboard shows their state but can
never set or reveal them. Flags: `--health` (container healthcheck),
`--check-config` (validate the file and exit), `--version`.

## Using the proxy

```sh
curl http://localhost:8080/v1/models -H "Authorization: Bearer $TOKEN"
curl http://localhost:8080/v1/chat/completions -H "Authorization: Bearer $TOKEN" -d '{
  "model": "qwen3.8:27b-ollama@minion1",
  "messages": [{"role": "user", "content": "hello"}],
  "stream": true
}'

# or the load-balanced alias, if one is configured
curl http://localhost:8080/v1/chat/completions -H "Authorization: Bearer $TOKEN" -d '{
  "model": "qwen",
  "messages": [{"role": "user", "content": "hello"}],
  "stream": true
}'
```

The inbound `Authorization` carries the proxy token and is dropped
before forwarding; upstream credentials come from the server's own
`auth_token`. Streams are flushed chunk by chunk; `stream_options`
defaults to `{"include_usage": true}` on streams unless the client set
it, so usage gets recorded either way. Errors are OpenAI-shaped with a
`reason` (`model_not_found` → `not_configured` / `not_available`,
`request_too_large`, `server_busy`, `server_unavailable`,
`upstream_timeout`, `upstream_error`, `unsupported_endpoint`). The `/v1`
mux answers CORS preflights; the dashboard deliberately sends no CORS
headers at all.

Health and routing are per server, per address: the first reachable
address in preference order serves until it fails, then the next — with
the switch logged and no fail-back. Three consecutive probe failures mark
a server down (first success revives it).

## Load balancing

A route puts one requestable name in front of several published ids. Clients
ask for the alias; El Pulpo picks the member. Each member carries a **cost** —
what one in-flight connection to it is worth, small on a fast machine, large on
a slow one — and the rule is simple:

- **Nothing in flight?** The first model on the list serves, whatever its cost.
- **Something already running?** The request goes to the member carrying the
  **least in-flight cost** (`cost × connections running on it`), so the machines
  converge on equal load. With costs 1 and 3 that means the fast box carries
  three times as many connections before the loads meet — three times the
  traffic.
- **A member that cannot answer is skipped**, not retried: the route keeps
  serving from the rest, and the alias leaves `GET /v1/models` only when the
  last member goes dark.

What counts as in flight is one admitted request until its response finishes —
the whole stream, including any time it waits for a concurrency slot. The count
is per published id, so traffic you send to an id directly is load every route
containing it can see.

Requests through an alias are recorded against the member that *served* them
(prices and savings key off the model that actually ran), while the response
still carries the alias you asked for, so a client-side cache keyed on model
name stays coherent. An alias is a name you choose freely, and a price entry
covering the same spelling changes nothing: routes are what makes a name
requestable, price entries only price it.

## Logs

Everything is structured `slog` text on stderr, filtered by
`ELPULPO_LOG_LEVEL`. Every request that reaches
`/v1/chat/completions` ends in exactly one line, `msg="chat request"`:

```
level=INFO msg="chat request" remote=127.0.0.1:42974 host=minion1 server=ollama model=qwen3.8:27b-ollama@minion1 stream=true status=ok http_status=200 latency_ms=812 wait_ms=0 tokens_in=111 tokens_out=42 tokens_cached=0 tokens_reasoning=0 estimated=false ttft_ms=140
```

`status` is the usage-row status (`ok`, `cancelled`, `upstream_error`,
`upstream_timeout`), so `grep 'msg="chat request"'` and a `SELECT` on the
usage table agree on what reached a server: one line per row. A request
turned away before routing — bad JSON, missing `model`, `413`, unknown
model, `429` after the queue timeout, a server that went down while
queued — logs the same `msg` at `WARN` with `status=rejected` and a
`reason`, and writes no row. `wait_ms` is the time spent waiting for a
concurrency slot; `ttft_ms` appears on streams. Tokens, passwords and
`auth_token` values never reach the log.

**Reaching a host** is an INFO event naming the models it returned:

```
level=INFO msg="server is up" host=minion1 server=ollama address=127.0.0.1 model_count=2 models="[qwen3.8:27b gemma4:31b]"
level=INFO msg="server model list changed" host=minion1 server=ollama address=127.0.0.1 added="[llama3.3:70b]" removed="[gemma4:31b]" model_count=2 models="[qwen3.8:27b llama3.3:70b]"
```

The list is named when a server is connected — coming up, switching or
re-electing an address — and again whenever it moves under an address that
kept answering. A routine probe with nothing new says nothing at INFO.

### Levels

| level | what lands there |
| --- | --- |
| `ERROR` | a failure of El Pulpo or its fleet: `upstream_error`, `upstream_timeout`, a server down after 3 failed probes (naming the models withdrawn), an active address lost to a fallback, dropped usage rows, prune failures, config that will not load or save, render and export failures |
| `WARN` | a client got a `4xx`: unknown model, `429` busy, `413` oversized, `400` bad payload; plus the two startup warnings for unset credentials |
| `INFO` | one line per proxied request, host connections with the models returned, model-list changes, config load/reload, address switches, pruning |
| `DEBUG` | the routing decision (id → host/server/address and the base name sent upstream), the upstream's own response code, every probe with its model count, each probe error as it happens |

The rule for `ERROR`: something El Pulpo or a host got wrong. A client
asking for a model that is not published, or hitting a concurrency limit,
is a `WARN` — `grep level=ERROR` should read as the fleet's fault list,
not a record of other people's typos.

## The dashboard

- **Servers** — three regions, each refreshing on its own. A state table
  with up/down, the active address (flagged when it is a fallback), model
  counts, in-flight requests and last error; under it **Available models**,
  every published id the fleet knows with its base model, host, server and
  the upstream serving it — the `available` ids are exactly what
  `GET /v1/models` answers, and a name whose server is dark stays listed as
  `withdrawn`; under that **Configure servers** — add/edit/delete, YAML
  export, import with an added/removed/changed preview. A mutation reloads
  the table and the model list immediately; probe results (a server
  flipping, a model loaded or unloaded upstream) land on the 5-second poll.
- **Load balancer** — the routes. The live panel shows each route with
  whether its alias is in `GET /v1/models` right now and, per member, its
  cost, requests in flight, the in-flight cost those add up to and the
  member a request arriving now would go to; a member that cannot answer
  is named as `withdrawn` rather than hidden. The editor adds, edits and
  deletes routes: alias, description, and one row per model with a cost,
  picked from a dropdown of the published ids or typed in, reordered one
  step per click (the order is the preference order).
- **Prices** — the document's price entries, base model names seen in
  usage that carry no price, and the read-only catalogue (~24 cloud
  models, embedded in the binary, `as_of`-labelled, stale after 180
  days) with one-click Apply into your prices.
- **Statistics** — filterable, sortable, paginated usage table over
  period presets (today / this month / custom range, server timezone),
  grouping by model/host/server/day with p50/p95 and amounts, totals
  row, and CSV exports that share one filter path with the screens — the
  displayed totals equal what the export re-computes.
- **Settings** — the global timing/size knobs (validated ranges, field
  errors inline, applied live), retention (`retention_days = 0` keeps
  everything; pruning is irreversible and logged), and the read-only
  credential state.

Estimated rows (upstreams that report no usage; character heuristic) are
marked everywhere and flagged in totals.

## Backup and retention

The SQLite database never leaves its data directory.

- **Database**: back up with `sqlite3 data/usage.db "VACUUM INTO '/backup/usage-$(date +%F).db'"`
  (or stop the process and copy the files).
- **Configuration**: a plain copy of the YAML, taken *after a save* —
  between saves the file may carry hand edits that the next save will
  normalise.

With `retention_days > 0` usage older than the window is deleted at
startup and daily; rows, and only rows, are affected — reference prices,
configuration and exported CSVs are outside its reach. The Statistics
and Settings screens state the limit while it is enabled.

## Deliberate limits (v1)

- The dashboard is plain HTTP; credentials are Basic. Set a password and
  put it behind TLS if it leaves your machine — config editing is
  remote control.
- `https` upstream servers are **not certificate-verified** (self-signed
  is the norm on inference boxes): anyone on the path can read that
  traffic and sees the forwarded `auth_token`.
- No upstream retry once bytes reached the client, and no retry before
  them either: a request is aimed once, at one member of a route.
  Balancing spreads the *choice*, not the failure — a machine that
  answers `5xx` keeps answering `5xx`.
- Route costs are declared, not measured: load is cost per in-flight
  connection, so a long request and a short one weigh the same until a
  measured signal replaces the static cost. No non-chat endpoints — the
  request vocabulary equals `GET /v1/models`.

## Development

```sh
make templ   # regenerate *_templ.go after editing *.templ (templ CLI; runtime v0.3.1020,
#            # generated with CLI v0.3.1001 — either recent CLI compiles the output)
make test    # go test -race ./...  (acceptance suite: acceptance/, ~2-4 min)
make vet
```

The acceptance suite mirrors the scenarios in
`specs/functional.specs.md`: one named test each (`TestAcceptance_N_…`),
driving real listeners against fake upstreams.
