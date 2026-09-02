# Dashboard implementation contract

This document is the binding API contract between `internal/app` and
`internal/dashboard`. The functional spec (`specs/functional.specs.md`,
"Dashboard" section) and acceptance scenarios decide anything ambiguous.

## Package surface (already fixed, do not change)

- `dashboard.New(d *Deps) *Handler`
- `(*Handler).Registered() http.Handler` — serves absolute paths; the app
  mounts it under both `/dashboard/` and `/api/` behind HTTP Basic auth
  (when a password is set) and a CSP middleware. `internal/app` owns those;
  the dashboard adds **no CORS headers ever** (scenario 44 is structural).
- `Deps{Store *config.Store, Settings *config.SettingsManager, Mgr
  *health.Manager, Repo *usage.Repo, Writer *usage.Writer, Catalogue
  *catalog.Catalogue, Open OpenInfo, Prune func() (int64,error), Log
  *slog.Logger}`

Key APIs to use:

```go
Store.Current() *config.Snapshot            // .Config, .FileHash ("" if no file)
Store.Save(cfg *Config, expectHash string)  // returns *config.ValidationError | config.ErrStaleSave
Store.ValidateForImport(b []byte) (*Config, []config.Violation)
Store.LastErr() string                      // failed hand-edit shown on every page
config.Canonical(cfg) []byte                // byte-stable export
Manager.States() []health.StateView
Settings.Get() *config.Settings
Settings.Update(ctx, patch map[string]string) (*config.Settings, []config.Violation)
Repo.Rows / Count / Totals / Summary / WriteRowsCSV / WriteSummaryCSV / Distinct
usage.NewPriceIndex(cfg.Prices) / ix.Lookup / usage.AmountTokens
Catalogue.File.Catalogue.{Version,AsOf,Currency,Rates}, Catalogue.Stale, Catalogue.Match(name)
```

## CSRF + auth marker

Basic auth is sent by the browser without consent, so **every POST under
`/dashboard/action/` additionally requires** header `X-CSRF-Token` equal to
the `elpulpo_csrf` cookie (`SameSite=Strict; Path=/; HttpOnly`, issued on
every dashboard GET when absent, 32 hex chars via crypto/rand). Constant-time
compare. Missing/mismatched → `403 {"error":"csrf check failed"}` and nothing
changes (scenario 41). Pages embed the token in
`<body hx-headers='{"X-CSRF-Token": "<token>"}'>` so every htmx request
carries it. `GET`s never mutate. `/api/*` is read-only; no CSRF needed.

## Query parameters (stats + both CSV exports)

`period=today|month|custom` (today/month via `usage.TodayBounds/MonthBounds`
in `time.Local`), `start=YYYY-MM-DD`, `end=YYYY-MM-DD` (inclusive days,
local), `host`/`server`/`endpoint`/`status` repeatable exact filters,
`model=<substring>`, `group_by=model|host|server|day`, `page` (1-based),
`page_size` (default 50), `sort` (table column, default `timestamp`),
`dir=asc|desc` (default desc). Build a `usage.Filter` from these; the
displayed totals must equal the sums over an exported CSV — one code path.

## Pages (templ; layout with nav Servers / Prices / Statistics / Settings)

CSP is `default-src 'self'` — **no inline JS anywhere**, behaviour via htmx
attributes only. Assets (embedded via `go:embed`):
`/dashboard/static/htmx.min.js` (already vendored), `/dashboard/static/app.css`.

Persistent banner when `Open.ProxyOpen` or `Open.DashOpen` (every start,
scenario 42) and when `Store.LastErr() != ""` (scenario 39).

- `GET /dashboard/` → 302 `/dashboard/servers`
- `GET /dashboard/servers` — hosts and servers from `Mgr.States()` with
  up/down, `active_address` flagged when a fallback (not first configured
  address), model count, in-flight, last probe, last error; add/edit/delete
  forms (see actions); YAML export link + import panel with
  added/removed/changed preview + confirm. Refresh: `hx-get="/dashboard/part/servers"` `hx-trigger="every 5s"` `hx-swap="outerHTML"`.
- `GET /dashboard/prices` — currency + entries (model, aliases, 4 rates);
  base model names in usage matching no entry listed as **no price set**
  (from `Repo.Summary` over all time, `NoPriceNames`); catalogue panel:
  version, `as_of`, `stale` label when `Catalogue.Stale`, rates table, and
  per no-price model an exact catalogue match (`Catalogue.Match`, case-insensitive)
  offered with an Apply button (overwrite confirm checkbox).
- `GET /dashboard/stats` — filters, period presets + custom range, grouped
  summary (group_by selector), totals row (estimated rows flagged, amounts
  when prices exist, `no price set` list), rows table with pagination and
  sortable columns; estimated rows visually distinct; retention note when
  `retention_days > 0` ("history beyond N days is not available"); links to
  both CSV exports carrying the current filters. Fragment `GET /dashboard/part/stats`
  re-rendered by htmx every 5 s.
- `GET /dashboard/settings` — every knob (durations as "30s" strings, size
  as "32MiB", `retention_days` int, `total_timeout` 0 = off) with
  `hx-post="/dashboard/action/settings/save"`; field errors re-rendered
  inline. Read-only env-credential display (`ELPULPO_PROXY_TOKEN`,
  `ELPULPO_DASHBOARD_USER/PASSWORD`: set/unset only — never values),
  warning when unset; "pruning is irreversible" note; "Run prune now" button.
- Fragments: `GET /dashboard/part/servers|prices|stats|settings`.

## Actions (POST; accept JSON body or form fields; JSON responses)

All config-mutating actions carry the live `FileHash` as field/param `h`
(`Store.Current().FileHash`; "" when no file). Outcomes:
`200 {"ok":true,"hash":"<new>"}` · `422 {"violations":[{"path","line","msg"}]}` ·
`409 {"error":"config changed on disk, reload first"}` (stale) ·
`400/500 {"error":"..."}`.

| route | body | behaviour |
| --- | --- | --- |
| `/dashboard/action/config/save` | full config doc JSON (`hosts`, optional `prices`) + `h` | `Store.Save` |
| `/dashboard/action/config/import` | `{"yaml": "..."}` | validate + preview `{"import_id","preview":{"added_hosts":[],"removed_hosts":[],"changed_hosts":[]}}` (compare vs live by canonical YAML per host); violations → 422, nothing staged |
| `/dashboard/action/config/import/apply` | `{"import_id","h"}` | apply staged import through the same save path (hash guard). Staged imports live in memory only |
| `/dashboard/action/host/save` | `{"host":{...},"original_id":""}` + `h` | upsert host in live config, save |
| `/dashboard/action/host/delete` | `{"id","h"}` | |
| `/dashboard/action/server/save` | `{"host_id","server":{...},"original_id":""}` + `h` | upsert server within host |
| `/dashboard/action/server/delete` | `{"host_id","id","h"}` | |
| `/dashboard/action/prices/save` | `{"prices":{"currency","models"} or null}` + `h` | replace prices section (null → drop section; canonical export then omits `prices`) |
| `/dashboard/action/catalog/apply` | `{"model":"<base name>","overwrite":bool,"h"}` | see below |
| `/dashboard/action/settings/save` | settings fields (see `config.SettingsManager.Update`) | `Update`; violations → 422 |
| `/dashboard/action/prune/run` | — | `Deps.Prune()` → `{"removed":N}`; retention off → 400 |

**catalog/apply:** match `model` via `Catalogue.Match` (case-insensitive
exact); no match → 400 (never a fuzzy guess, scenario 34). If the document
currency exists and is not USD → 400 "no currency conversion" (scenario 36).
If a price entry already exists for that name (or its aliases) and
`overwrite != true` → 409 (scenario 35). Otherwise write rates into
`config.Prices` (section created with `currency: USD` when absent) and save.

## Exports

- `GET /dashboard/export/config.yaml` → `config.Canonical(Store.Current().Config)`,
  `Content-Disposition: attachment; filename=elpulpo.yaml`, `text/yaml`.
- `GET /dashboard/export/rows.csv?<query>` → `Repo.WriteRowsCSV` (streamed,
  RFC 4180 quoting, UTC ISO timestamps, header-only when empty).
- `GET /dashboard/export/summary.csv?<query>` → `Repo.WriteSummaryCSV`.
  Both `Content-Disposition: attachment`, `text/csv; charset=utf-8`.

## JSON API (GET, read-only, mounted at `/api/*`)

- `/api/health` → `{"ok":true,"version":"v1"}`
- `/api/config` → `{"config":<doc>,"hash":"<FileHash>"}`
- `/api/settings` → settings JSON + `{"open":{...}}` from `Deps.Open`
- `/api/servers` → `Mgr.States()`
- `/api/usage/rows?<query>` → `{"rows":[...],"total":N,"page":P,"page_size":S,"totals":{...}}`
- `/api/usage/summary?<query>` → `Repo.Summary` JSON (with `currency`,
  `no_price_models`, grand total)
- `/api/catalogue` → `{"version","as_of","currency","stale","rates":[...]}`

## templ workflow

Files under `internal/dashboard/`, then
`~/go/bin/templ generate` (CLI v0.3.1001, runtime already pinned). Keep
plain-Go helpers in `*.go`; templates in `*.templ`. `go build ./...` and
`go vet ./...` must pass with `GOPATH=/tmp/gopath GOMODCACHE=/tmp/gopath/pkg/mod GOCACHE=/tmp/gocache`.
