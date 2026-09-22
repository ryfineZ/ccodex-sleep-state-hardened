// The recorder is a separate foreground process. It never modifies system
// proxies or trust stores. Setup mode changes only the selected Codex provider
// after an explicit button action, using the original reversible transaction layer.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gylive/ccodex-sleep-state/internal/requestrecorder"
)

const version = "0.5.0-state-cookie"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(args []string, out, errOut io.Writer) (result error) {
	setupMode := len(args) > 0 && args[0] == "setup"
	if setupMode {
		args = args[1:]
	}
	fs := flag.NewFlagSet("ccodex-request-recorder", flag.ContinueOnError)
	fs.SetOutput(errOut)
	config := fs.String("config", "config/request-recorder.json", "recorder configuration file")
	noBrowser := fs.Bool("no-browser", false, "do not open the browser in setup mode")
	codexHome := fs.String("codex-home", "", "optional Codex home for one-click onboarding")
	profile := fs.String("profile", "", "optional Codex profile for one-click onboarding")
	check := fs.Bool("check", false, "validate configuration; no sockets or files created")
	ver := fs.Bool("version", false, "print version")
	sensitive := fs.Bool("allow-sensitive-recording", false, "explicitly acknowledge full-mode credential/body capture")
	analyze := fs.String("analyze", "", "offline reanalysis of a saved JSON record; no network")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional argument")
	}
	if *ver {
		fmt.Fprintln(out, version)
		return nil
	}
	if *analyze != "" {
		f, err := os.Open(*analyze)
		if err != nil {
			return errors.New("cannot open saved record")
		}
		defer f.Close()
		var r requestrecorder.Record
		d := json.NewDecoder(io.LimitReader(f, (96<<20)+1))
		if d.Decode(&r) != nil || d.Decode(new(any)) != io.EOF || r.Schema != 1 {
			return errors.New("invalid saved record")
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(requestrecorder.AnalyzeSaved(r))
	}
	explicitConfig := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			explicitConfig = true
		}
	})
	c, err := requestrecorder.Load(*config)
	if setupMode && !explicitConfig {
		if _, statErr := os.Stat(*config); os.IsNotExist(statErr) {
			base, baseErr := os.UserConfigDir()
			if baseErr != nil {
				return baseErr
			}
			c = requestrecorder.Default()
			c.Directory = filepath.Join(base, "ccodex-request-recorder", "recordings")
			err = c.Validate()
		}
	}
	if err != nil {
		return err
	}
	if *check {
		fmt.Fprintf(out, "Configuration valid: mode=%s; fixed upstream=%s; no sockets opened.\n", c.Mode, c.Upstream)
		fmt.Fprintf(out, "Model override enabled=%t; target=%q.\n", c.ForceModelEnabled, c.ForceModel)
		return nil
	}
	if c.Mode == "full" && !*sensitive {
		return errors.New("full mode may save credentials and private text; restart with -allow-sensitive-recording to acknowledge")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	e, err := requestrecorder.New(c)
	if err != nil {
		return err
	}
	defer e.Close()
	var setup *requestrecorder.SetupController
	if setupMode {
		home := *codexHome
		if home == "" {
			home = os.Getenv("CODEX_HOME")
		}
		if home == "" {
			userHome, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			home = filepath.Join(userHome, ".codex")
		}
		home, err = filepath.Abs(home)
		if err != nil {
			return err
		}
		controlDir := filepath.Join(filepath.Dir(c.Directory), "recorder-control")
		setup, err = e.EnableSetup(controlDir, home, *profile, stop)
		if err != nil {
			if link, reopenErr := requestrecorder.ExistingSetupURL(ctx, controlDir); reopenErr == nil {
				fmt.Fprintln(out, "记录器已运行，已重新打开原面板；没有启动第二个服务。")
				if !*noBrowser {
					return requestrecorder.OpenBrowser(link)
				}
				fmt.Fprintln(out, link)
				return nil
			}
			return err
		}
		defer func() {
			if setup.Status().Managed {
				if err := setup.Restore(); err != nil {
					result = errors.Join(result, err)
					fmt.Fprintln(errOut, "自动恢复未完成，请重新双击启动并点击恢复。备份已保留。")
				} else {
					fmt.Fprintln(out, "已恢复 Codex 原连接。")
				}
			}
			setup.Close()
		}()
	}
	l, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return errors.New("本地端口被占用，未修改 Codex 配置；请先关闭占用该端口的程序")
	}
	if setup != nil {
		if err := setup.WriteRuntime(); err != nil {
			l.Close()
			return err
		}
	}
	server := &http.Server{Handler: e, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 32 << 10}
	done := make(chan error, 1)
	go func() { done <- server.Serve(l) }()
	if setupMode {
		link := e.LaunchURL()
		fmt.Fprintln(out, "记录器已启动。请在自动打开的页面点击「一键接入 Codex」，再重启 Codex。")
		fmt.Fprintln(out, "退出请点击页面「恢复并退出」，或按 Ctrl+C；正常退出会恢复原连接。")
		fmt.Fprintln(out, "面板（链接一分钟内有效）：", link)
		if !*noBrowser {
			if err := requestrecorder.OpenBrowser(link); err != nil {
				fmt.Fprintln(errOut, "浏览器未能自动打开，请使用上面的面板链接。")
			}
		}
	} else {
		fmt.Fprintf(out, "Recorder: http://%s\nPanel (private local token): http://%s/__recorder/#token=%s\nRecords: %s\nMode: %s. Redacted bodies may still contain private prompts/replies. No upstream requests are sent until a client connects.\n", c.Listen, c.Listen, e.Token(), c.Directory, c.Mode)
		fmt.Fprintf(out, "Model override enabled=%t; target=%q. Panel changes last for this process only.\n", c.ForceModelEnabled, c.ForceModel)
	}
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := server.Shutdown(shutdown)
		if err != nil {
			_ = server.Close()
		}
		serveErr := <-done
		if err != nil {
			return err
		}
		if errors.Is(serveErr, http.ErrServerClosed) {
			return nil
		}
		return serveErr
	}
}
