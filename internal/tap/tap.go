package tap

import (
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/wolffseb/ocpp-tap/internal/forwarder"
)

// DefaultCapacity is how many frames the tap buffers before it drops
// (ADR-0001 §3).
const DefaultCapacity = 4096

// Handler is given each frame the tap does not drop, one at a time, in the
// order offered. The frame's Data is shared with the forwarder and must not
// be modified.
type Handler func(forwarder.Frame)

// Options configure a Tap.
type Options struct {
	// Capacity is the buffer size in frames; 0 means DefaultCapacity.
	Capacity int
	// Handler receives the frames; nil discards them.
	Handler Handler
	Log     *slog.Logger
}

// Tap buffers frame copies from the forwarder and hands them to a handler
// in its own goroutine. Offer never blocks: when the buffer is full the
// frame is dropped and counted. A panicking handler is recovered and
// counted, and the next frame is handled as usual.
type Tap struct {
	frames  chan forwarder.Frame
	handler Handler
	log     *slog.Logger

	dropped atomic.Uint64
	panics  atomic.Uint64

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// New starts a Tap.
func New(opts Options) *Tap {
	if opts.Capacity <= 0 {
		opts.Capacity = DefaultCapacity
	}
	if opts.Handler == nil {
		opts.Handler = func(forwarder.Frame) {}
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	t := &Tap{
		frames:  make(chan forwarder.Frame, opts.Capacity),
		handler: opts.Handler,
		log:     opts.Log,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go t.consume()
	return t
}

// Offer implements forwarder.FrameSink. It costs the forwarder one select,
// never a wait.
func (t *Tap) Offer(f forwarder.Frame) {
	select {
	case t.frames <- f:
	default:
		t.dropped.Add(1)
	}
}

// Dropped is how many frames were dropped because the buffer was full: the
// tap_events_dropped counter.
func (t *Tap) Dropped() uint64 { return t.dropped.Load() }

// Panics is how many times the handler panicked.
func (t *Tap) Panics() uint64 { return t.panics.Load() }

// Close hands the frames already buffered to the handler, then stops the
// consumer and waits for it. Frames offered afterwards are dropped. Close
// waits for the handler, so a handler that never returns blocks it.
func (t *Tap) Close() {
	t.closeOnce.Do(func() { close(t.stop) })
	<-t.done
}

func (t *Tap) consume() {
	defer close(t.done)
	for {
		select {
		case f := <-t.frames:
			t.handle(f)
		case <-t.stop:
			// Drain only what is buffered now, so a sink that keeps offering
			// cannot hold Close open.
			for n := len(t.frames); n > 0; n-- {
				t.handle(<-t.frames)
			}
			return
		}
	}
}

// handle runs the handler on one frame, containing a panic.
func (t *Tap) handle(f forwarder.Frame) {
	defer func() {
		if r := recover(); r != nil {
			t.panics.Add(1)
			// Never the payload: frames can hold idTags.
			t.log.Error("tap handler panicked", "charge_point", f.ChargerID, "kind", f.Kind, "panic", r)
		}
	}()
	t.handler(f)
}
