package ratelimit

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── ParseRetryAfter ────────────────────────────────────────────────────────

func TestParseRetryAfter_IntegerSeconds(t *testing.T) {
	got := ParseRetryAfter("30", 10*time.Second)
	if got != 30*time.Second {
		t.Errorf("got %v, want 30s", got)
	}
}

func TestParseRetryAfter_Zero(t *testing.T) {
	got := ParseRetryAfter("0", 10*time.Second)
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

func TestParseRetryAfter_Negative(t *testing.T) {
	got := ParseRetryAfter("-5", 10*time.Second)
	if got != 10*time.Second {
		t.Errorf("negative value should fall back to default, got %v", got)
	}
}

func TestParseRetryAfter_EmptyFallsToDefault(t *testing.T) {
	got := ParseRetryAfter("", 15*time.Second)
	if got != 15*time.Second {
		t.Errorf("empty header should return default, got %v", got)
	}
}

func TestParseRetryAfter_InvalidFallsToDefault(t *testing.T) {
	got := ParseRetryAfter("not-a-number", 15*time.Second)
	if got != 15*time.Second {
		t.Errorf("invalid header should return default, got %v", got)
	}
}

func TestParseRetryAfter_HTTPDate(t *testing.T) {
	// Build a date ~30s in the future.
	futureDate := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	got := ParseRetryAfter(futureDate, 5*time.Second)
	// Allow ±5s for test execution time.
	if got < 25*time.Second || got > 35*time.Second {
		t.Errorf("HTTP-date parse: expected ~30s, got %v", got)
	}
}

func TestParseRetryAfter_HTTPDatePast(t *testing.T) {
	// A date in the past should return 0 (not negative).
	pastDate := time.Now().Add(-10 * time.Second).UTC().Format(http.TimeFormat)
	got := ParseRetryAfter(pastDate, 5*time.Second)
	if got != 0 {
		t.Errorf("past HTTP-date should return 0, got %v", got)
	}
}

// ── Basic rate limiting ────────────────────────────────────────────────────

func TestLimiter_BurstTokensConsumedImmediately(t *testing.T) {
	lim := New(1, 5) // 1 RPS, burst 5
	start := time.Now()
	for i := 0; i < 5; i++ {
		if err := lim.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("first 5 (burst) tokens should be immediate, took %v", elapsed)
	}
}

func TestLimiter_RateEnforced(t *testing.T) {
	lim := New(10, 1) // 10 RPS, burst 1
	lim.Wait(context.Background()) // consume burst token

	start := time.Now()
	lim.Wait(context.Background()) // must wait ~100ms for next token
	elapsed := time.Since(start)

	if elapsed < 80*time.Millisecond {
		t.Errorf("rate limiting: expected ≥80ms, got %v", elapsed)
	}
}

func TestLimiter_ContextCancellation(t *testing.T) {
	lim := New(0.001, 1) // extremely slow: one token per 1000 seconds
	lim.Wait(context.Background())  // consume the one burst token

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := lim.Wait(ctx)
	if err == nil {
		t.Fatal("expected context cancellation error, got nil")
	}
}

// ── SetPause / Retry-After ─────────────────────────────────────────────────

func TestLimiter_SetPause_BlocksUntilExpiry(t *testing.T) {
	lim := New(100, 100)
	lim.SetPause(200 * time.Millisecond)

	start := time.Now()
	if err := lim.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	if elapsed < 150*time.Millisecond {
		t.Errorf("SetPause(200ms): expected ≥150ms wait, got %v", elapsed)
	}
}

func TestLimiter_SetPause_NoBurstAfterPause(t *testing.T) {
	// A high-capacity limiter — without the post-pause reset, we'd get
	// a massive burst of accumulated tokens after a pause.
	lim := New(100, 1000)
	lim.SetPause(100 * time.Millisecond)

	// Wait out the pause.
	lim.Wait(context.Background())

	// Now measure how long the second token takes.  With a proper reset,
	// the limiter starts from 0 tokens, so we wait ~10ms for the next one.
	start := time.Now()
	lim.Wait(context.Background())
	elapsed := time.Since(start)

	// Should not be instant (i.e. not from accumulated tokens).
	if elapsed < 5*time.Millisecond {
		t.Errorf("burst after pause: expected throttled second token, got %v", elapsed)
	}
}

func TestLimiter_SetPause_LongerPauseWins(t *testing.T) {
	lim := New(100, 100)
	lim.SetPause(50 * time.Millisecond)
	lim.SetPause(200 * time.Millisecond) // longer — should win
	lim.SetPause(10 * time.Millisecond)  // shorter — ignored

	pe := lim.PauseEnd()
	remaining := time.Until(pe)
	if remaining < 150*time.Millisecond {
		t.Errorf("longest SetPause should win; pause ends in %v, expected ≥150ms", remaining)
	}
}

func TestLimiter_SetPause_ContextCancelledDuringPause(t *testing.T) {
	lim := New(100, 100)
	lim.SetPause(10 * time.Second) // very long pause

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := lim.Wait(ctx)
	if err == nil {
		t.Fatal("expected context error during forced pause, got nil")
	}
}

// ── Concurrency ────────────────────────────────────────────────────────────

func TestLimiter_ConcurrentWaiters_AllEventuallyPass(t *testing.T) {
	const goroutines = 20
	lim := New(50, 5) // 50 RPS, burst 5

	var wg sync.WaitGroup
	var passed int64
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := lim.Wait(ctx); err == nil {
				atomic.AddInt64(&passed, 1)
			}
		}()
	}
	wg.Wait()

	if int(passed) != goroutines {
		t.Errorf("expected all %d goroutines to pass, only %d did", goroutines, passed)
	}
}

func TestLimiter_SetPause_AllWaitersBlocked(t *testing.T) {
	// Multiple goroutines should all be blocked by a single SetPause.
	lim := New(1000, 1000)
	pause := 150 * time.Millisecond
	lim.SetPause(pause)

	var wg sync.WaitGroup
	starts := make([]time.Duration, 5)
	for i := range starts {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			t0 := time.Now()
			lim.Wait(context.Background())
			starts[idx] = time.Since(t0)
		}(i)
	}
	wg.Wait()

	for i, d := range starts {
		if d < 100*time.Millisecond {
			t.Errorf("goroutine %d: expected ≥100ms wait, got %v", i, d)
		}
	}
}

// ── PauseEnd ───────────────────────────────────────────────────────────────

func TestLimiter_PauseEnd_ZeroWhenNoPause(t *testing.T) {
	lim := New(10, 10)
	if !lim.PauseEnd().IsZero() {
		t.Errorf("PauseEnd should be zero when no pause is active")
	}
}

func TestLimiter_PauseEnd_NonZeroWhilePaused(t *testing.T) {
	lim := New(10, 10)
	lim.SetPause(time.Second)
	if lim.PauseEnd().IsZero() {
		t.Errorf("PauseEnd should be non-zero while pause is active")
	}
}

// ── Throughput sanity ──────────────────────────────────────────────────────

func TestLimiter_ThroughputApproximate(t *testing.T) {
	// 20 RPS, burst 1. Run 10 acquisitions and verify time is roughly 10/20 = 0.5s.
	lim := New(20, 1)
	n := 10
	start := time.Now()
	for i := 0; i < n; i++ {
		lim.Wait(context.Background())
	}
	elapsed := time.Since(start)
	expected := time.Duration(n-1) * (time.Second / 20) // n-1 gaps
	// Allow 50% slack for scheduling jitter.
	if elapsed < expected/2 || elapsed > expected*3 {
		t.Errorf("throughput: %d acquisitions at 20 RPS took %v, expected ~%v",
			n, elapsed, expected)
	}
}

// ── Stringer (useful in error messages) ────────────────────────────────────

func TestLimiter_String(t *testing.T) {
	lim := New(5, 10)
	// Just verify it doesn't panic — no specific format required.
	_ = fmt.Sprintf("%+v", lim)
}
