// Package recorder is the charger-side half of the frame-identity check.
//
// A Recorder is a WebSocket hop placed directly in front of the proxy under
// test: charger → Recorder → proxy → harness CPMS. It records every message
// the charger sent and every message handed to the charger, as exact bytes,
// and forwards each one unchanged. The external charge point simulator does
// not expose its frames, so this is where they are captured. Compare the
// result with the harness CPMS's log using frames.Compare.
//
// It is deliberately separate from the forwarder it measures: the
// recorder is part of the measuring instrument, the proxy is the thing
// measured, and they must not share code.
package recorder

import (
	"encoding/base64"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/wolffseb/ocpp-tap/harness/frames"
)

const writeWait = 10 * time.Second

// Recorder is an http.Handler that accepts chargers and relays them to
// Upstream.
type Recorder struct {
	// Credentials, when set before serving, supplies Basic Auth for chargers
	// that cannot send it themselves; ok false leaves the request as is. A
	// charger's own Authorization header always goes through unchanged.
	Credentials func(id string) (user, pass string, ok bool)

	upstream string
	log      *slog.Logger
	frames   frames.Log

	mu     sync.Mutex
	open   map[*websocket.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// New builds a Recorder relaying to upstream, a ws:// or wss:// base URL; the
// charger's request path is appended to it.
func New(upstream string, log *slog.Logger) *Recorder {
	if log == nil {
		log = slog.Default()
	}
	return &Recorder{
		upstream: strings.TrimRight(upstream, "/"),
		log:      log,
		open:     map[*websocket.Conn]struct{}{},
	}
}

// Frames is the charger-side log: Upstream is what chargers sent,
// Downstream is what was delivered to them.
func (r *Recorder) Frames() *frames.Log { return &r.frames }

// Close drops every relayed connection and waits for the relays to end.
func (r *Recorder) Close() {
	r.mu.Lock()
	r.closed = true
	for ws := range r.open {
		_ = ws.Close()
	}
	r.mu.Unlock()
	r.wg.Wait()
}

func (r *Recorder) track(ws *websocket.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	r.open[ws] = struct{}{}
	return true
}

func (r *Recorder) untrack(ws *websocket.Conn) {
	r.mu.Lock()
	delete(r.open, ws)
	r.mu.Unlock()
}

// ServeHTTP dials upstream first, offering the charger's subprotocols, and
// only then accepts the charger with the subprotocol upstream chose.
func (r *Recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	id := path.Base(strings.TrimRight(req.URL.Path, "/"))
	target := r.upstream + req.URL.Path
	if req.URL.RawQuery != "" {
		target += "?" + req.URL.RawQuery
	}

	header := http.Header{}
	if auth := req.Header.Get("Authorization"); auth != "" {
		header.Set("Authorization", auth)
	} else if r.Credentials != nil {
		if user, pass, ok := r.Credentials(id); ok {
			header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
		}
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		Subprotocols:     websocket.Subprotocols(req),
	}
	up, resp, err := dialer.DialContext(req.Context(), target, header)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		r.log.Info("recorder: upstream dial failed", "charge_point", id, "error", err)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}

	var accept []string
	if sp := up.Subprotocol(); sp != "" {
		accept = []string{sp}
	}
	upgrader := websocket.Upgrader{
		Subprotocols: accept,
		CheckOrigin:  func(*http.Request) bool { return true },
	}
	down, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		_ = up.Close()
		return
	}

	if !r.track(up) || !r.track(down) {
		_ = up.Close()
		_ = down.Close()
		return
	}
	r.wg.Add(1)
	defer r.wg.Done()
	defer r.untrack(up)
	defer r.untrack(down)

	done := make(chan struct{}, 2)
	// Charger → upstream: record what the charger sent, as read.
	go func() {
		relay(down, up, func(data []byte) { r.frames.Add(id, frames.Upstream, data) }, nil)
		done <- struct{}{}
	}()
	// Upstream → charger: record what was delivered to the charger, after
	// the write succeeded.
	go func() {
		relay(up, down, nil, func(data []byte) { r.frames.Add(id, frames.Downstream, data) })
		done <- struct{}{}
	}()

	<-done
	_ = up.Close()
	_ = down.Close()
	<-done
}

// relay copies messages from src to dst until either fails.
func relay(src, dst *websocket.Conn, onRead, onWritten func([]byte)) {
	for {
		typ, data, err := src.ReadMessage()
		if err != nil {
			return
		}
		if onRead != nil {
			onRead(data)
		}
		_ = dst.SetWriteDeadline(time.Now().Add(writeWait))
		if err := dst.WriteMessage(typ, data); err != nil {
			return
		}
		if onWritten != nil {
			onWritten(data)
		}
	}
}
