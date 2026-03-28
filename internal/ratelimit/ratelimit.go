// Package ratelimit provides a token-bucket rate limiter with forced-pause
// support for honouring HTTP 429 Retry-After headers.
package ratelimit

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Limiter is a thread-safe token-bucket rate limiter.
//
// Normal operation: callers call Wait, which blocks until a token is
// available, then proceeds. Tokens refill at the configured rate.
//
// Rate-limit response handling: when a 429 is received from a server,
// call SetPause(d) with the Retry-After duration. All subsequent Wait
// calls will block until the pause expires. After the pause, token
// accumulation restarts from zero (no burst spike after the pause).
type Limiter struct {
	mu       sync.Mutex
	tokens   float64
	capacity float64
	rate     float64   // tokens per second
	lastFill time.Time
	pauseEnd time.Time // forced pause; zero = no pause
}

// New returns a Limiter allowing up to rps requests per second with a
// burst capacity of burst tokens.
func New(rps float64, burst int) *Limiter {
	if rps <= 0 {
		rps = 1
	}
	if burst < 1 {
		burst = 1
	}
	cap := float64(burst)
	return &Limiter{
		tokens:   cap,
		capacity: cap,
		rate:     rps,
		lastFill: time.Now(),
	}
}

// Wait blocks until a token is available or ctx is done.
// Returns ctx.Err() if the context is cancelled.
func (l *Limiter) Wait(ctx context.Context) error {
	for {
		wait := l.tryAcquire()
		if wait == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// tryAcquire attempts to consume one token without blocking.
// Returns 0 if a token was acquired; otherwise the duration to wait.
func (l *Limiter) tryAcquire() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()

	// Honour a forced Retry-After pause.
	if !l.pauseEnd.IsZero() && now.Before(l.pauseEnd) {
		// +1ms avoids a tight busy-loop at the boundary.
		return l.pauseEnd.Sub(now) + time.Millisecond
	}

	// If we just exited a pause, reset the fill clock to pauseEnd so tokens
	// do not accumulate during the pause period (which would cause a burst).
	if !l.pauseEnd.IsZero() && !now.Before(l.pauseEnd) {
		l.tokens = 0
		l.lastFill = l.pauseEnd
		l.pauseEnd = time.Time{}
	}

	// Refill tokens based on elapsed time.
	elapsed := now.Sub(l.lastFill).Seconds()
	l.tokens += elapsed * l.rate
	if l.tokens > l.capacity {
		l.tokens = l.capacity
	}
	l.lastFill = now

	if l.tokens >= 1 {
		l.tokens--
		return 0
	}

	// Return how long until the next token is available.
	need := 1 - l.tokens
	return time.Duration(need/l.rate*float64(time.Second)) + time.Millisecond
}

// SetPause forces all Wait calls to block until at least d elapses.
// If a longer pause is already in effect this is a no-op.
// This is the mechanism for honouring Retry-After headers from 429s:
// one call to SetPause blocks every goroutine sharing this limiter.
func (l *Limiter) SetPause(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	end := time.Now().Add(d)
	if end.After(l.pauseEnd) {
		l.pauseEnd = end
	}
}

// PauseEnd returns the time at which the current forced pause expires.
// Returns the zero value when no pause is active.
func (l *Limiter) PauseEnd() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pauseEnd
}

// ParseRetryAfter parses the value of a Retry-After HTTP response header.
// Accepts both integer-seconds ("30") and HTTP-date formats.
// Returns def if the header is empty or unparseable.
func ParseRetryAfter(header string, def time.Duration) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return def
	}
	// Integer seconds
	if n, err := strconv.Atoi(header); err == nil {
		if n < 0 {
			return def
		}
		return time.Duration(n) * time.Second
	}
	// HTTP-date (e.g. "Thu, 25 Mar 2026 12:00:00 GMT")
	if t, err := http.ParseTime(header); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
		return 0
	}
	return def
}
