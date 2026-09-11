package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"elpulpo/internal/config"
	"elpulpo/internal/usage"
)

// routeAction is the single entry for every POST under /dashboard/action/.
// The CSRF marker is verified before anything is read or changed: a missing
// or mismatched token is a 403 and nothing mutates (scenario 41).
func (h *Handler) routeAction(w http.ResponseWriter, r *http.Request) {
	if !h.checkCSRF(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "csrf check failed"})
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/dashboard/action/")
	switch name {
	case "config/save":
		h.actionConfigSave(w, r)
	case "config/import":
		h.actionConfigImport(w, r)
	case "config/import/apply":
		h.actionConfigImportApply(w, r)
	case "host/save":
		h.actionHostSave(w, r)
	case "host/delete":
		h.actionHostDelete(w, r)
	case "server/save":
		h.actionServerSave(w, r)
	case "server/delete":
		h.actionServerDelete(w, r)
	case "prices/save":
		h.actionPricesSave(w, r)
	case "catalog/apply":
		h.actionCatalogApply(w, r)
	case "route/save":
		h.actionRouteSave(w, r)
	case "route/delete":
		h.actionRouteDelete(w, r)
	case "settings/save":
		h.actionSettingsSave(w, r)
	case "prune/run":
		h.actionPruneRun(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown action"})
	}
}

// --- outcomes ---------------------------------------------------------------

type violationsBody struct {
	Violations []config.Violation `json:"violations"`
}

func errBody(err error) map[string]string {
	return map[string]string{"error": err.Error()}
}

func errMsg(msg string) map[string]string {
	return map[string]string{"error": msg}
}

func badRequest(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusBadRequest, errBody(err))
}

func violationsJSON(w http.ResponseWriter, vs []config.Violation) {
	if vs == nil {
		vs = []config.Violation{}
	}
	writeJSON(w, http.StatusUnprocessableEntity, violationsBody{Violations: vs})
}

// saveOutcome maps a Store.Save result onto the contract's JSON shapes and
// nudges open dashboard pages to re-read state on success.
func (h *Handler) saveOutcome(w http.ResponseWriter, snap *config.Snapshot, err error) {
	if err != nil {
		if errors.Is(err, config.ErrStaleSave) {
			writeJSON(w, http.StatusConflict, errMsg("config changed on disk, reload first"))
			return
		}
		var ve *config.ValidationError
		if errors.As(err, &ve) {
			violationsJSON(w, ve.Violations)
			return
		}
		h.d.Log.Error("dashboard save failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, errBody(err))
		return
	}
	w.Header().Set("HX-Trigger", "elpulpo-changed")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "hash": snap.FileHash})
}

// saveCopy mutates a deep copy of the live config through fn and saves it
// with the caller's config hash as the stale-save guard.
func (h *Handler) saveCopy(w http.ResponseWriter, expectHash string, fn func(*config.Config)) {
	cfg := cloneConfig(h.d.Store.Current().Config)
	fn(cfg)
	config.Normalize(cfg)
	snap, err := h.d.Store.Save(cfg, expectHash)
	h.saveOutcome(w, snap, err)
}

// --- config/save, config/import, config/import/apply -------------------------

func (h *Handler) actionConfigSave(w http.ResponseWriter, r *http.Request) {
	fields, err := bodyFields(r)
	if err != nil {
		badRequest(w, fmt.Errorf("invalid body: %w", err))
		return
	}
	hp := fieldStr(fields, "h")
	if raw, ok := fields["yaml"]; ok {
		cfg, violations := h.d.Store.ValidateForImport([]byte(unquote(raw)))
		if len(violations) > 0 {
			violationsJSON(w, violations)
			return
		}
		config.Normalize(cfg)
		snap, err := h.d.Store.Save(cfg, hp)
		h.saveOutcome(w, snap, err)
		return
	}
	if _, ok := fields["hosts"]; !ok {
		badRequest(w, errors.New(`body must carry the config document ("hosts", optional "prices" and "loadbalancer", "h")`))
		return
	}
	doc := map[string]any{"hosts": jsonValue(fields["hosts"])}
	if raw, ok := fields["prices"]; ok && strings.TrimSpace(raw) != "" {
		doc["prices"] = jsonValue(raw)
	}
	if raw, ok := fields["loadbalancer"]; ok && strings.TrimSpace(raw) != "" {
		doc["loadbalancer"] = jsonValue(raw)
	}
	yb, err := yaml.Marshal(doc)
	if err != nil {
		badRequest(w, fmt.Errorf("config document is not encodable: %w", err))
		return
	}
	cfg, violations := h.d.Store.ValidateForImport(yb)
	if len(violations) > 0 {
		violationsJSON(w, violations)
		return
	}
	config.Normalize(cfg)
	snap, err := h.d.Store.Save(cfg, hp)
	h.saveOutcome(w, snap, err)
}

// jsonValue turns a raw field (JSON text or plain string) into a generic
// value with integral JSON numbers normalised to ints.
func jsonValue(raw string) any {
	var v any
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return raw
	}
	return normJSON(v)
}

func (h *Handler) actionConfigImport(w http.ResponseWriter, r *http.Request) {
	fields, err := bodyFields(r)
	if err != nil {
		badRequest(w, fmt.Errorf("invalid body: %w", err))
		return
	}
	raw, ok := fields["yaml"]
	if !ok {
		badRequest(w, errors.New(`body must be {"yaml": "..."}`))
		return
	}
	cfg, violations := h.d.Store.ValidateForImport([]byte(unquote(raw)))
	if len(violations) > 0 {
		violationsJSON(w, violations) // nothing is staged on a failure
		return
	}
	config.Normalize(cfg)
	id := newImportID()
	h.mu.Lock()
	h.imports[id] = &stagedImport{cfg: cfg, hash: h.d.Store.Current().FileHash}
	h.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"import_id": id,
		"preview":   previewImport(h.d.Store.Current().Config, cfg),
	})
}

func (h *Handler) actionConfigImportApply(w http.ResponseWriter, r *http.Request) {
	fields, err := bodyFields(r)
	if err != nil {
		badRequest(w, fmt.Errorf("invalid body: %w", err))
		return
	}
	hp := fieldStr(fields, "h")
	var cfg *config.Config
	if raw, ok := fields["yaml"]; ok && strings.TrimSpace(unquote(raw)) != "" {
		// The UI re-posts the same YAML: it revalidates and goes through
		// the same save path with the same hash guard.
		var violations []config.Violation
		cfg, violations = h.d.Store.ValidateForImport([]byte(unquote(raw)))
		if len(violations) > 0 {
			violationsJSON(w, violations)
			return
		}
	} else if id := fieldStr(fields, "import_id"); id != "" {
		h.mu.Lock()
		staged := h.imports[id]
		if staged != nil {
			delete(h.imports, id) // staged imports are one-shot and in-memory only
		}
		h.mu.Unlock()
		if staged == nil {
			writeJSON(w, http.StatusBadRequest, errMsg("import not found (staged imports live in memory only)"))
			return
		}
		cfg = staged.cfg
	} else {
		badRequest(w, errors.New(`body must carry "import_id" or "yaml"`))
		return
	}
	config.Normalize(cfg)
	snap, err := h.d.Store.Save(cfg, hp)
	h.saveOutcome(w, snap, err)
}

// previewImport compares two configurations host-by-host on the canonical
// YAML of each host, so the preview matches what the export shows.
func previewImport(live, next *config.Config) map[string][]string {
	liveHosts := map[string]config.Host{}
	for _, hs := range live.Hosts {
		liveHosts[hs.ID] = hs
	}
	nextHosts := map[string]config.Host{}
	for _, hs := range next.Hosts {
		nextHosts[hs.ID] = hs
	}
	var added, removed, changed []string
	for id := range nextHosts {
		lh, ok := liveHosts[id]
		if !ok {
			added = append(added, id)
			continue
		}
		if hostCanonical(lh) != hostCanonical(nextHosts[id]) {
			changed = append(changed, id)
		}
	}
	for id := range liveHosts {
		if _, ok := nextHosts[id]; !ok {
			removed = append(removed, id)
		}
	}
	// Load-balancer routes travel with the document, so an import that only
	// touched routes says what it changed instead of reporting nothing.
	addedR, removedR, changedR := previewRoutes(live.Routes(), next.Routes())
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changed)
	sort.Strings(addedR)
	sort.Strings(removedR)
	sort.Strings(changedR)
	return map[string][]string{
		"added_hosts":    orEmpty(added),
		"removed_hosts":  orEmpty(removed),
		"changed_hosts":  orEmpty(changed),
		"added_routes":   orEmpty(addedR),
		"removed_routes": orEmpty(removedR),
		"changed_routes": orEmpty(changedR),
	}
}

// previewRoutes compares routes by alias, on the canonical YAML of each route,
// so a member reorder or a cost edit reads as a change.
func previewRoutes(live, next []config.Route) (added, removed, changed []string) {
	liveR := map[string]config.Route{}
	for _, r := range live {
		liveR[r.Alias] = r
	}
	nextR := map[string]config.Route{}
	for _, r := range next {
		nextR[r.Alias] = r
	}
	for alias, nr := range nextR {
		lr, ok := liveR[alias]
		if !ok {
			added = append(added, alias)
			continue
		}
		if routeCanonical(lr) != routeCanonical(nr) {
			changed = append(changed, alias)
		}
	}
	for alias := range liveR {
		if _, ok := nextR[alias]; !ok {
			removed = append(removed, alias)
		}
	}
	return added, removed, changed
}

func routeCanonical(rt config.Route) string {
	return string(config.Canonical(&config.Config{
		LoadBalancer: &config.LoadBalancer{Routes: []config.Route{rt}},
	}))
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func hostCanonical(hs config.Host) string {
	return string(config.Canonical(&config.Config{Hosts: []config.Host{hs}}))
}

// --- hosts ------------------------------------------------------------------

func parseHostFields(fields map[string]string) (config.Host, error) {
	var host config.Host
	if raw, ok := fields["host"]; ok && strings.TrimSpace(raw) != "" {
		if err := decodeDocJSON(raw, &host); err != nil {
			return host, fmt.Errorf("host is not a valid host object: %w", err)
		}
		return host, nil
	}
	host.ID = strings.TrimSpace(fieldStr(fields, "id"))
	host.Description = fieldStr(fields, "description")
	if raw, ok := fields["addresses"]; ok {
		host.HostAddresses = splitLinesList(unquote(raw))
	} else if raw, ok := fields["host_addresses"]; ok {
		host.HostAddresses = splitLinesList(unquote(raw))
	}
	if raw, ok := fields["servers"]; ok && strings.TrimSpace(raw) != "" {
		if err := decodeDocJSON(raw, &host.Servers); err != nil {
			return host, fmt.Errorf("servers is not a valid server list: %w", err)
		}
	}
	return host, nil
}

func (h *Handler) actionHostSave(w http.ResponseWriter, r *http.Request) {
	fields, err := bodyFields(r)
	if err != nil {
		badRequest(w, fmt.Errorf("invalid body: %w", err))
		return
	}
	newHost, err := parseHostFields(fields)
	if err != nil {
		badRequest(w, err)
		return
	}
	original := strings.TrimSpace(fieldStr(fields, "original_id"))
	hp := fieldStr(fields, "h")
	formOnly := !isJSONHostBody(fields)

	h.saveCopy(w, hp, func(cfg *config.Config) {
		idx := -1
		for i, hs := range cfg.Hosts {
			if (original != "" && hs.ID == original) ||
				(original == "" && strings.EqualFold(hs.ID, newHost.ID)) {
				idx = i
				break
			}
		}
		// A meta-field form edit keeps the host's servers: they are CRUDed
		// per server. A JSON body replaces the whole host.
		if idx >= 0 && formOnly && len(newHost.Servers) == 0 {
			newHost.Servers = cfg.Hosts[idx].Servers
		}
		if idx >= 0 {
			cfg.Hosts[idx] = newHost
		} else {
			cfg.Hosts = append(cfg.Hosts, newHost)
		}
	})
}

func isJSONHostBody(fields map[string]string) bool {
	_, ok := fields["host"]
	return ok
}

func (h *Handler) actionHostDelete(w http.ResponseWriter, r *http.Request) {
	fields, err := bodyFields(r)
	if err != nil {
		badRequest(w, fmt.Errorf("invalid body: %w", err))
		return
	}
	id := strings.TrimSpace(fieldStr(fields, "id"))
	hp := fieldStr(fields, "h")
	if id == "" {
		badRequest(w, errors.New(`body must carry "id"`))
		return
	}
	found := false
	for _, hs := range h.d.Store.Current().Config.Hosts {
		if strings.EqualFold(hs.ID, id) {
			found = true
			break
		}
	}
	if !found {
		badRequest(w, fmt.Errorf("host %q does not exist", id))
		return
	}
	h.saveCopy(w, hp, func(cfg *config.Config) {
		out := cfg.Hosts[:0]
		for _, hs := range cfg.Hosts {
			if strings.EqualFold(hs.ID, id) {
				continue
			}
			out = append(out, hs)
		}
		cfg.Hosts = out
	})
}

// --- servers ----------------------------------------------------------------

func parseServerFields(fields map[string]string) (config.Server, error) {
	var srv config.Server
	if raw, ok := fields["server"]; ok && strings.TrimSpace(raw) != "" {
		if err := decodeDocJSON(raw, &srv); err != nil {
			return srv, fmt.Errorf("server is not a valid server object: %w", err)
		}
		return srv, nil
	}
	srv.ID = strings.TrimSpace(fieldStr(fields, "id"))
	srv.Description = fieldStr(fields, "description")
	srv.API = strings.TrimSpace(fieldStr(fields, "api"))
	srv.Scheme = strings.TrimSpace(fieldStr(fields, "scheme"))
	srv.AuthToken = fieldStr(fields, "auth_token")
	if p := atoi(fieldStr(fields, "port")); p != 0 {
		srv.Port = p
	}
	if mc := atoi(fieldStr(fields, "max_concurrency")); mc != 0 {
		srv.MaxConcurrency = mc
	}
	return srv, nil
}

func (h *Handler) actionServerSave(w http.ResponseWriter, r *http.Request) {
	fields, err := bodyFields(r)
	if err != nil {
		badRequest(w, fmt.Errorf("invalid body: %w", err))
		return
	}
	srv, err := parseServerFields(fields)
	if err != nil {
		badRequest(w, err)
		return
	}
	hostID := strings.TrimSpace(fieldStr(fields, "host_id"))
	original := strings.TrimSpace(fieldStr(fields, "original_id"))
	hp := fieldStr(fields, "h")

	live := h.d.Store.Current().Config
	hostIdx := -1
	for i, hs := range live.Hosts {
		if strings.EqualFold(hs.ID, hostID) {
			hostIdx = i
			break
		}
	}
	if hostIdx < 0 {
		badRequest(w, fmt.Errorf("host %q does not exist", hostID))
		return
	}
	h.saveCopy(w, hp, func(cfg *config.Config) {
		for i := range cfg.Hosts {
			if !strings.EqualFold(cfg.Hosts[i].ID, hostID) {
				continue
			}
			idx := -1
			for j, s := range cfg.Hosts[i].Servers {
				if (original != "" && s.ID == original) || (original == "" && s.ID == srv.ID) {
					idx = j
					break
				}
			}
			if idx >= 0 {
				cfg.Hosts[i].Servers[idx] = srv
			} else {
				cfg.Hosts[i].Servers = append(cfg.Hosts[i].Servers, srv)
			}
		}
	})
}

func (h *Handler) actionServerDelete(w http.ResponseWriter, r *http.Request) {
	fields, err := bodyFields(r)
	if err != nil {
		badRequest(w, fmt.Errorf("invalid body: %w", err))
		return
	}
	hostID := strings.TrimSpace(fieldStr(fields, "host_id"))
	id := strings.TrimSpace(fieldStr(fields, "id"))
	hp := fieldStr(fields, "h")
	if hostID == "" || id == "" {
		badRequest(w, errors.New(`body must carry "host_id" and "id"`))
		return
	}
	h.saveCopy(w, hp, func(cfg *config.Config) {
		for i := range cfg.Hosts {
			if !strings.EqualFold(cfg.Hosts[i].ID, hostID) {
				continue
			}
			out := cfg.Hosts[i].Servers[:0]
			for _, s := range cfg.Hosts[i].Servers {
				if s.ID == id {
					continue
				}
				out = append(out, s)
			}
			cfg.Hosts[i].Servers = out
		}
	})
}

// --- prices -----------------------------------------------------------------

// parsePriceSection builds the new prices section from either the contract's
// JSON shape ({"prices": {...}} or {"prices": null}) or the form the
// dashboard renders (currency + per-entry fields, optional delete_model
// button param). An empty section collapses to null — the canonical export
// then omits prices entirely (scenario 45).
func parsePriceSection(fields map[string]string) (*config.Prices, error) {
	if raw, ok := fields["prices"]; ok {
		t := strings.TrimSpace(raw)
		if t == "" || t == "null" {
			return nil, nil
		}
		var p config.Prices
		if err := decodeDocJSON(raw, &p); err != nil {
			return nil, fmt.Errorf("prices is not a valid price section: %w", err)
		}
		return &p, nil
	}
	if _, ok := fields["currency"]; !ok {
		if _, has := fields["delete_model"]; !has {
			return nil, errors.New(`body must carry "prices" (object or null), or "currency"/"models" fields`)
		}
	}
	if raw, ok := fields["models"]; ok && strings.HasPrefix(strings.TrimSpace(raw), "[") {
		var ms []config.ModelPrice
		if err := decodeDocJSON(raw, &ms); err != nil {
			return nil, fmt.Errorf("models is not a valid price list: %w", err)
		}
		return &config.Prices{Currency: fieldStr(fields, "currency"), Models: ms}, nil
	}
	// Dashboard row fields, zipped by position (multi-valued form fields).
	models := fieldAll(fields, "model")
	aliases := fieldAll(fields, "aliases")
	inputs := fieldAll(fields, "input")
	outputs := fieldAll(fields, "output")
	caches := fieldAll(fields, "cached_input")
	reasonings := fieldAll(fields, "reasoning_output")
	var entries []config.ModelPrice
	for i, modelName := range models {
		name := strings.TrimSpace(modelName)
		at := func(vals []string) string {
			if i < len(vals) {
				return vals[i]
			}
			return ""
		}
		in, okIn := parsePrice(at(inputs))
		out, okOut := parsePrice(at(outputs))
		cached, okCached := parsePrice(at(caches))
		reasoning, okReasoning := parsePrice(at(reasonings))
		if !okIn || !okOut || !okCached || !okReasoning {
			return nil, fmt.Errorf("price entry %d: rates must be numbers", i+1)
		}
		if name == "" {
			continue // blank (add) row
		}
		entries = append(entries, config.ModelPrice{
			Model: name, Aliases: splitLinesList(at(aliases)),
			Input: in, Output: out, CachedInput: cached, ReasoningOutput: reasoning,
		})
	}
	if dm := fieldStr(fields, "delete_model"); strings.TrimSpace(dm) != "" {
		target := strings.TrimSpace(dm)
		out := entries[:0]
		for _, e := range entries {
			if e.Model == target {
				continue
			}
			out = append(out, e)
		}
		entries = out
	}
	if len(entries) == 0 {
		return nil, nil
	}
	return &config.Prices{Currency: fieldStr(fields, "currency"), Models: entries}, nil
}

func (h *Handler) actionPricesSave(w http.ResponseWriter, r *http.Request) {
	fields, err := bodyFields(r)
	if err != nil {
		badRequest(w, fmt.Errorf("invalid body: %w", err))
		return
	}
	section, err := parsePriceSection(fields)
	if err != nil {
		badRequest(w, err)
		return
	}
	hp := fieldStr(fields, "h")
	h.saveCopy(w, hp, func(cfg *config.Config) {
		cfg.Prices = section // nil drops the section
	})
}

func (h *Handler) actionCatalogApply(w http.ResponseWriter, r *http.Request) {
	fields, err := bodyFields(r)
	if err != nil {
		badRequest(w, fmt.Errorf("invalid body: %w", err))
		return
	}
	model := strings.TrimSpace(fieldStr(fields, "model"))
	hp := fieldStr(fields, "h")
	if model == "" {
		badRequest(w, errors.New(`body must carry "model"`))
		return
	}
	matches := h.d.Catalogue.Match(model)
	if len(matches) == 0 {
		// Never a fuzzy guess (scenario 34).
		badRequest(w, fmt.Errorf("no exact catalogue match for %q", model))
		return
	}
	rate := matches[0]

	live := h.d.Store.Current().Config
	if live.Prices != nil && live.Prices.Currency != "" &&
		!strings.EqualFold(live.Prices.Currency, "USD") {
		// Scenario 36: no currency conversion, the USD rate is reference only.
		writeJSON(w, http.StatusBadRequest,
			errMsg("no currency conversion: catalogue rates are USD and the document currency is "+
				live.Prices.Currency+", enter the rates yourself"))
		return
	}
	overwrite := fieldBool(fields, "overwrite")
	ix := usage.NewPriceIndex(live.Prices)
	existing := ix.Lookup(model)
	if existing == nil {
		existing = ix.Lookup(rate.Model)
	}
	if existing != nil && !overwrite {
		writeJSON(w, http.StatusConflict,
			errMsg("a price entry for this name already exists; confirm overwrite to replace it"))
		return
	}

	h.saveCopy(w, hp, func(cfg *config.Config) {
		if cfg.Prices == nil {
			cfg.Prices = &config.Prices{Currency: "USD"}
		}
		// Replace the entry the live index resolved for this name (its model
		// or one of its aliases), keeping that entry's model and aliases.
		names := []string{model, rate.Model}
		matchesEntry := func(e *config.ModelPrice) bool {
			for _, n := range names {
				if e.Model == n {
					return true
				}
				for _, a := range e.Aliases {
					if a == n {
						return true
					}
				}
			}
			return false
		}
		apply := func(e *config.ModelPrice) {
			e.Input = copyFloat(rate.Input)
			e.Output = copyFloat(rate.Output)
			e.CachedInput = copyFloat(rate.CachedInput)
			e.ReasoningOutput = copyFloat(rate.ReasoningOutput)
		}
		for i := range cfg.Prices.Models {
			if matchesEntry(&cfg.Prices.Models[i]) {
				apply(&cfg.Prices.Models[i])
				return
			}
		}
		e := config.ModelPrice{Model: model}
		apply(&e)
		cfg.Prices.Models = append(cfg.Prices.Models, e)
	})
}

// --- load-balancer routes ----------------------------------------------------

// parseRouteFields builds one route from either the JSON shape
// ({"route": {"alias","description","models":[…]}}) or the flat form the
// dashboard renders: alias, description, and one `model` plus one `cost`
// field per member row, zipped by position. A blank model row is the empty
// "add member" row and is dropped. `delete_member`/`move_up`/`move_down`
// carry the model id of the row their button was clicked on.
func parseRouteFields(fields map[string]string) (config.Route, error) {
	var rt config.Route
	if raw, ok := fields["route"]; ok && strings.TrimSpace(raw) != "" {
		if err := decodeDocJSON(raw, &rt); err != nil {
			return rt, fmt.Errorf("route is not a valid route object: %w", err)
		}
		return rt, nil
	}
	rt.Alias = strings.TrimSpace(fieldStr(fields, "alias"))
	rt.Description = fieldStr(fields, "description")
	models := fieldAll(fields, "model")
	costs := fieldAll(fields, "cost")
	// One row per submitted model field, cost zipped by position — blank rows
	// kept, because a row's buttons address it by position and have to land on
	// the row the screen showed. Blanks are dropped after the row actions.
	rows := make([]config.RouteMember, 0, len(models))
	for i, m := range models {
		row := config.RouteMember{Model: strings.TrimSpace(unquote(m))}
		if row.Model != "" {
			costText := ""
			if i < len(costs) {
				costText = unquote(costs[i])
			}
			c, ok := parseCost(costText)
			if !ok {
				return rt, fmt.Errorf("member %q: cost must be a number greater than 0", row.Model)
			}
			row.Cost = c
		}
		rows = append(rows, row)
	}
	// A button in the member table acts on the row it sits in.
	if i, ok := rowIndex(fields, "delete_member", len(rows)); ok {
		rows = append(rows[:i], rows[i+1:]...)
	}
	if i, ok := rowIndex(fields, "move_up", len(rows)); ok {
		moveMember(rows, i, -1)
	}
	if i, ok := rowIndex(fields, "move_down", len(rows)); ok {
		moveMember(rows, i, +1)
	}
	for _, m := range rows {
		if m.Model == "" {
			continue
		}
		rt.Members = append(rt.Members, m)
	}
	return rt, nil
}

// rowIndex reads the value a member row's button carries: the row's position
// among the submitted rows. Absent means no button was clicked; a value that
// names no row — a form from a screen that has since changed — means no action,
// which is the only safe reading of a stale click.
func rowIndex(fields map[string]string, name string, n int) (int, bool) {
	s := strings.TrimSpace(fieldStr(fields, name))
	if s == "" {
		return 0, false
	}
	i, err := strconv.Atoi(s)
	if err != nil || i < 0 || i >= n {
		return 0, false
	}
	return i, true
}

// moveMember shifts row i one slot towards the front (delta -1) or the back
// (+1). Preference order has no JavaScript to drag with, so a row moves by one
// step per click, and a row already at an end stays put.
func moveMember(ms []config.RouteMember, i, delta int) {
	j := i + delta
	if i < 0 || i >= len(ms) || j < 0 || j >= len(ms) {
		return
	}
	ms[i], ms[j] = ms[j], ms[i]
}

// parseCost reads a cost field: absent or blank means the default of 1, which
// the canonical form materialises on save.
func parseCost(s string) (*float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, true
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return nil, false
	}
	return &f, true
}

func (h *Handler) actionRouteSave(w http.ResponseWriter, r *http.Request) {
	fields, err := bodyFields(r)
	if err != nil {
		badRequest(w, fmt.Errorf("invalid body: %w", err))
		return
	}
	route, err := parseRouteFields(fields)
	if err != nil {
		badRequest(w, err)
		return
	}
	if route.Alias == "" {
		badRequest(w, errors.New(`route needs an "alias"`))
		return
	}
	original := strings.TrimSpace(fieldStr(fields, "original_alias"))
	if original == "" {
		original = route.Alias
	}
	hp := fieldStr(fields, "h")
	h.saveCopy(w, hp, func(cfg *config.Config) {
		if cfg.LoadBalancer == nil {
			cfg.LoadBalancer = &config.LoadBalancer{}
		}
		idx := -1
		for i, rt := range cfg.LoadBalancer.Routes {
			if strings.EqualFold(rt.Alias, original) {
				idx = i
				break
			}
		}
		if idx >= 0 {
			cfg.LoadBalancer.Routes[idx] = route
		} else {
			cfg.LoadBalancer.Routes = append(cfg.LoadBalancer.Routes, route)
		}
	})
}

func (h *Handler) actionRouteDelete(w http.ResponseWriter, r *http.Request) {
	fields, err := bodyFields(r)
	if err != nil {
		badRequest(w, fmt.Errorf("invalid body: %w", err))
		return
	}
	alias := strings.TrimSpace(fieldStr(fields, "alias"))
	hp := fieldStr(fields, "h")
	if alias == "" {
		badRequest(w, errors.New(`body must carry "alias"`))
		return
	}
	found := false
	for _, rt := range h.d.Store.Current().Config.Routes() {
		if strings.EqualFold(rt.Alias, alias) {
			found = true
			break
		}
	}
	if !found {
		badRequest(w, fmt.Errorf("route %q does not exist", alias))
		return
	}
	h.saveCopy(w, hp, func(cfg *config.Config) {
		if cfg.LoadBalancer == nil {
			return
		}
		out := cfg.LoadBalancer.Routes[:0]
		for _, rt := range cfg.LoadBalancer.Routes {
			if strings.EqualFold(rt.Alias, alias) {
				continue
			}
			out = append(out, rt)
		}
		cfg.LoadBalancer.Routes = out
	})
}

// --- settings, prune ----------------------------------------------------------

var settingFieldNames = []string{
	"health_interval", "probe_timeout", "connect_timeout", "first_byte_timeout",
	"stream_idle_timeout", "total_timeout", "max_request_size", "queue_timeout",
	"retention_days",
}

func (h *Handler) actionSettingsSave(w http.ResponseWriter, r *http.Request) {
	fields, err := bodyFields(r)
	if err != nil {
		badRequest(w, fmt.Errorf("invalid body: %w", err))
		return
	}
	patch := map[string]string{}
	for _, k := range settingFieldNames {
		if raw, ok := fields[k]; ok {
			patch[k] = unquote(raw)
		}
	}
	_, violations := h.d.Settings.Update(r.Context(), patch)
	if len(violations) > 0 {
		violationsJSON(w, violations)
		return
	}
	w.Header().Set("HX-Trigger", "elpulpo-changed")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) actionPruneRun(w http.ResponseWriter, r *http.Request) {
	if h.d.Prune == nil {
		badRequest(w, errors.New("pruning is not available"))
		return
	}
	n, err := h.d.Prune()
	if err != nil {
		// Retention off (or any store problem): nothing was pruned.
		writeJSON(w, http.StatusBadRequest, errBody(err))
		return
	}
	w.Header().Set("HX-Trigger", "elpulpo-changed")
	writeJSON(w, http.StatusOK, map[string]any{"removed": n})
}
