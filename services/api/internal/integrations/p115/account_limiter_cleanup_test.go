package p115

import (
	"context"
	"testing"
	"time"
)

// TestAccountLimiterReclaimsIdleBuckets waits for natural refill, so removing
// unused UIDs never resets their burst budget while they are still depleted.
func TestAccountLimiterReclaimsIdleBuckets(t *testing.T) {
	clock := newManualClock(time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))
	limiter := newManualLimiter(clock, DataPlaneLimitConfig{RPS: 1, Burst: 1, MaxWait: 3 * time.Second})
	if err := limiter.Wait(context.Background(), "12345"); err != nil {
		t.Fatal(err)
	}
	clock.Advance(999 * time.Millisecond)
	if len(limiter.buckets) != 1 {
		t.Fatal("depleted bucket was reset early")
	}
	clock.Advance(time.Millisecond)
	if len(limiter.buckets) != 0 {
		t.Fatal("fully refilled idle UID remained in the map")
	}
}

// TestForgottenUIDKeepsQueuedTokens prevents deletion/rebind from granting
// fresh bursts, dropping FIFO waiters or clearing another UID's pacing state.
func TestForgottenUIDKeepsQueuedTokens(t *testing.T) {
	clock := newManualClock(time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))
	limiter := newManualLimiter(clock, DataPlaneLimitConfig{RPS: 1, Burst: 1, MaxWait: 3 * time.Second})
	_ = limiter.Wait(context.Background(), "12345")
	_ = limiter.Wait(context.Background(), "67890")
	first, err := limiter.enqueue("12345")
	if err != nil {
		t.Fatal(err)
	}
	limiter.forgetAccount("12345")
	second, err := limiter.enqueue("12345")
	if err != nil || first == nil || second == nil {
		t.Fatal("rebind reset a depleted bucket")
	}
	clock.Advance(time.Second)
	if !first.granted || second.granted {
		t.Fatal("revocation broke FIFO grants")
	}
	clock.Advance(time.Second)
	if !second.granted {
		t.Fatal("queued rebound request lost its token")
	}
	clock.Advance(time.Second)
	if len(limiter.buckets) != 0 {
		t.Fatal("drained and refilled UID buckets were retained")
	}
}

// TestOldIdleTimerCannotTouchReboundUID exercises an already-dispatched timer
// callback after its UID was reclaimed and a new bucket reused its key.
func TestOldIdleTimerCannotTouchReboundUID(t *testing.T) {
	clock := newManualClock(time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))
	limiter := newManualLimiter(clock, DataPlaneLimitConfig{RPS: 1, Burst: 1, MaxWait: 3 * time.Second})
	_ = limiter.Wait(context.Background(), "12345")
	oldCallback := clock.timers[0].fn
	clock.Advance(time.Second)
	_ = limiter.Wait(context.Background(), "12345")
	bucket := limiter.buckets["12345"]
	generation := bucket.timerGen
	oldCallback()
	if limiter.buckets["12345"] != bucket || bucket.timerGen != generation || bucket.tokens != 0 {
		t.Fatal("old callback modified the rebound account budget")
	}
	clock.Advance(time.Second)
	if len(limiter.buckets) != 0 {
		t.Fatal("new bucket did not complete its own cleanup")
	}
}

// TestProductionAdaptersShareDeletionCleanup proves that revocation reaches
// the same process-local limiter used by production adapters, without HTTP I/O.
func TestProductionAdaptersShareDeletionCleanup(t *testing.T) {
	first, second := &CookieHTTPAdapter{}, &CookieHTTPAdapter{}
	attachDataPlaneLimiter(first)
	attachDataPlaneLimiter(second)
	if first.limiter != second.limiter {
		t.Fatal("production cleanup cannot reach all local adapters")
	}
	first.limiter.mu.Lock()
	first.limiter.bucketLocked("999999")
	first.limiter.mu.Unlock()
	ForgetDataPlaneAccount("999999")
	second.limiter.mu.Lock()
	_, exists := second.limiter.buckets["999999"]
	second.limiter.mu.Unlock()
	if exists {
		t.Fatal("committed revocation retained a fully idle bucket")
	}
}
