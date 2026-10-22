//go:build e2e

// Package e2e runs the SP0 harness end to end:
//
//	charger (headless-csms simulator, or loadgen) → recorder → proxy under test → harness CPMS
//
// and checks that every frame the charger sent is byte-identical to what the
// CPMS received, and the other way round. Run it with `make e2e`, which
// builds the pinned simulator and the proxy and sets:
//
//	E2E_SIMULATOR  path to headless-csms's cpms binary
//	E2E_PROXY      path to the proxy under test (flags: -listen, -upstream)
//
// Fail-closed timing and latency targets are SP1 and are not asserted here.
package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/wolffseb/ocpp-tap/harness/cpms"
	"github.com/wolffseb/ocpp-tap/harness/frames"
	"github.com/wolffseb/ocpp-tap/harness/loadgen"
	"github.com/wolffseb/ocpp-tap/harness/recorder"
	"github.com/wolffseb/ocpp-tap/internal/ocpp"
)

var (
	simulatorBin string
	proxyBin     string
)

func TestMain(m *testing.M) {
	simulatorBin = os.Getenv("E2E_SIMULATOR")
	proxyBin = os.Getenv("E2E_PROXY")
	// Missing binaries are a failure, not a skip: a skipped e2e suite would
	// read as green.
	for name, path := range map[string]string{"E2E_SIMULATOR": simulatorBin, "E2E_PROXY": proxyBin} {
		if path == "" {
			fmt.Fprintf(os.Stderr, "e2e: %s is not set; run `make e2e`\n", name)
			os.Exit(2)
		}
		if _, err := os.Stat(path); err != nil {
			fmt.Fprintf(os.Stderr, "e2e: %s: %v\n", name, err)
			os.Exit(2)
		}
	}
	began := time.Now()
	code := m.Run()
	fmt.Printf("e2e: suite took %s (budget 3m)\n", time.Since(began).Round(100*time.Millisecond))
	os.Exit(code)
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func wsURL(srv *httptest.Server) string { return "ws" + strings.TrimPrefix(srv.URL, "http") }

// rig is one isolated chain: harness CPMS ← proxy ← recorder.
type rig struct {
	cpms     *cpms.Server
	rec      *recorder.Recorder
	proxyURL string // chargers that record themselves dial this
	recURL   string // chargers that need recording dial this
}

func newRig(t *testing.T, opts cpms.Options) *rig {
	t.Helper()
	opts.Log = quiet()
	c := cpms.New(opts)
	cpmsSrv := httptest.NewServer(c)
	t.Cleanup(func() { c.Close(); cpmsSrv.Close() })

	proxyURL := startProxy(t, wsURL(cpmsSrv))
	return withRecorder(t, c, proxyURL)
}

func withRecorder(t *testing.T, c *cpms.Server, proxyURL string) *rig {
	t.Helper()
	rec := recorder.New(proxyURL, quiet())
	recSrv := httptest.NewServer(rec)
	t.Cleanup(func() { rec.Close(); recSrv.Close() })
	return &rig{cpms: c, rec: rec, proxyURL: proxyURL, recURL: wsURL(recSrv)}
}

// startProxy runs the proxy under test as its own process and waits until it
// accepts connections.
func startProxy(t *testing.T, upstream string) string {
	t.Helper()
	addr := freeAddr(t)
	var out syncBuffer
	cmd := exec.Command(proxyBin, "-listen", addr, "-upstream", upstream)
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("proxy output:\n%s", out.String())
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return "ws://" + addr
		}
		if time.Now().After(deadline) {
			t.Fatalf("proxy not listening on %s: %v\n%s", addr, err, out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// simulator is one `cpms simulate charger` process.
type simulator struct {
	cmd  *exec.Cmd
	out  *syncBuffer
	done chan struct{}
	err  error
}

func startSimulator(t *testing.T, url, id, scenario string) *simulator {
	t.Helper()
	s := &simulator{out: &syncBuffer{}, done: make(chan struct{})}
	s.cmd = exec.Command(simulatorBin,
		"--config", t.TempDir()+"/absent.yaml", // never pick up a local config
		"simulate", "charger",
		"--csms", url, "--id", id, "--scenario", scenario,
		"--connectors", "2", "--log-level", "debug")
	s.cmd.Stdout, s.cmd.Stderr = s.out, s.out
	if err := s.cmd.Start(); err != nil {
		t.Fatalf("start simulator: %v", err)
	}
	go func() { s.err = s.cmd.Wait(); close(s.done) }()
	t.Cleanup(func() {
		s.stop()
		if t.Failed() {
			t.Logf("simulator %s output:\n%s", id, s.out.String())
		}
	})
	return s
}

// stop asks the simulator to exit cleanly and waits for it.
func (s *simulator) stop() {
	select {
	case <-s.done:
		return
	default:
	}
	_ = s.cmd.Process.Signal(syscall.SIGINT)
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		_ = s.cmd.Process.Kill()
		<-s.done
	}
}

// exited waits for the simulator to exit on its own and returns its error.
func (s *simulator) exited(t *testing.T, within time.Duration) error {
	t.Helper()
	select {
	case <-s.done:
		return s.err
	case <-time.After(within):
		t.Fatalf("simulator still running after %s", within)
		return nil
	}
}

// assertIdentical waits for in-flight frames to settle, then requires zero
// diffs between the charger side and the CPMS side. It also requires that
// frames flowed at all, so an empty run cannot pass.
func assertIdentical(t *testing.T, chargerSide, cpmsSide *frames.Log, minFrames int) {
	t.Helper()
	var diffs []frames.Diff
	deadline := time.Now().Add(5 * time.Second)
	for {
		diffs = frames.Compare(chargerSide.Frames(), cpmsSide.Frames())
		if len(diffs) == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := chargerSide.Len(); n < minFrames {
		t.Errorf("only %d frames recorded, want at least %d", n, minFrames)
	}
	if len(diffs) > 0 {
		for i, d := range diffs {
			if i == 10 {
				t.Errorf("… and %d more", len(diffs)-10)
				break
			}
			t.Errorf("frame diff: %v", d)
		}
		t.FailNow()
	}
	t.Logf("frame identity: %d frames, 0 diffs", chargerSide.Len())
}

func waitCalls(t *testing.T, c *cpms.Server, id, action string, n int, within time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	if err := c.WaitCalls(ctx, id, action, n); err != nil {
		t.Fatalf("%s: waiting for %d× %s: %v (got %d)", id, n, action, err, c.CallCount(id, action))
	}
}

// answered accepts a CALLRESULT or a CALLERROR: both are frames that went
// through the proxy. Only a transport failure or timeout is an error here.
func answered(t *testing.T, what string, _ []byte, err error) {
	t.Helper()
	var rpcErr *ocpp.RPCError
	if err != nil && !errors.As(err, &rpcErr) {
		t.Fatalf("%s: %v", what, err)
	}
}

// --- simulator scenarios ---------------------------------------------------

// Every headless-csms simulator scenario, through the proxy, with
// CSMS-initiated RemoteStart, GetConfiguration and DataTransfer.
func TestSimulatorScenarios(t *testing.T) {
	t.Parallel()

	type step func(t *testing.T, ctx context.Context, c *cpms.Server, id string)
	remoteStart := func(t *testing.T, ctx context.Context, c *cpms.Server, id string) {
		res, err := c.RemoteStart(ctx, id, 1, "TAG-E2E")
		answered(t, "RemoteStart", res, err)
	}
	getConfig := func(t *testing.T, ctx context.Context, c *cpms.Server, id string) {
		res, err := c.GetConfiguration(ctx, id, "LocalAuthorizeOffline", "AuthorizationCacheEnabled", "AllowOfflineTxForUnknownId")
		answered(t, "GetConfiguration", res, err)
	}
	dataTransfer := func(t *testing.T, ctx context.Context, c *cpms.Server, id string) {
		res, err := c.DataTransfer(ctx, id, "com.acme", "ping", `{"x":"é"}`)
		answered(t, "DataTransfer", res, err)
	}
	call := func(action string, payload any) step {
		return func(t *testing.T, ctx context.Context, c *cpms.Server, id string) {
			res, err := c.Call(ctx, id, action, payload)
			answered(t, action, res, err)
		}
	}
	reserve := call("ReserveNow", map[string]any{
		"connectorId": 2, "expiryDate": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"idTag": "TAG-E2E", "reservationId": 7,
	})
	unlock := call("UnlockConnector", map[string]any{"connectorId": 2})
	waitTx := func(t *testing.T, _ context.Context, c *cpms.Server, id string) {
		waitCalls(t, c, id, "StartTransaction", 1, 5*time.Second)
	}
	remoteStop := func(t *testing.T, ctx context.Context, c *cpms.Server, id string) {
		res, err := c.Call(ctx, id, "RemoteStopTransaction", map[string]any{"transactionId": 1})
		answered(t, "RemoteStopTransaction", res, err)
		waitCalls(t, c, id, "StopTransaction", 1, 5*time.Second)
	}
	common := []step{remoteStart, waitTx, getConfig, dataTransfer}

	cases := []struct {
		scenario string
		steps    []step
	}{
		{"normal", append(append([]step{}, common...), remoteStop, reserve, unlock)},
		{"reject-reserve", append(append([]step{}, common...), reserve)},
		{"occupied", append(append([]step{}, common...), reserve)},
		{"unlock-fails", append(append([]step{}, common...), unlock)},
		{"silent", common},
		// The simulator stalls every answer for a fixed 45 s and handles
		// calls one at a time, so this scenario gets a single call.
		{"slow", []step{getConfig}},
	}
	for _, tc := range cases {
		t.Run(tc.scenario, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, cpms.Options{HeartbeatInterval: 2, CallTimeout: 60 * time.Second})
			id := "SIM-" + tc.scenario
			startSimulator(t, r.recURL, id, tc.scenario)

			// Boot plus status for the station and both connectors.
			waitCalls(t, r.cpms, id, "BootNotification", 1, 10*time.Second)
			waitCalls(t, r.cpms, id, "StatusNotification", 3, 10*time.Second)

			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			for _, s := range tc.steps {
				s(t, ctx, r.cpms, id)
			}
			assertIdentical(t, r.rec.Frames(), r.cpms.Frames(), 8)
		})
	}
}

// --- CPMS failure modes ----------------------------------------------------

func TestCPMSDown(t *testing.T) {
	t.Parallel()

	r := newRig(t, cpms.Options{Mode: cpms.ModeDown})
	sim := startSimulator(t, r.recURL, "SIM-down", "normal")
	if err := sim.exited(t, 15*time.Second); err == nil {
		t.Error("simulator exited cleanly with the CPMS down")
	}
	if n := r.cpms.Frames().Len(); n != 0 {
		t.Errorf("CPMS logged %d frames while down", n)
	}
	if n := r.rec.Frames().Len(); n != 0 {
		t.Errorf("charger exchanged %d frames with nothing behind the proxy", n)
	}
}

func TestCPMSRejectBoot(t *testing.T) {
	t.Parallel()

	r := newRig(t, cpms.Options{Mode: cpms.ModeRejectBoot})
	sim := startSimulator(t, r.recURL, "SIM-reject", "normal")
	if err := sim.exited(t, 15*time.Second); err == nil {
		t.Error("simulator exited cleanly after a rejected boot")
	}
	if !strings.Contains(sim.out.String(), "Rejected") {
		t.Errorf("simulator did not report the rejection:\n%s", sim.out.String())
	}
	assertIdentical(t, r.rec.Frames(), r.cpms.Frames(), 2)
}

func TestCPMSSlow(t *testing.T) {
	t.Parallel()

	const delay = 1 * time.Second
	r := newRig(t, cpms.Options{Mode: cpms.ModeSlow, SlowDelay: delay, HeartbeatInterval: 2})
	began := time.Now()
	startSimulator(t, r.recURL, "SIM-slowcpms", "normal")
	waitCalls(t, r.cpms, "SIM-slowcpms", "StatusNotification", 3, 20*time.Second)
	if took := time.Since(began); took < 3*delay {
		t.Errorf("boot and three statuses took %s, want at least %s with a slow CPMS", took, 3*delay)
	}
	assertIdentical(t, r.rec.Frames(), r.cpms.Frames(), 6)
}

func TestCPMSDropAfterN(t *testing.T) {
	t.Parallel()

	const n = 3
	r := newRig(t, cpms.Options{Mode: cpms.ModeDropAfterN, DropAfter: n})
	sim := startSimulator(t, r.recURL, "SIM-drop", "normal")

	// Boot, status(0), status(1): the third frame is received and the CPMS
	// hangs up without answering. The simulator must see the drop.
	if err := sim.exited(t, 15*time.Second); err == nil {
		t.Error("simulator exited cleanly after the CPMS dropped it")
	}
	if got := len(r.cpms.Frames().For("SIM-drop", frames.Upstream)); got != n {
		t.Errorf("CPMS received %d frames, want %d", got, n)
	}
	assertIdentical(t, r.rec.Frames(), r.cpms.Frames(), 2*n-1)
}

// --- load ------------------------------------------------------------------

// 100 chargers in one process at 1 message/s each, through the proxy.
// Latency is reported, not asserted: the targets belong to SP1.
func TestLoad(t *testing.T) {
	t.Parallel()

	const chargers, messages = 100, 20
	r := newRig(t, cpms.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	fleet := loadgen.Start(ctx, loadgen.Options{
		URL: r.proxyURL, N: chargers, Messages: messages, Interval: time.Second, Log: quiet(),
	})
	res := fleet.Wait()
	defer fleet.Close()

	if res.Booted != chargers || len(res.Errors) > 0 {
		t.Fatalf("booted %d of %d, errors: %v", res.Booted, chargers, res.Errors)
	}
	if want := chargers * (2 + messages); len(res.Latencies) != want {
		t.Errorf("%d calls answered, want %d", len(res.Latencies), want)
	}
	l := res.Latencies
	slices.Sort(l)
	t.Logf("load: %d chargers, %d calls, round trip p50 %s p99 %s max %s (informational; SP1 owns the target)",
		chargers, len(l), l[len(l)/2], l[len(l)*99/100], l[len(l)-1])

	assertIdentical(t, fleet.Frames(), r.cpms.Frames(), chargers*(2+messages)*2)
}

// --- the check itself -------------------------------------------------------

// A proxy that changes one byte must fail the identity check; otherwise a
// green suite proves nothing.
func TestFrameCheckCatchesAlteration(t *testing.T) {
	t.Parallel()

	c := cpms.New(cpms.Options{Log: quiet()})
	cpmsSrv := httptest.NewServer(c)
	t.Cleanup(func() { c.Close(); cpmsSrv.Close() })
	mutating := httptest.NewServer(mutatingProxy(wsURL(cpmsSrv)))
	t.Cleanup(mutating.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fleet := loadgen.Start(ctx, loadgen.Options{URL: wsURL(mutating), N: 1, Messages: 1, Interval: 10 * time.Millisecond, Log: quiet()})
	res := fleet.Wait()
	defer fleet.Close()
	if res.Booted != 1 {
		t.Fatalf("mutating proxy broke the exchange itself: %v", res.Errors)
	}

	time.Sleep(200 * time.Millisecond)
	diffs := frames.Compare(fleet.Frames().Frames(), c.Frames().Frames())
	if len(diffs) == 0 {
		t.Fatal("a proxy that alters frames passed the identity check")
	}
	t.Logf("altering proxy caught: %d diffs, first: %v", len(diffs), diffs[0])
}

// mutatingProxy forwards like the placeholder but appends a space to every
// charger-to-CPMS frame: still valid JSON, no longer byte-identical.
func mutatingProxy(upstream string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up, resp, err := (&websocket.Dialer{Subprotocols: websocket.Subprotocols(r)}).Dial(upstream+r.URL.Path, nil)
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer up.Close()
		down, err := (&websocket.Upgrader{Subprotocols: []string{up.Subprotocol()}}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer down.Close()
		done := make(chan struct{}, 2)
		go func() {
			defer func() { done <- struct{}{} }()
			for {
				typ, data, err := down.ReadMessage()
				if err != nil || up.WriteMessage(typ, append(data, ' ')) != nil {
					return
				}
			}
		}()
		go func() {
			defer func() { done <- struct{}{} }()
			for {
				typ, data, err := up.ReadMessage()
				if err != nil || down.WriteMessage(typ, data) != nil {
					return
				}
			}
		}()
		<-done
	})
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
