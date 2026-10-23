//go:build e2e

package e2e

import (
	"context"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wolffseb/ocpp-tap/harness/cpms"
	"github.com/wolffseb/ocpp-tap/harness/frames"
	"github.com/wolffseb/ocpp-tap/harness/loadgen"
	"github.com/wolffseb/ocpp-tap/internal/forwarder"
	"github.com/wolffseb/ocpp-tap/internal/tap"
)

// SP2 isolation targets, from docs/PRD.md. Changing them needs an ADR.
const (
	tapBlockedFor = 60 * time.Second
	// latencyTolerance: with the tap blocked, forwarder latency stays within
	// 10% of baseline.
	latencyTolerance = 0.10
	// baselineFor is how long the healthy-tap baseline runs (issue #7:
	// at least 20 s, so its p99 is stable).
	baselineFor = 20 * time.Second
)

// tapRig is an in-process chain: harness CPMS ← forwarder (+ tap) ← chargers.
// It runs the forwarder in this process, unlike newRig, because the tests
// need to block and break the tap and read its counters.
type tapRig struct {
	cpms *cpms.Server
	fwd  *forwarder.Forwarder
	tap  *tap.Tap
	url  string
}

// chargerTable is the forwarder's charger configuration for the rig.
type chargerTable map[string]forwarder.Charger

func (c chargerTable) Charger(id string) (forwarder.Charger, bool) {
	ch, ok := c[id]
	return ch, ok
}

func newTapRig(t *testing.T, handler func(forwarder.Frame), ids ...string) *tapRig {
	t.Helper()
	c := cpms.New(cpms.Options{Log: quiet()})
	cpmsSrv := httptest.NewServer(c)
	t.Cleanup(func() { c.Close(); cpmsSrv.Close() })

	table := chargerTable{}
	for _, id := range ids {
		table[id] = forwarder.Charger{ID: id, Password: password(id), UpstreamURL: wsURL(cpmsSrv) + "/ocpp/" + id}
	}
	tp := tap.New(tap.Options{Handler: handler, Log: quiet()})
	fwd := forwarder.New(forwarder.Options{Chargers: table, Sink: tp, Log: quiet()})
	fwdSrv := httptest.NewServer(fwd)
	// Cleanups run last-first: forwarder, then tap, as in cmd/octap.
	t.Cleanup(tp.Close)
	t.Cleanup(func() { fwd.Close(); fwdSrv.Close() })
	return &tapRig{cpms: c, fwd: fwd, tap: tp, url: wsURL(fwdSrv)}
}

// requireAllConnected checks that every charger is still on its first
// connection on both sides of the forwarder.
func (r *tapRig) requireAllConnected(t *testing.T, ids []string) {
	t.Helper()
	if n := r.fwd.Active(); n != len(ids) {
		t.Errorf("forwarder has %d chargers connected, want %d", n, len(ids))
	}
	for _, id := range ids {
		if !r.cpms.Connected(id) || r.cpms.Connections(id) != 1 {
			t.Errorf("%s: connected to CPMS %v, %d connections; want still on its first", id, r.cpms.Connected(id), r.cpms.Connections(id))
		}
	}
}

func fleetIDs(prefix string, n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = loadgen.ChargerID(prefix, i)
	}
	return ids
}

// gate blocks the tap handler while closed over a channel.
type gate struct{ ch atomic.Pointer[chan struct{}] }

func (g *gate) block() { ch := make(chan struct{}); g.ch.Store(&ch) }
func (g *gate) release() {
	if ch := g.ch.Swap(nil); ch != nil {
		close(*ch)
	}
}
func (g *gate) wait() {
	if ch := g.ch.Load(); ch != nil {
		<-*ch
	}
}

// SP2 isolation: "with the tap blocked for 60 s, forwarder latency stays
// within 10% of baseline and a tap_events_dropped counter increases."
//
// One fleet of 100 chargers × 1 msg/s runs through one rig. A healthy-tap
// baseline phase is followed by a phase with the tap handler blocked for
// 60 s. The p99 of one-way latency (as in TestLoad) is compared between the
// two, by the time each frame was sent.
//
// Not parallel: Go runs it before every parallel test, so no other test's
// CPU load distorts either phase, and it does not distort TestLoad.
func TestTapBlockedKeepsForwarderLatency(t *testing.T) {
	const chargers = 100
	const warmup = 3 * time.Second // dials spread over 1 s, then boot and status
	// Enough MeterValues for warmup + baseline + blocked phase, with margin.
	messages := int((warmup+baselineFor+tapBlockedFor)/time.Second) + 3

	ids := fleetIDs("TAPB", chargers)
	var g gate
	var handled atomic.Uint64
	r := newTapRig(t, func(forwarder.Frame) { g.wait(); handled.Add(1) }, ids...)
	t.Cleanup(g.release) // never leave the handler blocked behind a failure

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fleet := loadgen.Start(ctx, loadgen.Options{
		URL: r.url, N: chargers, IDPrefix: "TAPB", Messages: messages, Interval: time.Second,
		Password: password, Log: quiet(),
	})
	defer fleet.Close()

	time.Sleep(warmup)
	baselineStart := time.Now()
	time.Sleep(baselineFor)

	blockedStart := time.Now()
	droppedBefore := r.tap.Dropped()
	g.block()
	time.Sleep(tapBlockedFor)
	blockedEnd := time.Now()
	droppedBlocked := r.tap.Dropped()
	// Every charger is still connected at the end of the blocked phase.
	r.requireAllConnected(t, ids)
	g.release()

	res := fleet.Wait()
	if res.Booted != chargers || len(res.Errors) > 0 {
		t.Fatalf("booted %d of %d, errors: %v", res.Booted, chargers, res.Errors)
	}
	assertIdentical(t, fleet.Frames(), r.cpms.Frames(), chargers*(2+messages)*2)
	r.requireAllConnected(t, ids)

	if droppedBefore != 0 {
		t.Errorf("tap dropped %d frames with a healthy handler", droppedBefore)
	}
	t.Logf("tap_events_dropped: %d before blocking, %d after %s blocked; handled %d", droppedBefore, droppedBlocked, tapBlockedFor, handled.Load())
	if droppedBlocked <= droppedBefore {
		t.Errorf("tap_events_dropped did not increase while the tap was blocked (%d → %d)", droppedBefore, droppedBlocked)
	}

	base := latenciesSentBetween(t, fleet.Frames(), r.cpms.Frames(), baselineStart, blockedStart)
	blocked := latenciesSentBetween(t, fleet.Frames(), r.cpms.Frames(), blockedStart, blockedEnd)
	bp50, bp99, bmax := percentiles(base)
	kp50, kp99, kmax := percentiles(blocked)
	limit := time.Duration(float64(bp99) * (1 + latencyTolerance))
	t.Logf("baseline (healthy tap, %s): %d frames, p50 %s, p99 %s, max %s", blockedStart.Sub(baselineStart).Round(time.Second), len(base), bp50, bp99, bmax)
	t.Logf("tap blocked (%s): %d frames, p50 %s, p99 %s, max %s", blockedEnd.Sub(blockedStart).Round(time.Second), len(blocked), kp50, kp99, kmax)
	t.Logf("blocked p99 / baseline p99 = %.3f (limit %.2f, p99 ≤ %s)", float64(kp99)/float64(bp99), 1+latencyTolerance, limit)
	if kp99 > limit {
		t.Errorf("with the tap blocked, latency p99 %s is more than %.0f%% above the baseline's %s",
			kp99, latencyTolerance*100, bp99)
	}
}

// SP2: "A panic in the tap does not close any charger connection." The
// handler panics on every frame; every charger stays on its first
// connection and every frame is forwarded unchanged.
func TestTapPanicKeepsChargersConnected(t *testing.T) {
	t.Parallel()

	const chargers, messages = 100, 10
	ids := fleetIDs("TAPP", chargers)
	r := newTapRig(t, func(forwarder.Frame) { panic("tap bug") }, ids...)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fleet := loadgen.Start(ctx, loadgen.Options{
		URL: r.url, N: chargers, IDPrefix: "TAPP", Messages: messages, Interval: time.Second,
		Password: password, Log: quiet(),
	})
	defer fleet.Close()
	res := fleet.Wait()
	if res.Booted != chargers || len(res.Errors) > 0 {
		t.Fatalf("booted %d of %d, errors: %v", res.Booted, chargers, res.Errors)
	}
	assertIdentical(t, fleet.Frames(), r.cpms.Frames(), chargers*(2+messages)*2)

	// Every frame reached the tap (and panicked it), or was dropped: one
	// Connected per charger plus every forwarded message.
	offered := uint64(chargers + r.cpms.Frames().Len())
	deadline := time.Now().Add(5 * time.Second)
	for r.tap.Panics()+r.tap.Dropped() < offered && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("tap panicked %d times, dropped %d, of %d frames offered", r.tap.Panics(), r.tap.Dropped(), offered)
	if r.tap.Panics() == 0 || r.tap.Panics()+r.tap.Dropped() != offered {
		t.Errorf("tap panics %d + dropped %d, want %d offered", r.tap.Panics(), r.tap.Dropped(), offered)
	}
	r.requireAllConnected(t, ids)
}

// SP2: the tap "reads a copy of every frame … never writes back to any
// connection." A healthy tap sees exactly the frames the CPMS exchanged, byte
// for byte, plus one Connected notice per charger; and the charger and CPMS
// sides stay identical, so nothing was injected.
func TestTapSeesEveryFrame(t *testing.T) {
	t.Parallel()

	const chargers, messages = 10, 3
	ids := fleetIDs("TAPS", chargers)
	var seen frames.Log
	var connected atomic.Int64
	r := newTapRig(t, func(f forwarder.Frame) {
		switch f.Kind {
		case forwarder.Connected:
			connected.Add(1)
		case forwarder.Message:
			dir := frames.Upstream
			if f.Dir == forwarder.Downstream {
				dir = frames.Downstream
			}
			seen.Add(f.ChargerID, dir, f.Data)
		}
	}, ids...)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fleet := loadgen.Start(ctx, loadgen.Options{
		URL: r.url, N: chargers, IDPrefix: "TAPS", Messages: messages, Interval: 100 * time.Millisecond,
		Password: password, Log: quiet(),
	})
	defer fleet.Close()
	res := fleet.Wait()
	if res.Booted != chargers || len(res.Errors) > 0 {
		t.Fatalf("booted %d of %d, errors: %v", res.Booted, chargers, res.Errors)
	}
	assertIdentical(t, fleet.Frames(), r.cpms.Frames(), chargers*(2+messages)*2)
	// The tap side against the CPMS side, with the same check.
	assertIdentical(t, &seen, r.cpms.Frames(), chargers*(2+messages)*2)
	if n := connected.Load(); n != chargers {
		t.Errorf("tap saw %d Connected notices, want %d", n, chargers)
	}
	if n := r.tap.Dropped(); n != 0 {
		t.Errorf("tap dropped %d frames", n)
	}
}

// latenciesSentBetween pairs each frame with its counterpart on the other
// side, as oneWayLatencies does, and keeps those sent in [from, to): by the
// charger for upstream frames, by the CPMS for downstream ones.
func latenciesSentBetween(t *testing.T, chargerSide, cpmsSide *frames.Log, from, to time.Time) []time.Duration {
	t.Helper()
	type key struct {
		charger string
		dir     frames.Direction
	}
	group := func(l *frames.Log) map[key][]frames.Frame {
		m := map[key][]frames.Frame{}
		for _, f := range l.Frames() {
			k := key{f.Charger, f.Dir}
			m[k] = append(m[k], f)
		}
		return m
	}
	a, b := group(chargerSide), group(cpmsSide)
	var out []time.Duration
	for k, atCharger := range a {
		atCPMS := b[k]
		if len(atCPMS) != len(atCharger) {
			t.Fatalf("%v: %d vs %d frames", k, len(atCharger), len(atCPMS))
		}
		for i := range atCharger {
			sent, recv := atCharger[i].At, atCPMS[i].At
			if k.dir == frames.Downstream {
				sent, recv = recv, sent
			}
			if !sent.Before(from) && sent.Before(to) {
				out = append(out, recv.Sub(sent))
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("no frames sent between %s and %s", from.Format(time.TimeOnly), to.Format(time.TimeOnly))
	}
	return out
}

// percentiles returns p50, p99 and max, indexed as in TestLoad.
func percentiles(lat []time.Duration) (p50, p99, worst time.Duration) {
	s := slices.Clone(lat)
	slices.Sort(s)
	return s[len(s)/2], s[len(s)*99/100], s[len(s)-1]
}
