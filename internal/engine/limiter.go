package engine

import (
	"context"
	"sync"
	"time"
)

// limiter is a leaky-bucket rate limiter shared by all download workers.
// Take(n) charges n bytes and sleeps off any deficit, so sustained throughput
// converges on rate bytes/sec regardless of connection count.
type limiter struct {
	mu     sync.Mutex
	rate   float64 // bytes per second
	bucket float64 // available bytes (can go negative = debt)
	last   time.Time
}

func newLimiter(bytesPerSec int64) *limiter {
	if bytesPerSec <= 0 {
		return nil
	}
	return &limiter{rate: float64(bytesPerSec), bucket: float64(bytesPerSec), last: time.Now()}
}

func (l *limiter) Take(ctx context.Context, n int) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	now := time.Now()
	l.bucket += now.Sub(l.last).Seconds() * l.rate
	if l.bucket > l.rate { // cap burst at ~1s worth
		l.bucket = l.rate
	}
	l.last = now
	l.bucket -= float64(n)
	deficit := -l.bucket
	l.mu.Unlock()

	if deficit <= 0 {
		return nil
	}
	wait := time.Duration(deficit / l.rate * float64(time.Second))
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
		return nil
	}
}
