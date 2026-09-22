package requestrecorder

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"
)

type launchTicket struct {
	mu      sync.Mutex
	value   string
	expires time.Time
}

func (t *launchTicket) renew() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var b [32]byte
	_, _ = rand.Read(b[:])
	t.value = hex.EncodeToString(b[:])
	t.expires = time.Now().Add(time.Minute)
	return t.value
}
func (e *Engine) LaunchURL() string {
	return "http://" + e.config.Listen + "/__recorder/#launch=" + e.launch.renew()
}
func (e *Engine) exchangeLaunch(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		localError(w, 405, "method_not_allowed")
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+e.config.Listen {
		localError(w, 403, "same_origin_required")
		return
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		localError(w, 403, "same_origin_required")
		return
	}
	var v struct {
		Ticket string `json:"ticket"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 512))
	d.DisallowUnknownFields()
	if d.Decode(&v) != nil || d.Decode(new(any)) != io.EOF {
		localError(w, 400, "invalid_launch_ticket")
		return
	}
	e.launch.mu.Lock()
	defer e.launch.mu.Unlock()
	if e.launch.value == "" || time.Now().After(e.launch.expires) || subtle.ConstantTimeCompare([]byte(v.Ticket), []byte(e.launch.value)) != 1 {
		localError(w, 401, "launch_ticket_expired")
		return
	}
	e.launch.value = ""
	responseJSON(w, 200, map[string]string{"token": e.Token()})
}

// No shell interpolation and no persistent admin token in browser arguments.
func OpenBrowser(link string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", link)
	case "windows":
		cmd = exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", link)
	default:
		cmd = exec.Command("xdg-open", link)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

type setupRuntime struct {
	Address string `json:"address"`
	Token   string `json:"token"`
}

func (c *SetupController) WriteRuntime() error {
	data, _ := json.Marshal(setupRuntime{c.e.config.Listen, c.e.Token()})
	_ = c.root.Remove("runtime.json")
	f, err := c.root.OpenFile("runtime.json", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := checkPrivateHandle(f, false); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}

// Reads only a private runtime marker and contacts its loopback admin endpoint.
// The marker is not used as a browser URL and remote redirects are never followed.
func ExistingSetupURL(ctx context.Context, dir string) (string, error) {
	root, err := openRecordingRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	info, err := root.Lstat("runtime.json")
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("现有服务状态无法读取")
	}
	f, err := root.Open("runtime.json")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if checkPrivateHandle(f, false) != nil {
		return "", errors.New("现有服务状态不安全")
	}
	var rt setupRuntime
	if json.NewDecoder(io.LimitReader(f, 4096)).Decode(&rt) != nil || !loopbackAddress(rt.Address) || len(rt.Token) > 256 || rt.Token == "" {
		return "", errors.New("现有服务地址或令牌无效")
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://"+rt.Address+"/__recorder/api/new-launch", bytes.NewBufferString("{}"))
	req.Header.Set("X-Recorder-Token", rt.Token)
	tr := &http.Transport{Proxy: nil}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return "", errors.New("无法连接已运行的记录器")
	}
	defer resp.Body.Close()
	var v struct {
		Ticket string `json:"ticket"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 512)).Decode(&v) != nil || len(v.Ticket) != 64 {
		return "", errors.New("现有记录器无法提供启动链接")
	}
	for _, r := range v.Ticket {
		if !(r >= 'a' && r <= 'f' || r >= '0' && r <= '9') {
			return "", errors.New("启动链接无效")
		}
	}
	return "http://" + rt.Address + "/__recorder/#launch=" + v.Ticket, nil
}
