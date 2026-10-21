// Package frames records OCPP-J frames as raw bytes and compares two
// recordings byte for byte. It is the core of the SP0 frame-identity check:
// what the charger sent must be exactly what the CPMS received, and the
// other way round.
//
// Identity is checked at the WebSocket message-payload level (after
// defragmentation), which is where OCPP-J frames exist.
package frames

import (
	"bytes"
	"fmt"
	"sync"
	"time"
)

// Direction is which way a frame travelled, independent of who recorded it.
type Direction int

// The two directions on a charger's connection.
const (
	// Upstream is charger to CPMS.
	Upstream Direction = iota
	// Downstream is CPMS to charger.
	Downstream
)

// String names the direction.
func (d Direction) String() string {
	if d == Upstream {
		return "upstream"
	}
	return "downstream"
}

// Frame is one recorded WebSocket message.
type Frame struct {
	Charger string
	Dir     Direction
	At      time.Time
	Data    []byte
}

// Log is a concurrency-safe, append-only recording.
type Log struct {
	mu     sync.Mutex
	frames []Frame
}

// Add records a copy of data.
func (l *Log) Add(charger string, dir Direction, data []byte) {
	fr := Frame{Charger: charger, Dir: dir, At: time.Now(), Data: bytes.Clone(data)}
	l.mu.Lock()
	l.frames = append(l.frames, fr)
	l.mu.Unlock()
}

// Frames returns everything recorded so far, in recording order.
func (l *Log) Frames() []Frame {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Frame(nil), l.frames...)
}

// For returns the frames of one charger and direction, in order.
func (l *Log) For(charger string, dir Direction) []Frame {
	return filter(l.Frames(), charger, dir)
}

// Len is how many frames have been recorded.
func (l *Log) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.frames)
}

func filter(all []Frame, charger string, dir Direction) []Frame {
	var out []Frame
	for _, fr := range all {
		if fr.Charger == charger && fr.Dir == dir {
			out = append(out, fr)
		}
	}
	return out
}

// Diff is one position where the two recordings disagree.
type Diff struct {
	Charger string
	Dir     Direction
	// Index is the position within this charger and direction.
	Index int
	// Sent is what the sending side recorded; nil if it sent nothing here.
	Sent []byte
	// Received is what the receiving side recorded; nil if nothing arrived.
	Received []byte
}

// String says where the recordings disagree and shows both sides.
func (d Diff) String() string {
	show := func(b []byte) string {
		if b == nil {
			return "<missing>"
		}
		const limit = 300
		if len(b) > limit {
			return fmt.Sprintf("%q…(%d bytes)", b[:limit], len(b))
		}
		return fmt.Sprintf("%q", b)
	}
	return fmt.Sprintf("charger %s %s #%d: sent %s, received %s",
		d.Charger, d.Dir, d.Index, show(d.Sent), show(d.Received))
}

// Compare checks a charger-side recording against a CPMS-side recording.
//
// Upstream frames the charger side recorded must equal, in order, the
// upstream frames the CPMS side recorded; downstream frames the CPMS side
// recorded must equal, in order, the downstream frames the charger side
// recorded. Each is compared per charger, since frames of different chargers
// and directions may interleave differently on the two sides.
func Compare(chargerSide, cpmsSide []Frame) []Diff {
	type key struct {
		charger string
		dir     Direction
	}
	seen := map[key]bool{}
	var keys []key
	for _, all := range [][]Frame{chargerSide, cpmsSide} {
		for _, fr := range all {
			k := key{fr.Charger, fr.Dir}
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}

	var diffs []Diff
	for _, k := range keys {
		sent := filter(chargerSide, k.charger, k.dir)
		recv := filter(cpmsSide, k.charger, k.dir)
		if k.dir == Downstream {
			sent, recv = recv, sent
		}
		for i := 0; i < max(len(sent), len(recv)); i++ {
			var s, r []byte
			if i < len(sent) {
				s = sent[i].Data
			}
			if i < len(recv) {
				r = recv[i].Data
			}
			if s == nil || r == nil || !bytes.Equal(s, r) {
				diffs = append(diffs, Diff{Charger: k.charger, Dir: k.dir, Index: i, Sent: s, Received: r})
			}
		}
	}
	return diffs
}
