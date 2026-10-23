package events

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// ids makes event IDs time-ordered within this process, even when many share
// a millisecond: RFC 9562 §6.2 method 1, a 12-bit counter in rand_a.
var ids struct {
	sync.Mutex
	ms  int64  // unix milliseconds of the last ID
	seq uint16 // counter within ms, 12 bits
}

// NewID returns a new UUIDv7 (RFC 9562) as a lowercase string. IDs from one
// process sort, as strings, in the order NewID returned them.
func NewID() string {
	var b [16]byte
	rand.Read(b[:]) // never fails (crypto/rand, Go 1.24)

	now := time.Now().UnixMilli()
	ids.Lock()
	switch {
	case now > ids.ms:
		ids.ms = now
		// A random start makes IDs harder to guess; its top bit is clear so
		// a burst has at least 2048 steps before the counter rolls over.
		ids.seq = uint16(b[6])<<8&0x0700 | uint16(b[7])
	case ids.seq < 0x0fff:
		// Same millisecond, or the clock stepped back: count on.
		ids.seq++
	default:
		// Counter exhausted: borrow the next millisecond.
		ids.ms++
		ids.seq = 0
	}
	ms, seq := ids.ms, ids.seq
	ids.Unlock()

	for i := range 6 {
		b[i] = byte(ms >> (40 - 8*i))
	}
	b[6] = 0x70 | byte(seq>>8) // version 7
	b[7] = byte(seq)
	b[8] = 0x80 | b[8]&0x3f // variant 10

	var s [36]byte
	hex.Encode(s[0:8], b[0:4])
	s[8] = '-'
	hex.Encode(s[9:13], b[4:6])
	s[13] = '-'
	hex.Encode(s[14:18], b[6:8])
	s[18] = '-'
	hex.Encode(s[19:23], b[8:10])
	s[23] = '-'
	hex.Encode(s[24:], b[10:])
	return string(s[:])
}
