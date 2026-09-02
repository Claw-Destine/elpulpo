package dashboard

import (
	"net/http"
	"net/url"
	"strconv"
	"time"

	"elpulpo/internal/usage"
)

// statsQuery is the parsed form of the contract's query parameters
// (period/start/end/host/server/endpoint/status/model/group_by/page/
// page_size/sort/dir). The stats view, both CSV exports and /api/usage/*
// all build their usage.Filter through here — displayed totals and export
// sums therefore share one code path (scenario 13).
type statsQuery struct {
	Period, Start, End string
	Hosts              []string
	Servers            []string
	Endpoints          []string
	Statuses           []string
	Model              string
	GroupBy            string
	Sort               string
	Desc               bool
	Page               int
	PageSize           int
	Filter             usage.Filter
}

func parseStatsQuery(r *http.Request) statsQuery {
	q := r.URL.Query()
	sq := statsQuery{
		Period:    q.Get("period"),
		Start:     q.Get("start"),
		End:       q.Get("end"),
		Hosts:     nonEmpty(q["host"]),
		Servers:   nonEmpty(q["server"]),
		Endpoints: nonEmpty(q["endpoint"]),
		Statuses:  nonEmpty(q["status"]),
		Model:     q.Get("model"),
		GroupBy:   q.Get("group_by"),
		Sort:      q.Get("sort"),
		Desc:      q.Get("dir") != "asc",
		Page:      1,
		PageSize:  50,
	}
	if sq.Period == "" {
		sq.Period = "all"
	}
	if v := atoi(q.Get("page")); v > 0 {
		sq.Page = v
	}
	if v := atoi(q.Get("page_size")); v > 0 && v <= 1000 {
		sq.PageSize = v
	}
	switch sq.GroupBy {
	case "model", "host", "server", "day":
	default:
		sq.GroupBy = "model"
	}
	switch sq.Sort {
	case "timestamp", "id", "host", "server", "model", "endpoint", "status",
		"http_status", "tokens_in", "tokens_out", "tokens_cached",
		"tokens_reasoning", "estimated", "latency_ms", "ttft_ms":
	default:
		sq.Sort = "timestamp"
	}

	now := time.Now()
	switch sq.Period {
	case "today":
		from, to := usage.TodayBounds(now)
		sq.Filter.FromMs, sq.Filter.ToMs = from, to
	case "month":
		from, to := usage.MonthBounds(now)
		sq.Filter.FromMs, sq.Filter.ToMs = from, to
	default: // "custom" (or "all" with explicit bounds) — inclusive local days
		if sq.Start != "" {
			if ms, ok := dayStart(sq.Start); ok {
				sq.Filter.FromMs = ms
			}
		}
		if sq.End != "" {
			if ms, ok := dayStart(sq.End); ok {
				sq.Filter.ToMs = ms + 24*60*60*1000 // end day inclusive
			}
		}
	}
	sq.Filter.Hosts = sq.Hosts
	sq.Filter.Servers = sq.Servers
	sq.Filter.Endpoints = sq.Endpoints
	sq.Filter.Statuses = sq.Statuses
	sq.Filter.ModelContains = sq.Model
	return sq
}

// canon re-encodes the query in canonical form for fragment polling and
// export links, so htmx re-requests and downloads carry the current state.
func (sq statsQuery) canon() string {
	v := url.Values{}
	if sq.Period != "" && sq.Period != "all" {
		v.Set("period", sq.Period)
	}
	if sq.Start != "" {
		v.Set("start", sq.Start)
	}
	if sq.End != "" {
		v.Set("end", sq.End)
	}
	for _, h := range sq.Hosts {
		v.Add("host", h)
	}
	for _, s := range sq.Servers {
		v.Add("server", s)
	}
	for _, e := range sq.Endpoints {
		v.Add("endpoint", e)
	}
	for _, s := range sq.Statuses {
		v.Add("status", s)
	}
	if sq.Model != "" {
		v.Set("model", sq.Model)
	}
	v.Set("group_by", sq.GroupBy)
	v.Set("page", strconv.Itoa(sq.Page))
	v.Set("page_size", strconv.Itoa(sq.PageSize))
	v.Set("sort", sq.Sort)
	if !sq.Desc {
		v.Set("dir", "asc")
	}
	return v.Encode()
}

// with returns the canonical query with overrides (pagination, sorting).
func (sq statsQuery) with(kvs ...string) string {
	v, _ := url.ParseQuery(sq.canon())
	for i := 0; i+1 < len(kvs); i += 2 {
		v.Set(kvs[i], kvs[i+1])
	}
	return v.Encode()
}

// sortFor returns the dir a column header link should request: same column
// toggles, a new column starts desc.
func (sq statsQuery) sortFor(col string) string {
	if sq.Sort == col {
		if sq.Desc {
			return "asc"
		}
		return "desc"
	}
	return "desc"
}

func dayStart(date string) (int64, bool) {
	d, err := time.ParseInLocation("2006-01-02", date, time.Local)
	if err != nil {
		return 0, false
	}
	start := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.Local)
	return start.UnixMilli(), true
}

func nonEmpty(vals []string) []string {
	var out []string
	for _, v := range vals {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
