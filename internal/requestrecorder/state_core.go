package requestrecorder

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gylive/ccodex-sleep-state/internal/cookiebundle"
	"github.com/gylive/ccodex-sleep-state/internal/routehealth"
	"github.com/gylive/ccodex-sleep-state/internal/turnstate"
)

// CorePolicy controls the explicitly enabled experimental collector. Freshness
// is a conservative LOCAL reuse ceiling, never a server validity assertion.
type CorePolicy struct {
	Enabled         bool     `json:"enabled"`
	AccountMode     string   `json:"account_mode"`
	FreshSeconds    int      `json:"freshness_seconds"`
	CooldownSeconds int      `json:"probe_cooldown_seconds"`
	ProbeSeconds    int      `json:"probe_timeout_seconds"`
	MaxProbes       int      `json:"max_probes_per_round"`
	HourlyBudget    int      `json:"probe_budget_per_hour"`
	ProxyURLs       []string `json:"proxy_urls,omitempty"`
}

func coreDefaults() CorePolicy {
	return CorePolicy{AccountMode: "auto", FreshSeconds: 120, CooldownSeconds: 30, ProbeSeconds: 20, MaxProbes: 2, HourlyBudget: 30}
}
func (p CorePolicy) validate() error {
	if p.AccountMode != "auto" && p.AccountMode != "personal" && p.AccountMode != "team" {
		return errors.New("账号规则需为 auto、personal 或 team")
	}
	if p.FreshSeconds < 30 || p.FreshSeconds > 600 || p.CooldownSeconds < 30 || p.CooldownSeconds > 3600 || p.ProbeSeconds < 1 || p.ProbeSeconds > 60 || p.MaxProbes < 1 || p.MaxProbes > 6 || p.HourlyBudget < 1 || p.HourlyBudget > 120 || len(p.ProxyURLs) > 32 {
		return errors.New("采集参数超出安全范围")
	}
	for _, raw := range p.ProxyURLs {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return errors.New("采集代理地址无效；池中不允许空地址或隐式直连")
		}
	}
	return nil
}
func coreAllowed(u *url.URL) bool {
	if u == nil {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return (u.Scheme == "https" && strings.EqualFold(u.Hostname(), "chatgpt.com") && (u.Port() == "" || u.Port() == "443")) || (u.Scheme == "http" && ip != nil && ip.IsLoopback())
}

type coreError struct {
	Status int
	Code   string
	Retry  int
}

func (e *coreError) Error() string                      { return e.Code }
func coreUnavailable(code string, retry int) *coreError { return &coreError{503, code, max(1, retry)} }

type CoreAudit struct {
	Enabled           bool   `json:"enabled"`
	Injected          bool   `json:"injected"`
	Model             string `json:"model,omitempty"`
	Route             string `json:"route,omitempty"`
	StateFingerprint  string `json:"state_fingerprint,omitempty"`
	StateLength       int    `json:"state_length,omitempty"`
	StateVersion      uint64 `json:"state_version,omitempty"`
	StateAgeSeconds   int    `json:"state_age_seconds,omitempty"`
	CookieFingerprint string `json:"cookie_fingerprint,omitempty"`
	CookieVersion     uint64 `json:"cookie_version,omitempty"`
	CookieCount       int    `json:"cookie_count,omitempty"`
}
type coreRoute struct {
	id        string
	transport http.RoundTripper
	owned     bool
}
type coreCandidate struct {
	token                      turnstate.Token
	route                      coreRoute
	acquired, deadline, lastOK time.Time
	jar                        *cookiebundle.Store
	names                      map[string]bool
	version                    uint64
	strikes                    int
}
type coreSession struct {
	id, scope, model, origin string
	headers                  http.Header
	active, ready            *coreCandidate
	nextProbe, lastUsed      time.Time
	probing                  chan struct{}
	lastResult               string
	version, hits            uint64
	cursor                   int
}
type coreGuard struct {
	status                             int
	until, nextProbe, window, lastUsed time.Time
	probes                             int
}
type CoreProbe struct {
	At          time.Time `json:"at"`
	Session     string    `json:"session"`
	Model       string    `json:"model"`
	Route       string    `json:"route"`
	Status      int       `json:"http_status"`
	Result      string    `json:"result"`
	StateLength int       `json:"state_length"`
	CookieCount int       `json:"cookie_count"`
}
type StateCore struct {
	idle      time.Duration
	mu        sync.Mutex
	policy    CorePolicy
	epoch     uint64
	base      *url.URL
	routes    []coreRoute
	sessions  map[string]*coreSession
	guards    map[string]*coreGuard
	health    *routehealth.Manager
	probeSlot chan struct{}
	history   []CoreProbe
	now       func() time.Time
	ctx       context.Context
	cancel    context.CancelFunc
}

func newStateCore(p CorePolicy) *StateCore {
	ctx, cancel := context.WithCancel(context.Background())
	c := &StateCore{policy: p, epoch: 1, sessions: map[string]*coreSession{}, guards: map[string]*coreGuard{}, probeSlot: make(chan struct{}, 1), now: time.Now, ctx: ctx, cancel: cancel}
	c.health = routehealth.New(routehealth.Config{Threshold: 2, Base: 30 * time.Second, Maximum: 5 * time.Minute, Now: func() time.Time { return c.now() }})
	return c
}
func (c *StateCore) options() CorePolicy {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.policy
	p.ProxyURLs = append([]string(nil), p.ProxyURLs...)
	return p
}
func (c *StateCore) configure(p CorePolicy, base *url.URL, proxy string, tr http.RoundTripper, config Config) error {
	if err := p.validate(); err != nil {
		return err
	}
	if p.Enabled && !coreAllowed(base) {
		return errors.New("state/Cookie 采集仅用于官方 Codex 上游；第三方或 API 上游请用仅记录模式")
	}
	routes := []coreRoute{}
	seen := map[string]bool{}
	if len(p.ProxyURLs) == 0 {
		routes = append(routes, coreRoute{"connection-" + fingerprint(base.String()+"\x00"+proxy), tr, false})
	} else {
		for _, raw := range p.ProxyURLs {
			if seen[raw] {
				continue
			}
			seen[raw] = true
			cfg := config
			cfg.ProxyURL = raw
			routes = append(routes, coreRoute{"proxy-" + fingerprint(base.String()+"\x00"+raw), recorderTransport(cfg), true})
		}
	}
	c.mu.Lock()
	old := c.routes
	c.routes = routes
	c.idle = time.Duration(config.IdleSeconds) * time.Second
	copyURL := *base
	c.base = &copyURL
	c.policy = p
	c.epoch++
	// Discard candidates, not account stops/budgets. Old requests keep their own
	// leases; old probe results cannot publish into this generation.
	for _, s := range c.sessions {
		s.active = nil
		s.ready = nil
		s.lastResult = "configuration_changed"
	}
	c.mu.Unlock()
	for _, r := range old {
		if r.owned {
			closeCoreTransport(r.transport)
		}
	}
	return nil
}
func closeCoreTransport(tr http.RoundTripper) {
	if x, ok := tr.(interface{ CloseIdleConnections() }); ok {
		x.CloseIdleConnections()
	}
}
func (c *StateCore) close() {
	c.cancel()
	c.mu.Lock()
	routes := append([]coreRoute(nil), c.routes...)
	c.mu.Unlock()
	for _, r := range routes {
		if r.owned {
			closeCoreTransport(r.transport)
		}
	}
}
func (c *StateCore) disable() { c.mu.Lock(); defer c.mu.Unlock(); c.policy.Enabled = false; c.epoch++ }
func coreScope(h http.Header, origin string) string {
	return fingerprint(origin + "\x00" + h.Get("Authorization") + "\x00" + h.Get("ChatGPT-Account-Id") + "\x00" + h.Get("OpenAI-Organization") + "\x00" + h.Get("OpenAI-Project"))
}
func (c *StateCore) guardLocked(scope string) *coreGuard {
	g := c.guards[scope]
	if g == nil {
		if len(c.guards) >= 128 {
			return nil
		}
		g = &coreGuard{}
		c.guards[scope] = g
	}
	g.lastUsed = c.now()
	return g
}
func (c *StateCore) rejectionLocked(scope string) *coreError {
	g := c.guards[scope]
	if g == nil {
		return nil
	}
	if g.status == 401 || g.status == 403 {
		return &coreError{g.status, "state_core_auth_blocked", 0}
	}
	if c.now().Before(g.until) {
		return &coreError{429, "state_core_rate_limited", max(1, int(g.until.Sub(c.now()).Seconds())+1)}
	}
	return nil
}
func (c *StateCore) reject(scope string, status int, delay time.Duration) {
	if status != 401 && status != 403 && status != 429 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	g := c.guardLocked(scope)
	if g == nil {
		return
	}
	if status == 401 || status == 403 {
		g.status = status
		return
	}
	g.status = 429
	until := c.now().Add(max(30*time.Second, delay))
	if until.After(g.until) {
		g.until = until
	}
}
func coreRetry(h http.Header) time.Duration {
	value := h.Get("Retry-After")
	if d, err := time.ParseDuration(value + "s"); err == nil && d > 0 {
		return d
	}
	if t, err := http.ParseTime(value); err == nil {
		return max(0, time.Until(t))
	}
	return 0
}
func coreBlocks(mode string, h http.Header) int {
	if mode == "team" {
		return 12
	}
	if mode == "personal" {
		return 10
	}
	parts := strings.Split(strings.TrimPrefix(h.Get("Authorization"), "Bearer "), ".")
	if len(parts) != 3 {
		return 10
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(raw) > 16384 {
		return 10
	}
	var v struct {
		Auth struct {
			Plan    string `json:"chatgpt_plan_type"`
			Account string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return 10
	}
	if selected := h.Get("ChatGPT-Account-Id"); selected != "" && selected != v.Auth.Account {
		return 10
	}
	if v.Auth.Plan == "team" || v.Auth.Plan == "business" {
		return 12
	}
	return 10
}
func coreHeaders(h http.Header) http.Header {
	out := make(http.Header)
	for _, k := range []string{"Authorization", "ChatGPT-Account-Id", "OpenAI-Organization", "OpenAI-Project", "User-Agent", "Version", "Originator", "OpenAI-Beta"} {
		if v := h.Get(k); v != "" {
			out.Set(k, v)
		}
	}
	return out
}
func coreCookieLease(a *coreCandidate, scope string, u *url.URL) (cookiebundle.Lease, bool) {
	if a == nil {
		return cookiebundle.Lease{}, false
	}
	l := a.jar.Begin(cookiebundle.Scope{Credential: scope, Route: a.route.id}, u)
	names := map[string]bool{}
	for _, v := range l.Cookies {
		names[v.Name] = true
	}
	if len(a.names) == 0 {
		return l, false
	}
	for n := range a.names {
		if !names[n] {
			return l, false
		}
	}
	return l, true
}
func (c *StateCore) usableLocked(s *coreSession, a *coreCandidate, u *url.URL) bool {
	if a == nil || a.strikes >= 2 || !c.now().Before(a.deadline) || !c.health.Available(a.route.id) {
		return false
	}
	_, ok := coreCookieLease(a, s.scope, u)
	return ok
}
func (c *StateCore) selectLocked(s *coreSession, u *url.URL) *coreCandidate {
	if c.usableLocked(s, s.ready, u) && (!c.usableLocked(s, s.active, u) || s.active.deadline.Sub(c.now()) <= 20*time.Second) {
		s.active = s.ready
		s.ready = nil
	}
	if c.usableLocked(s, s.active, u) {
		return s.active
	}
	return nil
}
func (c *StateCore) borrow(h http.Header, model string, u *url.URL) (*coreSession, error) {
	auth := h.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") || len(strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))) < 8 || len(auth) > 16384 {
		return nil, &coreError{401, "state_core_auth_required", 0}
	}
	scope := coreScope(h, u.Scheme+"://"+u.Host)
	key := scope + "\x00" + model
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for k, s := range c.sessions {
		if s.probing == nil && now.Sub(s.lastUsed) > 30*time.Minute {
			delete(c.sessions, k)
		}
	}
	if err := c.rejectionLocked(scope); err != nil {
		return nil, err
	}
	if c.guardLocked(scope) == nil {
		return nil, coreUnavailable("state_core_credential_capacity", 30)
	}
	s := c.sessions[key]
	if s == nil {
		if len(c.sessions) >= 64 {
			return nil, coreUnavailable("state_core_session_capacity", 30)
		}
		s = &coreSession{id: fingerprint(key), scope: scope, model: model, origin: u.Scheme + "://" + u.Host}
		c.sessions[key] = s
	}
	s.headers = coreHeaders(h)
	s.lastUsed = now
	return s, nil
}

// ensure performs single-flight, request-driven refresh. There is no periodic
// hunter. A manual refresh has exactly the same cooldown, budget and stop rules.
func (c *StateCore) ensure(ctx context.Context, s *coreSession, force bool) (*coreCandidate, error) {
	c.mu.Lock()
	u, _ := url.Parse(s.origin + "/backend-api/codex/responses")
	if err := c.rejectionLocked(s.scope); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	if !c.policy.Enabled || c.base == nil || strings.TrimRight(c.base.String(), "/") != s.origin || c.ctx.Err() != nil {
		c.mu.Unlock()
		return nil, coreUnavailable("state_core_configuration_changed", 1)
	}
	active := c.selectLocked(s, u)
	if active != nil && !force && active.deadline.Sub(c.now()) > 20*time.Second {
		c.mu.Unlock()
		return active, nil
	}
	if pending := s.probing; pending != nil {
		c.mu.Unlock()
		select {
		case <-pending:
			return c.ensure(ctx, s, false)
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.ctx.Done():
			return nil, context.Canceled
		}
	}
	g := c.guards[s.scope]
	next := maxCoreTime(s.nextProbe, g.nextProbe)
	if c.now().Before(next) {
		c.mu.Unlock()
		if active != nil && !force {
			return active, nil
		}
		return nil, coreUnavailable("state_core_probe_cooldown", int(next.Sub(c.now()).Seconds())+1)
	}
	done := make(chan struct{})
	s.probing = done
	p := c.policy
	epoch := c.epoch
	headers := s.headers.Clone()
	routes := append([]coreRoute(nil), c.routes...)
	start := s.cursor
	s.cursor++
	c.mu.Unlock()
	attempts := 0
	slot := false
	defer func() {
		c.mu.Lock()
		if attempts > 0 {
			next := c.now().Add(time.Duration(p.CooldownSeconds) * time.Second)
			s.nextProbe = maxCoreTime(s.nextProbe, next)
			g.nextProbe = maxCoreTime(g.nextProbe, next)
		}
		s.probing = nil
		close(done)
		c.mu.Unlock()
		if slot {
			<-c.probeSlot
		}
	}()
	select {
	case c.probeSlot <- struct{}{}:
		slot = true
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, context.Canceled
	}
	// A different model/account may have used the slot while this request waited.
	// Recheck shared cooldown under the same lock before spending any budget.
	c.mu.Lock()
	next = maxCoreTime(s.nextProbe, g.nextProbe)
	if c.now().Before(next) {
		active = c.selectLocked(s, u)
		c.mu.Unlock()
		if active != nil && !force {
			return active, nil
		}
		return nil, coreUnavailable("state_core_probe_cooldown", int(next.Sub(c.now()).Seconds())+1)
	}
	c.mu.Unlock()
	last := coreUnavailable("state_core_routes_cooling", 30)
	for i := 0; i < len(routes) && attempts < p.MaxProbes; i++ {
		r := routes[(start+i)%len(routes)]
		permit, ok := c.health.Begin(r.id)
		if !ok {
			continue
		}
		c.mu.Lock()
		if c.epoch != epoch || !c.policy.Enabled {
			c.mu.Unlock()
			permit.Finish(routehealth.Neutral)
			return nil, coreUnavailable("state_core_configuration_changed", 1)
		}
		if err := c.rejectionLocked(s.scope); err != nil {
			c.mu.Unlock()
			permit.Finish(routehealth.Neutral)
			return nil, err
		}
		if g.window.IsZero() || c.now().Sub(g.window) >= time.Hour {
			g.window = c.now()
			g.probes = 0
		}
		if g.probes >= p.HourlyBudget {
			retry := max(1, int(g.window.Add(time.Hour).Sub(c.now()).Seconds())+1)
			active = c.selectLocked(s, u)
			c.mu.Unlock()
			permit.Finish(routehealth.Neutral)
			if active != nil && !force {
				return active, nil
			}
			return nil, coreUnavailable("state_core_probe_budget", retry)
		}
		if ctx.Err() != nil || c.ctx.Err() != nil {
			c.mu.Unlock()
			permit.Finish(routehealth.Neutral)
			return nil, context.Canceled
		}
		g.probes++
		attempts++
		g.nextProbe = maxCoreTime(g.nextProbe, c.now().Add(time.Duration(p.CooldownSeconds)*time.Second))
		c.mu.Unlock()
		result := c.probe(ctx, s.scope, s.model, headers, u, r, p)
		permit.Finish(result.health)
		c.reject(s.scope, result.status, result.retry)
		c.mu.Lock()
		s.lastResult = result.reason
		c.history = append(c.history, CoreProbe{c.now().UTC(), s.id, s.model, r.id, result.httpStatus, result.reason, len(result.token.Value), result.cookieCount})
		if len(c.history) > 50 {
			c.history = append([]CoreProbe(nil), c.history[len(c.history)-50:]...)
		}
		if result.candidate != nil && c.epoch == epoch && c.policy.Enabled && ctx.Err() == nil && c.ctx.Err() == nil {
			s.version++
			result.candidate.version = s.version
			if c.usableLocked(s, s.active, u) && s.active.deadline.Sub(c.now()) > 20*time.Second && s.active.token.Fingerprint != result.candidate.token.Fingerprint {
				s.ready = result.candidate
			} else {
				s.active = result.candidate
				s.ready = nil
			}
			accepted := c.selectLocked(s, u)
			c.mu.Unlock()
			if accepted != nil {
				return accepted, nil
			}
			return nil, coreUnavailable("state_core_candidate_unusable", p.CooldownSeconds)
		}
		stop := c.rejectionLocked(s.scope)
		c.mu.Unlock()
		if stop != nil {
			return nil, stop
		}
		last = coreUnavailable(result.reason, p.CooldownSeconds)
		if result.terminal {
			break
		}
	}
	c.mu.Lock()
	active = c.selectLocked(s, u)
	if attempts == 0 {
		s.lastResult = last.Code
	}
	c.mu.Unlock()
	if active != nil && !force {
		return active, nil
	}
	return nil, last
}
func maxCoreTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

type coreRequestLease struct {
	idle      time.Duration
	epoch     uint64
	request   *http.Request
	transport http.RoundTripper
	session   *coreSession
	candidate *coreCandidate
	cookies   cookiebundle.Lease
	audit     CoreAudit
	core      *StateCore
}

func (c *StateCore) prepare(req *http.Request) (*coreRequestLease, error) {
	passthrough := &coreRequestLease{request: req, core: c}
	origin := req.URL.Scheme + "://" + req.URL.Host
	c.mu.Lock()
	p := c.policy
	stop := c.rejectionLocked(coreScope(req.Header, origin))
	c.mu.Unlock()
	if stop != nil {
		return nil, stop
	}
	if !p.Enabled || req.Method != "POST" || req.URL.Path != "/backend-api/codex/responses" {
		return passthrough, nil
	}
	if !coreAllowed(req.URL) {
		return nil, coreUnavailable("state_core_official_upstream_required", 30)
	}
	if req.Body == nil {
		return nil, &coreError{400, "state_core_model_required", 0}
	}
	raw, err := io.ReadAll(io.LimitReader(req.Body, (16<<20)+1))
	req.Body.Close()
	if err != nil {
		return nil, &coreError{400, "state_core_request_read_failed", 0}
	}
	if len(raw) > 16<<20 {
		return nil, &coreError{413, "state_core_request_limit", 0}
	}
	req.Body = io.NopCloser(bytes.NewReader(raw))
	req.GetBody = nil
	decoded, note := decodeBody(raw, req.Header.Get("Content-Encoding"))
	if note != "" {
		return nil, &coreError{400, "state_core_" + note, 0}
	}
	var input struct {
		Model string            `json:"model"`
		Input []json.RawMessage `json:"input"`
	}
	// Model parsing is independent from input's type: Codex also permits strings.
	var modelInput struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(decoded, &modelInput) != nil || !validModelName(modelInput.Model) {
		return nil, &coreError{400, "state_core_model_required", 0}
	}
	if json.Unmarshal(decoded, &input) == nil && len(input.Input) > 0 {
		var last struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(input.Input[len(input.Input)-1], &last) == nil && last.Type == "compaction_trigger" {
			return passthrough, nil
		}
	}
	s, err := c.borrow(req.Header, modelInput.Model, req.URL)
	if err != nil {
		return nil, err
	}
	a, err := c.ensure(req.Context(), s, false)
	if err != nil {
		return nil, err
	}
	cookies, valid := coreCookieLease(a, s.scope, req.URL)
	if !valid {
		return nil, coreUnavailable("state_core_cookie_expired", 30)
	}
	next := req.Clone(req.Context())
	next.GetBody = nil
	next.Header.Set(turnstate.Header, a.token.Value)
	originalCookies := next.Cookies()
	next.Header.Del("Cookie")
	for _, v := range originalCookies {
		if v.Name != "__cflb" && v.Name != "__oailb" {
			next.AddCookie(v)
		}
	}
	for _, v := range cookies.Cookies {
		next.AddCookie(v)
	}
	// Uncompressed streaming observation is requested only in enabled core mode.
	next.Header.Set("Accept-Encoding", "identity")
	c.mu.Lock()
	epoch := c.epoch
	if !c.policy.Enabled || (s.active != a && s.ready != a) {
		c.mu.Unlock()
		return nil, coreUnavailable("state_core_configuration_changed", 1)
	}
	idle := c.idle
	audit := CoreAudit{true, true, s.model, a.route.id, a.token.Fingerprint, len(a.token.Value), a.version, max(0, int(c.now().Sub(a.token.Issued).Seconds())), cookies.Fingerprint, cookies.Version, len(cookies.Cookies)}
	c.mu.Unlock()
	return &coreRequestLease{epoch: epoch, idle: idle, request: next, transport: a.route.transport, session: s, candidate: a, cookies: cookies, audit: audit, core: c}, nil
}
func (c *StateCore) refresh(ctx context.Context, id string) error {
	c.mu.Lock()
	var session *coreSession
	for _, s := range c.sessions {
		if s.id == id {
			session = s
			break
		}
	}
	c.mu.Unlock()
	if session == nil {
		return errors.New("请先在 Codex 发送消息，凭据只从客户端请求借用")
	}
	_, err := c.ensure(ctx, session, true)
	return err
}
func (c *StateCore) status() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	rows := []map[string]any{}
	now := c.now()
	for _, s := range c.sessions {
		u, _ := url.Parse(s.origin + "/backend-api/codex/responses")
		a := c.selectLocked(s, u)
		phase := "waiting_for_request"
		if !c.policy.Enabled {
			phase = "record_only"
		} else if s.probing != nil {
			phase = "collecting"
		} else if a != nil {
			phase = "ready"
		} else {
			phase = "waiting_for_bundle"
		}
		row := map[string]any{"id": s.id, "model": s.model, "phase": phase, "result": s.lastResult, "injections": s.hits, "credential_scope": s.scope, "ready": c.usableLocked(s, s.ready, u)}
		if err := c.rejectionLocked(s.scope); err != nil {
			row["phase"] = "account_paused"
			row["rejected_status"] = err.Status
			row["retry_after_seconds"] = err.Retry
		}
		next := s.nextProbe
		if g := c.guards[s.scope]; g != nil {
			if g.nextProbe.After(next) {
				next = g.nextProbe
			}
			row["probes_this_hour"] = g.probes
		}
		row["cooldown_seconds"] = max(0, int(next.Sub(now).Seconds())+1)
		if s.active != nil {
			active := s.active
			l, _ := coreCookieLease(active, s.scope, u)
			row["state_fingerprint"] = active.token.Fingerprint
			row["state_length"] = len(active.token.Value)
			row["state_age_seconds"] = max(0, int(now.Sub(active.token.Issued).Seconds()))
			row["local_freshness_remaining"] = max(0, int(active.deadline.Sub(now).Seconds()))
			row["cookie_count"] = len(l.Cookies)
			row["cookie_version"] = l.Version
			row["cookie_fingerprint"] = l.Fingerprint
			row["route"] = active.route.id
			row["last_completed_at"] = active.lastOK
			row["strikes"] = active.strikes
			row["version"] = active.version
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i]["id"].(string) < rows[j]["id"].(string) })
	routes := []map[string]any{}
	for _, r := range c.routes {
		routes = append(routes, map[string]any{"id": r.id, "health": c.health.Status(r.id)})
	}
	p := c.policy
	p.ProxyURLs = nil
	return map[string]any{"policy": p, "sessions": rows, "routes": routes, "proxy_count": len(c.policy.ProxyURLs), "recent_probes": append([]CoreProbe(nil), c.history...), "storage": "memory_only", "refresh_mode": "request_driven", "experimental_cross_turn_reuse": true, "automatic_generation_replay": false}
}
