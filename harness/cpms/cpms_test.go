package cpms

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/wolffseb/ocpp-tap/harness/frames"
	"github.com/wolffseb/ocpp-tap/internal/ocpp/ocppj"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func start(t *testing.T, opts Options) (*Server, string) {
	t.Helper()
	opts.Log = quiet()
	s := New(opts)
	srv := httptest.NewServer(s)
	t.Cleanup(func() { s.Close(); srv.Close() })
	return s, "ws" + strings.TrimPrefix(srv.URL, "http")
}

// charger is a raw WebSocket client standing in for a charge point, so the
// tests see exactly the bytes the CPMS puts on the wire.
type charger struct {
	t  *testing.T
	ws *websocket.Conn
}

// dial returns the HTTP status of a refused handshake (0 if there was none).
func dial(t *testing.T, base, id string, subprotocols ...string) (*charger, int, error) {
	t.Helper()
	if subprotocols == nil {
		subprotocols = []string{"ocpp1.6"}
	}
	d := websocket.Dialer{Subprotocols: subprotocols, HandshakeTimeout: 2 * time.Second}
	ws, resp, err := d.Dial(base+"/ocpp/"+id, nil)
	status := 0
	if resp != nil {
		status = resp.StatusCode
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, status, err
	}
	t.Cleanup(func() { _ = ws.Close() })
	return &charger{t: t, ws: ws}, status, nil
}

func mustDial(t *testing.T, base, id string) *charger {
	t.Helper()
	c, _, err := dial(t, base, id)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c
}

func (c *charger) send(raw string) {
	c.t.Helper()
	if err := c.ws.WriteMessage(websocket.TextMessage, []byte(raw)); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

func (c *charger) read(within time.Duration) ([]byte, error) {
	_ = c.ws.SetReadDeadline(time.Now().Add(within))
	_, data, err := c.ws.ReadMessage()
	return data, err
}

func (c *charger) mustRead() ocppj.Frame {
	c.t.Helper()
	data, err := c.read(2 * time.Second)
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	f, _, rpcErr := ocppj.ParseFrame(data)
	if rpcErr != nil {
		c.t.Fatalf("parse %q: %v", data, rpcErr)
	}
	return f
}

func payloadField(t *testing.T, payload json.RawMessage, field string) any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatalf("payload %s: %v", payload, err)
	}
	return m[field]
}

const boot = `[2,"b1","BootNotification",{"chargePointVendor":"V","chargePointModel":"M"}]`

func TestBootAccepted(t *testing.T) {
	t.Parallel()

	_, base := start(t, Options{HeartbeatInterval: 7})
	c := mustDial(t, base, "CP1")
	c.send(boot)

	res := c.mustRead()
	if res.Type != ocppj.MessageTypeCallResult || res.ID != "b1" {
		t.Fatalf("got %+v, want CALLRESULT b1", res)
	}
	if got := payloadField(t, res.Payload, "status"); got != "Accepted" {
		t.Errorf("status = %v, want Accepted", got)
	}
	if got := payloadField(t, res.Payload, "interval"); got != float64(7) {
		t.Errorf("interval = %v, want 7", got)
	}
}

// The CPMS answers the core charger-initiated actions and refuses the rest
// with a CALLERROR, as a real CPMS would.
func TestAnswersChargerCalls(t *testing.T) {
	t.Parallel()

	_, base := start(t, Options{})
	c := mustDial(t, base, "CP1")

	cases := []struct {
		call     string
		wantType ocppj.MessageType
		field    string
		want     any
	}{
		{`[2,"1","Heartbeat",{}]`, ocppj.MessageTypeCallResult, "", nil},
		{`[2,"2","StatusNotification",{"connectorId":1,"errorCode":"NoError","status":"Available"}]`, ocppj.MessageTypeCallResult, "", nil},
		{`[2,"3","Authorize",{"idTag":"T"}]`, ocppj.MessageTypeCallResult, "", nil},
		{`[2,"4","StartTransaction",{"connectorId":1,"idTag":"T","meterStart":0,"timestamp":"2026-01-01T00:00:00Z"}]`, ocppj.MessageTypeCallResult, "transactionId", float64(1)},
		{`[2,"5","MeterValues",{"connectorId":1,"meterValue":[]}]`, ocppj.MessageTypeCallResult, "", nil},
		{`[2,"6","StopTransaction",{"transactionId":1,"meterStop":10,"timestamp":"2026-01-01T00:00:00Z"}]`, ocppj.MessageTypeCallResult, "", nil},
		{`[2,"7","DataTransfer",{"vendorId":"X"}]`, ocppj.MessageTypeCallResult, "status", "Accepted"},
		{`[2,"8","VendorSpecificThing",{}]`, ocppj.MessageTypeCallError, "", nil},
	}
	for _, tc := range cases {
		c.send(tc.call)
		res := c.mustRead()
		if res.Type != tc.wantType {
			t.Errorf("%s: type %d, want %d", tc.call, res.Type, tc.wantType)
			continue
		}
		if tc.field != "" {
			if got := payloadField(t, res.Payload, tc.field); got != tc.want {
				t.Errorf("%s: %s = %v, want %v", tc.call, tc.field, got, tc.want)
			}
		}
	}
}

// Every frame received and sent is logged with its exact bytes.
func TestLogsEveryFrameExactly(t *testing.T) {
	t.Parallel()

	s, base := start(t, Options{})
	c := mustDial(t, base, "CP1")

	odd := "[2, \"h1\" ,\"Heartbeat\",{ }]"
	c.send(odd)
	answer, err := c.read(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	bad := `not even json`
	c.send(bad)
	callErr, err := c.read(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool { return s.Frames().Len() >= 4 })
	recv := s.Frames().For("CP1", frames.Upstream)
	sent := s.Frames().For("CP1", frames.Downstream)
	if len(recv) != 2 || !bytes.Equal(recv[0].Data, []byte(odd)) || !bytes.Equal(recv[1].Data, []byte(bad)) {
		t.Errorf("received log = %q", datas(recv))
	}
	if len(sent) != 2 || !bytes.Equal(sent[0].Data, answer) || !bytes.Equal(sent[1].Data, callErr) {
		t.Errorf("sent log = %q, charger got %q and %q", datas(sent), answer, callErr)
	}
}

func TestFrameLogFileIsJSONLines(t *testing.T) {
	t.Parallel()

	var buf syncBuffer
	_, base := start(t, Options{FrameLog: &buf})
	c := mustDial(t, base, "CP1")
	c.send(`[2,"1","Heartbeat",{}]`)
	c.mustRead()

	waitFor(t, func() bool { return strings.Count(buf.String(), "\n") >= 2 })
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var first struct {
		Charger string `json:"charger"`
		Dir     string `json:"dir"`
		Data    string `json:"data"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line %q: %v", lines[0], err)
	}
	if first.Charger != "CP1" || first.Dir != "upstream" || first.Data != `[2,"1","Heartbeat",{}]` {
		t.Errorf("first line = %+v", first)
	}
}

// CSMS-initiated calls reach the charger and return its answer.
func TestCSMSInitiatedCalls(t *testing.T) {
	t.Parallel()

	s, base := start(t, Options{})
	c := mustDial(t, base, "CP1")
	c.send(boot)
	c.mustRead()
	ctx := context.Background()

	type result struct {
		payload json.RawMessage
		err     error
	}
	run := func(fn func() (json.RawMessage, error)) <-chan result {
		ch := make(chan result, 1)
		go func() { p, err := fn(); ch <- result{p, err} }()
		return ch
	}

	cases := []struct {
		name   string
		call   func() (json.RawMessage, error)
		action string
		field  string
		want   any
	}{
		{"RemoteStart", func() (json.RawMessage, error) { return s.RemoteStart(ctx, "CP1", 1, "TAG1") },
			"RemoteStartTransaction", "idTag", "TAG1"},
		{"GetConfiguration", func() (json.RawMessage, error) { return s.GetConfiguration(ctx, "CP1", "LocalAuthorizeOffline") },
			"GetConfiguration", "key", []any{"LocalAuthorizeOffline"}},
		{"DataTransfer", func() (json.RawMessage, error) { return s.DataTransfer(ctx, "CP1", "acme", "ping", "x") },
			"DataTransfer", "vendorId", "acme"},
	}
	for _, tc := range cases {
		ch := run(tc.call)
		got := c.mustRead()
		if got.Type != ocppj.MessageTypeCall || got.Action != tc.action {
			t.Fatalf("%s: charger got %+v", tc.name, got)
		}
		if v := payloadField(t, got.Payload, tc.field); !jsonEqual(v, tc.want) {
			t.Errorf("%s: %s = %v, want %v", tc.name, tc.field, v, tc.want)
		}
		c.send(`[3,"` + got.ID + `",{"status":"Accepted"}]`)
		res := <-ch
		if res.err != nil || payloadField(t, res.payload, "status") != "Accepted" {
			t.Errorf("%s: result %s, %v", tc.name, res.payload, res.err)
		}
	}
}

func TestCallUnknownCharger(t *testing.T) {
	t.Parallel()

	s, _ := start(t, Options{})
	_, err := s.Call(context.Background(), "nope", "Reset", map[string]any{"type": "Soft"})
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("err = %v, want ErrNotConnected", err)
	}
}

func TestWaitCalls(t *testing.T) {
	t.Parallel()

	s, base := start(t, Options{})
	c := mustDial(t, base, "CP1")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		c.send(`[2,"1","Heartbeat",{}]`)
		c.send(`[2,"2","Heartbeat",{}]`)
	}()
	if err := s.WaitCalls(ctx, "CP1", "Heartbeat", 2); err != nil {
		t.Fatal(err)
	}
	if err := s.WaitConnected(ctx, "CP1"); err != nil {
		t.Fatal(err)
	}
}

func TestConnectionsCountsEachHandshake(t *testing.T) {
	t.Parallel()

	s, base := start(t, Options{})
	first := mustDial(t, base, "CP1")
	_ = first.ws.Close()
	second := mustDial(t, base, "CP1")
	second.send(`[2,"1","Heartbeat",{}]`)
	second.mustRead()
	if n := s.Connections("CP1"); n != 2 {
		t.Errorf("Connections = %d, want 2", n)
	}
	if n := s.Connections("CP2"); n != 0 {
		t.Errorf("Connections(CP2) = %d, want 0", n)
	}
}

func TestRefusesWithoutOCPP16(t *testing.T) {
	t.Parallel()

	_, base := start(t, Options{})
	_, status, err := dial(t, base, "CP1", "ocpp2.0.1")
	if err == nil {
		t.Fatal("handshake without ocpp1.6 succeeded")
	}
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
}

// --- failure modes ---------------------------------------------------------

func TestModeRejectBoot(t *testing.T) {
	t.Parallel()

	_, base := start(t, Options{Mode: ModeRejectBoot})
	c := mustDial(t, base, "CP1")
	c.send(boot)
	if got := payloadField(t, c.mustRead().Payload, "status"); got != "Rejected" {
		t.Errorf("status = %v, want Rejected", got)
	}
}

func TestModeSlow(t *testing.T) {
	t.Parallel()

	const delay = 300 * time.Millisecond
	_, base := start(t, Options{Mode: ModeSlow, SlowDelay: delay})
	c := mustDial(t, base, "CP1")

	began := time.Now()
	c.send(`[2,"1","Heartbeat",{}]`)
	c.mustRead()
	if took := time.Since(began); took < delay {
		t.Errorf("answer after %s, want at least %s", took, delay)
	}
}

func TestModeDropAfterN(t *testing.T) {
	t.Parallel()

	s, base := start(t, Options{Mode: ModeDropAfterN, DropAfter: 3})
	c := mustDial(t, base, "CP1")

	c.send(`[2,"1","Heartbeat",{}]`)
	c.mustRead()
	c.send(`[2,"2","Heartbeat",{}]`)
	c.mustRead()
	c.send(`[2,"3","Heartbeat",{}]`)

	// The third frame is received, logged, and the socket closed; it is not
	// answered.
	if data, err := c.read(2 * time.Second); err == nil {
		t.Fatalf("got %q after the drop, want the connection closed", data)
	} else if isTimeout(err) {
		t.Fatal("connection still open after N frames")
	}
	waitFor(t, func() bool { return len(s.Frames().For("CP1", frames.Upstream)) == 3 })
	if n := len(s.Frames().For("CP1", frames.Downstream)); n != 2 {
		t.Errorf("sent %d frames, want 2 (no answer to the dropped one)", n)
	}

	// A new connection gets a fresh count.
	c2 := mustDial(t, base, "CP1")
	c2.send(`[2,"4","Heartbeat",{}]`)
	c2.mustRead()
}

func TestModeDown(t *testing.T) {
	t.Parallel()

	s, base := start(t, Options{})
	live := mustDial(t, base, "CP1")
	live.send(`[2,"1","Heartbeat",{}]`)
	live.mustRead()

	s.SetMode(ModeDown)

	// Live connections are dropped...
	if _, err := live.read(2 * time.Second); err == nil || isTimeout(err) {
		t.Fatalf("live connection survived ModeDown: %v", err)
	}
	// ...and new ones refused.
	_, status, err := dial(t, base, "CP2")
	if err == nil {
		t.Fatal("dial succeeded while down")
	}
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", status)
	}

	// Back up again.
	s.SetMode(ModeNormal)
	c := mustDial(t, base, "CP2")
	c.send(`[2,"1","Heartbeat",{}]`)
	c.mustRead()
}

func TestChargerID(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{"/ocpp/CP1": "CP1", "/CP1/": "CP1", "/a/b/CP-2": "CP-2", "/": "", "": ""} {
		if got := chargerID(in); got != want {
			t.Errorf("chargerID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseMode(t *testing.T) {
	t.Parallel()

	for _, m := range []string{"normal", "down", "slow", "reject-boot", "drop-after-n"} {
		if _, err := ParseMode(m); err != nil {
			t.Errorf("ParseMode(%q): %v", m, err)
		}
	}
	if _, err := ParseMode("bogus"); err == nil {
		t.Error("ParseMode(bogus) succeeded")
	}
}

// --- helpers ---------------------------------------------------------------

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 2s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func isTimeout(err error) bool {
	var ne interface{ Timeout() bool }
	return errors.As(err, &ne) && ne.Timeout()
}

func datas(fs []frames.Frame) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = string(f.Data)
	}
	return out
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
