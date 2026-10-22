package placeholder

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/wolffseb/ocpp-tap/harness/cpms"
	"github.com/wolffseb/ocpp-tap/harness/frames"
	"github.com/wolffseb/ocpp-tap/harness/recorder"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func wsURL(srv *httptest.Server) string { return "ws" + strings.TrimPrefix(srv.URL, "http") }

// chain is charger → recorder → placeholder → harness CPMS.
func chain(t *testing.T, opts cpms.Options) (*cpms.Server, *recorder.Recorder, string) {
	t.Helper()
	opts.Log = quiet()
	c := cpms.New(opts)
	cpmsSrv := httptest.NewServer(c)
	p := httptest.NewServer(New(wsURL(cpmsSrv), quiet()))
	rec := recorder.New(wsURL(p), quiet())
	recSrv := httptest.NewServer(rec)
	t.Cleanup(func() { rec.Close(); recSrv.Close(); p.Close(); c.Close(); cpmsSrv.Close() })
	return c, rec, wsURL(recSrv)
}

func dial(t *testing.T, base string) (*websocket.Conn, int, error) {
	t.Helper()
	d := websocket.Dialer{Subprotocols: []string{"ocpp1.6"}, HandshakeTimeout: 2 * time.Second}
	ws, resp, err := d.Dial(base+"/ocpp/CP1", nil)
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

func TestPassesFramesUnchangedBothWays(t *testing.T) {
	t.Parallel()

	c, rec, base := chain(t, cpms.Options{})
	ws, _, err := dial(t, base)
	if err != nil {
		t.Fatal(err)
	}
	if ws.Subprotocol() != "ocpp1.6" {
		t.Errorf("subprotocol = %q", ws.Subprotocol())
	}

	for _, msg := range []string{
		`[2,"b","BootNotification",{"chargePointVendor":"V","chargePointModel":"M"}]`,
		"[2, \"h\" ,\"Heartbeat\",{ }]",
		`[2,"v","AcmeVendorAction",{"x":"é"}]`,
	} {
		if err := ws.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
			t.Fatal(err)
		}
		_ = ws.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, _, err := ws.ReadMessage(); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		_ = ws.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, data, err := ws.ReadMessage()
		if err == nil {
			id := strings.Split(string(data), `"`)[1]
			_ = ws.WriteMessage(websocket.TextMessage, []byte(`[3,"`+id+`",{"status":"Accepted"}]`))
		}
	}()
	if _, err := c.DataTransfer(ctx, "CP1", "acme", "m", "d"); err != nil {
		t.Fatalf("DataTransfer through placeholder: %v", err)
	}

	waitSettled(t, rec.Frames(), c.Frames())
	if diffs := frames.Compare(rec.Frames().Frames(), c.Frames().Frames()); len(diffs) != 0 {
		t.Fatalf("diffs: %v", diffs)
	}
	if n := rec.Frames().Len(); n != 8 {
		t.Errorf("recorded %d frames, want 8", n)
	}
}

func TestUpstreamDownRefusesCharger(t *testing.T) {
	t.Parallel()

	_, _, base := chain(t, cpms.Options{Mode: cpms.ModeDown})
	if _, status, err := dial(t, base); err == nil || status != http.StatusBadGateway {
		t.Fatalf("err %v status %d, want a refused handshake (502)", err, status)
	}
}

func TestUpstreamDropClosesCharger(t *testing.T) {
	t.Parallel()

	c, _, base := chain(t, cpms.Options{})
	ws, _, err := dial(t, base)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.WaitConnected(ctx, "CP1"); err != nil {
		t.Fatal(err)
	}
	c.SetMode(cpms.ModeDown)
	_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := ws.ReadMessage(); err == nil {
		t.Fatal("charger still connected after upstream dropped")
	} else if ne, ok := err.(interface{ Timeout() bool }); ok && ne.Timeout() {
		t.Fatal("charger not closed")
	}
}

func waitSettled(t *testing.T, a, b *frames.Log) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(frames.Compare(a.Frames(), b.Frames())) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}
