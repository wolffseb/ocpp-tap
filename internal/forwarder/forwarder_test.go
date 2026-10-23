package forwarder

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// --- fixtures ----------------------------------------------------------------

type chargers map[string]Charger

func (c chargers) Charger(id string) (Charger, bool) {
	ch, ok := c[id]
	return ch, ok
}

type message struct {
	typ  int
	data []byte
}

// upstream is a raw CPMS stand-in that records how it was dialled and what it
// received, and sends what it is told to.
type upstream struct {
	srv *httptest.Server

	mu      sync.Mutex
	dials   int
	headers []http.Header
	paths   []string

	chooseNone  bool   // accept without a subprotocol
	forceChoice string // answer with this subprotocol whatever was offered
	refuse      int    // answer the handshake with this HTTP status
	silent      bool   // never read and never answer pings
	received    chan message
	toSend      chan message
	connections chan *websocket.Conn
	// ended gets a value each time an upstream connection's read loop ends.
	ended chan struct{}
}

func newUpstream(t *testing.T, configure func(*upstream)) *upstream {
	t.Helper()
	u := &upstream{
		received:    make(chan message, 1000),
		toSend:      make(chan message, 1000),
		connections: make(chan *websocket.Conn, 10),
		ended:       make(chan struct{}, 10),
	}
	if configure != nil {
		configure(u)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.dials++
		u.headers = append(u.headers, r.Header.Clone())
		u.paths = append(u.paths, r.URL.RequestURI())
		u.mu.Unlock()
		if u.refuse != 0 {
			http.Error(w, "refused", u.refuse)
			return
		}
		up := websocket.Upgrader{}
		var hdr http.Header
		switch offered := websocket.Subprotocols(r); {
		case u.forceChoice != "":
			// gorilla's Upgrader only echoes an offered subprotocol; force it.
			hdr = http.Header{"Sec-Websocket-Protocol": {u.forceChoice}}
		case !u.chooseNone && len(offered) > 0:
			up.Subprotocols = []string{offered[0]}
		}
		ws, err := up.Upgrade(w, r, hdr)
		if err != nil {
			return
		}
		u.connections <- ws
		defer func() { u.ended <- struct{}{} }()
		if u.silent {
			// No read loop means no pong is ever sent back.
			<-r.Context().Done()
			return
		}
		go func() {
			for m := range u.toSend {
				if m.data == nil {
					_ = ws.Close()
					return
				}
				_ = ws.WriteMessage(m.typ, m.data)
			}
		}()
		for {
			typ, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			u.received <- message{typ, data}
		}
	})
	u.srv = httptest.NewServer(handler)
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstream) url(path string) string {
	return "ws" + strings.TrimPrefix(u.srv.URL, "http") + path
}

func (u *upstream) dialCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.dials
}

func (u *upstream) lastHeader() http.Header {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.headers[len(u.headers)-1]
}

type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func start(t *testing.T, opts Options) (*Forwarder, string, *logBuffer) {
	t.Helper()
	logs := &logBuffer{}
	if opts.Log == nil {
		opts.Log = slog.New(slog.NewTextHandler(logs, nil))
	}
	f := New(opts)
	srv := httptest.NewServer(f)
	t.Cleanup(func() { f.Close(); srv.Close() })
	return f, "ws" + strings.TrimPrefix(srv.URL, "http"), logs
}

type dialOpts struct {
	user, pass   string
	subprotocols []string
	header       http.Header
}

func basic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// dial connects a raw charger and returns a refused handshake's HTTP status.
func dial(t *testing.T, base, path string, o dialOpts) (*websocket.Conn, int, error) {
	t.Helper()
	if o.subprotocols == nil {
		o.subprotocols = []string{"ocpp1.6"}
	}
	h := http.Header{}
	for k, v := range o.header {
		h[k] = v
	}
	if o.user != "" || o.pass != "" {
		h.Set("Authorization", basic(o.user, o.pass))
	}
	d := websocket.Dialer{Subprotocols: o.subprotocols, HandshakeTimeout: 3 * time.Second}
	ws, resp, err := d.Dial(base+path, h)
	status := 0
	if resp != nil {
		status = resp.StatusCode
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Cleanup(func() { _ = ws.Close() })
	}
	return ws, status, err
}

func mustDial(t *testing.T, base, path string, o dialOpts) *websocket.Conn {
	t.Helper()
	ws, status, err := dial(t, base, path, o)
	if err != nil {
		t.Fatalf("dial %s: %v (HTTP %d)", path, err, status)
	}
	return ws
}

func recv(t *testing.T, ch <-chan message) message {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(3 * time.Second):
		t.Fatal("nothing arrived upstream")
		return message{}
	}
}

func readFrom(t *testing.T, ws *websocket.Conn) message {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	typ, data, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("charger read: %v", err)
	}
	return message{typ, data}
}

func isTimeout(err error) bool {
	var ne interface{ Timeout() bool }
	return errors.As(err, &ne) && ne.Timeout()
}

// waitClosed requires the socket to close within d and returns the error the
// close produced.
func waitClosed(t *testing.T, ws *websocket.Conn, d time.Duration) error {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(d))
	for {
		_, _, err := ws.ReadMessage()
		if err == nil {
			continue
		}
		if isTimeout(err) {
			t.Fatalf("still connected after %s", d)
		}
		return err
	}
}

func cp(u *upstream, id, password string) Charger {
	return Charger{ID: id, Password: password, UpstreamURL: u.url("/cpms/ocpp/" + id)}
}

// --- passthrough -------------------------------------------------------------

var adversarial = [][]byte{
	[]byte(`[2,"1","BootNotification",{"chargePointVendor":"V","chargePointModel":"M"}]`),
	[]byte("[2, \"id with spaces\" ,\"Heartbeat\" , { }]"),
	[]byte(`[2,"0xDEAD","AcmeVendorAction",{"x":"é ","n":1e400}]`),
	[]byte(`[2,"dt","DataTransfer",{"vendorId":"com.acme","data":"<>&"}]`),
	[]byte(`this is not OCPP at all`),
	[]byte(`[5,"unknown message type"]`),
	bytes.Repeat([]byte("A"), 300000),
}

func TestPassesFramesUnchangedBothWays(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	_, base, _ := start(t, Options{Chargers: chargers{"CP1": cp(up, "CP1", "")}})
	ws := mustDial(t, base, "/ocpp/CP1", dialOpts{})

	for _, p := range adversarial {
		if err := ws.WriteMessage(websocket.TextMessage, p); err != nil {
			t.Fatal(err)
		}
		if got := recv(t, up.received); got.typ != websocket.TextMessage || !bytes.Equal(got.data, p) {
			t.Errorf("upstream got %.80q (type %d), want %.80q", got.data, got.typ, p)
		}
	}
	for _, p := range adversarial {
		up.toSend <- message{websocket.TextMessage, p}
		if got := readFrom(t, ws); got.typ != websocket.TextMessage || !bytes.Equal(got.data, p) {
			t.Errorf("charger got %.80q (type %d), want %.80q", got.data, got.typ, p)
		}
	}
}

func TestPreservesBinaryMessages(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	_, base, _ := start(t, Options{Chargers: chargers{"CP1": cp(up, "CP1", "")}})
	ws := mustDial(t, base, "/ocpp/CP1", dialOpts{})

	bin := []byte{0, 1, 2, 0xff}
	if err := ws.WriteMessage(websocket.BinaryMessage, bin); err != nil {
		t.Fatal(err)
	}
	if got := recv(t, up.received); got.typ != websocket.BinaryMessage || !bytes.Equal(got.data, bin) {
		t.Errorf("upstream got type %d %v", got.typ, got.data)
	}
	up.toSend <- message{websocket.BinaryMessage, bin}
	if got := readFrom(t, ws); got.typ != websocket.BinaryMessage || !bytes.Equal(got.data, bin) {
		t.Errorf("charger got type %d %v", got.typ, got.data)
	}
}

func TestKeepsOrder(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	_, base, _ := start(t, Options{Chargers: chargers{"CP1": cp(up, "CP1", "")}})
	ws := mustDial(t, base, "/ocpp/CP1", dialOpts{})

	const n = 500
	frame := func(i int) string { return `[2,"` + strconv.Itoa(i) + `","Heartbeat",{}]` }
	go func() {
		for i := 0; i < n; i++ {
			_ = ws.WriteMessage(websocket.TextMessage, []byte(frame(i)))
		}
	}()
	for i := 0; i < n; i++ {
		if got := recv(t, up.received); string(got.data) != frame(i) {
			t.Fatalf("frame %d = %s, want %s", i, got.data, frame(i))
		}
	}
}

// The upstream URL is dialled exactly as configured; the charger's own path
// does not leak into it.
func TestDialsConfiguredUpstreamURL(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	c := Charger{ID: "CP1", UpstreamURL: up.url("/somewhere/else/CP-UP?token=x")}
	_, base, _ := start(t, Options{Chargers: chargers{"CP1": c}})
	mustDial(t, base, "/any/prefix/CP1", dialOpts{})

	up.mu.Lock()
	defer up.mu.Unlock()
	if got := up.paths[0]; got != "/somewhere/else/CP-UP?token=x" {
		t.Errorf("upstream dialled at %q", got)
	}
}

// --- identity and auth -------------------------------------------------------

func TestUnknownChargerRefused(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	_, base, logs := start(t, Options{Chargers: chargers{"CP1": cp(up, "CP1", "")}})
	for _, path := range []string{"/ocpp/NOPE", "/"} {
		if _, status, err := dial(t, base, path, dialOpts{}); err == nil || status != http.StatusNotFound {
			t.Errorf("%q: err %v status %d, want 404", path, err, status)
		}
	}
	if up.dialCount() != 0 {
		t.Error("upstream dialled for an unknown charger")
	}
	if !strings.Contains(logs.String(), "NOPE") {
		t.Errorf("refusal not logged with the charger id:\n%s", logs)
	}
}

func TestBasicAuth(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	_, base, _ := start(t, Options{Chargers: chargers{
		"CP1":  cp(up, "CP1", "s3cret"),
		"OPEN": cp(up, "OPEN", ""),
	}})

	if _, status, err := dial(t, base, "/ocpp/CP1", dialOpts{user: "CP1", pass: "s3cret"}); err != nil {
		t.Fatalf("correct credentials refused: %v (HTTP %d)", err, status)
	}
	dials := up.dialCount()

	for name, o := range map[string]dialOpts{
		"no credentials": {},
		"wrong password": {user: "CP1", pass: "nope"},
		"wrong username": {user: "CP2", pass: "s3cret"},
		"empty password": {user: "CP1"},
	} {
		if _, status, err := dial(t, base, "/ocpp/CP1", o); err == nil || status != http.StatusUnauthorized {
			t.Errorf("%s: err %v status %d, want 401", name, err, status)
		}
	}
	if up.dialCount() != dials {
		t.Error("upstream dialled for a charger that failed auth")
	}

	// A charger configured without a password needs none (Security Profile 0).
	if _, status, err := dial(t, base, "/ocpp/OPEN", dialOpts{}); err != nil {
		t.Errorf("charger without password refused: %v (HTTP %d)", err, status)
	}
}

func TestUnauthorizedAsksForBasic(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	f := New(Options{Chargers: chargers{"CP1": cp(up, "CP1", "pw")}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	rec := httptest.NewRecorder()
	f.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ocpp/CP1", nil))
	if rec.Code != http.StatusUnauthorized || !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Basic") {
		t.Errorf("status %d, WWW-Authenticate %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
}

// --- upstream credentials ----------------------------------------------------

func TestUpstreamCredentials(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	withPass := cp(up, "CP1", "downstream-pw")
	withPass.UpstreamPassword = "upstream-pw"
	custom := cp(up, "CP2", "")
	custom.UpstreamUsername = "cpms-user"
	custom.UpstreamPassword = "pw2"
	none := cp(up, "CP3", "")
	_, base, _ := start(t, Options{Chargers: chargers{"CP1": withPass, "CP2": custom, "CP3": none}})

	mustDial(t, base, "/ocpp/CP1", dialOpts{user: "CP1", pass: "downstream-pw"})
	if got := up.lastHeader().Get("Authorization"); got != basic("CP1", "upstream-pw") {
		t.Errorf("CP1 upstream Authorization = %q; downstream credentials must not leak upstream", got)
	}
	mustDial(t, base, "/ocpp/CP2", dialOpts{})
	if got := up.lastHeader().Get("Authorization"); got != basic("cpms-user", "pw2") {
		t.Errorf("CP2 upstream Authorization = %q", got)
	}
	mustDial(t, base, "/ocpp/CP3", dialOpts{header: http.Header{"Authorization": {basic("x", "y")}}})
	if got := up.lastHeader().Get("Authorization"); got != "" {
		t.Errorf("CP3 upstream Authorization = %q, want none", got)
	}
}

func TestUpstreamTLS(t *testing.T) {
	t.Parallel()

	received := make(chan []byte, 1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{Subprotocols: websocket.Subprotocols(r)}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		if _, data, err := ws.ReadMessage(); err == nil {
			received <- data
		}
	}))
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())

	c := Charger{ID: "CP1", UpstreamURL: "wss" + strings.TrimPrefix(srv.URL, "https") + "/ocpp/CP1"}
	_, base, _ := start(t, Options{Chargers: chargers{"CP1": c}, UpstreamTLS: &tls.Config{RootCAs: roots}})
	ws := mustDial(t, base, "/ocpp/CP1", dialOpts{})
	if err := ws.WriteMessage(websocket.TextMessage, []byte(`[2,"1","Heartbeat",{}]`)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-received:
		if string(got) != `[2,"1","Heartbeat",{}]` {
			t.Errorf("got %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("nothing arrived over wss")
	}

	// Without the CA the dial fails, and the charger is refused.
	_, base2, _ := start(t, Options{Chargers: chargers{"CP1": c}})
	if _, status, err := dial(t, base2, "/ocpp/CP1", dialOpts{}); err == nil || status != http.StatusBadGateway {
		t.Errorf("untrusted wss upstream: err %v status %d, want 502", err, status)
	}
}

// --- subprotocol -------------------------------------------------------------

func TestOffersChargerSubprotocolsUpstream(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	_, base, _ := start(t, Options{Chargers: chargers{"CP1": cp(up, "CP1", "")}})
	ws := mustDial(t, base, "/ocpp/CP1", dialOpts{subprotocols: []string{"ocpp1.6", "ocpp2.0.1"}})

	if got := up.lastHeader().Get("Sec-Websocket-Protocol"); got != "ocpp1.6, ocpp2.0.1" {
		t.Errorf("offered upstream %q, want the charger's list in order", got)
	}
	if ws.Subprotocol() != "ocpp1.6" {
		t.Errorf("charger accepted with %q, want upstream's choice ocpp1.6", ws.Subprotocol())
	}
}

func TestSubprotocolMismatchRefused(t *testing.T) {
	t.Parallel()

	cases := map[string]func(*upstream){
		"upstream picks none":            func(u *upstream) { u.chooseNone = true },
		"upstream picks one not offered": func(u *upstream) { u.forceChoice = "ocpp2.0.1" },
	}
	for name, configure := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			up := newUpstream(t, configure)
			_, base, logs := start(t, Options{Chargers: chargers{"CP1": cp(up, "CP1", "")}})
			_, status, err := dial(t, base, "/ocpp/CP1", dialOpts{subprotocols: []string{"ocpp1.6"}})
			if err == nil {
				t.Fatal("charger accepted despite a subprotocol mismatch")
			}
			if status != http.StatusBadGateway {
				t.Errorf("status %d, want 502", status)
			}
			// The log line names both sides.
			out := logs.String()
			if !strings.Contains(out, "subprotocol mismatch") || !strings.Contains(out, "ocpp1.6") {
				t.Errorf("log does not name both sides:\n%s", out)
			}
			if up.forceChoice != "" && !strings.Contains(out, up.forceChoice) {
				t.Errorf("log does not name upstream's choice:\n%s", out)
			}
		})
	}
}

func TestNoSubprotocolOfferedRefused(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	_, base, _ := start(t, Options{Chargers: chargers{"CP1": cp(up, "CP1", "")}})
	if _, status, err := dial(t, base, "/ocpp/CP1", dialOpts{subprotocols: []string{}}); err == nil || status != http.StatusBadRequest {
		t.Errorf("err %v status %d, want 400", err, status)
	}
	if up.dialCount() != 0 {
		t.Error("upstream dialled without a subprotocol to offer")
	}
}

// --- fail closed -------------------------------------------------------------

func TestUpstreamUnreachableRefusesCharger(t *testing.T) {
	t.Parallel()

	refusing := newUpstream(t, func(u *upstream) { u.refuse = http.StatusServiceUnavailable })
	for name, c := range map[string]Charger{
		"connection refused": {ID: "CP1", UpstreamURL: "ws://127.0.0.1:1/ocpp/CP1"},
		"http error":         cp(refusing, "CP1", ""),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, base, logs := start(t, Options{Chargers: chargers{"CP1": c}})
			if _, status, err := dial(t, base, "/ocpp/CP1", dialOpts{}); err == nil || status != http.StatusBadGateway {
				t.Errorf("err %v status %d, want 502", err, status)
			}
			if !strings.Contains(logs.String(), "upstream") {
				t.Errorf("refusal not logged:\n%s", logs)
			}
		})
	}
}

// Upstream lost → charger connection closed within 2 s (SP1), with close
// code 1011.
func TestUpstreamLostClosesChargerWithin2s(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	_, base, _ := start(t, Options{Chargers: chargers{"CP1": cp(up, "CP1", "")}})
	ws := mustDial(t, base, "/ocpp/CP1", dialOpts{})
	<-up.connections

	began := time.Now()
	up.toSend <- message{} // upstream closes
	err := waitClosed(t, ws, 2*time.Second)
	t.Logf("charger closed %s after upstream", time.Since(began))
	if !websocket.IsCloseError(err, websocket.CloseInternalServerErr) {
		t.Errorf("close = %v, want code 1011", err)
	}
}

// A dead upstream that keeps its TCP connection open is caught by the
// keepalive: no pong within the timeout.
func TestSilentUpstreamDetectedByKeepalive(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, func(u *upstream) { u.silent = true })
	_, base, _ := start(t, Options{
		Chargers:     chargers{"CP1": cp(up, "CP1", "")},
		PingInterval: 100 * time.Millisecond,
	})
	ws := mustDial(t, base, "/ocpp/CP1", dialOpts{})
	waitClosed(t, ws, 2*time.Second)
}

func TestChargerGoneClosesUpstream(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	f, base, _ := start(t, Options{Chargers: chargers{"CP1": cp(up, "CP1", "")}})
	ws := mustDial(t, base, "/ocpp/CP1", dialOpts{})
	<-up.connections
	_ = ws.Close()

	select {
	case <-up.ended:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream still connected after the charger left")
	}
	waitFor(t, func() bool { return f.Active() == 0 })
}

// A charger that reconnects gets a new upstream connection, and the old
// session is closed.
func TestReconnectGetsNewUpstream(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	f, base, _ := start(t, Options{Chargers: chargers{"CP1": cp(up, "CP1", "")}})
	first := mustDial(t, base, "/ocpp/CP1", dialOpts{})
	<-up.connections
	second := mustDial(t, base, "/ocpp/CP1", dialOpts{})
	<-up.connections

	if n := up.dialCount(); n != 2 {
		t.Errorf("upstream dialled %d times, want 2", n)
	}
	waitClosed(t, first, 2*time.Second)

	if err := second.WriteMessage(websocket.TextMessage, []byte(`[2,"1","Heartbeat",{}]`)); err != nil {
		t.Fatal(err)
	}
	recv(t, up.received)
	waitFor(t, func() bool { return f.Active() == 1 })
}

func TestCloseDropsEverything(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	f, base, _ := start(t, Options{Chargers: chargers{"A": cp(up, "A", ""), "B": cp(up, "B", "")}})
	a := mustDial(t, base, "/ocpp/A", dialOpts{})
	b := mustDial(t, base, "/ocpp/B", dialOpts{})
	f.Close()
	waitClosed(t, a, 2*time.Second)
	waitClosed(t, b, 2*time.Second)
	if n := f.Active(); n != 0 {
		t.Errorf("Active = %d after Close", n)
	}
}

// --- frame sink ----------------------------------------------------------------

type recordingSink struct {
	mu     sync.Mutex
	frames []Frame
}

func (s *recordingSink) Offer(f Frame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, f)
}

func (s *recordingSink) snapshot() []Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Frame(nil), s.frames...)
}

// messages is the snapshot without connection lifecycle notices.
func (s *recordingSink) messages() []Frame {
	var out []Frame
	for _, f := range s.snapshot() {
		if f.Kind == Message {
			out = append(out, f)
		}
	}
	return out
}

func TestSinkGetsACopyOfEveryFrame(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	sink := &recordingSink{}
	_, base, _ := start(t, Options{Chargers: chargers{"CP1": cp(up, "CP1", "")}, Sink: sink})
	ws := mustDial(t, base, "/ocpp/CP1", dialOpts{})

	if err := ws.WriteMessage(websocket.TextMessage, []byte("up")); err != nil {
		t.Fatal(err)
	}
	recv(t, up.received)
	up.toSend <- message{websocket.TextMessage, []byte("down")}
	readFrom(t, ws)

	waitFor(t, func() bool { return len(sink.messages()) == 2 })
	got := sink.messages()
	if got[0].ChargerID != "CP1" || got[0].Dir != Upstream || string(got[0].Data) != "up" || got[0].At.IsZero() {
		t.Errorf("first frame = %+v", got[0])
	}
	if got[1].Dir != Downstream || string(got[1].Data) != "down" {
		t.Errorf("second frame = %+v", got[1])
	}
}

type panickingSink struct{ calls atomic.Int64 }

func (s *panickingSink) Offer(Frame) {
	s.calls.Add(1)
	panic("tap bug")
}

func TestPanickingSinkDoesNotAffectForwarding(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	sink := &panickingSink{}
	_, base, _ := start(t, Options{Chargers: chargers{"CP1": cp(up, "CP1", "")}, Sink: sink})
	ws := mustDial(t, base, "/ocpp/CP1", dialOpts{})

	for i := 0; i < 3; i++ {
		if err := ws.WriteMessage(websocket.TextMessage, []byte("x")); err != nil {
			t.Fatal(err)
		}
		recv(t, up.received)
		up.toSend <- message{websocket.TextMessage, []byte("y")}
		readFrom(t, ws)
	}
	// One Connected notice, then six messages.
	if n := sink.calls.Load(); n != 7 {
		t.Errorf("sink called %d times, want 7", n)
	}
}

// SP2: the sink learns when a charger connects and disconnects, so the tap
// can emit charger.connected and charger.disconnected. Connected comes
// before the session's first message and Disconnected after its last.
func TestSinkGetsConnectionLifecycle(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	sink := &recordingSink{}
	f, base, _ := start(t, Options{Chargers: chargers{"CP1": cp(up, "CP1", "")}, Sink: sink})
	ws := mustDial(t, base, "/ocpp/CP1", dialOpts{})
	if err := ws.WriteMessage(websocket.TextMessage, []byte("up")); err != nil {
		t.Fatal(err)
	}
	recv(t, up.received)
	_ = ws.Close()
	waitFor(t, func() bool { return f.Active() == 0 })
	waitFor(t, func() bool { return len(sink.snapshot()) == 3 })

	got := sink.snapshot()
	if c := got[0]; c.Kind != Connected || c.ChargerID != "CP1" || c.Subprotocol != "ocpp1.6" || c.At.IsZero() || c.Data != nil {
		t.Errorf("first notice = %+v, want Connected with subprotocol ocpp1.6", c)
	}
	if m := got[1]; m.Kind != Message || string(m.Data) != "up" {
		t.Errorf("second = %+v, want the message", m)
	}
	if d := got[2]; d.Kind != Disconnected || d.ChargerID != "CP1" || d.Reason != "charger left" || d.At.IsZero() {
		t.Errorf("last notice = %+v, want Disconnected with reason \"charger left\"", d)
	}
	if got[0].Session == 0 || got[1].Session != got[0].Session || got[2].Session != got[0].Session {
		t.Errorf("sessions %d, %d, %d: want one non-zero session", got[0].Session, got[1].Session, got[2].Session)
	}
}

// A reconnect is a new session. The replaced session's Disconnected can
// arrive after the new session's Connected, so the session number is what
// tells them apart.
func TestSinkSessionsDifferAcrossReconnect(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	sink := &recordingSink{}
	_, base, _ := start(t, Options{Chargers: chargers{"CP1": cp(up, "CP1", "")}, Sink: sink})
	first := mustDial(t, base, "/ocpp/CP1", dialOpts{})
	<-up.connections
	// Let the first session start; a connection superseded before it
	// registers never starts and gets no notices at all.
	waitFor(t, func() bool { return len(sink.snapshot()) == 1 })
	mustDial(t, base, "/ocpp/CP1", dialOpts{})
	<-up.connections
	waitClosed(t, first, 2*time.Second)
	waitFor(t, func() bool { return len(sink.snapshot()) == 3 })

	var connected []uint64
	var disconnected []Frame
	for _, fr := range sink.snapshot() {
		switch fr.Kind {
		case Connected:
			connected = append(connected, fr.Session)
		case Disconnected:
			disconnected = append(disconnected, fr)
		}
	}
	if len(connected) != 2 || connected[0] == connected[1] {
		t.Fatalf("Connected sessions %v, want two different ones", connected)
	}
	if len(disconnected) != 1 || disconnected[0].Session != min(connected[0], connected[1]) ||
		disconnected[0].Reason != "replaced by a new connection" {
		t.Errorf("Disconnected %+v, want the older session replaced", disconnected)
	}
}

// holdFirstConnectedSink stands in for a session goroutine that is
// descheduled between registering and offering its Connected: it holds the
// first Connected Offer until the second one has been recorded.
type holdFirstConnectedSink struct {
	mu       sync.Mutex
	frames   []Frame
	nConn    int
	entered  chan struct{} // closed when the first Connected is held
	released chan struct{} // closed when the second Connected is recorded
}

func (s *holdFirstConnectedSink) Offer(f Frame) {
	if f.Kind == Connected {
		s.mu.Lock()
		s.nConn++
		n := s.nConn
		s.mu.Unlock()
		switch n {
		case 1:
			close(s.entered)
			select {
			case <-s.released:
			case <-time.After(5 * time.Second):
			}
		case 2:
			defer close(s.released)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, f)
}

func (s *holdFirstConnectedSink) snapshot() []Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Frame(nil), s.frames...)
}

func describeFrames(fs []Frame) string {
	var b strings.Builder
	for _, f := range fs {
		fmt.Fprintf(&b, "%s(s=%d) ", f.Kind, f.Session)
	}
	return b.String()
}

// The notices of a replaced session, its Connected included, can reach the
// sink after the new session's Connected. This test forces that order and
// checks that the rule documented on Frame (per charger, the highest Session
// seen is current; lower is stale) leaves the charger connected.
func TestSinkLateConnectedOfReplacedSessionIsStale(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	sink := &holdFirstConnectedSink{entered: make(chan struct{}), released: make(chan struct{})}
	_, base, _ := start(t, Options{Chargers: chargers{"CP1": cp(up, "CP1", "")}, Sink: sink})

	firstCh := make(chan *websocket.Conn, 1)
	go func() {
		ws, _, err := dial(t, base, "/ocpp/CP1", dialOpts{})
		if err != nil {
			t.Errorf("first dial: %v", err)
		}
		firstCh <- ws
	}()
	<-up.connections
	select {
	case <-sink.entered: // session 1 is registered, its Connected is held
	case <-time.After(3 * time.Second):
		t.Fatal("first Connected never offered")
	}

	mustDial(t, base, "/ocpp/CP1", dialOpts{})
	<-up.connections
	first := <-firstCh
	if first == nil {
		t.FailNow()
	}
	waitClosed(t, first, 2*time.Second)
	waitFor(t, func() bool { return len(sink.snapshot()) == 3 })

	got := sink.snapshot()
	// The reordering is real: the sink saw the newer Connected first.
	if got[0].Kind != Connected || got[1].Kind != Connected || got[0].Session <= got[1].Session {
		t.Fatalf("sink saw %s, want Connected(newer), Connected(older), ...", describeFrames(got))
	}
	if got[2].Kind != Disconnected || got[2].Session != got[1].Session {
		t.Fatalf("sink saw %s, want the older session's Disconnected last", describeFrames(got))
	}

	// Apply the documented rule: the highest Session is current, anything
	// lower is stale.
	var current uint64
	connected := false
	for _, fr := range got {
		if fr.Session < current {
			continue // stale
		}
		current = fr.Session
		switch fr.Kind {
		case Connected:
			connected = true
		case Disconnected:
			connected = false
		}
	}
	if !connected || current != got[0].Session {
		t.Errorf("after the rule: connected=%v current=%d, want connected on session %d", connected, current, got[0].Session)
	}
}

// Upstream lost: the Disconnected notice says so.
func TestSinkDisconnectReasonUpstreamLost(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	sink := &recordingSink{}
	_, base, _ := start(t, Options{Chargers: chargers{"CP1": cp(up, "CP1", "")}, Sink: sink})
	ws := mustDial(t, base, "/ocpp/CP1", dialOpts{})
	<-up.connections
	up.toSend <- message{} // upstream closes
	waitClosed(t, ws, 2*time.Second)
	waitFor(t, func() bool { return len(sink.snapshot()) == 2 })
	if d := sink.snapshot()[1]; d.Kind != Disconnected || d.Reason != "upstream lost" {
		t.Errorf("notice = %+v, want Disconnected with reason \"upstream lost\"", d)
	}
}

func TestKindString(t *testing.T) {
	t.Parallel()

	for k, want := range map[Kind]string{Message: "message", Connected: "connected", Disconnected: "disconnected", Kind(9): "Kind(9)"} {
		if got := k.String(); got != want {
			t.Errorf("Kind(%d).String() = %q, want %q", int(k), got, want)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 3s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Two connections from the same charger whose registrations land out of
// order (the first handshake's goroutine is descheduled between accepting
// the charger and registering the session) must still leave the newer
// connection in place and close the older one. CI hit this interleaving in
// TestReconnectGetsNewUpstream; the hook makes it deterministic.
func TestOlderConnectionRegisteringLateDoesNotReplaceNewer(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	f, base, _ := start(t, Options{Chargers: chargers{"CP1": cp(up, "CP1", "")}})

	secondRegistered := make(chan struct{})
	var calls atomic.Int64
	f.beforeRegister = func(string) {
		switch calls.Add(1) {
		case 1:
			// Hold the first connection until the second has registered.
			<-secondRegistered
		case 2:
			defer close(secondRegistered)
		}
	}

	firstCh := make(chan *websocket.Conn, 1)
	go func() {
		ws, _, err := dial(t, base, "/ocpp/CP1", dialOpts{})
		if err != nil {
			t.Errorf("first dial: %v", err)
		}
		firstCh <- ws
	}()
	<-up.connections // the first charger is accepted, its registration held
	waitFor(t, func() bool { return calls.Load() == 1 })

	second := mustDial(t, base, "/ocpp/CP1", dialOpts{})
	<-up.connections
	first := <-firstCh
	if first == nil {
		t.FailNow()
	}

	// The older connection is the stale one, whatever order it registered in.
	waitClosed(t, first, 2*time.Second)
	if err := second.WriteMessage(websocket.TextMessage, []byte(`[2,"1","Heartbeat",{}]`)); err != nil {
		t.Fatalf("newer connection unusable: %v", err)
	}
	if got := recv(t, up.received); string(got.data) != `[2,"1","Heartbeat",{}]` {
		t.Errorf("upstream got %q", got.data)
	}
	waitFor(t, func() bool { return f.Active() == 1 })
}

// Close racing new handshakes must neither race nor leave a session behind:
// a charger accepted after Close begins is closed, not registered.
func TestCloseRacingNewConnections(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, nil)
	f, base, _ := start(t, Options{Chargers: chargers{"CP1": cp(up, "CP1", "")}})

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := websocket.Dialer{Subprotocols: []string{"ocpp1.6"}, HandshakeTimeout: 3 * time.Second}
			ws, resp, err := d.Dial(base+"/ocpp/CP1", nil)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if err == nil {
				_ = ws.Close()
			}
		}()
	}
	time.Sleep(time.Millisecond)
	f.Close()
	wg.Wait()
	if n := f.Active(); n != 0 {
		t.Errorf("Active = %d after Close", n)
	}
}
