package reliableproxy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gylive/ccodex-sleep-state/internal/cookiebundle"
	"github.com/gylive/ccodex-sleep-state/internal/routehealth"
)

const stateHeader = "X-Codex-Turn-State"
const retention = 30 * time.Minute // Local bookkeeping only, NOT token validity.
var errCircuit = errors.New("route circuit is open")

type runtimeRoute struct {
	id        string
	transport http.RoundTripper
}
type bindingKey struct {
	scope string
	state [32]byte
}
type binding struct {
	route   int
	touched time.Time
}
type accountGuard struct {
	authStatus          int
	pauseUntil, touched time.Time
	busy                int
}
type Engine struct {
	observationLimited                                                                  atomic.Uint64
	slots                                                                               chan struct{}
	config                                                                              Config
	target                                                                              *url.URL
	routes                                                                              []runtimeRoute
	health                                                                              *routehealth.Manager
	cookies                                                                             *cookiebundle.Store
	logger                                                                              *slog.Logger
	mu                                                                                  sync.Mutex
	bindings                                                                            map[bindingKey]binding
	accounts                                                                            map[string]*accountGuard
	cursor                                                                              int
	requests, dispatched, completed, incomplete, streamFailed, networkFailed, cancelled atomic.Uint64
	inflight                                                                            atomic.Int64
}

func New(c Config, logger *slog.Logger) (*Engine, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	target, _ := url.Parse(c.Upstream)
	e := &Engine{slots: make(chan struct{}, c.MaxConcurrentRequests), config: c, target: target, logger: logger, bindings: make(map[bindingKey]binding), accounts: make(map[string]*accountGuard), cookies: cookiebundle.New(128, time.Duration(c.SessionCookieSeconds)*time.Second)}
	e.health = routehealth.New(routehealth.Config{Threshold: c.FailureThreshold, Base: time.Duration(c.CircuitBaseSeconds) * time.Second, Maximum: time.Duration(c.CircuitMaxSeconds) * time.Second, Jitter: func(d time.Duration) time.Duration {
		n, err := rand.Int(rand.Reader, big.NewInt(201))
		if err != nil {
			return d
		}
		return d + time.Duration(n.Int64()-100)*d/1000
	}})
	for _, r := range c.Routes {
		tr := &http.Transport{DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext, ForceAttemptHTTP2: true, MaxIdleConns: 64, MaxIdleConnsPerHost: 8, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: time.Duration(c.ResponseHeaderSeconds) * time.Second, ExpectContinueTimeout: time.Second}
		if r.ProxyURL != "" {
			u, _ := url.Parse(r.ProxyURL)
			tr.Proxy = http.ProxyURL(u)
		}
		e.routes = append(e.routes, runtimeRoute{id: r.ID, transport: tr})
	}
	return e, nil
}
func (e *Engine) Close() {
	for _, r := range e.routes {
		if c, ok := r.transport.(interface{ CloseIdleConnections() }); ok {
			c.CloseIdleConnections()
		}
	}
}
func validState(s string) bool {
	if len(s) == 0 || len(s) > 4096 {
		return false
	}
	for _, c := range s {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}
func scopeFor(h http.Header) string {
	sum := sha256.Sum256([]byte(h.Get("Authorization") + "\x00" + h.Get("ChatGPT-Account-Id") + "\x00" + h.Get("OpenAI-Organization") + "\x00" + h.Get("OpenAI-Project")))
	return hex.EncodeToString(sum[:])
}
func (e *Engine) beginAccount(scope string) (int, int, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	for k, v := range e.accounts {
		if v.busy == 0 && v.authStatus == 0 && !now.Before(v.pauseUntil) && now.Sub(v.touched) > retention {
			delete(e.accounts, k)
		}
	}
	g := e.accounts[scope]
	if g == nil {
		if len(e.accounts) >= 128 {
			return 503, 1, false
		}
		g = &accountGuard{}
		e.accounts[scope] = g
	}
	g.touched = now
	if g.authStatus != 0 {
		return g.authStatus, 0, false
	}
	if now.Before(g.pauseUntil) {
		return 429, max(1, int(time.Until(g.pauseUntil).Seconds())+1), false
	}
	g.busy++
	return 0, 0, true
}
func (e *Engine) endAccount(scope string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if g := e.accounts[scope]; g != nil {
		g.busy--
		g.touched = time.Now()
	}
}
func (e *Engine) pause(scope string, status int, delay time.Duration) {
	if status != 401 && status != 403 && status != 429 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	g := e.accounts[scope]
	if g == nil {
		return
	}
	if status == 401 || status == 403 {
		g.authStatus = status
		return
	}
	if delay < 30*time.Second {
		delay = 30 * time.Second
	}
	until := time.Now().Add(delay)
	if until.After(g.pauseUntil) {
		g.pauseUntil = until
	}
}
func retryAfter(value string) time.Duration {
	if n, err := strconv.ParseInt(value, 10, 32); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil && time.Until(at) > 0 {
		return time.Until(at)
	}
	return 0
}
func (e *Engine) pruneBindings(now time.Time) {
	for k, b := range e.bindings {
		g := e.accounts[k.scope]
		if now.Sub(b.touched) > retention && (g == nil || g.busy == 0) {
			delete(e.bindings, k)
		}
	}
}
func (e *Engine) choose(scope, state string) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	e.pruneBindings(now)
	key := bindingKey{scope, sha256.Sum256([]byte(state))}
	if state != "" {
		if b, ok := e.bindings[key]; ok {
			// Never move an already-bound turn to a different proxy behind the client.
			if !e.health.Available(e.routes[b.route].id) {
				return 0, errCircuit
			}
			b.touched = now
			e.bindings[key] = b
			return b.route, nil
		}
		if len(e.bindings) >= 4096 {
			return 0, errors.New("binding capacity exhausted")
		}
	}
	for n := 0; n < len(e.routes); n++ {
		i := e.cursor % len(e.routes)
		e.cursor++
		if !e.health.Available(e.routes[i].id) {
			continue
		}
		if state != "" {
			e.bindings[key] = binding{route: i, touched: now}
		}
		return i, nil
	}
	return 0, errCircuit
}
func (e *Engine) bind(scope, state string, route int) {
	if !validState(state) {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	key := bindingKey{scope, sha256.Sum256([]byte(state))}
	if b, ok := e.bindings[key]; ok {
		if b.route == route {
			b.touched = time.Now()
			e.bindings[key] = b
		}
		return
	}
	e.pruneBindings(time.Now())
	if len(e.bindings) < 4096 {
		e.bindings[key] = binding{route: route, touched: time.Now()}
	}
}
func (e *Engine) Status() map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	rows := make([]map[string]any, 0, len(e.routes))
	for _, r := range e.routes {
		rows = append(rows, map[string]any{"id": r.id, "health": e.health.Status(r.id)})
	}
	return map[string]any{"mode": "client-managed-state", "synthetic_probes": false, "generation_replay": false, "token_validity": "owned by upstream; no local TTL assertion", "routes": rows, "cookie_scopes": e.cookies.Size(), "state_bindings": len(e.bindings), "requests": e.requests.Load(), "dispatched": e.dispatched.Load(), "completed": e.completed.Load(), "incomplete_streams": e.incomplete.Load(), "observation_limited": e.observationLimited.Load(), "upstream_stream_errors": e.streamFailed.Load(), "network_failures": e.networkFailed.Load(), "cancelled": e.cancelled.Load(), "inflight": e.inflight.Load()}
}
func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Reliable-Proxy-Error-Source", "local")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": "Request was not replayed. Inspect local health or resolve the upstream account response."}})
}
func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Host != e.config.Listen || r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" {
		writeError(w, 403, "local_clients_only")
		return
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() {
		writeError(w, 403, "loopback_only")
		return
	}
	if r.Method == "GET" && r.URL.Path == "/healthz" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(e.Status())
		return
	}
	if r.Header.Get("Upgrade") != "" {
		writeError(w, 426, "http_sse_required")
		return
	}
	generation := r.Method == "POST" && (r.URL.Path == "/backend-api/codex/responses" || r.URL.Path == "/backend-api/codex/responses/compact")
	allowed := generation || (r.Method == "GET" && r.URL.Path == "/backend-api/codex/models") || (r.Method == "POST" && r.URL.Path == "/backend-api/codex/alpha/search")
	if !allowed || r.URL.RawPath != "" {
		writeError(w, 404, "unsupported_endpoint")
		return
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") || len(strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))) < 8 || len(auth) > 16384 {
		writeError(w, 401, "authentication_required")
		return
	}
	state := r.Header.Get(stateHeader)
	if state != "" && !validState(state) {
		writeError(w, 400, "invalid_state_header")
		return
	}
	select {
	case e.slots <- struct{}{}:
		defer func() { <-e.slots }()
	default:
		writeError(w, 503, "local_concurrency_limit")
		return
	}
	scope := scopeFor(r.Header)
	e.requests.Add(1)
	status, delay, ok := e.beginAccount(scope)
	if !ok {
		if delay > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(delay))
		}
		writeError(w, status, "account_paused_or_capacity")
		return
	}
	defer e.endAccount(scope)
	selected, err := e.choose(scope, state)
	if err != nil {
		w.Header().Set("Retry-After", "1")
		writeError(w, 503, "route_unavailable")
		return
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, int64(e.config.MaxRequestMiB)<<20)
	}
	e.inflight.Add(1)
	defer e.inflight.Add(-1)
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = e.target.Scheme
			pr.Out.URL.Host = e.target.Host
			pr.Out.Host = e.target.Host
			pr.Out.URL.Path = strings.TrimRight(e.target.Path, "/") + strings.TrimPrefix(pr.In.URL.Path, "/backend-api/codex")
			pr.Out.URL.RawPath = ""
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Proxy-Authorization")
			// The state header is deliberately neither inserted nor replaced.
			pr.Out.Header.Set("Accept-Encoding", "identity")
			pr.Out.GetBody = nil // A dispatched POST cannot be rewound by this gateway.
		},
		Transport: e.transport(selected, scope), FlushInterval: -1, ErrorLog: log.New(io.Discard, "", 0),
		ModifyResponse: func(resp *http.Response) error { resp.Header.Del("Set-Cookie"); return nil },
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			var large *http.MaxBytesError
			switch {
			case errors.Is(err, errCircuit):
				w.Header().Set("Retry-After", "1")
				writeError(w, 503, "route_cooldown")
			case errors.As(err, &large):
				writeError(w, 413, "request_too_large")
			default:
				writeError(w, 502, "upstream_unavailable")
			}
		},
	}
	proxy.ServeHTTP(w, r)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func (e *Engine) transport(index int, scope string) http.RoundTripper {
	route := e.routes[index]
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		permit, ok := e.health.Begin(route.id)
		if !ok {
			return nil, errCircuit
		}
		started := time.Now()
		req = req.Clone(req.Context())
		req.Header = req.Header.Clone()
		req.GetBody = nil
		req.Header.Del("Cookie")
		cookieLease := e.cookies.Begin(cookiebundle.Scope{Credential: scope, Route: route.id}, req.URL)
		for _, c := range cookieLease.Cookies {
			req.AddCookie(c)
		}
		e.dispatched.Add(1)
		resp, err := route.transport.RoundTrip(req)
		if err != nil {
			outcome := routehealth.Failure
			kind := "transport_failure"
			var large *http.MaxBytesError
			if req.Context().Err() != nil {
				outcome = routehealth.Neutral
				kind = "client_cancelled"
				e.cancelled.Add(1)
			} else if errors.As(err, &large) {
				outcome = routehealth.Neutral
				kind = "local_request_too_large"
			} else {
				e.networkFailed.Add(1)
			}
			permit.Finish(outcome)
			e.logger.Info("request_finished", "route", route.id, "result", kind, "duration_ms", time.Since(started).Milliseconds())
			return nil, err
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			_ = resp.Body.Close()
			permit.Finish(routehealth.Success)
			e.logger.Info("request_finished", "route", route.id, "status", resp.StatusCode, "result", "unexpected_redirect")
			return nil, errors.New("upstream redirect was not followed")
		}
		e.cookies.Commit(cookieLease, req.URL, resp.Cookies())
		e.pause(scope, resp.StatusCode, retryAfter(resp.Header.Get("Retry-After")))
		responseState := resp.Header.Get(stateHeader)
		e.bind(scope, responseState, index)
		isSSE := strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
		encoding := resp.Header.Get("Content-Encoding")
		observedSSE := isSSE && (encoding == "" || strings.EqualFold(encoding, "identity"))
		body := &observedBody{source: resp.Body, idle: time.Duration(e.config.StreamIdleSeconds) * time.Second}
		if observedSSE {
			body.parser = &eventParser{emit: func(name string, data []byte) {
				parseEvent(name, data, &body.result, func(state string) { responseState = state; e.bind(scope, state, index) }, func(status int) { e.pause(scope, status, retryAfter(resp.Header.Get("Retry-After"))) })
			}}
		}
		body.onFinish = func(result streamResult, readErr error) {
			e.bind(scope, responseState, index)
			healthOutcome := routehealth.Neutral
			kind := "downstream_closed"
			switch {
			case req.Context().Err() != nil:
				e.cancelled.Add(1)
				kind = "client_cancelled"
			case readErr != nil && !errors.Is(readErr, io.EOF):
				healthOutcome = routehealth.Failure
				e.networkFailed.Add(1)
				kind = "transport_failure"
				if errors.Is(readErr, errStreamIdle) {
					kind = "upstream_idle_timeout"
				}
			case errors.Is(readErr, io.EOF):
				healthOutcome = routehealth.Success
				switch {
				case resp.StatusCode >= 400:
					kind = "upstream_http_error"
				case result.failed:
					kind = "upstream_stream_error"
					e.streamFailed.Add(1)
				case isSSE && (!observedSSE || result.oversized > 0):
					kind = "observation_limited"
					e.observationLimited.Add(1)
				case isSSE && !result.complete:
					kind = "incomplete_stream"
					e.incomplete.Add(1)
				default:
					kind = "completed"
					e.completed.Add(1)
				}
			}
			permit.Finish(healthOutcome)
			e.logger.Info("request_finished", "route", route.id, "status", resp.StatusCode, "result", kind, "duration_ms", time.Since(started).Milliseconds(), "cookie_version", cookieLease.Version, "oversized_events", result.oversized)
		}
		resp.Body = body
		return resp, nil
	})
}
