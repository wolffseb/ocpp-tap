// Package placeholder is the smallest passthrough that lets `make e2e` run
// charger → proxy → CPMS before the real forwarder exists.
//
// It is NOT SP1 and must not grow into it: no auth, no TLS, no config, no
// tap, no metrics, no timing guarantees. Delete it when internal/forwarder
// lands and `make e2e PROXY=octap` becomes the default.
package placeholder

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
)

// New returns a handler that relays each charger to upstream + its path.
func New(upstream string, log *slog.Logger) http.Handler {
	upstream = strings.TrimRight(upstream, "/")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d := websocket.Dialer{Subprotocols: websocket.Subprotocols(r)}
		up, resp, err := d.DialContext(r.Context(), upstream+r.URL.RequestURI(), nil)
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if err != nil {
			log.Info("placeholder: upstream dial failed", "path", r.URL.Path, "error", err)
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
			return
		}
		defer func() { _ = up.Close() }()

		u := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		if sp := up.Subprotocol(); sp != "" {
			u.Subprotocols = []string{sp}
		}
		down, err := u.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = down.Close() }()

		done := make(chan struct{}, 2)
		go pump(down, up, done)
		go pump(up, down, done)
		<-done
	})
}

func pump(src, dst *websocket.Conn, done chan<- struct{}) {
	defer func() { done <- struct{}{} }()
	for {
		typ, data, err := src.ReadMessage()
		if err != nil {
			return
		}
		if err := dst.WriteMessage(typ, data); err != nil {
			return
		}
	}
}
