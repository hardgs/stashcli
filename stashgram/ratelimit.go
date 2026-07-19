package stashgram

import (
	"sync"
	"time"
)

// RateLimiter is a simple token-bucket bandwidth throttle, in bytes/sec.
// It's intentionally chunk-grained rather than byte-grained: callers ask
// for a whole chunk's worth of budget at once (WaitN(chunkSize)) before
// sending/after receiving it, rather than hooking every low-level socket
// write. That's a good match for how uploads/downloads already move data
// here (one whole chunk per Telegram message), and needs no changes to the
// underlying gogram calls.
//
// A nil *RateLimiter is always unlimited — every method is nil-safe — so
// "no limit configured" just means "don't build one" (see NewRateLimiter),
// with zero overhead on the hot path.
type RateLimiter struct {
	mu         sync.Mutex
	ratePerSec float64
	burst      float64 // max bucket size, in bytes (~1 second worth)
	tokens     float64
	last       time.Time
}

// NewRateLimiter builds a limiter capped at bytesPerSec. bytesPerSec<=0
// means "unlimited", represented as a nil *RateLimiter.
func NewRateLimiter(bytesPerSec int64) *RateLimiter {
	if bytesPerSec <= 0 {
		return nil
	}
	rate := float64(bytesPerSec)
	return &RateLimiter{
		ratePerSec: rate,
		burst:      rate, // allow bursting up to ~1s worth so small files aren't needlessly slow
		tokens:     rate,
		last:       time.Now(),
	}
}

// WaitN blocks until n bytes' worth of bandwidth budget is available.
// Safe to call on a nil receiver (no-op — unlimited).
func (r *RateLimiter) WaitN(n int64) {
	if r == nil || n <= 0 {
		return
	}
	for {
		r.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(r.last).Seconds()
		r.last = now
		r.tokens += elapsed * r.ratePerSec
		if r.tokens > r.burst {
			r.tokens = r.burst
		}

		if r.tokens >= float64(n) {
			r.tokens -= float64(n)
			r.mu.Unlock()
			return
		}

		deficit := float64(n) - r.tokens
		wait := time.Duration(deficit / r.ratePerSec * float64(time.Second))
		r.mu.Unlock()

		// Sleep in bounded slices rather than one long sleep, so a
		// changed/cancelled context elsewhere in the process isn't stuck
		// behind an arbitrarily long single sleep.
		if wait > 250*time.Millisecond {
			wait = 250 * time.Millisecond
		}
		time.Sleep(wait)
	}
}
