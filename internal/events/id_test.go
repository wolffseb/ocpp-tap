package events

import (
	"encoding/hex"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var uuidV7 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewIDIsUUIDv7(t *testing.T) {
	t.Parallel()

	before := time.Now().UnixMilli()
	id := NewID()
	after := time.Now().UnixMilli()
	if !uuidV7.MatchString(id) {
		t.Fatalf("NewID() = %q, not a lowercase UUIDv7", id)
	}
	b, err := hex.DecodeString(strings.ReplaceAll(id, "-", "")[:12])
	if err != nil {
		t.Fatal(err)
	}
	var ms int64
	for _, x := range b {
		ms = ms<<8 | int64(x)
	}
	// The counter may borrow a millisecond or two from the future when
	// thousands of IDs share one; a single call doesn't.
	if ms < before || ms > after+1 {
		t.Errorf("timestamp %d not in [%d, %d]", ms, before, after)
	}
}

// IDs are time-ordered: as strings, each is greater than the one before,
// even many within one millisecond.
func TestNewIDMonotonic(t *testing.T) {
	t.Parallel()

	prev := NewID()
	for range 100_000 {
		id := NewID()
		if id <= prev {
			t.Fatalf("NewID() = %s after %s, not increasing", id, prev)
		}
		prev = id
	}
}

func TestNewIDConcurrentUnique(t *testing.T) {
	t.Parallel()

	const workers, each = 8, 5000
	var (
		mu   sync.Mutex
		seen = map[string]bool{}
		wg   sync.WaitGroup
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids := make([]string, each)
			for i := range ids {
				ids[i] = NewID()
			}
			mu.Lock()
			defer mu.Unlock()
			for _, id := range ids {
				if seen[id] {
					t.Errorf("duplicate id %s", id)
				}
				seen[id] = true
			}
		}()
	}
	wg.Wait()
}

func TestConstructorsSetEnvelope(t *testing.T) {
	t.Parallel()

	for typ, ev := range minimal() {
		if ev.Type != typ || ev.Data == nil {
			t.Errorf("%s: Type %q, Data %v", typ, ev.Type, ev.Data)
		}
		if !uuidV7.MatchString(ev.ID) {
			t.Errorf("%s: id %q is not a UUIDv7", typ, ev.ID)
		}
		if ev.ChargerID != "CP1" || !ev.ReceivedAt.Equal(received) {
			t.Errorf("%s: charger %q, received %v", typ, ev.ChargerID, ev.ReceivedAt)
		}
	}
	for _, typ := range []Type{TypeChargerConnected, TypeChargerDisconnected, TypeChargerBooted} {
		ev := minimal()[typ]
		if ev.ConnectorID == nil || *ev.ConnectorID != 0 || ev.Timestamp != nil {
			t.Errorf("%s: connector %v, timestamp %v; want 0 and nil", typ, ev.ConnectorID, ev.Timestamp)
		}
	}
	if ev := minimal()[TypeTransactionStopped]; ev.ConnectorID != nil {
		t.Errorf("transaction.stopped with unknown connector: connector %d, want nil", *ev.ConnectorID)
	}
}
