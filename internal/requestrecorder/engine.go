package requestrecorder

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Engine struct {
	overrideMu sync.RWMutex
	override   ModelOverridePolicy
	config     Config
	target     *url.URL
	transport  http.RoundTripper
	store      *Store
	token      string
	enabled    atomic.Bool
	active     atomic.Int64
	slots      chan struct{}
}

func New(c Config) (*Engine, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	store, err := NewStore(c)
	if err != nil {
		return nil, err
	}
	target, _ := url.Parse(c.Upstream)
	tr := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: time.Duration(c.HeaderTimeoutSeconds) * time.Second, ExpectContinueTimeout: time.Second, DisableCompression: true, DisableKeepAlives: true, ForceAttemptHTTP2: false}
	if c.ProxyURL != "" {
		p, _ := url.Parse(c.ProxyURL)
		tr.Proxy = http.ProxyURL(p)
	}
	e := &Engine{config: c, target: target, transport: tr, store: store, token: rand.Text(), slots: make(chan struct{}, c.MaxConcurrent)}
	e.override = ModelOverridePolicy{Enabled: c.ForceModelEnabled, Model: c.ForceModel, Revision: 1}
	e.enabled.Store(true)
	return e, nil
}
func (e *Engine) Token() string { return e.token }
func (e *Engine) Close() {
	if t, ok := e.transport.(interface{ CloseIdleConnections() }); ok {
		t.CloseIdleConnections()
	}
	e.store.Close()
}
func (e *Engine) Status() map[string]any {
	out := e.store.Status()
	out["recording"] = e.enabled.Load()
	out["model_override"] = e.ModelOverride()
	out["mode"] = e.config.Mode
	out["inflight"] = e.active.Load()
	out["upstream_origin"] = e.config.Upstream
	out["actual_execution_verified"] = false
	return out
}
func responseJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func localError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("X-Recorder-Error-Source", "local")
	responseJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": "The recorder did not retry this request."}})
}
func (e *Engine) local(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	return err == nil && ip != nil && ip.IsLoopback() && r.Host == e.config.Listen
}
func (e *Engine) allowed(path string) bool {
	if strings.Contains(path, "..") || strings.ContainsAny(path, "\\\r\n") {
		return false
	}
	for _, p := range e.config.AllowedPaths {
		if strings.HasSuffix(p, "/") {
			if strings.HasPrefix(path, p) {
				return true
			}
		} else if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}
func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !e.local(r) {
		localError(w, 403, "loopback_only")
		return
	}
	if r.URL.Path == "/healthz" && r.Method == "GET" {
		responseJSON(w, 200, map[string]any{"service": "ccodex-request-recorder", "recording": e.enabled.Load()})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/__recorder") {
		e.admin(w, r)
		return
	}
	if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" {
		localError(w, 403, "native_clients_only")
		return
	}
	if r.Method == "CONNECT" || r.Header.Get("Upgrade") != "" {
		localError(w, 426, "http_sse_only_no_connect")
		return
	}
	if r.Method == "TRACE" || !e.allowed(r.URL.Path) || strings.HasPrefix(r.RequestURI, "http:") || strings.HasPrefix(r.RequestURI, "https:") {
		localError(w, 404, "capture_path_not_allowed")
		return
	}
	if r.ContentLength > int64(e.config.MaxRequestMiB)<<20 {
		localError(w, 413, "request_too_large_not_sent")
		return
	}
	select {
	case e.slots <- struct{}{}:
		defer func() { <-e.slots }()
	default:
		localError(w, 503, "local_concurrency_limit")
		return
	}
	policy := e.ModelOverride() // immutable admission-time snapshot
	recording := e.enabled.Load()
	limit := 0
	if recording {
		limit = e.config.CaptureMiB << 20
	}
	started := time.Now()
	a, b := newSample(started, limit), newSample(started, limit)
	id := ""
	if recording {
		raw := make([]byte, 16)
		_, _ = rand.Read(raw)
		id = hex.EncodeToString(raw)
		w.Header().Set("X-Recorder-Request-Id", id)
	}
	rec := Record{Context: contextFrom(r.Header, e.config.RouteLabel), Schema: 1, ID: id, Started: started.UTC(), Mode: e.config.Mode, Method: r.Method, URI: recordedURI(r.URL, e.config.Mode == "full"), Upstream: e.config.Upstream, IncomingHeaders: r.Header.Clone(), Outcome: "http_complete"}
	rec.ModelOverride = ModelOverrideAudit{Policy: policy, Eligible: modelOverrideEndpoint(r.Method, r.URL.Path)}
	var incoming *captured
	var errMu sync.Mutex
	var failure string
	setFailure := func(kind string) {
		errMu.Lock()
		if failure == "" {
			failure = kind
		}
		errMu.Unlock()
	}
	tracked := &statusWriter{ResponseWriter: w, status: 200, onError: func() { setFailure("downstream_write_error") }}
	e.active.Add(1)
	defer e.active.Add(-1)
	defer func() {
		request, response := a.freeze(), b.freeze()
		if !recording {
			return
		}
		rec.DurationMS = time.Since(started).Milliseconds()
		rec.ClientStatus = tracked.status
		errMu.Lock()
		rec.Outcome = failure
		errMu.Unlock()
		if r.Context().Err() != nil {
			rec.Outcome = "client_cancelled"
		}
		if rec.Outcome == "" {
			if response.EOF {
				rec.Outcome = "http_complete"
			} else {
				rec.Outcome = "response_not_fully_observed"
			}
		}
		e.store.Submit(pending{record: rec, request: request, response: response, incoming: incoming})
	}()
	if policy.Enabled && rec.ModelOverride.Eligible {
		rewritten, original, audit, err := e.prepareModelOverride(tracked, r, policy, started, limit)
		incoming = &original
		rec.ModelOverride = audit
		if err != nil {
			setFailure(err.Code)
			localError(tracked, err.Status, err.Code)
			return
		}
		r = rewritten
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = e.target.Scheme
			pr.Out.URL.Host = e.target.Host
			pr.Out.Host = e.target.Host
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Header.Del("Proxy-Authorization")
			pr.Out.Header.Del("X-Recorder-Token")
			pr.Out.GetBody = nil
			// Model override is opt-in and already snapshotted; state/Cookie stay untouched.
		},
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			rec.RequestHeaders = req.Header.Clone()
			rec.OutgoingURI = recordedURI(req.URL, e.config.Mode == "full")
			if req.Body != nil {
				req.Body = &tap{source: http.MaxBytesReader(tracked, req.Body, int64(e.config.MaxRequestMiB)<<20), sample: a, onError: func(error) { setFailure("request_read_error") }}
			} else {
				a.feed(nil, io.EOF)
			}
			resp, err := e.transport.RoundTrip(req)
			if err != nil {
				setFailure("upstream_transport_error")
				return nil, err
			}
			rec.Status = resp.StatusCode
			rec.HeadersMS = time.Since(started).Milliseconds()
			rec.ResponseHeaders = resp.Header.Clone()
			rec.Context.response(resp.Header)
			resp.Body = &tap{source: resp.Body, sample: b, idle: time.Duration(e.config.IdleSeconds) * time.Second, onError: func(err error) {
				if errors.Is(err, errIdle) {
					setFailure("upstream_idle_timeout")
				} else {
					setFailure("upstream_body_read_error")
				}
			}}
			return resp, nil
		}),
		FlushInterval: -1, ErrorLog: log.New(io.Discard, "", 0),
		ModifyResponse: func(resp *http.Response) error {
			if recording {
				resp.Header.Set("X-Recorder-Request-Id", id)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			var large *http.MaxBytesError
			if errors.As(err, &large) {
				localError(w, 413, "request_limit_during_forwarding")
			} else {
				localError(w, 502, "upstream_unavailable")
			}
		},
	}
	proxy.ServeHTTP(tracked, r)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type statusWriter struct {
	http.ResponseWriter
	status  int
	wrote   bool
	onError func()
}

func (w *statusWriter) WriteHeader(s int) {
	if !w.wrote {
		w.status = s
		w.wrote = true
		w.ResponseWriter.WriteHeader(s)
	}
}
func (w *statusWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(200)
	}
	n, e := w.ResponseWriter.Write(p)
	if e != nil && w.onError != nil {
		w.onError()
	}
	return n, e
}
func (w *statusWriter) FlushError() error {
	if !w.wrote {
		w.WriteHeader(200)
	}
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if err != nil && w.onError != nil {
		w.onError()
	}
	return err
}
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
