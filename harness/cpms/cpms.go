// Package cpms is the harness's simulated CPMS: the upstream end a proxy
// under test forwards to.
//
// It accepts OCPP 1.6-J chargers on /…/{chargePointId}, answers the core
// charger-initiated actions, logs every frame it receives and sends as exact
// bytes, can send CSMS-initiated calls, and can misbehave on purpose through
// failure modes. It is deliberately simple; it is a test fixture, not a CSMS.
package cpms

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/wolffseb/ocpp-tap/harness/frames"
	"github.com/wolffseb/ocpp-tap/internal/ocpp"
	"github.com/wolffseb/ocpp-tap/internal/ocpp/ocppj"
)

// Mode is a failure mode.
type Mode string

// The failure modes.
const (
	// ModeNormal is a cooperative CPMS.
	ModeNormal Mode = "normal"
	// ModeDown refuses new connections with 503 and drops live ones.
	ModeDown Mode = "down"
	// ModeSlow delays every answer by Options.SlowDelay.
	ModeSlow Mode = "slow"
	// ModeRejectBoot answers BootNotification with Rejected.
	ModeRejectBoot Mode = "reject-boot"
	// ModeDropAfterN closes each connection right after receiving its
	// Options.DropAfter-th frame, without answering that frame.
	ModeDropAfterN Mode = "drop-after-n"
)

// Modes lists every mode.
func Modes() []Mode {
	return []Mode{ModeNormal, ModeDown, ModeSlow, ModeRejectBoot, ModeDropAfterN}
}

// ParseMode validates a mode name.
func ParseMode(s string) (Mode, error) {
	for _, m := range Modes() {
		if string(m) == s {
			return m, nil
		}
	}
	return "", fmt.Errorf("cpms: unknown mode %q", s)
}

// ErrNotConnected is returned by Call when the charger has no connection.
var ErrNotConnected = errors.New("cpms: charger not connected")

// Options configure a Server.
type Options struct {
	// Mode is the initial failure mode; SetMode changes it at runtime.
	Mode Mode
	// HeartbeatInterval is the interval in seconds sent in BootNotification.
	HeartbeatInterval int
	// SlowDelay is how long ModeSlow sits on each answer.
	SlowDelay time.Duration
	// DropAfter is N for ModeDropAfterN.
	DropAfter int
	// CallTimeout bounds a CSMS-initiated call.
	CallTimeout time.Duration
	// IdleTimeout drops a charger that sends nothing for this long.
	IdleTimeout time.Duration
	// FrameLog, when set, receives one JSON line per frame.
	FrameLog io.Writer
	Log      *slog.Logger
}

func (o *Options) applyDefaults() {
	if o.Mode == "" {
		o.Mode = ModeNormal
	}
	if o.HeartbeatInterval <= 0 {
		o.HeartbeatInterval = 300
	}
	if o.SlowDelay <= 0 {
		o.SlowDelay = 2 * time.Second
	}
	if o.DropAfter <= 0 {
		o.DropAfter = 5
	}
	if o.CallTimeout <= 0 {
		o.CallTimeout = 30 * time.Second
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = 10 * time.Minute
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
}

// Server is the simulated CPMS. It is an http.Handler.
type Server struct {
	opts   Options
	log    *slog.Logger
	frames frames.Log

	ctx    context.Context
	cancel context.CancelFunc

	mu    sync.Mutex
	mode  Mode
	conns map[string]*ocppj.Conn
	calls map[string]map[string]int

	txSeq atomic.Int64

	logMu sync.Mutex
	wg    sync.WaitGroup
}

// New builds a Server.
func New(opts Options) *Server {
	opts.applyDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		opts:   opts,
		log:    opts.Log,
		ctx:    ctx,
		cancel: cancel,
		mode:   opts.Mode,
		conns:  map[string]*ocppj.Conn{},
		calls:  map[string]map[string]int{},
	}
}

// Frames is the log of every frame received (Upstream) and sent (Downstream).
func (s *Server) Frames() *frames.Log { return &s.frames }

// Mode reports the current failure mode.
func (s *Server) Mode() Mode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode
}

// SetMode switches the failure mode. Switching to ModeDown drops every live
// connection.
func (s *Server) SetMode(m Mode) {
	s.mu.Lock()
	s.mode = m
	var drop []*ocppj.Conn
	if m == ModeDown {
		for _, c := range s.conns {
			drop = append(drop, c)
		}
	}
	s.mu.Unlock()
	for _, c := range drop {
		c.Close()
	}
	s.log.Info("mode changed", "mode", string(m))
}

// Close drops every connection and waits for them to finish.
func (s *Server) Close() {
	s.cancel()
	s.mu.Lock()
	var all []*ocppj.Conn
	for _, c := range s.conns {
		all = append(all, c)
	}
	s.mu.Unlock()
	for _, c := range all {
		c.Close()
	}
	s.wg.Wait()
}

// ServeHTTP accepts a charger's WebSocket handshake.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.Mode() == ModeDown {
		http.Error(w, "harness cpms is down", http.StatusServiceUnavailable)
		return
	}
	id := chargerID(r.URL.Path)
	if id == "" {
		http.Error(w, "charge point id missing from path", http.StatusNotFound)
		return
	}
	offered := websocket.Subprotocols(r)
	if !contains(offered, ocpp.Version16.Subprotocol()) {
		s.log.Warn("no ocpp1.6 offered", "charge_point", id, "offered", offered)
		http.Error(w, "no supported OCPP subprotocol offered", http.StatusBadRequest)
		return
	}

	upgrader := websocket.Upgrader{
		Subprotocols: []string{ocpp.Version16.Subprotocol()},
		CheckOrigin:  func(*http.Request) bool { return true },
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	s.wg.Add(1)
	defer s.wg.Done()

	var conn *ocppj.Conn
	var received atomic.Int64
	conn = ocppj.New(ws, ocppj.Options{
		ID:          id,
		Version:     ocpp.Version16,
		CallTimeout: s.opts.CallTimeout,
		IdleTimeout: s.opts.IdleTimeout,
		Log:         s.log,
		Observe: func(dir ocppj.Direction, data []byte) {
			if dir == ocppj.Outbound {
				s.record(id, frames.Downstream, data)
				return
			}
			s.record(id, frames.Upstream, data)
			if n := received.Add(1); s.Mode() == ModeDropAfterN && n == int64(s.opts.DropAfter) {
				s.log.Info("dropping connection", "charge_point", id, "after_frames", n)
				conn.Close()
			}
		},
	})

	s.mu.Lock()
	if old := s.conns[id]; old != nil {
		old.Close()
	}
	s.conns[id] = conn
	s.mu.Unlock()

	s.log.Info("charger connected", "charge_point", id, "path", r.URL.Path, "remote", r.RemoteAddr)
	reason := conn.Run(s.ctx, handler{s})
	s.log.Info("charger disconnected", "charge_point", id, "reason", reason)

	s.mu.Lock()
	if s.conns[id] == conn {
		delete(s.conns, id)
	}
	s.mu.Unlock()
}

func (s *Server) record(id string, dir frames.Direction, data []byte) {
	s.frames.Add(id, dir, data)
	if s.opts.FrameLog == nil {
		return
	}
	line, err := json.Marshal(struct {
		At      string `json:"at"`
		Charger string `json:"charger"`
		Dir     string `json:"dir"`
		Data    string `json:"data"`
		// Raw is the exact bytes; Data is lossy for invalid UTF-8.
		Raw string `json:"raw"`
	}{time.Now().UTC().Format(time.RFC3339Nano), id, dir.String(), string(data), base64.StdEncoding.EncodeToString(data)})
	if err != nil {
		return
	}
	s.logMu.Lock()
	defer s.logMu.Unlock()
	_, _ = s.opts.FrameLog.Write(append(line, '\n'))
}

// Connected reports whether the charger currently has a connection.
func (s *Server) Connected(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns[id] != nil
}

// CallCount is how many CALLs of action the charger has sent.
func (s *Server) CallCount(id, action string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[id][action]
}

// WaitConnected blocks until the charger is connected.
func (s *Server) WaitConnected(ctx context.Context, id string) error {
	return poll(ctx, func() bool { return s.Connected(id) })
}

// WaitCalls blocks until the charger has sent at least n CALLs of action.
func (s *Server) WaitCalls(ctx context.Context, id, action string, n int) error {
	return poll(ctx, func() bool { return s.CallCount(id, action) >= n })
}

func poll(ctx context.Context, cond func() bool) error {
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	for !cond() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	return nil
}

// Call sends a CSMS-initiated CALL and waits for the answer.
func (s *Server) Call(ctx context.Context, id, action string, payload any) (json.RawMessage, error) {
	s.mu.Lock()
	conn := s.conns[id]
	s.mu.Unlock()
	if conn == nil {
		return nil, ErrNotConnected
	}
	return conn.Call(ctx, action, payload)
}

// RemoteStart sends RemoteStartTransaction.
func (s *Server) RemoteStart(ctx context.Context, id string, connectorID int, idTag string) (json.RawMessage, error) {
	return s.Call(ctx, id, "RemoteStartTransaction", map[string]any{"connectorId": connectorID, "idTag": idTag})
}

// GetConfiguration sends GetConfiguration; no keys asks for all of them.
func (s *Server) GetConfiguration(ctx context.Context, id string, keys ...string) (json.RawMessage, error) {
	payload := map[string]any{}
	if len(keys) > 0 {
		payload["key"] = keys
	}
	return s.Call(ctx, id, "GetConfiguration", payload)
}

// DataTransfer sends a CSMS-initiated DataTransfer.
func (s *Server) DataTransfer(ctx context.Context, id, vendorID, messageID, data string) (json.RawMessage, error) {
	return s.Call(ctx, id, "DataTransfer", map[string]any{"vendorId": vendorID, "messageId": messageID, "data": data})
}

// handler answers charger-initiated CALLs.
type handler struct{ s *Server }

// Version reports OCPP 1.6, the only version the harness speaks.
func (handler) Version() ocpp.Version { return ocpp.Version16 }

// HandleCall answers one charger-initiated CALL.
func (h handler) HandleCall(ctx context.Context, id, action string, _ json.RawMessage) (any, *ocpp.RPCError) {
	s := h.s
	s.mu.Lock()
	if s.calls[id] == nil {
		s.calls[id] = map[string]int{}
	}
	s.calls[id][action]++
	mode := s.mode
	s.mu.Unlock()

	if mode == ModeSlow {
		select {
		case <-time.After(s.opts.SlowDelay):
		case <-ctx.Done():
			return nil, ocpp.Errorf(ocpp.ErrInternalError, "shutting down")
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	accepted := map[string]any{"idTagInfo": map[string]any{"status": "Accepted"}}
	switch action {
	case "BootNotification":
		status := "Accepted"
		if mode == ModeRejectBoot {
			status = "Rejected"
		}
		return map[string]any{"status": status, "currentTime": now, "interval": s.opts.HeartbeatInterval}, nil
	case "Heartbeat":
		return map[string]any{"currentTime": now}, nil
	case "StatusNotification", "MeterValues", "DiagnosticsStatusNotification", "FirmwareStatusNotification":
		return struct{}{}, nil
	case "Authorize", "StopTransaction":
		return accepted, nil
	case "StartTransaction":
		accepted["transactionId"] = s.txSeq.Add(1)
		return accepted, nil
	case "DataTransfer":
		return map[string]any{"status": "Accepted"}, nil
	default:
		return nil, ocpp.Errorf(ocpp.ErrNotImplemented, "action %q is not implemented", action)
	}
}

// chargerID takes the identity from the last path segment.
func chargerID(urlPath string) string {
	cleaned := strings.TrimRight(path.Clean("/"+urlPath), "/")
	if cleaned == "" {
		return ""
	}
	return path.Base(cleaned)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
