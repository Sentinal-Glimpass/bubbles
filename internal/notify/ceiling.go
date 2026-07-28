package notify

import (
	"sync"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

// Defaults for the flood ceiling. These are the regression gate for
// 632fe95, which reverted prior nudge work after a single already-delivered
// event was re-emitted 100-178 times fleet-wide: no matter what any rule,
// capability, or configuration decides above this point, a bubble can never
// receive more than DefaultCeilingBurst notices before the rate above
// throttles back to DefaultCeilingPerMinute.
const (
	DefaultCeilingPerMinute = 6.0
	DefaultCeilingBurst     = 6
)

// bucket is one address's token-bucket state.
type bucket struct {
	tokens float64
	last   time.Time
}

// Ceiling is a hard, per-bubble notification rate limiter. It sits below the
// policy engine (mute rules, capabilities, etc.) and cannot be disabled by
// any of them -- it is the last line of defense against a flood like
// 632fe95, so it deliberately has no bypass and no "unlimited" mode.
type Ceiling struct {
	mu    sync.Mutex
	rate  float64 // tokens refilled per second
	burst int
	b     map[addr.Address]*bucket
}

// NewCeiling returns a Ceiling that allows burst notices immediately per
// address, refilling at perMinute/60 tokens per second thereafter.
func NewCeiling(perMinute float64, burst int) *Ceiling {
	return &Ceiling{
		rate:  perMinute / 60,
		burst: burst,
		b:     map[addr.Address]*bucket{},
	}
}

// Allow reports whether a notice to a is permitted at now, spending one
// token if so. now is passed explicitly (never time.Now internally) so
// callers and tests can drive time deterministically. A cold-start address
// begins with a full bucket, mirroring the token-bucket in
// cmd/bubbles/webhook.go's rateLimiter, but scoped per-bubble-address
// instead of per-webhook-token.
func (c *Ceiling) Allow(a addr.Address, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	bk := c.b[a]
	if bk == nil {
		c.b[a] = &bucket{tokens: float64(c.burst) - 1, last: now}
		return true
	}

	bk.tokens += now.Sub(bk.last).Seconds() * c.rate
	if bk.tokens > float64(c.burst) {
		bk.tokens = float64(c.burst)
	}
	bk.last = now

	if bk.tokens < 1 {
		return false
	}
	bk.tokens--
	return true
}
