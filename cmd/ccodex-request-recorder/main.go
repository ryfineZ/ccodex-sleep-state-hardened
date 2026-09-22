// The recorder is a separate foreground process. It never modifies system
// proxies, trust stores, Codex configuration, credentials or running services.
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
	"syscall"
	"time"

	"github.com/gylive/ccodex-sleep-state/internal/requestrecorder"
)

const version = "0.3.2-windows-acl"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("ccodex-request-recorder", flag.ContinueOnError)
	fs.SetOutput(errOut)
	config := fs.String("config", "config/request-recorder.json", "recorder configuration file")
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
	c, err := requestrecorder.Load(*config)
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
	e, err := requestrecorder.New(c)
	if err != nil {
		return err
	}
	defer e.Close()
	l, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: e, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 32 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- server.Serve(l) }()
	fmt.Fprintf(out, "Recorder: http://%s\nPanel (private local token): http://%s/__recorder/#token=%s\nRecords: %s\nMode: %s. Redacted bodies may still contain private prompts/replies. No upstream requests are sent until a client connects.\n", c.Listen, c.Listen, e.Token(), c.Directory, c.Mode)
	fmt.Fprintf(out, "Model override enabled=%t; target=%q. Panel changes last for this process only.\n", c.ForceModelEnabled, c.ForceModel)
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
