// Command octap is the ocpp-tap binary: an OCPP 1.6-J forwarder with a
// read-only tap. The tap only counts and debug-logs frames so far; events
// come later in SP2 (docs/PRD.md).
//
//	octap run -config /data/config.json [-listen :9000]
//	octap version
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/wolffseb/ocpp-tap/internal/config"
	"github.com/wolffseb/ocpp-tap/internal/forwarder"
	"github.com/wolffseb/ocpp-tap/internal/tap"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// shutdownGrace bounds how long shutdown waits for handlers to return.
const shutdownGrace = 5 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: octap run -config FILE [-listen ADDR]")
	fmt.Fprintln(w, "       octap version")
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, "octap", version)
		return 0
	case "run":
		return runCommand(ctx, args[1:], stderr)
	default:
		fmt.Fprintf(stderr, "octap: unknown command %q\n", args[0])
		usage(stderr)
		return 2
	}
}

func runCommand(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "/data/config.json", "configuration file")
	listen := fs.String("listen", "", "charger-facing listen address (default: the config's listen)")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintln(stderr, "octap: invalid -log-level:", *logLevel)
		return 2
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "octap:", err)
		return 1
	}
	addr := cfg.Listen
	if *listen != "" {
		addr = *listen
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintln(stderr, "octap:", err)
		return 1
	}
	if err := serve(ctx, ln, cfg, log); err != nil {
		fmt.Fprintln(stderr, "octap:", err)
		return 1
	}
	return 0
}

// serve forwards chargers accepted on ln until ctx is cancelled.
func serve(ctx context.Context, ln net.Listener, cfg *config.Config, log *slog.Logger) error {
	upstreamTLS, err := upstreamTLSConfig(cfg.UpstreamCAFile)
	if err != nil {
		_ = ln.Close()
		return err
	}
	for _, id := range cfg.ChargerIDs() {
		if ch, _ := cfg.Charger(id); ch.Password == "" {
			log.Warn("charger has no password: any client using its id is accepted", "charge_point", id)
		}
	}

	var tapped atomic.Uint64
	tp := tap.New(tap.Options{Log: log, Handler: func(f forwarder.Frame) {
		tapped.Add(1)
		logTapFrame(log, f)
	}})
	// The tap stops after the forwarder, so it sees the disconnects.
	defer func() {
		tp.Close()
		log.Info("tap stopped", "frames", tapped.Load(), "dropped", tp.Dropped(), "panics", tp.Panics())
	}()

	fwd := forwarder.New(forwarder.Options{Chargers: cfg, Sink: tp, UpstreamTLS: upstreamTLS, Log: log})
	srv := &http.Server{
		Handler:           fwd,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	errCh := make(chan error, 1)
	go func() {
		if cfg.TLSCertFile != "" {
			errCh <- srv.ServeTLS(ln, cfg.TLSCertFile, cfg.TLSKeyFile)
		} else {
			errCh <- srv.Serve(ln)
		}
	}()
	scheme := "ws"
	if cfg.TLSCertFile != "" {
		scheme = "wss"
	}
	log.Info("octap forwarding", "version", version, "listen", scheme+"://"+ln.Addr().String(), "chargers", len(cfg.Chargers))

	select {
	case err := <-errCh:
		fwd.Close()
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	// Close the chargers first, so none is held while the listener drains.
	fwd.Close()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// logTapFrame is the tap's handler until frames become events: one debug
// line per frame, never the payload, which can hold idTags.
func logTapFrame(log *slog.Logger, f forwarder.Frame) {
	if !log.Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	switch f.Kind {
	case forwarder.Message:
		log.Debug("tap frame", "kind", f.Kind, "charge_point", f.ChargerID, "dir", f.Dir, "size", len(f.Data), "session", f.Session)
	case forwarder.Connected:
		log.Debug("tap frame", "kind", f.Kind, "charge_point", f.ChargerID, "subprotocol", f.Subprotocol, "session", f.Session)
	default:
		log.Debug("tap frame", "kind", f.Kind, "charge_point", f.ChargerID, "reason", f.Reason, "session", f.Session)
	}
}

// upstreamTLSConfig trusts the system roots plus the certificates in caFile.
func upstreamTLSConfig(caFile string) (*tls.Config, error) {
	if caFile == "" {
		return nil, nil
	}
	pemData, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("upstream_ca_file: %w", err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(pemData) {
		return nil, fmt.Errorf("upstream_ca_file %s: no PEM certificates found", caFile)
	}
	return &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, nil
}
