package proxy

import (
	"time"

	"elpulpo/internal/config"
	"elpulpo/internal/health"
	"elpulpo/internal/usage"
)

// debugCaptureLimit caps each stored side (context, response) so recordings
// cannot grow the database without bound. Buffered JSON is cut at the limit
// without a marker (staying parseable as often as possible); a truncated SSE
// stream instead ends with a comment line, which SSE readers ignore.
const debugCaptureLimit = 8 << 20

// recorder collects the two halves of one debug recording — the context the
// client sent and the answer that came back — for a request the debug
// settings selected. Buffered responses land via setBody, streams frame by
// frame via appendFrame; finish submits the capture wherever the request
// terminated.
type recorder struct {
	created  time.Time
	asked    string // model id exactly as the client spelled it
	route    string // alias it came through, "" for a direct id
	stream   bool
	host     string
	server   string
	req      string // request body, verbatim, cut at the capture limit
	resp     []byte // buffered response body
	frames   []byte // collected SSE frames
	truncReq bool
	truncRsp bool
}

// recorderFor returns a recorder holding the request body when the live
// settings select this request: debug recording on and not expired, and
// either no model filter, or a filter naming what the client asked for or
// where the request landed.
// Returns nil otherwise, so every capture branch on the request path sits
// behind one cheap nil check.
func (p *Proxy) recorderFor(c *call, target *health.Target, body []byte, set *config.Settings) *recorder {
	if p.deps.Debug == nil || !set.DebugOn(time.Now()) {
		return nil
	}
	if f := set.DebugModel; f != "" && f != c.asked && f != target.Published {
		return nil
	}
	r := &recorder{
		created: c.start, asked: c.asked, route: c.route, stream: c.stream,
		host: target.HostID, server: target.ServerID,
	}
	if len(body) > debugCaptureLimit {
		body, r.truncReq = body[:debugCaptureLimit], true
	}
	cp := make([]byte, len(body))
	copy(cp, body)
	r.req = string(cp)
	return r
}

// setBody stores a buffered response body (also used for upstream error
// bodies, which the client sees verbatim or as the 502 El Pulpo writes).
func (r *recorder) setBody(b []byte) {
	if r == nil || r.resp != nil || r.frames != nil {
		return
	}
	if len(b) > debugCaptureLimit {
		b, r.truncRsp = b[:debugCaptureLimit], true
	}
	r.resp = b
}

// appendFrame accumulates the raw SSE lines of a stream, up to the cap.
func (r *recorder) appendFrame(line []byte) {
	if r == nil || r.resp != nil || r.truncRsp {
		return
	}
	if len(r.frames)+len(line) > debugCaptureLimit {
		r.truncRsp = true
		r.frames = append(r.frames, []byte("\n: el-pulpo recording truncated\n\n")...)
		return
	}
	r.frames = append(r.frames, line...)
}

// capture renders what was recorded into the row the debug writer commits.
func (r *recorder) capture(status string, httpStatus int, lat, in, out int64) *usage.DebugCapture {
	if r == nil {
		return nil
	}
	c := &usage.DebugCapture{
		TsMs: r.created.UnixMilli(), HostID: r.host, ServerID: r.server,
		Model: r.asked, Route: r.route, Stream: r.stream,
		Status: status, HTTPStatus: httpStatus, LatencyMs: lat,
		TokensIn: in, TokensOut: out,
		RequestJSON: r.req,
	}
	switch {
	case r.frames != nil:
		c.ResponseSSE = string(r.frames)
	case r.resp != nil:
		c.ResponseJSON = string(r.resp)
		c.HasResponse = true
	}
	return c
}
