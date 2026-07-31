// Package importer holds the REST clients that fetch historical data.
//
// Webhooks only fire forward from when a subscription is created, so anything
// older — including everything predating this service — can only come from
// here. These clients reproduce the windowing and pagination the dashboard
// already uses rather than reinventing it, because that logic encodes provider
// quirks discovered in production.
//
// This is a batch job, not a request-per-delivery handler, so it owns its own
// throttling.
package importer

import (
	"context"
	"time"
)

// Limiter paces outbound requests.
//
// Hand-rolled rather than golang.org/x/time/rate: a ticker and a channel is
// twenty lines, and this module keeps its dependency list deliberately short.
// If bursting or a token bucket is ever genuinely needed, swap it then.
type Limiter struct {
	interval time.Duration
	last     time.Time
	// now is injectable so tests can assert pacing without sleeping.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// NewLimiter allows at most n requests per second. n <= 0 disables limiting.
func NewLimiter(perSecond float64) *Limiter {
	l := &Limiter{now: time.Now, sleep: sleepCtx}
	if perSecond > 0 {
		l.interval = time.Duration(float64(time.Second) / perSecond)
	}
	return l
}

// Wait blocks until the next request may be sent, or the context ends.
func (l *Limiter) Wait(ctx context.Context) error {
	if l == nil || l.interval == 0 {
		return ctx.Err()
	}

	now := l.now()
	if !l.last.IsZero() {
		if gap := l.interval - now.Sub(l.last); gap > 0 {
			if err := l.sleep(ctx, gap); err != nil {
				return err
			}
			now = l.now()
		}
	}
	l.last = now
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
