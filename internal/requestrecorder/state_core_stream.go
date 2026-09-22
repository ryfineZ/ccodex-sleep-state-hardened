package requestrecorder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gylive/ccodex-sleep-state/internal/cookiebundle"
	"github.com/gylive/ccodex-sleep-state/internal/routehealth"
	"github.com/gylive/ccodex-sleep-state/internal/turnstate"
)

// coreParser observes a bounded SSE event while original bytes keep streaming.
// CR, LF and CRLF delimiters are accepted. An unterminated event is never emitted.
type coreParser struct {
	line, data            []byte
	name                  string
	total, lineBytes      int
	cr, dropping, limited bool
	emit                  func(string, []byte)
}

func (p *coreParser) feed(data []byte) {
	for _, ch := range data {
		if p.cr {
			p.cr = false
			if ch == '\n' {
				continue
			}
		}
		if ch == '\r' || ch == '\n' {
			p.endLine()
			p.cr = ch == '\r'
			continue
		}
		p.lineBytes++
		p.total++
		if p.total > eventLimit {
			p.dropping = true
			p.limited = true
			p.line = nil
			p.data = nil
		}
		if !p.dropping {
			p.line = append(p.line, ch)
		}
	}
}
func (p *coreParser) endLine() {
	if p.lineBytes == 0 {
		if !p.dropping && len(p.data) > 0 && p.emit != nil {
			p.emit(p.name, bytes.TrimSuffix(p.data, []byte{'\n'}))
		}
		p.name = ""
		p.data = nil
		p.line = nil
		p.total = 0
		p.dropping = false
		return
	}
	if !p.dropping {
		line := bytes.TrimPrefix(p.line, []byte{0xef, 0xbb, 0xbf})
		field, value, _ := bytes.Cut(line, []byte{':'})
		value = bytes.TrimPrefix(value, []byte{' '})
		switch string(field) {
		case "event":
			if len(value) <= 128 {
				p.name = string(value)
			}
		case "data":
			p.data = append(p.data, value...)
			p.data = append(p.data, '\n')
		}
	}
	p.line = nil
	p.lineBytes = 0
}

type coreStreamResult struct {
	complete, failed, limited, suspect bool
	state                              string
	stateConflict                      bool
	status                             int
}

func coreInspect(name string, data []byte, r *coreStreamResult, onHeaders func(http.Header), onPause func(int)) {
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return
	}
	var event map[string]json.RawMessage
	if json.Unmarshal(data, &event) != nil || event == nil {
		r.limited = true
		return
	}
	var kind string
	_ = json.Unmarshal(event["type"], &kind)
	if kind != "" {
		name = kind
	}
	switch name {
	case "response.metadata":
		h := headersFromEvent(data)
		if state := h.Get(turnstate.Header); state != "" {
			if r.state != "" && r.state != state {
				r.stateConflict = true
			}
			r.state = state
		}
		if onHeaders != nil {
			onHeaders(h)
		}
	case "response.completed":
		r.complete = true
	case "response.failed", "response.incomplete", "error":
		r.failed = true
		var v struct {
			Code  string `json:"code"`
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
			Response struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			} `json:"response"`
		}
		_ = json.Unmarshal(data, &v)
		code := v.Response.Error.Code
		if code == "" {
			code = v.Error.Code
		}
		if code == "" {
			code = v.Code
		}
		if code == "rate_limit_exceeded" || code == "insufficient_quota" {
			r.status = 429
			if onPause != nil {
				onPause(429)
			}
		}
	}
}

type coreProbeResult struct {
	candidate                       *coreCandidate
	token                           turnstate.Token
	httpStatus, status, cookieCount int
	retry                           time.Duration
	reason                          string
	health                          routehealth.Outcome
	terminal                        bool
}

func (c *StateCore) probe(parent context.Context, scope, model string, h http.Header, u *url.URL, route coreRoute, p CorePolicy) coreProbeResult {
	out := coreProbeResult{reason: "state_core_probe_network_failed", health: routehealth.Failure}
	ctx, cancel := context.WithTimeout(parent, time.Duration(p.ProbeSeconds)*time.Second)
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	defer cancel()
	body := map[string]any{"model": model, "instructions": "Reply with OK.", "input": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Reply with OK."}}}}, "stream": true, "store": false, "parallel_tool_calls": true, "include": []string{"reasoning.encrypted_content"}}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(raw))
	if err != nil {
		out.terminal = true
		out.health = routehealth.Neutral
		return out
	}
	req.GetBody = nil
	req.Header = coreHeaders(h)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := route.transport.RoundTrip(req)
	if err != nil {
		if parent.Err() != nil || c.ctx.Err() != nil {
			out.reason = "state_core_cancelled"
			out.health = routehealth.Neutral
			out.terminal = true
		}
		return out
	}
	defer resp.Body.Close()
	out.httpStatus = resp.StatusCode
	out.status = resp.StatusCode
	out.retry = coreRetry(resp.Header)
	out.health = routehealth.Success
	if resp.StatusCode != 200 {
		out.reason = "state_core_upstream_http_error"
		out.terminal = true
		return out
	}
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if ct != "text/event-stream" {
		out.reason = "state_core_stream_required"
		out.terminal = true
		return out
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if readErr != nil {
		out.health = routehealth.Failure
		out.reason = "state_core_probe_read_failed"
		if parent.Err() != nil || c.ctx.Err() != nil {
			out.health = routehealth.Neutral
			out.reason = "state_core_cancelled"
			out.terminal = true
		}
		return out
	}
	if len(data) > 1<<20 {
		out.reason = "state_core_probe_capture_limit"
		out.terminal = true
		return out
	}
	data, note := decodeBody(data, resp.Header.Get("Content-Encoding"))
	if note != "" {
		out.reason = "state_core_probe_" + note
		out.terminal = true
		return out
	}
	stream := coreStreamResult{state: resp.Header.Get(turnstate.Header)}
	cookies := resp.Cookies()
	partial := walkEvents(data, func(name string, data []byte, seq, end int, oversized bool) {
		if oversized {
			stream.limited = true
			return
		}
		coreInspect(name, data, &stream, func(h http.Header) { cookies = append(cookies, (&http.Response{Header: h}).Cookies()...) }, nil)
	})
	if stream.status != 0 {
		out.status = stream.status
	}
	if stream.failed {
		out.reason = "state_core_upstream_stream_error"
		out.terminal = true
		return out
	}
	if !stream.complete || stream.limited || partial {
		out.reason = "state_core_probe_incomplete"
		out.terminal = true
		return out
	}
	if stream.stateConflict {
		out.reason = "state_core_conflicting_states"
		out.terminal = true
		return out
	}
	t, err := turnstate.Parse(stream.state)
	out.token = t
	if err != nil {
		out.reason = "state_core_missing_or_invalid_state"
		return out
	}
	if t.Blocks != coreBlocks(p.AccountMode, h) {
		out.reason = "state_core_shape_mismatch"
		return out
	}
	now := c.now()
	deadline := t.Issued.Add(time.Duration(p.FreshSeconds) * time.Second)
	if t.Issued.After(now.Add(30*time.Second)) || !deadline.After(now) {
		out.reason = "state_core_state_time_rejected"
		return out
	}
	ceiling := now.Add(time.Duration(p.FreshSeconds) * time.Second)
	if deadline.After(ceiling) {
		deadline = ceiling
	}
	jar := cookiebundle.New(1, time.Duration(p.FreshSeconds)*time.Second)
	l := jar.Begin(cookiebundle.Scope{Credential: scope, Route: route.id}, u)
	jar.Commit(l, u, cookies)
	l = jar.Begin(cookiebundle.Scope{Credential: scope, Route: route.id}, u)
	out.cookieCount = len(l.Cookies)
	if out.cookieCount == 0 {
		out.reason = "state_core_cookie_missing"
		return out
	}
	names := map[string]bool{}
	for _, cookie := range l.Cookies {
		names[cookie.Name] = true
	}
	out.candidate = &coreCandidate{token: t, route: route, acquired: now, deadline: deadline, jar: jar, names: names}
	out.reason = "accepted"
	return out
}

// coreObservedBody never buffers a complete generation and never rewrites it.
// Close and EOF both finalize once, including ReverseProxy abort paths.
type coreObservedBody struct {
	idle     time.Duration
	source   io.ReadCloser
	mu       sync.Mutex
	closed   sync.Once
	finished bool
	parser   *coreParser
	result   coreStreamResult
	finish   func(coreStreamResult, error)
}

func (b *coreObservedBody) Read(p []byte) (int, error) {
	var timer *time.Timer
	var fired chan struct{}
	if b.idle > 0 {
		fired = make(chan struct{})
		timer = time.AfterFunc(b.idle, func() { b.closed.Do(func() { _ = b.source.Close() }); close(fired) })
	}
	n, err := b.source.Read(p)
	if timer != nil && !timer.Stop() {
		<-fired
		err = errIdle
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.finished {
		if n > 0 && b.parser != nil {
			b.parser.feed(p[:n])
			b.result.limited = b.result.limited || b.parser.limited
		}
		if err != nil {
			b.end(err)
		}
	}
	return n, err
}
func (b *coreObservedBody) end(err error) {
	if b.finished {
		return
	}
	b.finished = true
	if b.parser != nil && (len(b.parser.data) > 0 || len(b.parser.line) > 0 || b.parser.dropping) {
		b.result.limited = true
	}
	if b.finish != nil {
		b.finish(b.result, err)
	}
}
func (b *coreObservedBody) Close() error {
	b.closed.Do(func() { _ = b.source.Close() })
	b.mu.Lock()
	defer b.mu.Unlock()
	b.end(nil)
	return nil
}

func (c *StateCore) roundTrip(l *coreRequestLease, fallback http.RoundTripper) (*http.Response, error) {
	req := l.request
	scope := coreScope(req.Header, req.URL.Scheme+"://"+req.URL.Host)
	c.mu.Lock()
	stop := c.rejectionLocked(scope)
	c.mu.Unlock()
	if stop != nil {
		return nil, stop
	}
	tr := fallback
	var permit *routehealth.Lease
	if l.transport != nil {
		tr = l.transport
		var ok bool
		permit, ok = c.health.Begin(l.audit.Route)
		if !ok {
			return nil, coreUnavailable("state_core_route_cooldown", 30)
		}
	}
	if l.candidate != nil {
		c.mu.Lock()
		l.session.hits++
		c.mu.Unlock()
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		outcome := routehealth.Failure
		if req.Context().Err() != nil {
			outcome = routehealth.Neutral
		}
		permit.Finish(outcome)
		return nil, err
	}
	c.mu.Lock()
	watch := c.policy.Enabled || c.guards[scope] != nil
	c.mu.Unlock()
	if watch {
		c.reject(scope, resp.StatusCode, coreRetry(resp.Header))
	}
	if l.candidate == nil {
		return resp, nil
	}
	a, s := l.candidate, l.session
	a.jar.Commit(l.cookies, req.URL, resp.Cookies())
	inspectState := func(h http.Header, result *coreStreamResult) {
		if value := h.Get(turnstate.Header); value != "" {
			t, err := turnstate.Parse(value)
			if err != nil || t.Blocks != a.token.Blocks {
				result.suspect = true
			}
		}
	}
	body := &coreObservedBody{source: resp.Body, idle: l.idle}
	inspectState(resp.Header, &body.result)
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	enc := resp.Header.Get("Content-Encoding")
	isSSE := ct == "text/event-stream"
	if isSSE && (enc == "" || strings.EqualFold(enc, "identity")) {
		body.parser = &coreParser{emit: func(name string, data []byte) {
			coreInspect(name, data, &body.result, func(h http.Header) { inspectState(h, &body.result) }, func(status int) { c.reject(scope, status, coreRetry(resp.Header)) })
		}}
	} else if isSSE {
		body.result.limited = true
	}
	body.finish = func(result coreStreamResult, readErr error) {
		outcome := routehealth.Neutral
		if req.Context().Err() == nil {
			if errors.Is(readErr, io.EOF) {
				outcome = routehealth.Success
			} else if readErr != nil {
				outcome = routehealth.Failure
			}
		}
		permit.Finish(outcome)
		c.mu.Lock()
		defer c.mu.Unlock()
		if l.epoch != c.epoch || (s.active != a && s.ready != a) {
			return
		}
		switch {
		case req.Context().Err() != nil:
			s.lastResult = "state_core_client_cancelled"
		case outcome == routehealth.Failure:
			s.lastResult = "state_core_transport_failure"
		case result.failed:
			s.lastResult = "state_core_upstream_stream_error"
		case resp.StatusCode >= 400:
			s.lastResult = "state_core_upstream_http_error"
		case result.suspect:
			a.strikes++
			s.lastResult = "state_core_shape_suspect"
		case result.limited:
			s.lastResult = "state_core_observation_limited"
		case isSSE && !result.complete:
			s.lastResult = "state_core_incomplete_stream"
		case errors.Is(readErr, io.EOF):
			a.strikes = 0
			a.lastOK = c.now()
			s.lastResult = "injected_response_completed"
		default:
			s.lastResult = "state_core_downstream_closed"
		}
	}
	resp.Body = body
	return resp, nil
}
