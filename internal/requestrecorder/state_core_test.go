package requestrecorder

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gylive/ccodex-sleep-state/internal/routehealth"
	"github.com/gylive/ccodex-sleep-state/internal/turnstate"
)

func coreToken(at time.Time, blocks int, marker byte) string {
	raw := make([]byte, 57+16*blocks)
	raw[0] = 0x80
	raw[9] = marker
	binary.BigEndian.PutUint64(raw[1:9], uint64(at.Unix()))
	return base64.URLEncoding.EncodeToString(raw)
}
func coreReply(w http.ResponseWriter, token string, cookies bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set(turnstate.Header, token)
	if cookies {
		w.Header().Add("Set-Cookie", "__cflb=fixture-route; Path=/; HttpOnly")
		w.Header().Add("Set-Cookie", "__oailb=fixture-worker; Path=/; HttpOnly")
	}
	fmt.Fprint(w, completeFixture)
}
func turnOnCore(t *testing.T, e *Engine, p ...CorePolicy) {
	t.Helper()
	policy := coreDefaults()
	if len(p) > 0 {
		policy = p[0]
	}
	policy.Enabled = true
	if err := e.ConfigureCore(policy); err != nil {
		t.Fatal(err)
	}
}
func TestCoreCollectsBothThenInjectsWithoutLeakingOrReplaying(t *testing.T) {
	token := coreToken(time.Now(), 10, 1)
	var probes, generations atomic.Int32
	e, server := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		if r.Header.Get(turnstate.Header) == "" {
			probes.Add(1)
			if bytes.Contains(data, []byte("private prompt")) || r.Header.Get("Cookie") != "" || r.Header.Get("Session_id") != "" {
				t.Error("collector copied client content/context")
			}
			coreReply(w, token, true)
			return
		}
		generations.Add(1)
		if r.Header.Get(turnstate.Header) != token || string(data) != requestFixture {
			t.Error("wrong state or changed generation")
		}
		for _, name := range []string{"__cflb", "__oailb", "unrelated"} {
			if _, err := r.Cookie(name); err != nil {
				t.Error("missing injected/preserved cookie", name)
			}
		}
		coreReply(w, token, false)
	}), "redacted")
	turnOnCore(t, e)
	h := http.Header{"Authorization": {"Bearer fixture-core-credential"}, "Cookie": {"__cflb=old; unrelated=keep"}}
	for i := 0; i < 2; i++ {
		resp, _ := post(t, server, requestFixture, h)
		if resp.StatusCode != 200 {
			t.Fatal(resp.StatusCode)
		}
	}
	if probes.Load() != 1 || generations.Load() != 2 {
		t.Fatal(probes.Load(), generations.Load())
	}
	waitRecords(t, e, 2)
	rec := latest(t, e)
	if !rec.Core.Injected || rec.Core.CookieCount != 2 || rec.Core.StateLength != 292 {
		t.Fatal(rec.Core)
	}
	data, _ := json.Marshal(rec)
	status, _ := json.Marshal(e.core.status())
	for _, secret := range []string{token, "fixture-core-credential", "fixture-worker", "fixture-route"} {
		if bytes.Contains(data, []byte(secret)) || bytes.Contains(status, []byte(secret)) {
			t.Fatal("secret recorded in redacted data/status")
		}
	}
}
func TestCoreAcceptsMetadataStateAndCookies(t *testing.T) {
	token := coreToken(time.Now(), 10, 2)
	var probes atomic.Int32
	e, server := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if r.Header.Get(turnstate.Header) == "" {
			probes.Add(1)
			event, _ := json.Marshal(map[string]any{"type": "response.metadata", "headers": map[string]any{turnstate.Header: token, "Set-Cookie": []string{"__cflb=fixture; Path=/", "__oailb=fixture; Path=/"}}})
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
		fmt.Fprint(w, completeFixture)
	}), "metadata")
	turnOnCore(t, e)
	resp, data := post(t, server, requestFixture, http.Header{"Authorization": {"Bearer fixture-metadata"}})
	if resp.StatusCode != 200 || probes.Load() != 1 {
		t.Fatal(resp.StatusCode, string(data), probes.Load())
	}
}
func TestCoreNeverPublishesIncompleteWrongShapeOrCookieLessProbe(t *testing.T) {
	for _, kind := range []string{"incomplete", "wrong_shape", "missing_cookie", "expired", "stream_failure", "conflicting_state"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			e, server := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				io.Copy(io.Discard, r.Body)
				if r.Header.Get(turnstate.Header) != "" {
					t.Error("formal generation was dispatched")
				}
				blocks := 10
				if kind == "wrong_shape" {
					blocks = 11
				}
				at := time.Now()
				if kind == "expired" {
					at = at.Add(-5 * time.Minute)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set(turnstate.Header, coreToken(at, blocks, 3))
				if kind != "missing_cookie" {
					w.Header().Add("Set-Cookie", "__oailb=fixture; Path=/")
				}
				switch kind {
				case "incomplete":
					fmt.Fprint(w, "data: {\"type\":\"response.created\"}\n\n")
				case "stream_failure":
					fmt.Fprint(w, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_is_overloaded\"}}}\n\n"+completeFixture)
				case "conflicting_state":
					fmt.Fprintf(w, "data: {\"type\":\"response.metadata\",\"headers\":{\"X-Codex-Turn-State\":%q}}\n\n%s", coreToken(at, 10, 9), completeFixture)
				default:
					fmt.Fprint(w, completeFixture)
				}
			}), "metadata")
			turnOnCore(t, e)
			resp, _ := post(t, server, requestFixture, http.Header{"Authorization": {"Bearer fixture-invalid"}})
			if resp.StatusCode != 503 || calls.Load() != 1 {
				t.Fatal(resp.StatusCode, calls.Load())
			}
		})
	}
}
func TestCoreCredentialStopsSurviveModeChangesAndModelSwitches(t *testing.T) {
	for _, status := range []int{401, 403, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			e, server := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Retry-After", "180")
				w.WriteHeader(status)
			}), "metadata")
			turnOnCore(t, e)
			h := http.Header{"Authorization": {"Bearer fixture-paused"}}
			for i := 0; i < 2; i++ {
				resp, _ := post(t, server, requestFixture, h)
				if resp.StatusCode != status {
					t.Fatal(resp.StatusCode)
				}
			}
			p := e.core.options()
			p.Enabled = false
			if err := e.ConfigureCore(p); err != nil {
				t.Fatal(err)
			}
			resp, _ := post(t, server, strings.Replace(requestFixture, "astra", "other", 1), h)
			if resp.StatusCode != status || calls.Load() != 1 {
				t.Fatal("limit bypass", resp.StatusCode, calls.Load())
			}
		})
	}
}
func TestCoreSSEQuotaStopsNextRequestDespiteHTTP200(t *testing.T) {
	var calls atomic.Int32
	token := coreToken(time.Now(), 10, 4)
	e, server := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.Copy(io.Discard, r.Body)
		if r.Header.Get(turnstate.Header) == "" {
			coreReply(w, token, true)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"insufficient_quota\"}}}\n\n")
	}), "metadata")
	turnOnCore(t, e)
	h := http.Header{"Authorization": {"Bearer fixture-sse-stop"}}
	first, _ := post(t, server, requestFixture, h)
	next, _ := post(t, server, requestFixture, h)
	if first.StatusCode != 200 || next.StatusCode != 429 || calls.Load() != 2 {
		t.Fatal(first.StatusCode, next.StatusCode, calls.Load())
	}
}
func TestCoreConcurrentBootstrapSingleFlight(t *testing.T) {
	var probes, generations atomic.Int32
	token := coreToken(time.Now(), 10, 5)
	e, server := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.Header.Get(turnstate.Header) == "" {
			probes.Add(1)
			time.Sleep(30 * time.Millisecond)
			coreReply(w, token, true)
		} else {
			generations.Add(1)
			coreReply(w, token, false)
		}
	}), "metadata")
	turnOnCore(t, e)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, _ := post(t, server, requestFixture, http.Header{"Authorization": {"Bearer fixture-singleflight"}})
			if resp.StatusCode != 200 {
				t.Error(resp.StatusCode)
			}
		}()
	}
	wg.Wait()
	if probes.Load() != 1 || generations.Load() != 6 {
		t.Fatal(probes.Load(), generations.Load())
	}
}
func TestCoreRefreshesAtLocalDeadlineWithoutExtendingOnUse(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().Unix())
	var probes atomic.Int32
	e, server := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.Header.Get(turnstate.Header) == "" {
			n := probes.Add(1)
			coreReply(w, coreToken(time.Unix(clock.Load(), 0), 10, byte(n)), true)
		} else {
			coreReply(w, r.Header.Get(turnstate.Header), false)
		}
	}), "metadata")
	e.core.now = func() time.Time { return time.Unix(clock.Load(), 0) }
	turnOnCore(t, e)
	h := http.Header{"Authorization": {"Bearer fixture-expiry"}}
	for _, advance := range []int64{0, 40, 90} {
		clock.Add(advance)
		resp, data := post(t, server, requestFixture, h)
		if resp.StatusCode != 200 {
			t.Fatal(resp.StatusCode, string(data))
		}
	}
	if probes.Load() != 2 {
		t.Fatal("deadline incorrectly prolonged", probes.Load())
	}
}
func TestCoreCookieDeletionMakesBundleUnavailableAndRefreshes(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().Unix())
	var probes atomic.Int32
	e, server := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.Header.Get(turnstate.Header) == "" {
			n := probes.Add(1)
			coreReply(w, coreToken(time.Unix(clock.Load(), 0), 10, byte(n)), true)
			return
		}
		w.Header().Add("Set-Cookie", "__oailb=; Max-Age=0; Expires=Thu, 01 Jan 1970 00:00:00 GMT; Path=/")
		coreReply(w, r.Header.Get(turnstate.Header), false)
	}), "metadata")
	e.core.now = func() time.Time { return time.Unix(clock.Load(), 0) }
	turnOnCore(t, e)
	h := http.Header{"Authorization": {"Bearer fixture-cookie-expiry"}}
	resp, _ := post(t, server, requestFixture, h)
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	resp, _ = post(t, server, requestFixture, h)
	if resp.StatusCode != 503 {
		t.Fatal("deleted cookie reused", resp.StatusCode)
	}
	clock.Add(31)
	resp, data := post(t, server, requestFixture, h)
	if resp.StatusCode != 200 || probes.Load() != 2 {
		t.Fatal(resp.StatusCode, string(data), probes.Load())
	}
}
func TestCoreHourlyBudgetSharedAcrossModelsAndNotResetByConfigure(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().Unix())
	var probes atomic.Int32
	e, server := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.Header.Get(turnstate.Header) == "" {
			probes.Add(1)
		}
		coreReply(w, coreToken(time.Unix(clock.Load(), 0), 10, 1), true)
	}), "metadata")
	e.core.now = func() time.Time { return time.Unix(clock.Load(), 0) }
	p := coreDefaults()
	p.HourlyBudget = 1
	turnOnCore(t, e, p)
	h := http.Header{"Authorization": {"Bearer fixture-budget"}}
	resp, _ := post(t, server, requestFixture, h)
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	clock.Add(31)
	p = e.core.options()
	if err := e.ConfigureCore(p); err != nil {
		t.Fatal(err)
	}
	resp, data := post(t, server, strings.Replace(requestFixture, "astra", "other", 1), h)
	if resp.StatusCode != 503 || !bytes.Contains(data, []byte("state_core_probe_budget")) || probes.Load() != 1 {
		t.Fatal(resp.StatusCode, string(data), probes.Load())
	}
}
func TestCoreCredentialAndWorkspaceIsolation(t *testing.T) {
	var probes atomic.Int32
	e, server := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.Header.Get(turnstate.Header) == "" {
			n := probes.Add(1)
			coreReply(w, coreToken(time.Now(), 10, byte(n)), true)
		} else {
			coreReply(w, r.Header.Get(turnstate.Header), false)
		}
	}), "metadata")
	turnOnCore(t, e)
	for _, h := range []http.Header{{"Authorization": {"Bearer fixture-one"}}, {"Authorization": {"Bearer fixture-two"}}, {"Authorization": {"Bearer fixture-one"}, "Chatgpt-Account-Id": {"workspace-two"}}} {
		resp, _ := post(t, server, requestFixture, h)
		if resp.StatusCode != 200 {
			t.Fatal(resp.StatusCode)
		}
	}
	if probes.Load() != 3 || len(e.core.status()["sessions"].([]map[string]any)) != 3 {
		t.Fatal(probes.Load())
	}
}
func TestCoreHealthSkipsFailedHarvestExitAndDoesNotDeadlockActive(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().Unix())
	var probes atomic.Int32
	e, server := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.Header.Get(turnstate.Header) == "" {
			probes.Add(1)
		}
		coreReply(w, coreToken(time.Unix(clock.Load(), 0), 10, byte(probes.Load())), true)
	}), "metadata")
	e.core.now = func() time.Time { return time.Unix(clock.Load(), 0) }
	turnOnCore(t, e)
	h := http.Header{"Authorization": {"Bearer fixture-health"}}
	post(t, server, requestFixture, h)
	clock.Add(31)
	e.core.mu.Lock()
	old := e.core.routes[0]
	e.core.routes = append(e.core.routes, coreRoute{id: "fixture-second-route", transport: old.transport})
	e.core.mu.Unlock()
	for i := 0; i < 2; i++ {
		l, _ := e.core.health.Begin(old.id)
		l.Finish(routehealth.Failure)
	}
	resp, data := post(t, server, requestFixture, h)
	if resp.StatusCode != 200 || probes.Load() != 2 {
		t.Fatal("stuck on failed route", resp.StatusCode, string(data))
	}
	if e.core.health.Status(old.id).State != "open" {
		t.Fatal("failed route silently recovered")
	}
}
func TestCoreNoProbeOnStartupOrUnauthenticatedControls(t *testing.T) {
	var calls atomic.Int32
	e, server := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }), "metadata")
	for _, token := range []string{"", e.Token()} {
		req, _ := http.NewRequest("POST", server.URL+"/__recorder/api/state-core/configure", strings.NewReader(`{"enabled":true}`))
		req.Header.Set("X-Recorder-Token", token)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		expected := 403
		if token != "" {
			expected = 400
		}
		if resp.StatusCode != expected {
			t.Fatal(resp.StatusCode)
		}
	}
	if e.core.options().Enabled || calls.Load() != 0 {
		t.Fatal("unsolicited probes or enabled without confirmation")
	}
	turnOnCore(t, e)
	if calls.Load() != 0 {
		t.Fatal("enable alone sent request")
	}
	if err := e.core.refresh(context.Background(), "missing"); err == nil {
		t.Fatal("refresh without credential accepted")
	}
}
func TestCoreIncrementalSSEParserBoundsCRLFAndResynchronizes(t *testing.T) {
	var events []string
	p := &coreParser{emit: func(name string, data []byte) { events = append(events, string(data)) }}
	data := "data: " + strings.Repeat("x", eventLimit+1) + "\r\ndata: discard\r\n\r\n" + "data: {\"type\":\"response.completed\"}\r\n\r\n"
	for start := 0; start < len(data); start += 7 {
		p.feed([]byte(data[start:min(start+7, len(data))]))
	}
	if !p.limited || len(events) != 1 || !strings.Contains(events[0], "response.completed") {
		t.Fatal(p.limited, events)
	}
}
func TestCoreStaleResponseCannotPoisonReplacement(t *testing.T) {
	token := coreToken(time.Now(), 10, 1)
	e, _ := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.Copy(io.Discard, r.Body); coreReply(w, token, true) }), "metadata")
	turnOnCore(t, e)
	u := *e.target
	u.Path = "/backend-api/codex/responses"
	req, _ := http.NewRequest("POST", u.String(), strings.NewReader(requestFixture))
	req.Header.Set("Authorization", "Bearer fixture-late")
	req.Header.Set("Content-Type", "application/json")
	lease, err := e.core.prepare(req)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := e.core.roundTrip(lease, e.transport)
	if err != nil {
		t.Fatal(err)
	}
	p := e.core.options()
	if err := e.ConfigureCore(p); err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	e.core.mu.Lock()
	defer e.core.mu.Unlock()
	if lease.session.active != nil || lease.session.ready != nil || lease.session.lastResult != "configuration_changed" {
		t.Fatal("old response revived stale bundle")
	}
}
func TestCoreCancelledCollectionDoesNotPublishOrPunishRoute(t *testing.T) {
	e, _ := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }), "metadata")
	turnOnCore(t, e)
	u := *e.target
	u.Path = "/backend-api/codex/responses"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", u.String(), strings.NewReader(requestFixture))
	req.Header.Set("Authorization", "Bearer fixture-cancel")
	_, err := e.core.prepare(req)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, r := range e.core.routes {
		if e.core.health.Status(r.id).Errors != 0 {
			t.Fatal("cancel punished route")
		}
	}
}
