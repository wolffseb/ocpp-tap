//go:build e2e

package e2e

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/wolffseb/ocpp-tap/harness/cpms"
)

// dialCharger connects a raw charger to url/ocpp/id and returns a refused
// handshake's HTTP status.
func dialCharger(t *testing.T, url, id, pass string, subprotocols ...string) (*websocket.Conn, int, error) {
	t.Helper()
	if subprotocols == nil {
		subprotocols = []string{"ocpp1.6"}
	}
	h := http.Header{}
	if pass != "" {
		h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(id+":"+pass)))
	}
	d := websocket.Dialer{Subprotocols: subprotocols, HandshakeTimeout: 5 * time.Second}
	ws, resp, err := d.Dial(url+"/ocpp/"+id, h)
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

func waitConnected(t *testing.T, c *cpms.Server, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.WaitConnected(ctx, id); err != nil {
		t.Fatalf("%s never reached the CPMS: %v", id, err)
	}
}

// Fail closed: upstream lost → charger connection closed within 2 s.
func TestFailClosedUpstreamLost(t *testing.T) {
	t.Parallel()

	const id = "FC-LOST"
	r := newRig(t, cpms.Options{}, id)
	ws, status, err := dialCharger(t, r.proxyURL, id, password(id))
	if err != nil {
		t.Fatalf("dial: %v (HTTP %d)", err, status)
	}
	waitConnected(t, r.cpms, id)

	began := time.Now()
	r.cpms.SetMode(cpms.ModeDown) // drops the upstream connection
	_ = ws.SetReadDeadline(began.Add(2 * time.Second))
	_, _, err = ws.ReadMessage()
	took := time.Since(began)
	if isTimeout(err) {
		t.Fatalf("charger still connected %s after upstream was lost", took)
	}
	t.Logf("charger closed %s after upstream was lost (target < 2s): %v", took.Round(time.Millisecond), err)
	if !websocket.IsCloseError(err, websocket.CloseInternalServerErr) {
		t.Errorf("close = %v, want 1011", err)
	}
}

// Fail closed: upstream unreachable → the charger is not accepted at all.
func TestFailClosedUpstreamUnreachable(t *testing.T) {
	t.Parallel()

	const id = "FC-DOWN"
	r := newRig(t, cpms.Options{Mode: cpms.ModeDown}, id)
	if _, status, err := dialCharger(t, r.proxyURL, id, password(id)); err == nil || status != http.StatusBadGateway {
		t.Fatalf("err %v status %d, want a refused handshake (502)", err, status)
	}
	if !strings.Contains(r.proxy.out.String(), "upstream unavailable") {
		t.Errorf("octap did not log the refusal:\n%s", r.proxy.out.String())
	}
}

// Charger reconnects → new upstream connection.
func TestReconnectGetsNewUpstream(t *testing.T) {
	t.Parallel()

	const id = "RECONNECT"
	r := newRig(t, cpms.Options{}, id)
	first, _, err := dialCharger(t, r.proxyURL, id, password(id))
	if err != nil {
		t.Fatal(err)
	}
	waitConnected(t, r.cpms, id)
	_ = first.Close()

	deadline := time.Now().Add(3 * time.Second)
	for r.cpms.Connected(id) {
		if time.Now().After(deadline) {
			t.Fatal("upstream still connected after the charger left")
		}
		time.Sleep(10 * time.Millisecond)
	}

	second, _, err := dialCharger(t, r.proxyURL, id, password(id))
	if err != nil {
		t.Fatal(err)
	}
	waitConnected(t, r.cpms, id)
	if n := r.cpms.Connections(id); n != 2 {
		t.Errorf("CPMS saw %d connections, want 2", n)
	}
	if err := second.WriteMessage(websocket.TextMessage, []byte(`[2,"1","Heartbeat",{}]`)); err != nil {
		t.Fatal(err)
	}
	waitCalls(t, r.cpms, id, "Heartbeat", 1, 3*time.Second)
}

// Per-charger Basic Auth, and unknown chargers, are refused before any
// upstream connection is made.
func TestRefusedBeforeUpstream(t *testing.T) {
	t.Parallel()

	const id = "AUTH"
	r := newRig(t, cpms.Options{}, id)
	cases := []struct {
		name, id, pass string
		status         int
	}{
		{"no password", id, "", http.StatusUnauthorized},
		{"wrong password", id, "wrong", http.StatusUnauthorized},
		{"unknown charger", "STRANGER", password("STRANGER"), http.StatusNotFound},
	}
	for _, tc := range cases {
		if _, status, err := dialCharger(t, r.proxyURL, tc.id, tc.pass); err == nil || status != tc.status {
			t.Errorf("%s: err %v status %d, want %d", tc.name, err, status, tc.status)
		}
	}
	if n := r.cpms.Connections(id) + r.cpms.Connections("STRANGER"); n != 0 {
		t.Errorf("CPMS saw %d connections from refused chargers", n)
	}
}

// The subprotocol the charger offers is what upstream sees; a CPMS that
// accepts none of them means the charger is refused.
func TestSubprotocol(t *testing.T) {
	t.Parallel()

	const id = "SUBPROTO"
	r := newRig(t, cpms.Options{}, id)
	ws, _, err := dialCharger(t, r.proxyURL, id, password(id), "ocpp1.6")
	if err != nil {
		t.Fatal(err)
	}
	if ws.Subprotocol() != "ocpp1.6" {
		t.Errorf("negotiated %q", ws.Subprotocol())
	}
	_ = ws.Close()

	// The harness CPMS speaks only 1.6, so a 2.0.1-only charger has no
	// common subprotocol upstream and must not be accepted.
	if _, status, err := dialCharger(t, r.proxyURL, id, password(id), "ocpp2.0.1"); err == nil || status != http.StatusBadGateway {
		t.Errorf("2.0.1-only charger: err %v status %d, want 502", err, status)
	}
}

// Unknown is forwarded: vendor actions, DataTransfer, unknown message types
// and even malformed frames reach the CPMS byte for byte, and its answers
// come back the same way.
func TestUnknownAndVendorPassThrough(t *testing.T) {
	t.Parallel()

	const id = "VENDOR"
	r := newRig(t, cpms.Options{}, id)
	ws, _, err := dialCharger(t, r.recURL, id, password(id))
	if err != nil {
		t.Fatal(err)
	}

	frames := []string{
		`[2,"v1","AcmeVendorThing",{"x":"é","n":1e400}]`,
		`[2,"v2","DataTransfer",{"vendorId":"com.acme","messageId":"m","data":"{\"nested\":true}"}]`,
		"[2, \"v3\" ,\"Heartbeat\" , { }]",
		`[9,"v4","NotAMessageType",{}]`,
		`not json at all`,
	}
	for _, f := range frames {
		if err := ws.WriteMessage(websocket.TextMessage, []byte(f)); err != nil {
			t.Fatal(err)
		}
		_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, _, err := ws.ReadMessage(); err != nil {
			t.Fatalf("no answer to %s: %v", f, err)
		}
	}
	assertIdentical(t, r.rec.Frames(), r.cpms.Frames(), 2*len(frames))
}

func isTimeout(err error) bool {
	var ne interface{ Timeout() bool }
	return errors.As(err, &ne) && ne.Timeout()
}
