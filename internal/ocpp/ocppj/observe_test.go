package ocppj

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/wolffseb/ocpp-tap/internal/ocpp"
)

type observed struct {
	dir  Direction
	data []byte
}

type recorder struct {
	mu     sync.Mutex
	frames []observed
}

func (r *recorder) observe(dir Direction, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames = append(r.frames, observed{dir, append([]byte(nil), data...)})
}

func (r *recorder) snapshot() []observed {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]observed(nil), r.frames...)
}

// observedPair returns a Conn on the server side with an Observe hook, and
// the raw client WebSocket talking to it.
func observedPair(t *testing.T) (*Conn, *websocket.Conn, *recorder) {
	t.Helper()

	rec := &recorder{}
	connCh := make(chan *Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		conn := New(ws, Options{
			ID:      "CP",
			Version: ocpp.Version16,
			Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
			Observe: rec.observe,
		})
		connCh <- conn
		conn.Run(context.Background(), nopHandler{})
	}))
	t.Cleanup(srv.Close)

	client, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	select {
	case conn := <-connCh:
		t.Cleanup(conn.Close)
		return conn, client, rec
	case <-time.After(2 * time.Second):
		t.Fatal("server never accepted")
		return nil, nil, nil
	}
}

func readMessage(t *testing.T, ws *websocket.Conn) []byte {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return data
}

// waitFrames waits until the recorder holds n frames.
func waitFrames(t *testing.T, rec *recorder, n int) []observed {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := rec.snapshot(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := rec.snapshot()
	t.Fatalf("observed %d frames, want %d", len(got), n)
	return got
}

func byDirection(frames []observed) map[Direction]observed {
	out := map[Direction]observed{}
	for _, f := range frames {
		out[f.dir] = f
	}
	return out
}

func assertFrame(t *testing.T, got observed, dir Direction, want []byte) {
	t.Helper()
	if got.dir != dir {
		t.Errorf("direction = %v, want %v", got.dir, dir)
	}
	if !bytes.Equal(got.data, want) {
		t.Errorf("bytes = %q, want %q", got.data, want)
	}
}

// The hook sees inbound bytes exactly as they arrived, before parsing, and
// outbound bytes exactly as they were written.
func TestObserveInboundCallAndOutboundResult(t *testing.T) {
	t.Parallel()

	_, client, rec := observedPair(t)

	// Whitespace and an escape that a re-encode would normalise.
	call := []byte("[2, \"m1\" ,\"Heartbeat\",{ \"x\":\"\\u00e9\" }]")
	if err := client.WriteMessage(websocket.TextMessage, call); err != nil {
		t.Fatalf("write: %v", err)
	}
	result := readMessage(t, client)

	got := waitFrames(t, rec, 2)
	assertFrame(t, got[0], Inbound, call)
	assertFrame(t, got[1], Outbound, result)
}

func TestObserveOutboundCallAndInboundResult(t *testing.T) {
	t.Parallel()

	conn, client, rec := observedPair(t)

	errCh := make(chan error, 1)
	go func() {
		_, err := conn.Call(context.Background(), "GetConfiguration", map[string]any{})
		errCh <- err
	}()

	call := readMessage(t, client)
	f, _, rpcErr := ParseFrame(call)
	if rpcErr != nil {
		t.Fatalf("parse: %v", rpcErr)
	}
	result := []byte(`[3,"` + f.ID + `",  {"configurationKey":[]}]`)
	if err := client.WriteMessage(websocket.TextMessage, result); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("Call: %v", err)
	}

	// The read and write goroutines observe independently, so only the
	// order within one direction is defined.
	got := byDirection(waitFrames(t, rec, 2))
	assertFrame(t, got[Outbound], Outbound, call)
	assertFrame(t, got[Inbound], Inbound, result)
}

// A frame the parser rejects is still observed: the recorder must see what
// was on the wire, not what made sense.
func TestObserveMalformedInbound(t *testing.T) {
	t.Parallel()

	_, client, rec := observedPair(t)

	bad := []byte(`not json`)
	if err := client.WriteMessage(websocket.TextMessage, bad); err != nil {
		t.Fatalf("write: %v", err)
	}
	callErr := readMessage(t, client)

	got := waitFrames(t, rec, 2)
	assertFrame(t, got[0], Inbound, bad)
	assertFrame(t, got[1], Outbound, callErr)
}
