// Package reliableproxy provides client-owned-state HTTP/SSE forwarding.
// It performs no synthetic model requests and never replaces a client's state.
package reliableproxy

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type RouteConfig struct {
	ID       string `json:"id"`
	ProxyURL string `json:"proxy_url,omitempty"`
}
type Config struct {
	StreamIdleSeconds     int           `json:"stream_idle_seconds"`
	MaxConcurrentRequests int           `json:"max_concurrent_requests"`
	Listen                string        `json:"listen"`
	Upstream              string        `json:"upstream"`
	Routes                []RouteConfig `json:"routes"`
	FailureThreshold      int           `json:"failure_threshold"`
	CircuitBaseSeconds    int           `json:"circuit_base_seconds"`
	CircuitMaxSeconds     int           `json:"circuit_max_seconds"`
	SessionCookieSeconds  int           `json:"session_cookie_seconds"`
	MaxRequestMiB         int           `json:"max_request_mib"`
	ResponseHeaderSeconds int           `json:"response_header_seconds"`
}

func DefaultConfig() Config {
	return Config{StreamIdleSeconds: 90, MaxConcurrentRequests: 32, Listen: "127.0.0.1:17842", Upstream: "https://chatgpt.com/backend-api/codex", Routes: []RouteConfig{{ID: "direct"}}, FailureThreshold: 2, CircuitBaseSeconds: 30, CircuitMaxSeconds: 300, SessionCookieSeconds: 180, MaxRequestMiB: 64, ResponseHeaderSeconds: 30}
}
func LoadConfig(name string) (Config, error) {
	c := DefaultConfig()
	f, err := os.Open(name)
	if err != nil {
		return c, err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, errors.New("invalid configuration JSON")
	}
	if d.Decode(new(any)) != io.EOF {
		return c, errors.New("trailing configuration data")
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	if c.StreamIdleSeconds < 1 || c.StreamIdleSeconds > 600 || c.MaxConcurrentRequests < 1 || c.MaxConcurrentRequests > 256 {
		return errors.New("invalid stream idle or concurrency limit")
	}
	host, port, err := net.SplitHostPort(c.Listen)
	ip := net.ParseIP(host)
	n, e := strconv.Atoi(port)
	if err != nil || ip == nil || !ip.IsLoopback() || e != nil || n < 1 || n > 65535 {
		return errors.New("listen must be a literal loopback address")
	}
	u, err := url.Parse(c.Upstream)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || strings.TrimRight(u.Path, "/") != "/backend-api/codex" {
		return errors.New("invalid upstream URL")
	}
	local := net.ParseIP(u.Hostname())
	isLocal := local != nil && local.IsLoopback()
	if !((u.Scheme == "https" && u.Host == "chatgpt.com") || (isLocal && (u.Scheme == "http" || u.Scheme == "https"))) {
		return errors.New("upstream must be the official HTTPS origin or a literal loopback test server")
	}
	if len(c.Routes) < 1 || len(c.Routes) > 256 {
		return errors.New("configure 1–256 routes")
	}
	seen := map[string]bool{}
	for _, r := range c.Routes {
		if len(r.ID) < 1 || len(r.ID) > 64 || seen[r.ID] {
			return errors.New("route IDs must be unique, 1–64 characters")
		}
		for _, v := range r.ID {
			if !(v >= 'a' && v <= 'z' || v >= 'A' && v <= 'Z' || v >= '0' && v <= '9' || v == '-' || v == '_') {
				return errors.New("route IDs may contain only letters, digits, hyphens and underscores")
			}
		}
		seen[r.ID] = true
		if r.ProxyURL != "" {
			p, err := url.Parse(r.ProxyURL)
			if err != nil || p.Hostname() == "" || p.RawQuery != "" || p.Fragment != "" || (p.Path != "" && p.Path != "/") || (p.Scheme != "http" && p.Scheme != "https" && p.Scheme != "socks5" && p.Scheme != "socks5h") {
				return errors.New("invalid proxy URL")
			}
		}
	}
	if c.FailureThreshold < 1 || c.FailureThreshold > 20 || c.CircuitBaseSeconds < 1 || c.CircuitMaxSeconds < c.CircuitBaseSeconds || c.CircuitMaxSeconds > 3600 {
		return errors.New("invalid circuit policy")
	}
	if c.SessionCookieSeconds < 10 || c.SessionCookieSeconds > 3600 || c.MaxRequestMiB < 1 || c.MaxRequestMiB > 128 || c.ResponseHeaderSeconds < 1 || c.ResponseHeaderSeconds > 300 {
		return errors.New("invalid resource limits")
	}
	return nil
}
