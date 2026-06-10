// Package ulid implements time-ordered, lexicographically-sortable 128-bit IDs
// (48-bit millisecond timestamp + 80-bit randomness, Crockford base32, 26
// chars). Used for audit record IDs so the on-disk chain is naturally
// time-ordered. Self-implemented to keep the dependency footprint minimal.
package ulid

import (
	"crypto/rand"
	"sync"
	"time"
)

const encoding = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // Crockford base32

// Generator produces monotonic ULIDs: within the same millisecond, the random
// component strictly increases so IDs remain sortable and unique.
type Generator struct {
	mu       sync.Mutex
	lastMS   int64
	lastRand [10]byte
}

func NewGenerator() *Generator { return &Generator{} }

var defaultGen = NewGenerator()

// New returns a ULID for the current time.
func New() string { return defaultGen.NewAt(time.Now()) }

// NewAt returns a ULID for t (default generator).
func NewAt(t time.Time) string { return defaultGen.NewAt(t) }

// NewAt returns a ULID for t, monotonic within a millisecond.
func (g *Generator) NewAt(t time.Time) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	ms := t.UnixMilli()
	var r [10]byte
	if ms == g.lastMS {
		r = g.lastRand
		for i := len(r) - 1; i >= 0; i-- {
			r[i]++
			if r[i] != 0 {
				break
			}
		}
	} else {
		if _, err := rand.Read(r[:]); err != nil {
			panic("ulid: crypto/rand failed: " + err.Error())
		}
		g.lastMS = ms
	}
	g.lastRand = r
	return encode(ms, r)
}

// encode packs the 48-bit ms + 80-bit random into 26 Crockford base32 chars.
func encode(ms int64, r [10]byte) string {
	var b [16]byte // 128 bits
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	copy(b[6:], r[:])

	out := make([]byte, 26)
	out[0] = encoding[(b[0]&224)>>5]
	out[1] = encoding[b[0]&31]
	out[2] = encoding[(b[1]&248)>>3]
	out[3] = encoding[((b[1]&7)<<2)|((b[2]&192)>>6)]
	out[4] = encoding[(b[2]&62)>>1]
	out[5] = encoding[((b[2]&1)<<4)|((b[3]&240)>>4)]
	out[6] = encoding[((b[3]&15)<<1)|((b[4]&128)>>7)]
	out[7] = encoding[(b[4]&124)>>2]
	out[8] = encoding[((b[4]&3)<<3)|((b[5]&224)>>5)]
	out[9] = encoding[b[5]&31]
	out[10] = encoding[(b[6]&248)>>3]
	out[11] = encoding[((b[6]&7)<<2)|((b[7]&192)>>6)]
	out[12] = encoding[(b[7]&62)>>1]
	out[13] = encoding[((b[7]&1)<<4)|((b[8]&240)>>4)]
	out[14] = encoding[((b[8]&15)<<1)|((b[9]&128)>>7)]
	out[15] = encoding[(b[9]&124)>>2]
	out[16] = encoding[((b[9]&3)<<3)|((b[10]&224)>>5)]
	out[17] = encoding[b[10]&31]
	out[18] = encoding[(b[11]&248)>>3]
	out[19] = encoding[((b[11]&7)<<2)|((b[12]&192)>>6)]
	out[20] = encoding[(b[12]&62)>>1]
	out[21] = encoding[((b[12]&1)<<4)|((b[13]&240)>>4)]
	out[22] = encoding[((b[13]&15)<<1)|((b[14]&128)>>7)]
	out[23] = encoding[(b[14]&124)>>2]
	out[24] = encoding[((b[14]&3)<<3)|((b[15]&224)>>5)]
	out[25] = encoding[b[15]&31]
	return string(out)
}
