// Package loadgen runs N simulated OCPP 1.6-J chargers in one process.
//
// Each charger boots, reports connector status, and then sends MeterValues
// at a fixed interval, waiting for each answer. Every charger records the
// exact bytes it sends and receives, so the fleet's log can be compared with
// the harness CPMS's log by frames.Compare. The chargers answer any
// CSMS-initiated call with {"status":"Accepted"}.
//
// headless-csms's simulator runs one charger per process; this exists for the
// SP0 load mode and the SP1 performance smoke.
package loadgen

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/wolffseb/ocpp-tap/harness/frames"
	"github.com/wolffseb/ocpp-tap/internal/ocpp"
	"github.com/wolffseb/ocpp-tap/internal/ocpp/ocppj"
)

// Options configure a fleet.
type Options struct {
	// URL is the ws:// base URL; each charger dials URL/ocpp/{id}.
	URL string
	// N is how many chargers to run.
	N int
	// IDPrefix names the chargers {IDPrefix}-0000, -0001, …
	IDPrefix string
	// Messages is how many MeterValues each charger sends after booting.
	Messages int
	// Interval is the pause between a charger's MeterValues.
	Interval time.Duration
	// CallTimeout bounds each call.
	CallTimeout time.Duration
	Log         *slog.Logger
}

func (o *Options) applyDefaults() {
	if o.IDPrefix == "" {
		o.IDPrefix = "LOAD"
	}
	if o.Interval <= 0 {
		o.Interval = time.Second
	}
	if o.CallTimeout <= 0 {
		o.CallTimeout = 30 * time.Second
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
}

// ChargerID is the identity of charger i.
func ChargerID(prefix string, i int) string { return fmt.Sprintf("%s-%04d", prefix, i) }

// Result summarises a finished run.
type Result struct {
	// Booted is how many chargers got an Accepted BootNotification.
	Booted int
	// Errors holds one entry per charger that failed.
	Errors []error
	// Latencies is the round trip of every answered call.
	Latencies []time.Duration
}

// Fleet is a running set of chargers.
type Fleet struct {
	opts   Options
	frames frames.Log

	mu     sync.Mutex
	result Result
	conns  []*ocppj.Conn

	wg sync.WaitGroup
}

// Start dials every charger and runs them in the background. The dials are
// spread over one Interval so N chargers do not arrive in the same instant.
func Start(ctx context.Context, opts Options) *Fleet {
	opts.applyDefaults()
	f := &Fleet{opts: opts}
	for i := 0; i < opts.N; i++ {
		f.wg.Add(1)
		delay := time.Duration(0)
		if opts.N > 1 {
			delay = opts.Interval * time.Duration(i) / time.Duration(opts.N)
		}
		go func(id string) {
			defer f.wg.Done()
			if err := f.runCharger(ctx, id, delay); err != nil {
				f.mu.Lock()
				f.result.Errors = append(f.result.Errors, fmt.Errorf("%s: %w", id, err))
				f.mu.Unlock()
			}
		}(ChargerID(opts.IDPrefix, i))
	}
	return f
}

// Frames is the fleet's own log: Upstream is what the chargers sent,
// Downstream what they received.
func (f *Fleet) Frames() *frames.Log { return &f.frames }

// Wait blocks until every charger has sent its messages or failed. The
// chargers stay connected until Close, so every frame has settled by the
// time Wait returns.
func (f *Fleet) Wait() Result {
	f.wg.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.result
	r.Errors = append([]error(nil), r.Errors...)
	r.Latencies = append([]time.Duration(nil), r.Latencies...)
	return r
}

// Close disconnects every charger.
func (f *Fleet) Close() {
	f.wg.Wait()
	f.mu.Lock()
	conns := f.conns
	f.conns = nil
	f.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

func (f *Fleet) runCharger(ctx context.Context, id string, delay time.Duration) error {
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return ctx.Err()
	}

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second, Subprotocols: []string{ocpp.Version16.Subprotocol()}}
	ws, resp, err := dialer.DialContext(ctx, strings.TrimRight(f.opts.URL, "/")+"/ocpp/"+id, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	conn := ocppj.New(ws, ocppj.Options{
		ID:          id,
		Version:     ocpp.Version16,
		CallTimeout: f.opts.CallTimeout,
		IdleTimeout: 24 * time.Hour, // a charger does not give up on a quiet CPMS
		Log:         f.opts.Log,
		Observe: func(dir ocppj.Direction, data []byte) {
			if dir == ocppj.Outbound {
				f.frames.Add(id, frames.Upstream, data)
			} else {
				f.frames.Add(id, frames.Downstream, data)
			}
		},
	})
	f.mu.Lock()
	f.conns = append(f.conns, conn)
	f.mu.Unlock()
	go conn.Run(context.WithoutCancel(ctx), acceptAll{})

	call := func(action string, payload any) (json.RawMessage, error) {
		began := time.Now()
		res, err := conn.Call(ctx, action, payload)
		if err == nil {
			f.mu.Lock()
			f.result.Latencies = append(f.result.Latencies, time.Since(began))
			f.mu.Unlock()
		}
		return res, err
	}

	raw, err := call("BootNotification", map[string]any{"chargePointVendor": "ocpp-tap", "chargePointModel": "loadgen"})
	if err != nil {
		return fmt.Errorf("BootNotification: %w", err)
	}
	var boot struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &boot); err != nil || boot.Status != "Accepted" {
		return fmt.Errorf("BootNotification answered %s", raw)
	}
	f.mu.Lock()
	f.result.Booted++
	f.mu.Unlock()

	if _, err := call("StatusNotification", map[string]any{
		"connectorId": 1, "errorCode": "NoError", "status": "Charging",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return fmt.Errorf("StatusNotification: %w", err)
	}

	energy := 0
	for i := 0; i < f.opts.Messages; i++ {
		if i > 0 {
			select {
			case <-time.After(f.opts.Interval):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		energy += 37
		if _, err := call("MeterValues", meterValues(energy)); err != nil {
			return fmt.Errorf("MeterValues #%d: %w", i, err)
		}
	}
	return nil
}

func meterValues(wh int) map[string]any {
	return map[string]any{
		"connectorId": 1,
		"meterValue": []any{map[string]any{
			"timestamp": time.Now().UTC().Format(time.RFC3339),
			"sampledValue": []any{
				map[string]any{"value": fmt.Sprint(wh), "measurand": "Energy.Active.Import.Register", "unit": "Wh"},
				map[string]any{"value": "11000", "measurand": "Power.Active.Import", "unit": "W"},
			},
		}},
	}
}

// acceptAll answers every CSMS-initiated call with Accepted.
type acceptAll struct{}

// Version reports OCPP 1.6.
func (acceptAll) Version() ocpp.Version { return ocpp.Version16 }

// HandleCall accepts anything.
func (acceptAll) HandleCall(context.Context, string, string, json.RawMessage) (any, *ocpp.RPCError) {
	return map[string]any{"status": "Accepted"}, nil
}
