// ccodex-reliable-proxy is a separate, opt-in entry point. It does not read
// Codex login files, import credentials, install itself, or alter Codex config.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/gylive/ccodex-sleep-state/internal/reliableproxy"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const version = "0.1.0-client-managed"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("ccodex-reliable-proxy", flag.ContinueOnError)
	fs.SetOutput(errOut)
	path := fs.String("config", "config/reliable-proxy.json", "configuration file")
	check := fs.Bool("check", false, "validate configuration without opening sockets")
	showVersion := fs.Bool("version", false, "print version")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional argument")
	}
	if *showVersion {
		fmt.Fprintln(out, version)
		return nil
	}
	c, err := reliableproxy.LoadConfig(*path)
	if err != nil {
		return err
	}
	if *check {
		fmt.Fprintf(out, "Configuration valid: %d routes; client-managed state; no synthetic probes.\n", len(c.Routes))
		return nil
	}
	logger := slog.New(slog.NewJSONHandler(errOut, nil))
	e, err := reliableproxy.New(c, logger)
	if err != nil {
		return err
	}
	defer e.Close()
	listener, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: e, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 32 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	logger.Info("listening", "address", c.Listen, "mode", "client-managed-state", "version", version)
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return err
		}
		err := <-done
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
