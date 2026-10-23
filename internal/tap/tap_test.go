package tap

import (
	"bytes"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wolffseb/ocpp-tap/internal/forwarder"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func msg(data string) forwarder.Frame {
	return forwarder.Frame{Kind: forwarder.Message, ChargerID: "CP1", At: time.Now(), Data: []byte(data)}
}

// recorder is a handler that keeps what it was given.
type recorder struct {
	mu     sync.Mutex
	frames []forwarder.Frame
}

func (r *recorder) handle(f forwarder.Frame) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames = append(r.frames, f)
}

func (r *recorder) data() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.frames))
	for i, f := range r.frames {
		out[i] = string(f.Data)
	}
	return out
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 3s")
		}
		time.Sleep(time.Millisecond)
	}
}

// The tap is a forwarder.FrameSink.
var _ forwarder.FrameSink = (*Tap)(nil)

// SP2 "Reads a copy of every frame": with room in the buffer, the handler
// sees every offered frame, in order, unchanged.
func TestHandlerSeesEveryFrameInOrder(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	tp := New(Options{Handler: rec.handle, Log: quiet()})
	t.Cleanup(tp.Close)

	want := []string{"a", "b", "c", "d", "e"}
	for _, d := range want {
		tp.Offer(msg(d))
	}
	waitFor(t, func() bool { return len(rec.data()) == len(want) })
	if got := rec.data(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("handler saw %v, want %v", got, want)
	}
	if n := tp.Dropped(); n != 0 {
		t.Errorf("Dropped = %d with room in the buffer", n)
	}
}

func TestDefaultCapacity(t *testing.T) {
	t.Parallel()

	tp := New(Options{Log: quiet()})
	t.Cleanup(tp.Close)
	if got := cap(tp.frames); got != DefaultCapacity || DefaultCapacity != 4096 {
		t.Errorf("capacity %d (DefaultCapacity %d), want 4096 (ADR-0001 §3)", got, DefaultCapacity)
	}
}

// SP2 isolation, unit level: a blocked handler fills the buffer; Offer still
// returns at once and every frame that does not fit is counted as dropped.
func TestBlockedHandlerDropsAndCountsWithoutBlockingOffer(t *testing.T) {
	t.Parallel()

	const capacity, extra = 16, 1000
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	tp := New(Options{Capacity: capacity, Log: quiet(), Handler: func(forwarder.Frame) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	}})
	t.Cleanup(func() { close(release); tp.Close() })

	// The first frame is taken by the consumer, which then blocks in the
	// handler; the buffer then holds capacity frames and the rest drop.
	tp.Offer(msg("first"))
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("handler never called")
	}

	done := make(chan time.Duration, 1)
	go func() {
		var worst time.Duration
		for i := 0; i < capacity+extra; i++ {
			began := time.Now()
			tp.Offer(msg("x"))
			if d := time.Since(began); d > worst {
				worst = d
			}
		}
		done <- worst
	}()
	select {
	case worst := <-done:
		t.Logf("slowest Offer with a blocked handler: %s", worst)
		// A blocking Offer would wait for the handler, which never returns
		// here; anything near this bound is a wait, not a failed select.
		if worst > 100*time.Millisecond {
			t.Errorf("an Offer took %s with the handler blocked", worst)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Offer blocked while the handler was blocked")
	}
	if n := tp.Dropped(); n != extra {
		t.Errorf("Dropped = %d, want %d", n, extra)
	}
}

// SP2 "A panic in the tap does not close any charger connection", unit
// level: a panicking handler is recovered and counted, and the frames after
// it are still handled.
func TestPanickingHandlerIsContained(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	var logs syncBuffer
	tp := New(Options{Log: slog.New(slog.NewTextHandler(&logs, nil)), Handler: func(f forwarder.Frame) {
		if string(f.Data) == "boom" {
			panic("decoder bug")
		}
		rec.handle(f)
	}})
	t.Cleanup(tp.Close)

	for _, d := range []string{"a", "boom", "b", "boom", "c"} {
		tp.Offer(msg(d))
	}
	waitFor(t, func() bool { return len(rec.data()) == 3 })
	if got := strings.Join(rec.data(), ","); got != "a,b,c" {
		t.Errorf("handler saw %s, want a,b,c", got)
	}
	if n := tp.Panics(); n != 2 {
		t.Errorf("Panics = %d, want 2", n)
	}
	waitFor(t, func() bool { return strings.Count(logs.String(), "tap handler panicked") == 2 })
	if s := logs.String(); !strings.Contains(s, "decoder bug") {
		t.Errorf("panic log does not name the panic: %s", s)
	}
}

// The panic log names the frame but never carries its payload, which can
// hold idTags.
func TestPanicLogHasNoPayload(t *testing.T) {
	t.Parallel()

	var logs syncBuffer
	tp := New(Options{Log: slog.New(slog.NewTextHandler(&logs, nil)), Handler: func(forwarder.Frame) { panic("bug") }})
	t.Cleanup(tp.Close)
	tp.Offer(msg(`[2,"1","Authorize",{"idTag":"SECRET-TAG"}]`))
	waitFor(t, func() bool { return tp.Panics() == 1 })
	waitFor(t, func() bool { return strings.Contains(logs.String(), "tap handler panicked") })
	if s := logs.String(); strings.Contains(s, "SECRET-TAG") || !strings.Contains(s, "charge_point=CP1") {
		t.Errorf("panic log: %s", s)
	}
}

func TestCloseStopsTheConsumer(t *testing.T) {
	t.Parallel()

	var handled atomic.Int64
	tp := New(Options{Capacity: 8, Log: quiet(), Handler: func(forwarder.Frame) { handled.Add(1) }})
	tp.Offer(msg("a"))
	waitFor(t, func() bool { return handled.Load() == 1 })

	closed := make(chan struct{})
	go func() { tp.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return")
	}
	tp.Close() // idempotent

	// After Close nothing is handled, and Offer still neither blocks nor
	// panics: the forwarder may outlive the tap during shutdown.
	for i := 0; i < 100; i++ {
		tp.Offer(msg("late"))
	}
	time.Sleep(20 * time.Millisecond)
	if n := handled.Load(); n != 1 {
		t.Errorf("handler called %d times, want 1 (nothing after Close)", n)
	}
	if n := tp.Dropped(); n != 100-8 {
		t.Errorf("Dropped = %d after Close, want %d", n, 100-8)
	}
}

// Close hands what is already buffered to the handler before it returns, so
// the disconnects the forwarder offered while shutting down are not lost.
func TestCloseHandlesWhatIsBuffered(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var handled atomic.Int64
	tp := New(Options{Capacity: 8, Log: quiet(), Handler: func(forwarder.Frame) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		handled.Add(1)
	}})
	tp.Offer(msg("first"))
	<-entered
	for i := 0; i < 5; i++ {
		tp.Offer(msg("buffered"))
	}
	closed := make(chan struct{})
	go func() { tp.Close(); close(closed) }()
	close(release)
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return")
	}
	if n := handled.Load(); n != 6 {
		t.Errorf("handled %d frames by Close, want 6", n)
	}
}

// "Never writes back to any connection": the tap package has no means to.
// It imports no networking package, so it cannot hold or write to a socket;
// it only ever sees forwarder.Frame values, which carry bytes, not
// connections.
func TestTapHasNoConnectionAccess(t *testing.T) {
	t.Parallel()

	out, err := exec.Command("go", "list", "-f", `{{join .Imports " "}}`, ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	forbidden := []string{
		"net", "net/http", "github.com/gorilla/websocket",
		"github.com/wolffseb/ocpp-tap/internal/ocpp/ocppj",
	}
	for _, imp := range strings.Fields(string(out)) {
		for _, f := range forbidden {
			if imp == f || strings.HasPrefix(imp, f+"/") {
				t.Errorf("tap imports %s", imp)
			}
		}
	}
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
