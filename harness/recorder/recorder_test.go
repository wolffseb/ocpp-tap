package recorder

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/wolffseb/ocpp-tap/harness/frames"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func wsURL(srv *httptest.Server) string { return "ws" + strings.TrimPrefix(srv.URL, "http") }

type message struct {
	typ  int
	data []byte
}

// upstream is a raw WebSocket server that reports what it receives and the
// path and subprotocols it was dialled with, and sends what it is told to.
type upstream struct {
	srv      *httptest.Server
	received chan message
	toSend   chan message
	path     chan string
	offered  chan []string
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{
		received: make(chan message, 100),
		toSend:   make(chan message, 100),
		path:     make(chan string, 10),
		offered:  make(chan []string, 10),
	}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.path <- r.URL.Path
		u.offered <- websocket.Subprotocols(r)
		up := websocket.Upgrader{Subprotocols: []string{"ocpp1.6"}}
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
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
	}))
	t.Cleanup(func() { close(u.toSend); u.srv.Close() })
	return u
}

func startRecorder(t *testing.T, upstreamURL string) (*Recorder, string) {
	t.Helper()
	r := New(upstreamURL, quiet())
	srv := httptest.NewServer(r)
	t.Cleanup(func() { r.Close(); srv.Close() })
	return r, wsURL(srv)
}

func dialCharger(t *testing.T, base, path string) (*websocket.Conn, int, error) {
	t.Helper()
	d := websocket.Dialer{Subprotocols: []string{"ocpp1.6"}, HandshakeTimeout: 2 * time.Second}
	ws, resp, err := d.Dial(base+path, nil)
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

func recv(t *testing.T, ch <-chan message) message {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("nothing arrived")
		return message{}
	}
}

var payloads = [][]byte{
	[]byte(`[2,"1","Heartbeat",{}]`),
	[]byte("[2, \"2\" ,\"BootNotification\" , {\"b\":1,\"a\":2}]"),
	[]byte(`[2,"3","DataTransfer",{"vendorId":"é","data":"<>&"}]`),
	[]byte(`[3,"4",{"n":1e400}]`),
	bytes.Repeat([]byte("A"), 200000),
}

func TestForwardsAndRecordsBothWaysUnchanged(t *testing.T) {
	t.Parallel()

	up := newUpstream(t)
	rec, base := startRecorder(t, wsURL(up.srv))
	charger, _, err := dialCharger(t, base, "/ocpp/CP1")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if got := charger.Subprotocol(); got != "ocpp1.6" {
		t.Errorf("charger subprotocol = %q, want ocpp1.6", got)
	}
	if got := <-up.path; got != "/ocpp/CP1" {
		t.Errorf("upstream path = %q, want /ocpp/CP1", got)
	}
	if got := <-up.offered; len(got) != 1 || got[0] != "ocpp1.6" {
		t.Errorf("upstream offered = %v, want [ocpp1.6]", got)
	}

	for _, p := range payloads {
		if err := charger.WriteMessage(websocket.TextMessage, p); err != nil {
			t.Fatal(err)
		}
		if got := recv(t, up.received); !bytes.Equal(got.data, p) || got.typ != websocket.TextMessage {
			t.Errorf("upstream got %q (type %d), want %q", got.data, got.typ, p)
		}
	}
	for _, p := range payloads {
		up.toSend <- message{websocket.TextMessage, p}
		_ = charger.SetReadDeadline(time.Now().Add(2 * time.Second))
		typ, got, err := charger.ReadMessage()
		if err != nil || !bytes.Equal(got, p) || typ != websocket.TextMessage {
			t.Errorf("charger got %q (type %d, %v), want %q", got, typ, err, p)
		}
	}

	waitLen(t, rec.Frames(), 2*len(payloads))
	for i, p := range payloads {
		if got := rec.Frames().For("CP1", frames.Upstream)[i].Data; !bytes.Equal(got, p) {
			t.Errorf("recorded upstream #%d = %q, want %q", i, got, p)
		}
		if got := rec.Frames().For("CP1", frames.Downstream)[i].Data; !bytes.Equal(got, p) {
			t.Errorf("recorded downstream #%d = %q, want %q", i, got, p)
		}
	}
}

func TestPreservesBinaryMessageType(t *testing.T) {
	t.Parallel()

	up := newUpstream(t)
	_, base := startRecorder(t, wsURL(up.srv))
	charger, _, err := dialCharger(t, base, "/CP1")
	if err != nil {
		t.Fatal(err)
	}
	if err := charger.WriteMessage(websocket.BinaryMessage, []byte{0, 1, 0xff}); err != nil {
		t.Fatal(err)
	}
	if got := recv(t, up.received); got.typ != websocket.BinaryMessage || !bytes.Equal(got.data, []byte{0, 1, 0xff}) {
		t.Errorf("upstream got type %d %v", got.typ, got.data)
	}
}

func TestUpstreamRefusalFailsChargerHandshake(t *testing.T) {
	t.Parallel()

	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	t.Cleanup(refusing.Close)

	_, base := startRecorder(t, wsURL(refusing))
	_, status, err := dialCharger(t, base, "/ocpp/CP1")
	if err == nil {
		t.Fatal("charger handshake succeeded with upstream refusing")
	}
	if status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", status)
	}
}

func TestUpstreamCloseClosesCharger(t *testing.T) {
	t.Parallel()

	up := newUpstream(t)
	_, base := startRecorder(t, wsURL(up.srv))
	charger, _, err := dialCharger(t, base, "/ocpp/CP1")
	if err != nil {
		t.Fatal(err)
	}
	up.toSend <- message{} // close
	_ = charger.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := charger.ReadMessage(); err == nil {
		t.Fatal("charger still connected")
	} else if ne, ok := err.(interface{ Timeout() bool }); ok && ne.Timeout() {
		t.Fatal("charger not closed within 2s")
	}
}

func TestChargerCloseClosesUpstream(t *testing.T) {
	t.Parallel()

	closed := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{Subprotocols: []string{"ocpp1.6"}}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				close(closed)
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	_, base := startRecorder(t, wsURL(srv))
	charger, _, err := dialCharger(t, base, "/ocpp/CP1")
	if err != nil {
		t.Fatal(err)
	}
	_ = charger.Close()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream not closed within 2s")
	}
}

func waitLen(t *testing.T, l *frames.Log, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for l.Len() < n {
		if time.Now().After(deadline) {
			t.Fatalf("recorded %d frames, want %d", l.Len(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
