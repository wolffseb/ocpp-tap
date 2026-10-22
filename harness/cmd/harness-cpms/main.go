// Command harness-cpms runs the harness's simulated CPMS on its own.
//
//	harness-cpms -listen :9100 -control :9101 -mode slow -frame-log frames.jsonl
//	curl -X POST 'localhost:9101/call?charger=CP1&action=RemoteStartTransaction' -d '{"connectorId":1,"idTag":"T"}'
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wolffseb/ocpp-tap/harness/cpms"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "harness-cpms:", err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", "127.0.0.1:9100", "OCPP listen address")
	control := flag.String("control", "127.0.0.1:9101", "control API listen address (empty: off)")
	mode := flag.String("mode", string(cpms.ModeNormal), "failure mode: normal, down, slow, reject-boot, drop-after-n")
	heartbeat := flag.Int("heartbeat", 300, "heartbeat interval sent in BootNotification, seconds")
	slowDelay := flag.Duration("slow-delay", 2*time.Second, "answer delay in slow mode")
	dropAfter := flag.Int("drop-after", 5, "N for drop-after-n")
	frameLog := flag.String("frame-log", "", "append every frame as a JSON line to this file")
	flag.Parse()

	m, err := cpms.ParseMode(*mode)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	opts := cpms.Options{Mode: m, HeartbeatInterval: *heartbeat, SlowDelay: *slowDelay, DropAfter: *dropAfter, Log: log}
	if *frameLog != "" {
		f, err := os.OpenFile(*frameLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		opts.FrameLog = f
	}
	s := cpms.New(opts)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 2)
	servers := []*http.Server{{Addr: *listen, Handler: s, ReadHeaderTimeout: 10 * time.Second}}
	if *control != "" {
		servers = append(servers, &http.Server{Addr: *control, Handler: s.ControlHandler(), ReadHeaderTimeout: 10 * time.Second})
	}
	for _, srv := range servers {
		go func() { errCh <- srv.ListenAndServe() }()
	}
	log.Info("harness cpms listening", "ocpp", *listen, "control", *control, "mode", string(m))

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	s.Close()
	for _, srv := range servers {
		_ = srv.Close()
	}
	return nil
}
