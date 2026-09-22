package requestrecorder

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gylive/ccodex-sleep-state/internal/codexconfig"
	"github.com/gylive/ccodex-sleep-state/internal/instance"
)

// SetupController is opt-in. Ordinary recorder mode never reads Codex files.
// Only an authenticated button action installs or restores the client provider.
type SetupController struct {
	mu                                      sync.RWMutex
	e                                       *Engine
	dir, home, profile                      string
	root                                    *os.Root
	lock                                    *instance.Lock
	managed, pending                        bool
	authMode, upstreamHost, connectionLabel string
	stop                                    func()
}

type SetupStatus struct {
	Managed         bool   `json:"managed"`
	RecoveryPending bool   `json:"recovery_pending"`
	Home            string `json:"codex_home"`
	Profile         string `json:"profile,omitempty"`
	UpstreamHost    string `json:"upstream_host,omitempty"`
	Connection      string `json:"connection,omitempty"`
	Message         string `json:"message"`
	Conflict        bool   `json:"conflict"`
}

func recorderTransport(c Config) *http.Transport {
	tr := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: time.Duration(c.HeaderTimeoutSeconds) * time.Second, ExpectContinueTimeout: time.Second, DisableCompression: true, DisableKeepAlives: true, ForceAttemptHTTP2: false}
	if c.ProxyURL != "" {
		p, _ := url.Parse(c.ProxyURL)
		tr.Proxy = http.ProxyURL(p)
	}
	return tr
}

// EnableSetup must run before serving requests. It does not inspect login data
// or change config.toml. A crashed previous transaction requires explicit recovery.
func (e *Engine) EnableSetup(dir, home, profile string, stop func()) (*SetupController, error) {
	root, err := openRecordingRoot(dir)
	if err != nil {
		return nil, err
	}
	lock, err := instance.Acquire(dir)
	if err != nil {
		root.Close()
		return nil, errors.New("one-click recorder is already running; reopen its panel")
	}
	c := &SetupController{e: e, dir: dir, home: home, profile: profile, root: root, lock: lock, stop: stop}
	if _, err := os.Lstat(filepath.Join(dir, "config-transaction.json")); err == nil {
		c.pending = true
	} else if !os.IsNotExist(err) {
		lock.Close()
		root.Close()
		return nil, err
	}
	e.setup = c
	return c, nil
}
func (c *SetupController) Status() SetupStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := SetupStatus{Managed: c.managed, RecoveryPending: c.pending, Home: c.home, Profile: c.profile, UpstreamHost: c.upstreamHost, Connection: c.connectionLabel, Message: "点击「一键接入 Codex」。保留原模型与登录方式，自动备份连接配置。"}
	if c.managed {
		s.Message = "已接入。重启 Codex 并新建会话，正常发送消息即可查看记录。"
		if c.admit() != nil {
			s.Conflict = true
			s.Message = "Codex 的连接或登录方式已改变，已停止转发。请先恢复连接，避免把凭据发送到旧上游。"
		}
	} else if c.pending {
		s.Message = "检测到上次未恢复的连接。点击「恢复原连接」，再重新接入；不会删除备份或覆盖冲突。"
	}
	return s
}

// Caller holds the controller read lock through the entire forwarding request.
func (c *SetupController) admit() error {
	if !c.managed {
		return errors.New("Codex is not connected")
	}
	if err := codexconfig.CheckManaged(c.dir); err != nil {
		return err
	}
	mode, err := codexconfig.ReadAuthMode(c.home)
	if err != nil {
		return err
	}
	if mode != c.authMode {
		return errors.New("authentication category changed")
	}
	return nil
}
func readClientConfig(home string) ([]byte, error) {
	p := filepath.Join(home, "config.toml")
	info, err := os.Lstat(p)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, errors.New("Codex 配置不是可读取的普通文件，或超过 1 MiB；原文件未修改")
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, errors.New("无法读取 Codex 配置，原文件未修改")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("Codex 配置读取期间发生变化")
	}
	b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(b) > 1<<20 {
		return nil, errors.New("Codex 配置无法安全读取")
	}
	return b, nil
}

func (c *SetupController) Connect(ctx context.Context, proxyMode, proxy string) error {
	if !c.mu.TryLock() {
		return errors.New("正在处理请求，请等回复结束后再接入")
	}
	defer c.mu.Unlock()
	if c.managed {
		return c.admit()
	}
	if c.pending {
		return errors.New("请先恢复上次连接；不会覆盖未完成的配置事务")
	}
	original, err := readClientConfig(c.home)
	if err != nil {
		return err
	}
	auth, err := codexconfig.ReadAuthMode(c.home)
	if err != nil {
		return errors.New("无法识别当前 Codex 登录方式，请先在 Codex 完成登录")
	}
	if auth == "ambiguous" {
		return errors.New("Codex 登录方式混合或无效，请先在 Codex 选择一种登录方式")
	}
	selection, err := codexconfig.ResolveWithAuth(original, c.profile, auth)
	if err != nil {
		return errors.New("无法安全接入当前 Codex 配置。请先停止其他接管工具，并在 Codex/CCS 中确认当前提供商、地址和认证方式")
	}
	u, err := url.Parse(selection.Upstream)
	if err != nil || u.RawPath != "" || !c.e.allowed(strings.TrimRight(u.Path, "/")+"/responses") {
		return errors.New("当前上游路径不在记录器支持范围内；原连接未修改")
	}
	if u.Host == c.e.config.Listen {
		return errors.New("上游已经指向本记录器，拒绝形成循环，请先恢复旧连接")
	}
	cfg := c.e.config
	cfg.Upstream = u.Scheme + "://" + u.Host
	label := "直连"
	switch proxyMode {
	case "", "auto":
		if cfg.ProxyURL == "" {
			cfg.ProxyURL = discoverRecorderSOCKS(ctx, []string{"127.0.0.1:7897", "127.0.0.1:7890", "127.0.0.1:10808"})
		}
		if cfg.ProxyURL != "" {
			label = "本机代理 / 已配置代理"
		}
	case "direct":
		cfg.ProxyURL = ""
	case "manual":
		if strings.TrimSpace(proxy) == "" {
			return errors.New("请填写代理地址")
		}
		cfg.ProxyURL = strings.TrimSpace(proxy)
		label = "手动代理"
	default:
		return errors.New("请选择自动、本机代理或直连")
	}
	if ctx.Err() != nil {
		return errors.New("接入操作已取消，原连接未修改")
	}
	if cfg.Validate() != nil {
		return errors.New("上游或代理地址无效，原连接未修改")
	}
	tr := recorderTransport(cfg)
	localBase := "http://" + cfg.Listen + strings.TrimRight(u.Path, "/")
	options := codexconfig.Options{Profile: selection.Profile, AuthMode: auth, PreserveModel: true, ExpectedConfigSHA256: selection.ConfigSHA256}
	if err := codexconfig.InstallWithOptions(c.dir, c.home, localBase, options); err != nil {
		tr.CloseIdleConnections()
		_, journalErr := os.Lstat(filepath.Join(c.dir, "config-transaction.json"))
		c.pending = journalErr == nil
		return errors.New("接入未完成，原设置或恢复备份已保留。请查看配置冲突，或点击「恢复原连接」")
	}
	c.e.connectionMu.Lock()
	old := c.e.transport
	c.e.target = &url.URL{Scheme: u.Scheme, Host: u.Host}
	c.e.transport = tr
	c.e.connectionMu.Unlock()
	if old, ok := old.(interface{ CloseIdleConnections() }); ok {
		old.CloseIdleConnections()
	}
	c.managed = true
	c.pending = true
	c.authMode = auth
	c.profile = selection.Profile
	c.upstreamHost = u.Hostname()
	c.connectionLabel = label
	return nil
}
func (c *SetupController) Restore() error {
	if !c.mu.TryLock() {
		return errors.New("正在处理请求，请等回复结束后再恢复")
	}
	defer c.mu.Unlock()
	if !c.pending {
		return nil
	}
	if err := codexconfig.Restore(c.dir); err != nil {
		return errors.New("恢复被保护性停止：配置被其他程序修改，或备份校验失败。原文件和备份均保留；不要删除恢复记录")
	}
	c.pending = false
	c.managed = false
	c.upstreamHost = ""
	c.connectionLabel = ""
	return nil
}
func (c *SetupController) Close() {
	_ = c.root.Remove("runtime.json")
	_ = c.lock.Close()
	_ = c.root.Close()
}
func (c *SetupController) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		localError(w, 405, "method_not_allowed")
		return
	}
	var v struct {
		ProxyMode string `json:"proxy_mode"`
		Proxy     string `json:"proxy"`
		Confirm   bool   `json:"confirm"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	d.DisallowUnknownFields()
	if d.Decode(&v) != nil || d.Decode(new(any)) != io.EOF || !v.Confirm {
		responseJSON(w, 400, map[string]string{"message": "请确认备份并接入或恢复操作"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	var err error
	switch r.URL.Path {
	case "/__recorder/api/setup/connect":
		err = c.Connect(ctx, v.ProxyMode, v.Proxy)
	case "/__recorder/api/setup/restore", "/__recorder/api/setup/stop":
		err = c.Restore()
	default:
		localError(w, 404, "unknown_setup_action")
		return
	}
	if err != nil {
		responseJSON(w, 409, map[string]string{"message": err.Error()})
		return
	}
	responseJSON(w, 200, c.Status())
	if r.URL.Path == "/__recorder/api/setup/stop" && c.stop != nil {
		c.stop()
	}
}

// Only a button-triggered, bounded SOCKS greeting to three loopback ports.
// It sends no hostname, credential or model request and never scans the network.
func discoverRecorderSOCKS(ctx context.Context, addresses []string) string {
	for _, address := range addresses {
		if ctx.Err() != nil {
			return ""
		}
		conn, err := (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext(ctx, "tcp", address)
		if err != nil {
			continue
		}
		_ = conn.SetDeadline(time.Now().Add(300 * time.Millisecond))
		_, err = conn.Write([]byte{5, 1, 0})
		var reply [2]byte
		if err == nil {
			_, err = io.ReadFull(conn, reply[:])
		}
		conn.Close()
		if err == nil && reply == [2]byte{5, 0} {
			return "socks5h://" + address
		}
	}
	return ""
}
