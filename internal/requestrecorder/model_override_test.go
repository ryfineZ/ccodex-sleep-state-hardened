package requestrecorder

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
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

	"github.com/gylive/ccodex-sleep-state/internal/modelaudit"
)

func TestOverrideDefaultsAndRemovedAliases(t *testing.T) {
	c := Default()
	if c.ForceModelEnabled || c.ForceModel != "" {
		t.Fatal("override unexpectedly enabled")
	}
	c.ForceModelEnabled = true
	if c.Validate() == nil {
		t.Fatal("empty target accepted")
	}
	for _, name := range []string{" ", "a\nb", strings.Repeat("a", 257)} {
		c.ForceModel = name
		if c.Validate() == nil {
			t.Fatal("invalid target accepted")
		}
	}
	c.ForceModel = "exact-model-mini"
	if c.Validate() != nil {
		t.Fatal("valid target rejected")
	}
	p := filepath.Join(t.TempDir(), "old-config.json")
	if err := os.WriteFile(p, []byte(`{"model_aliases":{"a":"b"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("removed alias config accepted")
	}
	d := modelaudit.New("astra", "astra")
	d.JSON([]byte(`{"model":"astra-snapshot"}`), "json", "", 0, 0)
	if d.Result().Verdict != "mismatch" {
		t.Fatal("snapshot implicitly aliased")
	}
}

func TestOverrideSplicesOnlyUniqueTopLevelValue(t *testing.T) {
	input := "{\n \"input\": {\"model\":\"nested\",\"text\":\"say astra\"}, \"n\":9007199254740993123, \"mo\\u0064el\" : \"astra\", \"stream\":true\n}"
	out, original, err := replaceTopLevelModel([]byte(input), "target")
	want := strings.Replace(input, `"astra"`, `"target"`, 1)
	if err != nil || original != "astra" || string(out) != want {
		t.Fatal(err, original, string(out))
	}
	for _, data := range []string{`{"model":"a","model":"b"}`, `{"model":"a","mo\u0064el":"b"}`, `{"nested":{"model":"a"}}`, `{"model":12}`, `{"model":null}`, `{"model":""}`, `{"model":"a"} {}`, `["a"]`, `{"model":"a"`} {
		if _, _, err := replaceTopLevelModel([]byte(data), "target"); err == nil {
			t.Fatal("ambiguous or invalid JSON accepted", data)
		}
	}
	quoted := `new"model`
	out, _, err = replaceTopLevelModel([]byte(requestFixture), quoted)
	if err != nil || modelaudit.RequestModel(out) != quoted {
		t.Fatal("target not JSON escaped", err, string(out))
	}
}

func TestOverrideDisabledPreservesBodyWithTargetConfigured(t *testing.T) {
	var seen string
	e, s := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = string(b)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"astra"}`)
	}), "redacted")
	if _, err := e.SetModelOverride(false, "luna"); err != nil {
		t.Fatal(err)
	}
	post(t, s, requestFixture, nil)
	rec := latest(t, e)
	if seen != requestFixture || rec.ModelOverride.Applied || rec.Model.RequestRewritten || rec.IncomingRequest != nil {
		t.Fatal("disabled override changed request", rec.ModelOverride)
	}
}

func TestOverrideThreeModelIdentitiesAndOfflineAnalysis(t *testing.T) {
	for _, returned := range []string{"luna", "terra"} {
		t.Run(returned, func(t *testing.T) {
			var calls atomic.Int32
			e, s := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				b, _ := io.ReadAll(r.Body)
				if modelaudit.RequestModel(b) != "luna" || !bytes.Contains(b, []byte("private prompt")) {
					t.Error("incorrect outgoing body")
				}
				if r.Header.Get("X-Codex-Turn-State") != "fixture-state" || r.Header.Get("Cookie") != "fixture=value" {
					t.Error("protocol context rewritten")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"model\":%q}}\n\n", returned)
			}), "redacted")
			e.SetModelOverride(true, "luna")
			post(t, s, requestFixture, http.Header{"X-Codex-Turn-State": {"fixture-state"}, "Cookie": {"fixture=value"}})
			rec := latest(t, e)
			want := "consistent"
			if returned != "luna" {
				want = "mismatch"
			}
			if calls.Load() != 1 || !rec.ModelOverride.Applied || !rec.ModelOverride.Changed || !rec.Model.RequestRewritten || rec.Model.Requested != "astra" || rec.Model.Forwarded != "luna" || rec.Model.Verdict != want || rec.Model.ActualVerified {
				t.Fatal(rec.Model, rec.ModelOverride)
			}
			if rec.IncomingRequest == nil || modelaudit.RequestModel(rec.IncomingRequest.JSON) != "astra" || modelaudit.RequestModel(rec.Request.JSON) != "luna" {
				t.Fatal("original/outgoing capture lost")
			}
			offline := AnalyzeSaved(rec)
			if offline.Requested != "astra" || offline.Forwarded != "luna" || offline.Verdict != want || !offline.RequestRewritten {
				t.Fatal("offline collapsed identities", offline)
			}
		})
	}
}

func TestOverrideCompressedRequestAndNoOp(t *testing.T) {
	var encoded bytes.Buffer
	z := gzip.NewWriter(&encoded)
	z.Write([]byte(requestFixture))
	z.Close()
	for _, target := range []string{"luna", "astra"} {
		t.Run(target, func(t *testing.T) {
			e, s := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				if target == "astra" {
					if !bytes.Equal(b, encoded.Bytes()) || r.Header.Get("Content-Encoding") != "gzip" {
						t.Error("no-op changed original compression")
					}
				} else {
					if r.Header.Get("Content-Encoding") != "" || modelaudit.RequestModel(b) != "luna" || r.ContentLength != int64(len(b)) {
						t.Error("bad rewritten content/length")
					}
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"model":%q}`, target)
			}), "full")
			e.SetModelOverride(true, target)
			post(t, s, encoded.String(), http.Header{"Content-Encoding": {"gzip"}})
			rec := latest(t, e)
			if rec.IncomingRequest == nil || !bytes.Equal(rec.IncomingRequest.Raw[0].Data, encoded.Bytes()) || rec.Model.Requested != "astra" || rec.Model.Forwarded != target {
				t.Fatal("compressed audit lost", rec.Model)
			}
			if rec.ModelOverride.EncodingChanged != (target != "astra") {
				t.Fatal(rec.ModelOverride)
			}
		})
	}
}

func TestOverrideInvalidRequestsNeverSent(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		headers    http.Header
		status     int
	}{
		{"missing", `{"input":"x"}`, nil, 400},
		{"duplicate", `{"model":"a","model":"b"}`, nil, 400},
		{"invalid", `not json`, nil, 400},
		{"encoding", requestFixture, http.Header{"Content-Encoding": {"unknown"}}, 415},
		{"digest", requestFixture, http.Header{"Content-Digest": {"fixture-digest"}}, 400},
		{"content-type", requestFixture, http.Header{"Content-Type": {"text/plain"}}, 415},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			e, s := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }), "metadata")
			e.SetModelOverride(true, "luna")
			resp, _ := post(t, s, tc.body, tc.headers)
			if resp.StatusCode != tc.status || calls.Load() != 0 {
				t.Fatal(resp.StatusCode, calls.Load())
			}
			rec := latest(t, e)
			if rec.ModelOverride.Applied || rec.Status != 0 || rec.ClientStatus != tc.status {
				t.Fatal(rec.ModelOverride, rec.Status)
			}
		})
	}
}

func TestOverrideLimitedToGenerationEndpoints(t *testing.T) {
	for _, path := range []string{"/backend-api/codex/models", "/backend-api/codex/alpha/search", "/v1/embeddings", "/v1/files", "/v1/responses/other"} {
		if modelOverrideEndpoint("POST", path) || modelOverrideEndpoint("GET", path) {
			t.Fatal(path)
		}
	}
	for _, path := range []string{"/backend-api/codex/responses", "/backend-api/codex/responses/compact", "/backend-api/conversation", "/v1/responses", "/v1/chat/completions"} {
		if !modelOverrideEndpoint("POST", path) || modelOverrideEndpoint("GET", path) {
			t.Fatal(path)
		}
	}
}

func TestOverrideAdminRequiresTokenAndOnlyChangesProcess(t *testing.T) {
	e, s := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), "metadata")
	send := func(body, token, origin string) int {
		req, _ := http.NewRequest("POST", s.URL+"/__recorder/api/model-override", strings.NewReader(body))
		req.Header.Set("X-Recorder-Token", token)
		req.Header.Set("Origin", origin)
		resp, err := s.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	body := `{"enabled":true,"model":"luna"}`
	if send(body, "", "") != 403 || send(body, e.Token(), "https://untrusted.invalid") != 403 {
		t.Fatal("unauthenticated switch")
	}
	for _, bad := range []string{`{"model":"luna"}`, `{"enabled":true}`, `{"enabled":"yes","model":"luna"}`, `{"enabled":true,"model":"luna","extra":true}`, body + body} {
		if send(bad, e.Token(), "") != 400 {
			t.Fatal("invalid policy accepted", bad)
		}
	}
	if e.ModelOverride().Enabled {
		t.Fatal("invalid action changed policy")
	}
	if send(body, e.Token(), "") != 200 || !e.ModelOverride().Enabled || e.config.ForceModelEnabled {
		t.Fatal("live policy not isolated from startup config")
	}
	if send(`{"enabled":false,"model":""}`, e.Token(), "") != 200 || e.ModelOverride().Enabled {
		t.Fatal("cannot disable")
	}
}

type overrideGate struct {
	once    sync.Once
	entered chan struct{}
	release <-chan struct{}
	input   io.Reader
}

func (g *overrideGate) Read(p []byte) (int, error) {
	g.once.Do(func() { close(g.entered) })
	<-g.release
	return g.input.Read(p)
}
func (g *overrideGate) Close() error { return nil }

func TestOverridePolicyIsImmutableForInFlightRequest(t *testing.T) {
	var got string
	e, s := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = modelaudit.RequestModel(b)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":%q}`, got)
	}), "metadata")
	e.SetModelOverride(true, "luna")
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	req := httptest.NewRequest("POST", s.URL+"/backend-api/codex/responses", nil)
	req.RequestURI = req.URL.RequestURI()
	req.RemoteAddr = "127.0.0.1:20000"
	req.Header.Set("Content-Type", "application/json")
	req.Body = &overrideGate{entered: entered, release: release, input: strings.NewReader(requestFixture)}
	rr := httptest.NewRecorder()
	go func() { defer close(finished); e.ServeHTTP(rr, req) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("request not admitted")
	}
	e.SetModelOverride(true, "terra")
	once.Do(func() { close(release) })
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("request stuck")
	}
	if got != "luna" || rr.Code != 200 || e.ModelOverride().Model != "terra" {
		t.Fatal(got, rr.Code, e.ModelOverride())
	}
	rec := latest(t, e)
	if rec.ModelOverride.Policy.Model != "luna" || rec.Model.Forwarded != "luna" {
		t.Fatal("snapshot changed", rec.ModelOverride)
	}
}

func TestOverrideWhileRecordingPausedAndNoAutomaticRetry(t *testing.T) {
	var calls atomic.Int32
	e, s := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		if modelaudit.RequestModel(b) != "luna" {
			t.Error("override disappeared during capture pause")
		}
		w.WriteHeader(429)
	}), "metadata")
	e.SetModelOverride(true, "luna")
	e.enabled.Store(false)
	resp, _ := post(t, s, requestFixture, nil)
	if resp.StatusCode != 429 || calls.Load() != 1 || len(e.store.List(100)) != 0 {
		t.Fatal("replayed or recorded paused request")
	}
}

func TestLegacyAliasRulesIgnoredByOfflineAnalysis(t *testing.T) {
	var rec Record
	json.Unmarshal([]byte(`{"schema_version":1,"mode":"redacted","model_detection":{"requested_model":"astra","forwarded_model":"astra","comparison_aliases":{"luna":"astra"}},"response_body":{"observed_eof":true,"json":{"model":"luna"}}}`), &rec)
	if AnalyzeSaved(rec).Verdict != "mismatch" {
		t.Fatal("legacy aliases still active")
	}
}
