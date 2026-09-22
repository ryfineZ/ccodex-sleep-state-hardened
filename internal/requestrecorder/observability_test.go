package requestrecorder

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestQuotaWindowsPreserveDurationAndRejectBadNumbers(t *testing.T) {
	q := &quotaCollector{}
	h := make(http.Header)
	h.Set("X-Codex-Primary-Used-Percent", "83.5")
	h.Set("X-Codex-Primary-Window-Minutes", "10080")
	h.Set("X-Codex-Secondary-Used-Percent", "NaN")
	h.Set("X-Codex-Secondary-Reset-At", "1234")
	h.Add("X-Codex-Other-Primary-Used-Percent", "20")
	h.Add("X-Codex-Other-Primary-Used-Percent", "40")
	q.Headers(h, "response_header", 0, 12)
	if len(q.Windows) != 3 {
		t.Fatal(q.Windows)
	}
	var primary, secondary, duplicate *QuotaWindow
	for i := range q.Windows {
		w := &q.Windows[i]
		if w.LimitID == "codex-other" {
			duplicate = w
		} else if w.Window == "primary" {
			primary = w
		} else {
			secondary = w
		}
	}
	if primary == nil || primary.WindowMinutes == nil || *primary.WindowMinutes != 10080 || *primary.UsedPercent != 83.5 {
		t.Fatal(primary)
	}
	if secondary == nil || secondary.UsedPercent != nil || len(secondary.Invalid) != 1 || duplicate.UsedPercent != nil {
		t.Fatal(q.Windows)
	}
	for i := 0; i < 100; i++ {
		q.Headers(h, "response_header", 0, 0)
	}
	if len(q.Windows) > 64 || !q.Limited {
		t.Fatal("quota observation bound")
	}
}
func TestMetadataQuotaAndRateEventsWithoutBodyRetention(t *testing.T) {
	q := &quotaCollector{}
	q.Event("response.metadata", []byte(`{"headers":{"x-codex-primary-used-percent":["25"],"x-codex-primary-window-minutes":"300"}}`), 1, 9)
	q.Event("codex.rate_limits", []byte(`{"rate_limits":{"secondary":{"used_percent":60,"window_minutes":10080,"reset_at":2000000000}}}`), 2, 20)
	if len(q.Windows) != 2 || q.Windows[0].Source != "sse_metadata" || q.Windows[1].Source != "sse_rate_limits" || *q.Windows[1].WindowMinutes != 10080 {
		t.Fatal(q.Windows)
	}
	e, s := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.metadata\ndata: {\"headers\":{\"x-codex-primary-used-percent\":\"12\",\"x-codex-primary-window-minutes\":\"60\"}}\n\n"+completeFixture)
	}), "metadata")
	post(t, s, requestFixture, nil)
	rec := latest(t, e)
	if len(rec.Quota) != 1 || *rec.Quota[0].UsedPercent != 12 || len(rec.Response.Raw) != 0 || rec.Response.Events[0].Data != nil {
		t.Fatal(rec)
	}
}
func TestLateModelBeyond32KiBAndIncompleteFinalEvent(t *testing.T) {
	data := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"" + strings.Repeat("x", 40<<10) + "\"}\n\n" + completeFixture
	sample := newSample(time.Now(), 2<<20)
	sample.feed([]byte(data), io.EOF)
	p := pending{record: Record{Mode: "metadata", RequestHeaders: http.Header{"Content-Type": {"application/json"}}, ResponseHeaders: http.Header{"Content-Type": {"text/event-stream"}}}, response: sample.freeze()}
	rec := p.analyse()
	if rec.Model.Verdict != "observed" || rec.Model.Declared[0] != "astra" {
		t.Fatal(rec.Model)
	}
	c := newSample(time.Now(), 1<<20)
	c.feed([]byte("data: {\"model\":\"luna\"}"), io.EOF)
	p.response = c.freeze()
	rec = p.analyse()
	if rec.Model.Verdict != "unknown" || !rec.Model.Limited || rec.Response.StreamOutcome != "observation_limited" {
		t.Fatal(rec.Model, rec.Response)
	}
}
func TestExactOfflineAndCredentialScopedObservations(t *testing.T) {
	e, s := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		if string(data) != requestFixture {
			t.Error("request rewritten")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"astra-snapshot"}`)
	}), "redacted")
	e.config.RouteLabel = "fixture-route"
	for _, key := range []string{"fixture-account-one", "fixture-account-two"} {
		post(t, s, requestFixture, http.Header{"Authorization": {"Bearer " + key}, "X-Codex-Turn-State": {"fixture-state"}, "Cookie": {"fixture=cookie"}})
	}
	waitRecords(t, e, 2)
	groups := e.store.Observations()
	if len(groups) != 2 || groups[0].Scope == groups[1].Scope || groups[0].Different != 1 {
		t.Fatal(groups)
	}
	rec := latest(t, e)
	if rec.Model.Verdict != "mismatch" || AnalyzeSaved(rec).Verdict != "mismatch" || rec.Context.StateLength != 13 {
		t.Fatal(rec.Model, rec.Context)
	}
	encoded, _ := json.Marshal(groups)
	if strings.Contains(string(encoded), "fixture-account") {
		t.Fatal("scope leaked raw identity")
	}
	req, _ := http.NewRequest("GET", s.URL+"/__recorder/api/observations", nil)
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatal("unauthorized observations")
	}
	req.Header.Set("X-Recorder-Token", e.Token())
	resp, err = s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
}
func TestRouteLabelValidation(t *testing.T) {
	c := Default()
	c.RouteLabel = "line\nbreak"
	if c.Validate() == nil {
		t.Fatal("multiline route label")
	}
}

func TestMalformedCompletionNeverCountsAsCompleted(t *testing.T) {
	for _, payload := range []string{"not json", "[]", "null"} {
		sample := newSample(time.Now(), 1<<20)
		sample.feed([]byte("event: response.completed\ndata: "+payload+"\n\n"), io.EOF)
		b := analyseBody(sample.freeze(), http.Header{"Content-Type": {"text/event-stream"}}, "metadata", nil, true)
		if b.StreamOutcome != "observation_limited" || !b.EventsLimited {
			t.Fatal(payload, b)
		}
	}
}
func TestDashboardContainsNewControlsAndNoExternalScripts(t *testing.T) {
	html, err := assets.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{`id="verdict"`, `id="groups"`, `id="details"`, `id="token"`} {
		if !strings.Contains(string(html), id) {
			t.Fatal("missing control", id)
		}
	}
	js, err := assets.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(js), "innerHTML") || strings.Contains(string(html), `src="https://`) {
		t.Fatal("unsafe or remote rendering dependency")
	}
}
