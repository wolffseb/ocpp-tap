// Package forwarder passes OCPP-J frames unchanged between a charger and its
// CPMS. It is the only inline part of ocpp-tap.
//
// It is the smallest, most boring part of the system (ADR-0001):
//
//   - It never parses a frame. Message IDs, actions and payloads go through as
//     the bytes they arrived as, so correlation is preserved by construction
//     and unknown or vendor actions need no special case.
//   - It fails closed. The upstream connection is dialled before the
//     charger's handshake is accepted; if upstream cannot be reached the
//     charger is refused with 502. When either leg ends, the other is closed
//     at once, so a charger is never held without a CPMS behind it.
//   - Its one outward hook is FrameSink, which receives a copy of every frame
//     plus a notice when a charger connects or disconnects, and must not
//     block. A panicking sink is contained.
//   - It imports nothing from the tap, API or UI packages (depguard, and
//     TestForwarderDependencies).
package forwarder

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Charger is one charger's forwarding configuration.
type Charger struct {
	// ID is the charge point identity: the last segment of the URL path the
	// charger dials.
	ID string
	// Password is the Basic Auth password the charger must present, with its
	// ID as the username (Security Profile 1/2). Empty means no auth.
	Password string
	// UpstreamURL is the CPMS endpoint for this charger, ws:// or wss://,
	// dialled exactly as given.
	UpstreamURL string
	// UpstreamUsername is the Basic Auth username sent to the CPMS; empty
	// means ID.
	UpstreamUsername string
	// UpstreamPassword is the Basic Auth password sent to the CPMS; empty
	// means no Authorization header.
	UpstreamPassword string
}

// Chargers looks up a charger's configuration by ID.
type Chargers interface {
	Charger(id string) (Charger, bool)
}

// Direction is which way a frame travelled.
type Direction int

// The two directions.
const (
	// Upstream is charger to CPMS.
	Upstream Direction = iota
	// Downstream is CPMS to charger.
	Downstream
)

// String names the direction.
func (d Direction) String() string {
	if d == Upstream {
		return "upstream"
	}
	return "downstream"
}

// Kind is what a Frame reports.
type Kind int

// The kinds of Frame.
const (
	// Message is a copy of one forwarded WebSocket message.
	Message Kind = iota
	// Connected: a charger was accepted and its relay is about to start.
	Connected
	// Disconnected: a charger's relay has ended and both legs are closed.
	Disconnected
)

// String names the kind.
func (k Kind) String() string {
	switch k {
	case Message:
		return "message"
	case Connected:
		return "connected"
	case Disconnected:
		return "disconnected"
	}
	return "Kind(" + strconv.Itoa(int(k)) + ")"
}

// Frame is a copy of one forwarded WebSocket message, or a notice that a
// charger connected or disconnected. Data must be treated as read-only by
// everyone.
//
// For one session, Connected comes before its first Message and
// Disconnected after its last. Across a reconnect, the replaced session's
// Disconnected can come after the new session's Connected; Session tells
// them apart.
type Frame struct {
	Kind      Kind
	ChargerID string
	// Session numbers charger connections in the order their handshakes
	// arrived; a newer connection has a higher number.
	Session uint64
	At      time.Time

	// Message only.
	Dir    Direction
	Binary bool
	Data   []byte

	// Subprotocol is the one agreed with both sides (Connected only).
	Subprotocol string
	// Reason is why the session ended (Disconnected only).
	Reason string
}

// FrameSink receives a copy of every forwarded frame, and a notice when a
// charger connects or disconnects. Offer must not block.
type FrameSink interface {
	Offer(Frame)
}

// Options configure a Forwarder.
type Options struct {
	Chargers Chargers
	// Sink receives frame copies; nil means none.
	Sink FrameSink
	// UpstreamTLS configures wss:// dials; nil means system defaults.
	UpstreamTLS *tls.Config
	// DialTimeout bounds the upstream handshake.
	DialTimeout time.Duration
	// WriteTimeout bounds a single write to either leg.
	WriteTimeout time.Duration
	// PingInterval is how often both legs are pinged. A leg that sends
	// nothing, not even a pong, for two intervals is considered lost.
	PingInterval time.Duration
	Log          *slog.Logger
}

func (o *Options) applyDefaults() {
	if o.DialTimeout <= 0 {
		o.DialTimeout = 10 * time.Second
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = 10 * time.Second
	}
	if o.PingInterval <= 0 {
		o.PingInterval = 10 * time.Second
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
}

// closeWait bounds sending a close frame; the socket is closed regardless.
const closeWait = time.Second

// Forwarder is an http.Handler that accepts chargers and relays each one to
// its CPMS.
type Forwarder struct {
	opts Options
	log  *slog.Logger

	// seq numbers handshakes in arrival order, so a reconnect is told apart
	// from a stale connection however their goroutines are scheduled.
	seq atomic.Uint64

	mu       sync.Mutex
	sessions map[string]*session
	closed   bool
	wg       sync.WaitGroup

	// beforeRegister, if set, runs just before a session is registered.
	// Tests use it to force registrations out of order.
	beforeRegister func(id string)
}

// New builds a Forwarder.
func New(opts Options) *Forwarder {
	opts.applyDefaults()
	return &Forwarder{opts: opts, log: opts.Log, sessions: map[string]*session{}}
}

// Active is the number of chargers currently connected.
func (f *Forwarder) Active() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sessions)
}

// Close drops every connection and waits for the relays to end. New
// connections are refused afterwards.
func (f *Forwarder) Close() {
	f.mu.Lock()
	f.closed = true
	all := make([]*session, 0, len(f.sessions))
	for _, s := range f.sessions {
		all = append(all, s)
	}
	f.mu.Unlock()
	for _, s := range all {
		s.close(websocket.CloseGoingAway, "octap shutting down", websocket.CloseGoingAway, "octap shutting down")
	}
	f.wg.Wait()
}

// ServeHTTP handles one charger's WebSocket handshake and then relays it.
func (f *Forwarder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	seq := f.seq.Add(1)
	id := chargerID(r.URL.Path)
	log := f.log.With("charge_point", id, "remote", r.RemoteAddr)

	f.mu.Lock()
	closed := f.closed
	f.mu.Unlock()
	if closed {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}

	cfg, ok := f.opts.Chargers.Charger(id)
	if id == "" || !ok {
		log.Warn("refused unknown charger", "path", r.URL.Path)
		http.Error(w, "unknown charge point", http.StatusNotFound)
		return
	}
	if !authorized(r, cfg) {
		log.Warn("refused charger: bad credentials")
		w.Header().Set("WWW-Authenticate", `Basic realm="octap", charset="UTF-8"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	offered := websocket.Subprotocols(r)
	if len(offered) == 0 {
		log.Warn("refused charger: no subprotocol offered")
		http.Error(w, "no OCPP subprotocol offered", http.StatusBadRequest)
		return
	}

	// Fail closed: dial upstream before accepting the charger.
	up, err := f.dialUpstream(r.Context(), cfg, offered)
	if err != nil {
		log.Warn("refused charger: upstream unavailable", "upstream", hostOf(cfg.UpstreamURL), "error", err)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	chosen := up.Subprotocol()
	if chosen == "" || !slices.Contains(offered, chosen) {
		log.Warn("refused charger: subprotocol mismatch",
			"charger_offered", offered, "upstream_chose", chosen, "upstream", hostOf(cfg.UpstreamURL))
		_ = up.Close()
		http.Error(w, "upstream did not accept an offered subprotocol", http.StatusBadGateway)
		return
	}

	upgrader := websocket.Upgrader{
		Subprotocols: []string{chosen},
		// Chargers send no Origin; this is not a browser endpoint.
		CheckOrigin: func(*http.Request) bool { return true },
	}
	down, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade has written the HTTP error.
		_ = up.Close()
		return
	}

	s := &session{id: id, seq: seq, down: down, up: up, writeTimeout: f.opts.WriteTimeout, done: make(chan struct{})}
	if f.beforeRegister != nil {
		f.beforeRegister(id)
	}
	switch f.register(s) {
	case shuttingDown:
		s.close(websocket.CloseGoingAway, "octap shutting down", websocket.CloseGoingAway, "octap shutting down")
		return
	case superseded:
		log.Info("charger connection superseded by a newer one before it started")
		s.close(websocket.CloseNormalClosure, "replaced by a new connection", websocket.CloseNormalClosure, "replaced by a new connection")
		return
	}
	defer f.wg.Done()
	defer f.unregister(s)

	began := time.Now()
	log.Info("charger connected", "subprotocol", chosen, "upstream", hostOf(cfg.UpstreamURL))
	f.offer(Frame{Kind: Connected, ChargerID: id, Session: seq, At: began, Subprotocol: chosen})
	reason := f.relay(s)
	f.offer(Frame{Kind: Disconnected, ChargerID: id, Session: seq, At: time.Now(), Reason: reason})
	log.Info("charger disconnected", "reason", reason, "duration", time.Since(began).Round(time.Millisecond).String())
}

func (f *Forwarder) dialUpstream(ctx context.Context, cfg Charger, offered []string) (*websocket.Conn, error) {
	header := http.Header{}
	if cfg.UpstreamPassword != "" {
		user := cfg.UpstreamUsername
		if user == "" {
			user = cfg.ID
		}
		header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+cfg.UpstreamPassword)))
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: f.opts.DialTimeout,
		Subprotocols:     offered,
		TLSClientConfig:  f.opts.UpstreamTLS,
	}
	ctx, cancel := context.WithTimeout(ctx, f.opts.DialTimeout)
	defer cancel()
	up, resp, err := dialer.DialContext(ctx, cfg.UpstreamURL, header)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil && resp != nil {
		return nil, errors.Join(err, errors.New("upstream answered HTTP "+resp.Status))
	}
	return up, err
}

// registration is the outcome of register.
type registration int

const (
	registered registration = iota
	// superseded: a newer connection from the same charger is already
	// registered, so this one is stale.
	superseded
	shuttingDown
)

// register makes s the charger's current session, closing the one it
// replaces. The newer handshake always wins, whichever registers first.
//
// A registered session is counted in f.wg under the same lock that checks
// f.closed, so Close's Wait never runs concurrently with an Add; the caller
// calls f.wg.Done when the session ends.
func (f *Forwarder) register(s *session) registration {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return shuttingDown
	}
	old := f.sessions[s.id]
	if old != nil && old.seq > s.seq {
		f.mu.Unlock()
		return superseded
	}
	f.sessions[s.id] = s
	f.wg.Add(1)
	f.mu.Unlock()
	if old != nil {
		// The charger reconnected; its previous connection is stale.
		old.close(websocket.CloseNormalClosure, "replaced by a new connection", websocket.CloseNormalClosure, "replaced by a new connection")
	}
	return registered
}

func (f *Forwarder) unregister(s *session) {
	f.mu.Lock()
	if f.sessions[s.id] == s {
		delete(f.sessions, s.id)
	}
	f.mu.Unlock()
}

// leg names which side of a session failed.
type leg int

const (
	chargerLeg leg = iota
	upstreamLeg
)

// relay pumps both directions until either leg ends, then closes the other:
// upstream gone means the charger is closed with 1011 (fail closed).
func (f *Forwarder) relay(s *session) string {
	failed := make(chan leg, 2)
	go func() { failed <- f.pump(s, s.down, s.up, Upstream) }()
	go func() { failed <- f.pump(s, s.up, s.down, Downstream) }()
	go s.keepalive(f.opts.PingInterval)

	pending := 2
	select {
	case l := <-failed:
		pending--
		// If the session was closed from outside, that close is what made
		// the pump fail; s.close is then a no-op and its reason stands.
		if l == upstreamLeg {
			s.close(websocket.CloseInternalServerErr, "upstream lost", websocket.CloseNormalClosure, "upstream lost")
		} else {
			s.close(websocket.CloseNormalClosure, "", websocket.CloseNormalClosure, "charger left")
		}
	case <-s.done:
		// Closed from outside: replaced by a reconnect, or shutdown.
	}
	// Closing both sockets ends whichever pumps are still running.
	for ; pending > 0; pending-- {
		<-failed
	}
	return s.reason()
}

// pump copies messages from src to dst and reports which leg failed.
func (f *Forwarder) pump(s *session, src, dst *websocket.Conn, dir Direction) leg {
	srcLeg, dstLeg := chargerLeg, upstreamLeg
	if dir == Downstream {
		srcLeg, dstLeg = upstreamLeg, chargerLeg
	}
	timeout := 2 * f.opts.PingInterval
	src.SetPongHandler(func(string) error { return src.SetReadDeadline(time.Now().Add(timeout)) })
	_ = src.SetReadDeadline(time.Now().Add(timeout))

	for {
		typ, data, err := src.ReadMessage()
		if err != nil {
			return srcLeg
		}
		_ = src.SetReadDeadline(time.Now().Add(timeout))
		f.offer(Frame{Kind: Message, ChargerID: s.id, Session: s.seq, At: time.Now(), Dir: dir, Binary: typ == websocket.BinaryMessage, Data: data})

		_ = dst.SetWriteDeadline(time.Now().Add(f.opts.WriteTimeout))
		if err := dst.WriteMessage(typ, data); err != nil {
			return dstLeg
		}
	}
}

// offer hands a frame to the sink. A panicking sink must not reach the relay.
func (f *Forwarder) offer(fr Frame) {
	if f.opts.Sink == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			f.log.Error("frame sink panicked", "charge_point", fr.ChargerID, "panic", r)
		}
	}()
	f.opts.Sink.Offer(fr)
}

// session is one charger's pair of connections.
type session struct {
	id           string
	seq          uint64
	down, up     *websocket.Conn
	writeTimeout time.Duration

	closeOnce   sync.Once
	closeReason string
	done        chan struct{}
}

// close sends close frames to both legs and closes them, once. The first
// caller's reason is the session's.
func (s *session) close(chargerCode int, chargerText string, upstreamCode int, reason string) {
	s.closeOnce.Do(func() {
		s.closeReason = reason
		deadline := time.Now().Add(closeWait)
		_ = s.down.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(chargerCode, chargerText), deadline)
		_ = s.up.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(upstreamCode, ""), deadline)
		_ = s.down.Close()
		_ = s.up.Close()
		close(s.done)
	})
}

func (s *session) reason() string {
	if s.closeReason == "" {
		return "closed"
	}
	return s.closeReason
}

// keepalive pings both legs; the pumps' read deadlines notice a missing pong.
func (s *session) keepalive(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			deadline := time.Now().Add(s.writeTimeout)
			if s.down.WriteControl(websocket.PingMessage, nil, deadline) != nil ||
				s.up.WriteControl(websocket.PingMessage, nil, deadline) != nil {
				return
			}
		}
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

// authorized checks Basic Auth: username is the charger ID, password as
// configured. A charger without a password needs no credentials.
func authorized(r *http.Request, cfg Charger) bool {
	if cfg.Password == "" {
		return true
	}
	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(cfg.ID)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(cfg.Password)) == 1
	return userOK && passOK
}

// hostOf names the upstream in logs without its path, query or credentials.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "invalid-url"
	}
	return u.Scheme + "://" + u.Host
}
