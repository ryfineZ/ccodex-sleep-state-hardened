package reliableproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gylive/ccodex-sleep-state/internal/routehealth"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const fixtureBody = `{"model":"client-selected-model","input":"private user prompt","stream":true}`
const doneEvent = "data: {\"type\":\"response.completed\"}\n\n"

func fixture(t *testing.T, h http.HandlerFunc) (*Engine, *bytes.Buffer) {
	t.Helper()
	up := httptest.NewServer(h)
	t.Cleanup(up.Close)
	c := DefaultConfig()
	c.Upstream = up.URL + "/backend-api/codex"
	logs := new(bytes.Buffer)
	e, err := New(c, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e, logs
}
func req(e *Engine, state, credential string) *http.Request {
	r := httptest.NewRequest("POST", "http://"+e.config.Listen+"/backend-api/codex/responses", strings.NewReader(fixtureBody))
	r.RemoteAddr = "127.0.0.1:32100"
	r.Header.Set("Authorization", "Bearer "+credential)
	r.Header.Set("Content-Type", "application/json")
	if state != "" {
		r.Header.Set(stateHeader, state)
	}
	return r
}
func send(e *Engine, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	e.ServeHTTP(w, r)
	return w
}
func sse(w http.ResponseWriter, data string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, data)
}
func TestClientOwnsOpaqueStateAndNoSyntheticRequests(t *testing.T) {
	var calls atomic.Int32
	e, logs := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if string(body) != fixtureBody {
			t.Error("body changed")
		}
		if r.Header.Get(stateHeader) != "opaque-client-owned-value" {
			t.Error("state replaced or probe sent")
		}
		w.Header().Set(stateHeader, "new-server-owned-value")
		sse(w, doneEvent)
	})
	w := send(e, req(e, "opaque-client-owned-value", "synthetic-secret-key"))
	if w.Code != 200 || calls.Load() != 1 || w.Body.String() != doneEvent || w.Header().Get(stateHeader) != "new-server-owned-value" {
		t.Fatal(w.Code, calls.Load(), w.Body.String())
	}
	for _, secret := range []string{"synthetic-secret-key", "private user prompt", "opaque-client-owned-value", "new-server-owned-value"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("secret logged")
		}
	}
	if e.completed.Load() != 1 {
		t.Fatal("completion not observed")
	}
}
func TestNoStateInventedForNewTurn(t *testing.T) {
	var calls atomic.Int32
	e, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get(stateHeader) != "" {
			t.Error("injected previous turn state")
		}
		w.Header().Set(stateHeader, "server-token")
		sse(w, doneEvent)
	})
	for i := 0; i < 2; i++ {
		if w := send(e, req(e, "", "fixture-key")); w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("unexpected synthetic request")
	}
}
func TestCookieLifecycleAndCredentialIsolation(t *testing.T) {
	var calls atomic.Int32
	e, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		cookies := map[string]string{}
		for _, c := range r.Cookies() {
			cookies[c.Name] = c.Value
		}
		if n == 1 {
			if len(cookies) != 0 {
				t.Error("client cookie forwarded")
			}
			http.SetCookie(w, &http.Cookie{Name: "__cflb", Value: "first", Path: "/"})
			http.SetCookie(w, &http.Cookie{Name: "__oailb", Value: "paired", Path: "/"})
			http.SetCookie(w, &http.Cookie{Name: "unrelated-session", Value: "never-forward", Path: "/"})
		} else if n == 2 {
			if cookies["__cflb"] != "first" || cookies["__oailb"] != "paired" || cookies["unrelated-session"] != "" {
				t.Error("cookie bundle not preserved")
			}
			http.SetCookie(w, &http.Cookie{Name: "__cflb", Value: "second", Path: "/"})
		} else if n == 3 {
			if cookies["__cflb"] != "second" {
				t.Error("rotation lost")
			}
			http.SetCookie(w, &http.Cookie{Name: "__cflb", Path: "/", MaxAge: -1})
		} else if n == 4 {
			if cookies["__cflb"] != "" || cookies["__oailb"] != "paired" {
				t.Error("deletion damaged bundle")
			}
		} else if len(cookies) != 0 {
			t.Error("cookies leaked between credentials")
		}
		sse(w, doneEvent)
	})
	for i := 0; i < 5; i++ {
		key := "account-one-key"
		if i == 4 {
			key = "account-two-key"
		}
		r := req(e, fmt.Sprint("turn-", i), key)
		r.Header.Set("Cookie", "__cflb=forged; private=secret")
		w := send(e, r)
		if w.Code != 200 || w.Header().Get("Set-Cookie") != "" {
			t.Fatal(w.Code, "upstream cookie exposed")
		}
	}
}
func TestHTTPAccountStopsApplyAcrossTurns(t *testing.T) {
	for _, status := range []int{401, 403, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			e, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(status)
			})
			first := send(e, req(e, "turn-one", "fixture-key"))
			second := send(e, req(e, "turn-two", "fixture-key"))
			if first.Code != status || second.Code != status || calls.Load() != 1 {
				t.Fatal(first.Code, second.Code, calls.Load())
			}
			if status == 429 && second.Header().Get("Retry-After") == "" {
				t.Fatal("missing retry delay")
			}
			if e.health.Status("direct").Failures != 0 {
				t.Fatal("account refusal poisoned network health")
			}
		})
	}
}
func TestSSELimitStopsNextRequestEvenAfterCompletedEvent(t *testing.T) {
	failure := "event: response.failed\r\ndata: {\"response\":{\r\ndata: \"error\":{\"code\":\"insufficient_quota\"}}}\r\n\r\n" + doneEvent
	var calls atomic.Int32
	e, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); sse(w, failure) })
	first := send(e, req(e, "", "fixture-key"))
	second := send(e, req(e, "different-turn", "fixture-key"))
	if first.Code != 200 || first.Body.String() != failure || second.Code != 429 || calls.Load() != 1 {
		t.Fatal(first.Code, second.Code, calls.Load())
	}
	if e.streamFailed.Load() != 1 || e.completed.Load() != 0 {
		t.Fatal("failure mistaken for success")
	}
}
func TestCircuitSkipsBadRoutePinsExistingTurnAndRecovers(t *testing.T) {
	e, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { sse(w, doneEvent) })
	real := e.routes[0].transport
	var attempts atomic.Int32
	broken := true
	e.routes[0].transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts.Add(1)
		if broken {
			return nil, errors.New("synthetic dial error")
		}
		return real.RoundTrip(r)
	})
	e.routes = append(e.routes, runtimeRoute{id: "second", transport: real})
	now := time.Unix(1000, 0)
	e.health = routehealth.New(routehealth.Config{Threshold: 1, Base: time.Second, Maximum: time.Second, Now: func() time.Time { return now }})
	if w := send(e, req(e, "pinned-turn", "fixture-key")); w.Code != 502 {
		t.Fatal(w.Code)
	}
	if attempts.Load() != 1 {
		t.Fatal("generation replayed")
	}
	if w := send(e, req(e, "pinned-turn", "fixture-key")); w.Code != 503 {
		t.Fatal("bound turn silently rerouted", w.Code)
	}
	if w := send(e, req(e, "", "fixture-key")); w.Code != 200 {
		t.Fatal("healthy route not selected", w.Code)
	}
	if attempts.Load() != 1 {
		t.Fatal("open circuit retried")
	}
	now = now.Add(time.Second)
	broken = false
	if w := send(e, req(e, "pinned-turn", "fixture-key")); w.Code != 200 {
		t.Fatal("half-open recovery failed", w.Code)
	}
	if attempts.Load() != 2 || e.health.Status("direct").State != "healthy" {
		t.Fatal("did not recover")
	}
}
func TestHTTPServerErrorIsNotProxyFailure(t *testing.T) {
	var calls atomic.Int32
	e, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) })
	for i := 0; i < 3; i++ {
		if w := send(e, req(e, "", "fixture-key")); w.Code != 503 {
			t.Fatal(w.Code)
		}
	}
	if calls.Load() != 3 || e.health.Status("direct").Failures != 0 {
		t.Fatal("upstream failure retried or proxy penalized")
	}
}
func TestTruncatedSSEIsNotSuccessfulGeneration(t *testing.T) {
	e, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { sse(w, "data: {\"type\":\"response.completed\"}\n") })
	w := send(e, req(e, "", "fixture-key"))
	if w.Code != 200 || e.incomplete.Load() != 1 || e.completed.Load() != 0 {
		t.Fatal(w.Code, "truncated event accepted")
	}
}

type brokenBody struct {
	read   bool
	cancel context.CancelFunc
}

func (b *brokenBody) Read(p []byte) (int, error) {
	if !b.read {
		b.read = true
		return copy(p, []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")), nil
	}
	if b.cancel != nil {
		b.cancel()
	}
	return 0, io.ErrUnexpectedEOF
}
func (b *brokenBody) Close() error { return nil }
func serveAllowAbort(t *testing.T, e *Engine, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil && p != http.ErrAbortHandler {
			panic(p)
		}
	}()
	e.ServeHTTP(w, r)
}
func TestMidstreamFailureAccountedEvenOnReverseProxyAbort(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelled), func(t *testing.T) {
			e, _ := fixture(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected network request") })
			calls := 0
			r := req(e, "", "fixture-key")
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			r = r.WithContext(ctx)
			e.routes[0].transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				b := &brokenBody{}
				if cancelled {
					b.cancel = cancel
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: b}, nil
			})
			e.health = routehealth.New(routehealth.Config{Threshold: 1})
			serveAllowAbort(t, e, httptest.NewRecorder(), r)
			if calls != 1 || e.inflight.Load() != 0 {
				t.Fatal("replay or missing cleanup")
			}
			if cancelled {
				if e.networkFailed.Load() != 0 || e.cancelled.Load() != 1 || e.health.Status("direct").State != "healthy" {
					t.Fatal("client cancellation blamed on route")
				}
			} else if e.networkFailed.Load() != 1 || e.health.Status("direct").State != "open" {
				t.Fatal("stream failure not recorded")
			}
		})
	}
}
func TestMetadataBindsRouteWithoutReplacingClientState(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	e, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("X-Test-Route"))
		mu.Unlock()
		sse(w, "event: response.metadata\ndata: {\"headers\":{\"X-Codex-Turn-State\":\"metadata-token\"}}\n\n"+doneEvent)
	})
	base := e.routes[0].transport
	e.routes = append(e.routes, runtimeRoute{id: "second", transport: base})
	for i := range e.routes {
		id := e.routes[i].id
		e.routes[i].transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			r.Header.Set("X-Test-Route", id)
			return base.RoundTrip(r)
		})
	}
	for _, state := range []string{"", "metadata-token", ""} {
		if w := send(e, req(e, state, "fixture-key")); w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(seen, ",") != "direct,direct,second" {
		t.Fatal(seen)
	}
}
func TestHealthReadAndLocalSecuritySendNoUpstreamTraffic(t *testing.T) {
	var calls atomic.Int32
	e, _ := fixture(t, func(http.ResponseWriter, *http.Request) { calls.Add(1) })
	for _, kind := range []string{"health", "origin", "host", "remote", "auth", "path", "state"} {
		r := req(e, "", "fixture-key")
		expected := 403
		switch kind {
		case "health":
			r.Method = "GET"
			r.URL.Path = "/healthz"
			expected = 200
		case "origin":
			r.Header.Set("Origin", "https://untrusted.invalid")
		case "host":
			r.Host = "untrusted.invalid"
		case "remote":
			r.RemoteAddr = "192.0.2.1:1234"
		case "auth":
			r.Header.Del("Authorization")
			expected = 401
		case "path":
			r.URL.Path = "/arbitrary"
			expected = 404
		case "state":
			r.Header.Set(stateHeader, strings.Repeat("x", 5000))
			expected = 400
		}
		w := send(e, r)
		if w.Code != expected {
			t.Fatal(kind, w.Code)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("local check contacted upstream")
	}
}
func TestConcurrentIndependentTurns(t *testing.T) {
	e, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "__cflb", Value: "stable", Path: "/"})
		sse(w, doneEvent)
	})
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Go(func() {
			if w := send(e, req(e, "", "fixture-key")); w.Code != 200 {
				t.Error(w.Code)
			}
		})
	}
	wg.Wait()
	if e.completed.Load() != 24 || e.inflight.Load() != 0 {
		t.Fatal("concurrent accounting failed")
	}
	raw, _ := json.Marshal(e.Status())
	if strings.Contains(string(raw), "fixture-key") || strings.Contains(string(raw), "stable") {
		t.Fatal("secret exposed in health")
	}
}

type stalledBody struct {
	closed chan struct{}
	once   sync.Once
}

func (b *stalledBody) Read([]byte) (int, error) { <-b.closed; return 0, io.ErrClosedPipe }
func (b *stalledBody) Close() error             { b.once.Do(func() { close(b.closed) }); return nil }
func TestStreamIdleTimeoutIsBoundedAndRecorded(t *testing.T) {
	e, logs := fixture(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected upstream request") })
	e.config.StreamIdleSeconds = 1
	e.health = routehealth.New(routehealth.Config{Threshold: 1})
	e.routes[0].transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: &stalledBody{closed: make(chan struct{})}}, nil
	})
	started := time.Now()
	serveAllowAbort(t, e, httptest.NewRecorder(), req(e, "", "fixture-key"))
	if time.Since(started) > 3*time.Second || e.networkFailed.Load() != 1 || e.health.Status("direct").State != "open" {
		t.Fatal("idle stream not interrupted")
	}
	if !strings.Contains(logs.String(), "upstream_idle_timeout") {
		t.Fatal("timeout attribution missing", logs.String())
	}
}
func TestLargeEventsAreForwardedButNotClaimedAsVerified(t *testing.T) {
	wire := "data: " + strings.Repeat("x", eventLimit+100) + "\n\n" + doneEvent
	e, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { sse(w, wire) })
	w := send(e, req(e, "", "fixture-key"))
	if w.Body.String() != wire || e.observationLimited.Load() != 1 || e.completed.Load() != 0 || e.networkFailed.Load() != 0 {
		t.Fatal("large event was altered or misclassified")
	}
}
func TestConcurrencyLimitAndRedirectDoNotReplay(t *testing.T) {
	var calls atomic.Int32
	e, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Location", "https://untrusted.invalid/")
		w.WriteHeader(302)
	})
	e.slots = make(chan struct{}, 1)
	e.slots <- struct{}{}
	if w := send(e, req(e, "", "fixture-key")); w.Code != 503 || calls.Load() != 0 {
		t.Fatal("capacity limit failed")
	}
	<-e.slots
	w := send(e, req(e, "", "fixture-key"))
	if w.Code != 502 || calls.Load() != 1 || w.Header().Get("Location") != "" {
		t.Fatal("redirect leaked or followed")
	}
}
