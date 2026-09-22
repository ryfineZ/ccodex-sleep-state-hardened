package requestrecorder

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const requestFixture = `{"model":"astra","input":"private prompt","stream":true}`
const completeFixture = "data: {\"type\":\"response.completed\",\"response\":{\"model\":\"astra\"}}\n\n"

func setup(t *testing.T, h http.Handler, mode string) (*Engine, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(h)
	t.Cleanup(up.Close)
	c := Default()
	c.Upstream = up.URL
	c.Directory = filepath.Join(t.TempDir(), "captures")
	c.Mode = mode
	e, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	server := httptest.NewServer(e)
	e.config.Listen = server.Listener.Addr().String()
	t.Cleanup(server.Close)
	return e, server
}
func post(t *testing.T, s *httptest.Server, body string, headers http.Header) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("POST", s.URL+"/backend-api/codex/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header[k] = v
	}
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, b
}
func waitRecords(t *testing.T, e *Engine, n int) []Summary {
	t.Helper()
	end := time.Now().Add(5 * time.Second)
	for time.Now().Before(end) {
		rows := e.store.List(200)
		if len(rows) >= n {
			return rows
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("wanted %d records: %+v", n, e.store.Status())
	return nil
}
func latest(t *testing.T, e *Engine) Record {
	t.Helper()
	rows := waitRecords(t, e, 1)
	r, err := e.store.Read(rows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestIndependentForwardingCookieStateNoReplayAndRedaction(t *testing.T) {
	var calls atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		if string(b) != requestFixture {
			t.Error("body rewritten")
		}
		if r.Header.Get("X-Codex-Turn-State") != "secret-state" || r.Header.Get("Cookie") != "__oailb=secret-cookie" || r.Header.Get("Authorization") != "Bearer secret-credential" {
			t.Error("protocol headers changed")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Add("Set-Cookie", "__oailb=new-secret-cookie; Path=/")
		fmt.Fprint(w, completeFixture)
	})
	e, s := setup(t, h, "redacted")
	resp, body := post(t, s, requestFixture, http.Header{"Authorization": {"Bearer secret-credential"}, "Cookie": {"__oailb=secret-cookie"}, "X-Codex-Turn-State": {"secret-state"}})
	if string(body) != completeFixture || resp.Header.Get("Set-Cookie") == "" || calls.Load() != 1 {
		t.Fatal("not transparent or replayed")
	}
	r := latest(t, e)
	b, _ := json.Marshal(r)
	for _, secret := range []string{"secret-credential", "secret-cookie", "secret-state"} {
		if bytes.Contains(b, []byte(secret)) {
			t.Fatal("secret persisted:", secret)
		}
	}
	if r.Model.Verdict != "consistent" || r.Model.ActualVerified || len(r.Request.Raw) != 0 || !r.Response.EOF || r.Outcome != "http_complete" {
		t.Fatalf("%+v", r)
	}
	if !bytes.Contains(b, []byte("private prompt")) {
		t.Fatal("redacted mode body missing; docs promise structural redaction, not prompt deletion")
	}
	entries, _ := os.ReadDir(e.config.Directory)
	for _, entry := range entries {
		f, err := os.Open(filepath.Join(e.config.Directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		err = checkPrivateHandle(f, false)
		f.Close()
		if err != nil {
			t.Fatal("private file permissions:", err)
		}
	}
}
func TestModelMismatchConflictUnknownAndSelfReportIgnored(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"mismatch", `{"model":"luna","output":"ok"}`, "mismatch"},
		{"conflict", `{"model":"astra","response":{"model":"luna"}}`, "conflict"},
		{"self report", `{"output":[{"text":"I am luna"}]}`, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, s := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.body)
			}), "redacted")
			post(t, s, requestFixture, nil)
			r := latest(t, e)
			if r.Model.Verdict != tc.want {
				t.Fatal(r.Model)
			}
			offline := AnalyzeSaved(r)
			if offline.Verdict != tc.want {
				t.Fatal("offline", offline)
			}
		})
	}
}
func TestFullModeRawBytesAndMetadataMode(t *testing.T) {
	for _, mode := range []string{"full", "metadata"} {
		t.Run(mode, func(t *testing.T) {
			e, s := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, completeFixture)
			}), mode)
			post(t, s, requestFixture, http.Header{"Authorization": {"Bearer fixture-only"}})
			r := latest(t, e)
			if mode == "full" {
				var raw []byte
				for _, c := range r.Response.Raw {
					raw = append(raw, c.Data...)
				}
				if string(raw) != completeFixture || r.RequestHeaders.Get("Authorization") != "Bearer fixture-only" {
					t.Fatal("raw capture incorrect")
				}
				if AnalyzeSaved(r).Verdict != "consistent" {
					t.Fatal("raw offline analysis")
				}
			}
			if mode == "metadata" {
				b, _ := json.Marshal(r)
				if bytes.Contains(b, []byte("private prompt")) || len(r.Response.Raw) != 0 || len(r.Request.JSON) != 0 {
					t.Fatal("metadata persisted content")
				}
				if r.Model.Verdict != "consistent" {
					t.Fatal("metadata detection missing")
				}
			}
		})
	}
}
func TestGzipRequestAndResponseUnchanged(t *testing.T) {
	compress := func(b string) []byte {
		var buf bytes.Buffer
		z := gzip.NewWriter(&buf)
		z.Write([]byte(b))
		z.Close()
		return buf.Bytes()
	}
	requestBytes := compress(requestFixture)
	responseBytes := compress(completeFixture)
	e, s := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !bytes.Equal(b, requestBytes) || r.Header.Get("Content-Encoding") != "gzip" {
			t.Error("request encoding changed")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(responseBytes)
	}), "full")
	resp, body := post(t, s, string(requestBytes), http.Header{"Content-Encoding": {"gzip"}, "Accept-Encoding": {"gzip"}})
	if !bytes.Equal(body, responseBytes) || resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("response compression changed")
	}
	r := latest(t, e)
	if r.Model.Verdict != "consistent" || !r.Response.TimingLimited {
		t.Fatal(r.Model, r.Response.Note)
	}
}
func TestRecordsDoNotWaitForSSECompletionToForward(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	first := "data: {\"type\":\"response.created\",\"response\":{\"model\":\"astra\"}}\n\n"
	e, s := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, first)
		w.(http.Flusher).Flush()
		<-release
		fmt.Fprint(w, completeFixture)
	}), "redacted")
	req, _ := http.NewRequest("POST", s.URL+"/v1/responses", strings.NewReader(requestFixture))
	req.Header.Set("Content-Type", "application/json")
	ch := make(chan error, 1)
	go func() {
		resp, err := s.Client().Do(req)
		if err != nil {
			ch <- err
			return
		}
		defer resp.Body.Close()
		b := make([]byte, len(first))
		_, err = io.ReadFull(resp.Body, b)
		if err == nil && string(b) != first {
			err = errors.New("changed SSE bytes")
		}
		ch <- err
		once.Do(func() { close(release) })
		io.Copy(io.Discard, resp.Body)
	}()
	select {
	case err := <-ch:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		once.Do(func() { close(release) })
		t.Fatal("SSE was buffered")
	}
	waitRecords(t, e, 1)
}

type brokenBody struct{ sent bool }

func (b *brokenBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, []byte("data: {\"type\":\"response.created\"}\n\n")), io.ErrUnexpectedEOF
	}
	return 0, io.ErrUnexpectedEOF
}
func (b *brokenBody) Close() error { return nil }
func TestStreamAbortRecordedAndNeverReplayed(t *testing.T) {
	e, s := setup(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unused transport") }), "redacted")
	var calls atomic.Int32
	e.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &brokenBody{}}, nil
	})
	req, _ := http.NewRequest("POST", s.URL+"/v1/responses", strings.NewReader(requestFixture))
	resp, err := s.Client().Do(req)
	if err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	r := latest(t, e)
	if calls.Load() != 1 || r.Outcome != "upstream_body_read_error" || r.Response.EOF {
		t.Fatal(calls.Load(), r.Outcome)
	}
}
func adminCall(t *testing.T, e *Engine, s *httptest.Server, path, method, body, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, s.URL+"/__recorder/api/"+path, strings.NewReader(body))
	req.Header.Set("X-Recorder-Token", token)
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
func TestAdminAuthenticationPauseAndCaptureDoesNotCreateTraffic(t *testing.T) {
	var calls atomic.Int32
	e, s := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"astra"}`)
	}), "redacted")
	r := adminCall(t, e, s, "records", "GET", "", "")
	r.Body.Close()
	if r.StatusCode != 403 {
		t.Fatal("unauthenticated record access")
	}
	r = adminCall(t, e, s, "status", "GET", "", e.Token())
	r.Body.Close()
	if r.StatusCode != 200 || calls.Load() != 0 {
		t.Fatal("admin generated upstream traffic")
	}
	r = adminCall(t, e, s, "capture", "POST", `{"enabled":false}`, e.Token())
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatal("pause")
	}
	post(t, s, requestFixture, nil)
	if calls.Load() != 1 || len(e.store.List(10)) != 0 {
		t.Fatal("pause stopped forwarding or recorded")
	}
	r = adminCall(t, e, s, "capture", "POST", `{"enabled":true}`, e.Token())
	r.Body.Close()
	post(t, s, requestFixture, nil)
	waitRecords(t, e, 1)
	r = adminCall(t, e, s, "records/../../go.mod", "GET", "", e.Token())
	r.Body.Close()
	if r.StatusCode != 404 {
		t.Fatal("path traversal")
	}
}
func TestBrowserAndCONNECTRejectedAndNoAutomaticRedirectFollow(t *testing.T) {
	var calls atomic.Int32
	e, s := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Location", "https://must-not-be-contacted.invalid/")
		w.WriteHeader(302)
	}), "redacted")
	req, _ := http.NewRequest("POST", s.URL+"/v1/responses", strings.NewReader(requestFixture))
	req.Header.Set("Origin", "https://evil.invalid")
	resp, _ := s.Client().Do(req)
	resp.Body.Close()
	if resp.StatusCode != 403 || calls.Load() != 0 {
		t.Fatal("cross origin accepted")
	}
	req, _ = http.NewRequest("CONNECT", s.URL+"/v1/responses", nil)
	resp, _ = s.Client().Do(req)
	resp.Body.Close()
	if resp.StatusCode != 426 {
		t.Fatal("CONNECT accepted")
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ = http.NewRequest("POST", s.URL+"/v1/responses", strings.NewReader(requestFixture))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 302 || calls.Load() != 1 {
		t.Fatal("redirect followed by recorder")
	}
	latest(t, e)
}
func TestConcurrentRecordsAreIsolated(t *testing.T) {
	e, s := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":%q}`, r.Header.Get("X-Test-Model"))
	}), "redacted")
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			model := fmt.Sprintf("model-%d", i)
			post(t, s, fmt.Sprintf(`{"model":%q}`, model), http.Header{"X-Test-Model": {model}})
		}(i)
	}
	wg.Wait()
	rows := waitRecords(t, e, 6)
	seen := map[string]bool{}
	for _, r := range rows {
		if r.Verdict != "consistent" || seen[r.Requested] {
			t.Fatal("cross-request pollution", r)
		}
		seen[r.Requested] = true
	}
}
