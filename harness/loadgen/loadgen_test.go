package loadgen

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wolffseb/ocpp-tap/harness/cpms"
	"github.com/wolffseb/ocpp-tap/harness/frames"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func harnessCPMS(t *testing.T, opts cpms.Options) (*cpms.Server, string) {
	t.Helper()
	opts.Log = quiet()
	s := cpms.New(opts)
	srv := httptest.NewServer(s)
	t.Cleanup(func() { s.Close(); srv.Close() })
	return s, "ws" + strings.TrimPrefix(srv.URL, "http")
}

// N chargers in one process boot, report status, send their messages, and
// every frame they recorded matches what the CPMS recorded.
func TestFleetRunsNChargersInOneProcess(t *testing.T) {
	t.Parallel()

	s, url := harnessCPMS(t, cpms.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const n, messages = 20, 5
	fleet := Start(ctx, Options{URL: url, N: n, Messages: messages, Interval: 20 * time.Millisecond, Log: quiet()})
	res := fleet.Wait()
	defer fleet.Close()

	if res.Booted != n {
		t.Errorf("booted %d, want %d (errors: %v)", res.Booted, n, res.Errors)
	}
	if len(res.Errors) != 0 {
		t.Errorf("errors: %v", res.Errors)
	}
	// Boot + status + messages per charger.
	if want := n * (2 + messages); len(res.Latencies) != want {
		t.Errorf("%d calls answered, want %d", len(res.Latencies), want)
	}
	for i := 0; i < n; i++ {
		id := ChargerID("LOAD", i)
		if got := s.CallCount(id, "MeterValues"); got != messages {
			t.Errorf("%s: CPMS got %d MeterValues, want %d", id, got, messages)
		}
	}
	if diffs := frames.Compare(fleet.Frames().Frames(), s.Frames().Frames()); len(diffs) != 0 {
		t.Fatalf("%d diffs, first: %v", len(diffs), diffs[0])
	}
}

func TestFleetAnswersCSMSCalls(t *testing.T) {
	t.Parallel()

	s, url := harnessCPMS(t, cpms.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fleet := Start(ctx, Options{URL: url, N: 1, Messages: 50, Interval: 20 * time.Millisecond, Log: quiet()})
	defer fleet.Close()
	id := ChargerID("LOAD", 0)
	if err := s.WaitCalls(ctx, id, "BootNotification", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DataTransfer(ctx, id, "acme", "ping", ""); err != nil {
		t.Fatalf("DataTransfer: %v", err)
	}
}

func TestFleetReportsRejectedBoot(t *testing.T) {
	t.Parallel()

	_, url := harnessCPMS(t, cpms.Options{Mode: cpms.ModeRejectBoot})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fleet := Start(ctx, Options{URL: url, N: 3, Messages: 1, Interval: 10 * time.Millisecond, Log: quiet()})
	res := fleet.Wait()
	fleet.Close()
	if res.Booted != 0 || len(res.Errors) != 3 {
		t.Errorf("booted %d, errors %v; want 0 booted, 3 errors", res.Booted, res.Errors)
	}
}

func TestChargerID(t *testing.T) {
	t.Parallel()
	if got := ChargerID("LOAD", 7); got != "LOAD-0007" {
		t.Errorf("ChargerID = %q", got)
	}
}
