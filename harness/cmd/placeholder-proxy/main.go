// Command placeholder-proxy runs the placeholder passthrough (not SP1).
//
//	placeholder-proxy -listen 127.0.0.1:9000 -upstream ws://127.0.0.1:9100
//
// These two flags are the proxy contract the e2e suite uses; `octap` will be
// run with the same ones when it replaces this.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/wolffseb/ocpp-tap/harness/placeholder"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:9000", "listen address for chargers")
	upstream := flag.String("upstream", "", "upstream ws:// base URL")
	flag.Parse()
	if *upstream == "" {
		fmt.Fprintln(os.Stderr, "placeholder-proxy: -upstream is required")
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	srv := &http.Server{Addr: *listen, Handler: placeholder.New(*upstream, log), ReadHeaderTimeout: 10 * time.Second}
	log.Info("placeholder proxy listening", "listen", *listen, "upstream", *upstream)
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "placeholder-proxy:", err)
		os.Exit(1)
	}
}
