package dashboard

import (
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"elpulpo/internal/config"
)

// The route form is the one place the dashboard zips repeated inputs into a
// structured value, and the one place a click carries a positional argument.
// Both are worth pinning here rather than only through the screen: the failures
// they can produce are silent saves, not errors.

// routeFields is what the route action sees when the screen posts a route form:
// bodyFields folds the repeated row inputs into one flat map, exactly as the
// handler receives them.
func routeFields(t *testing.T, form url.Values) map[string]string {
	t.Helper()
	r := httptest.NewRequest("POST", "/dashboard/action/route/save", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	fields, err := bodyFields(r)
	if err != nil {
		t.Fatalf("bodyFields: %v", err)
	}
	return fields
}

// parseForm runs one route form through the parser the action uses.
func parseForm(t *testing.T, form url.Values) (config.Route, error) {
	t.Helper()
	return parseRouteFields(routeFields(t, form))
}

// membersOf renders a route's members as "model/cost" for a compact
// expectation, with "1" standing for the default a blank field means.
func membersOf(rt config.Route) string {
	parts := make([]string, 0, len(rt.Members))
	for _, m := range rt.Members {
		cost := "1"
		if m.Cost != nil {
			cost = fmt.Sprintf("%g", *m.Cost)
		}
		parts = append(parts, m.Model+"/"+cost)
	}
	return strings.Join(parts, " ")
}

func routeForm(models, costs []string, action string) url.Values {
	v := url.Values{}
	v.Set("alias", "qwen")
	for i, m := range models {
		v.Add("model", m)
		if i < len(costs) {
			v.Add("cost", costs[i])
		}
	}
	if action != "" {
		k, val, _ := strings.Cut(action, "=")
		v.Set(k, val)
	}
	return v
}

func TestRouteFormReadsRowsByPosition(t *testing.T) {
	rt, err := parseForm(t, routeForm(
		[]string{"qwen3.8:27b-ollama@minion1", "qwen3.8:27b-ollama@minion2"},
		[]string{"", "3"}, ""))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got, want := membersOf(rt),
		"qwen3.8:27b-ollama@minion1/1 qwen3.8:27b-ollama@minion2/3"; got != want {
		t.Fatalf("rows zip by position, a blank cost keeps the default:\n got %s\nwant %s", got, want)
	}

	// A cleared model row drops out with the cost that sat beside it: the row
	// said nothing, so nothing of it is saved.
	rt, err = parseForm(t, routeForm(
		[]string{"a-ollama@minion1", "", "b-ollama@minion1"},
		[]string{"1", "2", "3"}, ""))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got, want := membersOf(rt), "a-ollama@minion1/1 b-ollama@minion1/3"; got != want {
		t.Fatalf("a blank row must not steal another row's cost:\n got %s\nwant %s", got, want)
	}
}

// The buttons in a row name the row by position, because htmx submits the
// edited inputs too: a button carrying the id the row used to hold would aim at
// a model the operator has already renamed, and the click would do nothing
// while the save still answered ok.
func TestRouteFormRowActionFollowsTheRowNotTheModel(t *testing.T) {
	// The screen showed [A, B]; the operator retyped row 1 to C and clicked that
	// row's Delete.
	rt, err := parseForm(t, routeForm(
		[]string{"a-ollama@minion1", "c-ollama@minion1"}, []string{"1", "1"}, "delete_member=1"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got, want := membersOf(rt), "a-ollama@minion1/1"; got != want {
		t.Fatalf("the deleted row is the row that was clicked:\n got %s\nwant %s", got, want)
	}

	// The same for a move: row 1, renamed, still moves.
	rt, err = parseForm(t, routeForm(
		[]string{"a-ollama@minion1", "c-ollama@minion1"}, []string{"1", "1"}, "move_up=1"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got, want := membersOf(rt), "c-ollama@minion1/1 a-ollama@minion1/1"; got != want {
		t.Fatalf("the moved row is the row whose button was clicked:\n got %s\nwant %s", got, want)
	}
}

func TestRouteFormRowActionsAreBounded(t *testing.T) {
	const two = "a-ollama@minion1/1 b-ollama@minion1/1"
	for _, action := range []string{"move_up=0", "move_down=1", "move_up=7", "move_down=-1",
		"move_up=not-a-row", "delete_member=9"} {
		rt, err := parseForm(t, routeForm(
			[]string{"a-ollama@minion1", "b-ollama@minion1"}, []string{"1", "1"}, action))
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		if got := membersOf(rt); got != two {
			t.Fatalf("%s must leave the route alone (a click from a screen that has since "+
				"changed names no row):\n got %s\nwant %s", action, got, two)
		}
	}
}

func TestRouteFormRejectsANonNumericCost(t *testing.T) {
	for _, cost := range []string{"0", "-2", "abc", "1,5"} {
		_, err := parseForm(t, routeForm([]string{"a-ollama@minion1"}, []string{cost}, ""))
		if err == nil {
			t.Fatalf("cost %q must be refused, not defaulted", cost)
		}
		if !strings.Contains(err.Error(), "cost must be a number greater than 0") {
			t.Fatalf("cost %q: wrong error %v", cost, err)
		}
	}
	// A cost on a row with no model is nobody's business: the row is not saved.
	if _, err := parseForm(t, routeForm([]string{"a-ollama@minion1", ""}, []string{"1", "nonsense"}, "")); err != nil {
		t.Fatalf("the blank add row must not be validated: %v", err)
	}
}

func TestRouteFormAcceptsARouteObject(t *testing.T) {
	fields := map[string]string{
		"route": `{"alias":"qwen","description":"any","models":[` +
			`{"model":"a-ollama@minion1","cost":2},{"model":"b-ollama@minion1"}]}`,
	}
	rt, err := parseRouteFields(fields)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if rt.Alias != "qwen" || rt.Description != "any" {
		t.Fatalf("route object fields lost: %+v", rt)
	}
	if got, want := membersOf(rt), "a-ollama@minion1/2 b-ollama@minion1/1"; got != want {
		t.Fatalf("a route posted as JSON must read the same:\n got %s\nwant %s", got, want)
	}
}
