package store

import (
	"crypto/rand"
	"encoding/binary"
	"sync"
	"time"
)

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var (
	idMu       sync.Mutex
	lastMillis uint64
	lastRand   [10]byte
)

// NewID returns a 26-char lexicographically sortable id (ULID layout: 48-bit
// millisecond timestamp + 80 bits of randomness, Crockford base32).
//
// Ids minted in the same millisecond increment the random component rather
// than re-randomizing it, so ordering by id is stable even for a burst of
// writes. `store list` breaks updated_at ties by id, so without this two
// records written in the same millisecond could swap places between pages.
//
// Requirement R3: callers may mint ids themselves and pass them to Put; this
// is only the default when they don't.
func NewID() string {
	idMu.Lock()
	ms := uint64(time.Now().UTC().UnixMilli())
	if ms == lastMillis {
		// Increment the 80-bit random field as a big-endian integer.
		for i := len(lastRand) - 1; i >= 0; i-- {
			lastRand[i]++
			if lastRand[i] != 0 {
				break
			}
		}
	} else {
		lastMillis = ms
		_, _ = rand.Read(lastRand[:])
		// Leave headroom so a long burst inside one millisecond cannot wrap
		// past the timestamp boundary.
		lastRand[0] &= 0x7f
	}
	var b [16]byte
	var t [8]byte
	binary.BigEndian.PutUint64(t[:], ms)
	copy(b[0:6], t[2:8])
	copy(b[6:], lastRand[:])
	idMu.Unlock()

	return encodeCrockford(b)
}

func encodeCrockford(b [16]byte) string {
	out := make([]byte, 26)
	var acc uint32
	var bits uint
	pos := 25
	for i := 15; i >= 0; i-- {
		acc |= uint32(b[i]) << bits
		bits += 8
		for bits >= 5 {
			out[pos] = crockford[acc&31]
			pos--
			acc >>= 5
			bits -= 5
		}
	}
	if pos >= 0 {
		out[pos] = crockford[acc&31]
	}
	return string(out)
}
