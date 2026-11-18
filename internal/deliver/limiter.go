package deliver

import (
	"context"
	"math"
	"sync"
	"time"
)

// Limiter is a token bucket: up to Burst requests at once, refilled at
// PerHour requests an hour. It bounds how fast this plugin calls muster, so an
// upstream storm queues in the outbox instead of flooding muster.
type Limiter struct {
	mu      sync.Mutex
	perHour int
	burst   float64
	tokens  float64
	last    time.Time
	now     func() time.Time
}

// NewLimiter returns a full bucket. now may be nil (time.Now).
func NewLimiter(perHour, burst int, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	burst = max(burst, 1)
	return &Limiter{perHour: perHour, burst: float64(burst), tokens: float64(burst), last: now(), now: now}
}

// reserve takes a token if one is there, or says how long until one is.
func (l *Limiter) reserve() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	rate := float64(l.perHour) / float64(time.Hour) // tokens per nanosecond
	l.tokens = min(l.burst, l.tokens+float64(now.Sub(l.last))*rate)
	l.last = now
	if l.tokens >= 1 {
		l.tokens--
		return 0
	}
	if rate <= 0 {
		return time.Hour
	}
	// Rounded UP, and never 0: a wait of 0 would read as "take it" without
	// a token having been taken.
	return max(time.Duration(math.Ceil((1-l.tokens)/rate)), 1)
}

// Wait blocks until a token is free (through sleep), then takes it.
func (l *Limiter) Wait(ctx context.Context, sleep func(context.Context, time.Duration) error) error {
	for {
		d := l.reserve()
		if d <= 0 {
			return nil
		}
		if err := sleep(ctx, d); err != nil {
			return err
		}
	}
}
