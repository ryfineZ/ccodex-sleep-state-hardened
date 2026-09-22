// Package requestrecorder implements opt-in application-layer HTTP/SSE recording.
// It is not a system proxy, MITM proxy, state collector, cookie jar or retry loop.
package requestrecorder

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	ForceModelEnabled    bool     `json:"force_model_enabled"`
	ForceModel           string   `json:"force_model"`
	RouteLabel           string   `json:"route_label,omitempty"`
	Listen               string   `json:"listen"`
	Upstream             string   `json:"upstream"`
	ProxyURL             string   `json:"proxy_url,omitempty"`
	Directory            string   `json:"directory"`
	Mode                 string   `json:"mode"`
	AllowedPaths         []string `json:"allowed_path_prefixes"`
	CaptureMiB           int      `json:"capture_body_mib"`
	DiskMiB              int      `json:"disk_quota_mib"`
	MaxRecords           int      `json:"max_records"`
	QueueSize            int      `json:"queue_size"`
	MaxConcurrent        int      `json:"max_concurrent_requests"`
	MaxRequestMiB        int      `json:"max_request_mib"`
	HeaderTimeoutSeconds int      `json:"response_header_timeout_seconds"`
	IdleSeconds          int      `json:"stream_idle_seconds"`
}

func Default() Config {
	return Config{
		Listen: "127.0.0.1:17843", Upstream: "https://chatgpt.com", Directory: "../.local/recordings", Mode: "redacted",
		AllowedPaths: []string{"/backend-api/codex/", "/backend-api/conversation", "/v1/"},
		CaptureMiB:   2, DiskMiB: 256, MaxRecords: 2000, QueueSize: 8, MaxConcurrent: 8, MaxRequestMiB: 64, HeaderTimeoutSeconds: 30, IdleSeconds: 120,
	}
}
func Load(path string) (Config, error) {
	c := Default()
	f, err := os.Open(path)
	if err != nil {
		return c, errors.New("cannot open recorder configuration")
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF {
		return c, errors.New("invalid recorder JSON or unknown field")
	}
	if !filepath.IsAbs(c.Directory) {
		c.Directory = filepath.Join(filepath.Dir(path), c.Directory)
	}
	c.Directory, err = filepath.Abs(c.Directory)
	if err != nil {
		return c, err
	}
	return c, c.Validate()
}
func loopbackAddress(s string) bool {
	h, p, e := net.SplitHostPort(s)
	n, x := strconv.Atoi(p)
	ip := net.ParseIP(h)
	return e == nil && x == nil && n > 0 && n <= 65535 && ip != nil && ip.IsLoopback()
}
func (c Config) Validate() error {
	if err := validateModelOverride(c.ForceModelEnabled, c.ForceModel); err != nil {
		return err
	}
	if len(c.RouteLabel) > 128 || strings.ContainsAny(c.RouteLabel, "\r\n\t") {
		return errors.New("invalid route label")
	}

	if !loopbackAddress(c.Listen) {
		return errors.New("listen must be a literal loopback address with port")
	}
	u, e := url.Parse(c.Upstream)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return errors.New("upstream must be an origin URL without credentials, path or query")
	}
	ip := net.ParseIP(u.Hostname())
	local := ip != nil && ip.IsLoopback()
	if u.Scheme != "https" && !(u.Scheme == "http" && local) {
		return errors.New("upstream must use HTTPS; HTTP is only allowed for literal loopback")
	}
	if u.Host == c.Listen {
		return errors.New("upstream cannot be the recorder itself")
	}
	if c.ProxyURL != "" {
		p, e := url.Parse(c.ProxyURL)
		if e != nil || p.Host == "" || (p.Scheme != "http" && p.Scheme != "https" && p.Scheme != "socks5" && p.Scheme != "socks5h") || p.RawQuery != "" || p.Fragment != "" || (p.Path != "" && p.Path != "/") {
			return errors.New("invalid explicit network proxy URL")
		}
	}
	if c.Mode != "redacted" && c.Mode != "metadata" && c.Mode != "full" {
		return errors.New("mode must be metadata, redacted or full")
	}
	if c.Directory == "" {
		return errors.New("recording directory required")
	}
	if c.CaptureMiB < 1 || c.CaptureMiB > 8 || c.DiskMiB < 8 || c.DiskMiB > 4096 || c.MaxRecords < 1 || c.MaxRecords > 10000 || c.QueueSize < 1 || c.QueueSize > 32 || c.MaxConcurrent < 1 || c.MaxConcurrent > 32 || c.MaxRequestMiB < 1 || c.MaxRequestMiB > 128 || c.HeaderTimeoutSeconds < 1 || c.HeaderTimeoutSeconds > 120 || c.IdleSeconds < 1 || c.IdleSeconds > 600 {
		return errors.New("recorder resource limits out of range")
	}
	if len(c.AllowedPaths) == 0 || len(c.AllowedPaths) > 32 {
		return errors.New("one to 32 allowed path prefixes required")
	}
	for _, p := range c.AllowedPaths {
		if !strings.HasPrefix(p, "/") || strings.Contains(p, "..") || strings.ContainsAny(p, "%?#\\\r\n") || strings.HasPrefix(p, "/__recorder") || p == "/" {
			return errors.New("invalid or overly broad capture path prefix")
		}
	}
	return nil
}
