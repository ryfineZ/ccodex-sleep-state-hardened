package requestrecorder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testSetup(t *testing.T, h http.Handler) (*Engine, *SetupController, *httptest.Server, []byte) {
	t.Helper()
	e, s := setup(t, h, "metadata")
	home := t.TempDir()
	original := []byte(fmt.Sprintf("# preserve all original bytes\nmodel='fixture-custom'\nmodel_provider='existing'\n[model_providers.existing]\nname='Existing provider'\nbase_url='%s/v1'\nwire_api='responses'\nenv_key='FIXTURE_KEY_NEVER_READ'\nrequires_openai_auth=false\n", e.config.Upstream))
	if err := os.WriteFile(filepath.Join(home, "config.toml"), original, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := e.EnableSetup(filepath.Join(t.TempDir(), "control"), home, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Restore(); c.Close() })
	return e, c, s, original
}
func setupAction(t *testing.T, s *httptest.Server, e *Engine, action, body string, authorized bool) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", s.URL+"/__recorder/api/setup/"+action, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authorized {
		req.Header.Set("X-Recorder-Token", e.Token())
	}
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
func TestOneClickConnectRoundTripAndExactRestore(t *testing.T) {
	var calls atomic.Int32
	e, c, s, original := testSetup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/responses" {
			t.Error(r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		if string(b) != requestFixture {
			t.Error("request changed")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"astra"}`)
	}))
	if c.Status().Managed || c.Status().RecoveryPending {
		t.Fatal("setup wrote config before button")
	}
	if err := c.Connect(context.Background(), "direct", ""); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("setup sent a model request")
	}
	b, _ := os.ReadFile(filepath.Join(c.home, "config.toml"))
	if !bytes.Contains(b, []byte("model='fixture-custom'")) || !bytes.Contains(b, []byte(s.Listener.Addr().String()+"/v1")) {
		t.Fatal("provider patch incorrect")
	}
	req, _ := http.NewRequest("POST", s.URL+"/v1/responses", strings.NewReader(requestFixture))
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || calls.Load() != 1 {
		t.Fatal(resp.StatusCode, calls.Load())
	}
	if err := c.Restore(); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(c.home, "config.toml"))
	if !bytes.Equal(b, original) {
		t.Fatal("not byte-exact restore")
	}
	if c.Status().Managed {
		t.Fatal("still managed")
	}
	if e.ModelOverride().Enabled {
		t.Fatal("override unexpectedly enabled")
	}
}
func TestOneClickAuthorizationAndExplicitConsent(t *testing.T) {
	e, c, s, original := testSetup(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unexpected upstream") }))
	for _, tc := range []struct {
		auth bool
		body string
		want int
	}{{false, `{"confirm":true}`, 403}, {true, `{}`, 400}, {true, `{"confirm":true,"unknown":1}`, 400}} {
		r := setupAction(t, s, e, "connect", tc.body, tc.auth)
		r.Body.Close()
		if r.StatusCode != tc.want {
			t.Fatal(r.StatusCode, tc.want)
		}
	}
	b, _ := os.ReadFile(filepath.Join(c.home, "config.toml"))
	if !bytes.Equal(b, original) {
		t.Fatal("unauthorized write")
	}
	r := setupAction(t, s, e, "connect", `{"confirm":true,"proxy_mode":"direct"}`, true)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	r = setupAction(t, s, e, "restore", `{"confirm":true}`, true)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
}
func TestOneClickConfigConflictNeverOverwritten(t *testing.T) {
	_, c, s, _ := testSetup(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("request sent after config changed") }))
	if err := c.Connect(context.Background(), "direct", ""); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(c.home, "config.toml")
	b, _ := os.ReadFile(p)
	b = bytes.Replace(b, []byte("model_provider = \"ccodex-sleep-state\""), []byte("model_provider = 'other'"), 1)
	// Source uses a compact single-quoted setting; replace installed value regardless of whitespace.
	b = bytes.Replace(b, []byte("\"ccodex-sleep-state\""), []byte("\"external-change\""), 1)
	_ = os.WriteFile(p, b, 0600)
	if !c.Status().Conflict {
		t.Fatal("conflict not detected")
	}
	resp, _ := post(t, s, requestFixture, nil)
	if resp.StatusCode != 409 {
		t.Fatal(resp.StatusCode)
	}
	if c.Restore() == nil {
		t.Fatal("overwrote externally changed provider")
	}
	after, _ := os.ReadFile(p)
	if !bytes.Equal(after, b) {
		t.Fatal("external edits overwritten")
	}
}
func TestOneClickBusyAndInvalidProxyDoNotChangeConfig(t *testing.T) {
	_, c, _, original := testSetup(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if c.Connect(context.Background(), "manual", "bad-proxy") == nil {
		t.Fatal("bad proxy accepted")
	}
	c.mu.RLock()
	err := c.Connect(context.Background(), "direct", "")
	c.mu.RUnlock()
	if err == nil {
		t.Fatal("did not refuse busy operation")
	}
	b, _ := os.ReadFile(filepath.Join(c.home, "config.toml"))
	if !bytes.Equal(b, original) {
		t.Fatal("config changed")
	}
}
func TestOneClickCrashRecoveryAndSingleInstance(t *testing.T) {
	e, c, _, original := testSetup(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if _, err := e.EnableSetup(c.dir, c.home, "", nil); err == nil {
		t.Fatal("second owner allowed")
	}
	if err := c.Connect(context.Background(), "direct", ""); err != nil {
		t.Fatal(err)
	}
	c.Close() // simulate app termination without restoring; receipt remains
	e2, err := New(e.config)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	recovered, err := e2.EnableSetup(c.dir, c.home, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if !recovered.Status().RecoveryPending || recovered.Status().Managed {
		t.Fatal("missing recovery state")
	}
	if err := recovered.Restore(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(c.home, "config.toml"))
	if !bytes.Equal(b, original) {
		t.Fatal("crash restore failed")
	}
	c.pending = false
	c.managed = false // old cleanup has nothing to restore
}
func TestLaunchTicketIsOneUseAndRejectsForeignOrigin(t *testing.T) {
	e, s := setup(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), "metadata")
	ticket := strings.Split(e.LaunchURL(), "#launch=")[1]
	for _, tc := range []struct {
		origin string
		want   int
	}{{"https://evil.example", 403}, {"", 200}, {"", 401}} {
		req, _ := http.NewRequest("POST", s.URL+"/__recorder/api/launch", strings.NewReader(`{"ticket":"`+ticket+`"}`))
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		resp, err := s.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&v)
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatal(resp.StatusCode)
		}
		if tc.want == 200 && v["token"] != e.Token() {
			t.Fatal("bad exchanged token")
		}
	}
	e.launch.renew()
	e.launch.expires = time.Now().Add(-time.Second)
	req, _ := http.NewRequest("POST", s.URL+"/__recorder/api/launch", strings.NewReader(`{"ticket":"`+e.launch.value+`"}`))
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatal("expired ticket accepted")
	}
}
func TestOneClickReopenAndStop(t *testing.T) {
	e, c, s, original := testSetup(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if err := c.WriteRuntime(); err != nil {
		t.Fatal(err)
	}
	link, err := ExistingSetupURL(context.Background(), c.dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link, s.URL+"/__recorder/#launch=") || strings.Contains(link, e.Token()) {
		t.Fatal("unsafe reopen URL")
	}
	if err := c.Connect(context.Background(), "direct", ""); err != nil {
		t.Fatal(err)
	}
	var stopped atomic.Bool
	c.stop = func() { stopped.Store(true) }
	resp := setupAction(t, s, e, "stop", `{"confirm":true}`, true)
	resp.Body.Close()
	if resp.StatusCode != 200 || !stopped.Load() {
		t.Fatal("stop action failed")
	}
	b, _ := os.ReadFile(filepath.Join(c.home, "config.toml"))
	if !bytes.Equal(b, original) {
		t.Fatal("did not restore before stop")
	}
}
func TestBoundedProxyDiscoveryUsesOnlySOCKSGreeting(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	done := make(chan []byte, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			done <- nil
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(time.Second))
		b := make([]byte, 3)
		_, _ = io.ReadFull(c, b)
		done <- b
		_, _ = c.Write([]byte{5, 0})
	}()
	got := discoverRecorderSOCKS(context.Background(), []string{l.Addr().String()})
	if got != "socks5h://"+l.Addr().String() {
		t.Fatal(got)
	}
	if !bytes.Equal(<-done, []byte{5, 1, 0}) {
		t.Fatal("unexpected proxy traffic")
	}
}
