package p115

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseDataPlaneLimitConfigUsesConservativeDefaults(t *testing.T) {
	t.Setenv("P115_DATA_PLANE_RPS", "")
	t.Setenv("P115_DATA_PLANE_BURST", "")
	t.Setenv("P115_DATA_PLANE_MAX_WAIT", "")
	config := ParseDataPlaneLimitConfig()
	if config.RPS != defaultDataPlaneRPS || config.Burst != defaultDataPlaneBurst || config.MaxWait != defaultDataPlaneMaxWait {
		t.Fatalf("defaults = %+v", config)
	}
}

func TestParseDataPlaneLimitConfigAcceptsValidOverrides(t *testing.T) {
	t.Setenv("P115_DATA_PLANE_RPS", "1.5")
	t.Setenv("P115_DATA_PLANE_BURST", "3")
	t.Setenv("P115_DATA_PLANE_MAX_WAIT", "500ms")
	config := ParseDataPlaneLimitConfig()
	if config.RPS != 1.5 || config.Burst != 3 || config.MaxWait != 500*time.Millisecond {
		t.Fatalf("overrides = %+v", config)
	}
}

func TestParseDataPlaneLimitConfigKeepsDefaultsForInvalidValues(t *testing.T) {
	t.Setenv("P115_DATA_PLANE_RPS", "-1")
	t.Setenv("P115_DATA_PLANE_BURST", "nope")
	t.Setenv("P115_DATA_PLANE_MAX_WAIT", "forever")
	config := ParseDataPlaneLimitConfig()
	if config.RPS != defaultDataPlaneRPS || config.Burst != defaultDataPlaneBurst || config.MaxWait != defaultDataPlaneMaxWait {
		t.Fatalf("invalid override leaked: %+v", config)
	}
}

func TestParseDataPlaneLimitConfigRejectsNonPositiveBurstAndMaxWait(t *testing.T) {
	t.Setenv("P115_DATA_PLANE_RPS", "0")
	t.Setenv("P115_DATA_PLANE_BURST", "0")
	t.Setenv("P115_DATA_PLANE_MAX_WAIT", "0")
	config := ParseDataPlaneLimitConfig()
	if config.RPS != 0 {
		t.Fatalf("RPS 0 should disable waiting: %+v", config)
	}
	if config.Burst != defaultDataPlaneBurst {
		t.Fatalf("Burst 0 leaked as %d, want default %d", config.Burst, defaultDataPlaneBurst)
	}
	if config.MaxWait != defaultDataPlaneMaxWait {
		t.Fatalf("MaxWait 0 leaked as %s, want default %s", config.MaxWait, defaultDataPlaneMaxWait)
	}

	t.Setenv("P115_DATA_PLANE_RPS", "-0.5")
	t.Setenv("P115_DATA_PLANE_BURST", "-2")
	t.Setenv("P115_DATA_PLANE_MAX_WAIT", "-1s")
	config = ParseDataPlaneLimitConfig()
	if config.RPS != defaultDataPlaneRPS || config.Burst != defaultDataPlaneBurst || config.MaxWait != defaultDataPlaneMaxWait {
		t.Fatalf("negative override leaked: %+v", config)
	}
}

func TestAccountRateLimiterRejectsWhenWaitExceedsMax(t *testing.T) {
	limiter := newAccountRateLimiter(DataPlaneLimitConfig{RPS: 1, Burst: 1, MaxWait: time.Millisecond})
	now := time.Now()
	limiter.now = func() time.Time { return now }
	if err := limiter.Wait(context.Background(), "uid-1"); err != nil {
		t.Fatalf("first token: %v", err)
	}
	if err := limiter.Wait(context.Background(), "uid-1"); !errors.Is(err, ErrDataPlaneQueueLimited) || errors.Is(err, ErrProviderRateLimited) {
		t.Fatalf("second token error = %v, want local queue limit", err)
	}
	if err := limiter.Wait(context.Background(), "uid-2"); err != nil {
		t.Fatalf("other account was throttled: %v", err)
	}
}

func TestAccountRateLimiterCanceledWaitDoesNotBecomeRateLimit(t *testing.T) {
	limiter := newAccountRateLimiter(DataPlaneLimitConfig{RPS: 1, Burst: 1, MaxWait: time.Second})
	if err := limiter.Wait(context.Background(), "uid-1"); err != nil {
		t.Fatalf("first token: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(limiter.Wait(ctx, "uid-1"), context.Canceled) {
		t.Fatal("canceled wait lost context error")
	}
}

func TestAccountRateLimiterCanceledWaitRestoresCapacity(t *testing.T) {
	limiter := newAccountRateLimiter(DataPlaneLimitConfig{RPS: 1, Burst: 1, MaxWait: 2 * time.Second})
	now := time.Unix(1_700_000_000, 0).UTC()
	limiter.now = func() time.Time { return now }
	if err := limiter.Wait(context.Background(), "uid-1"); err != nil {
		t.Fatalf("burst token: %v", err)
	}

	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		started := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			close(started)
			done <- limiter.Wait(ctx, "uid-1")
		}()
		<-started
		waitForReservation(t, limiter, "uid-1")
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled waiter %d error = %v", i, err)
		}
	}

	reserved, err := limiter.reserve("uid-1")
	if err != nil {
		t.Fatalf("real request after canceled waiters: %v", err)
	}
	if reserved.wait != time.Second {
		t.Fatalf("after canceled waiters wait = %s, want 1s refill not a leaked queue", reserved.wait)
	}
}

func TestAccountRateLimiterCancelOnlyRewindsTipReservation(t *testing.T) {
	limiter := newAccountRateLimiter(DataPlaneLimitConfig{RPS: 1, Burst: 1, MaxWait: 2 * time.Second})
	now := time.Unix(1_700_000_000, 0).UTC()
	limiter.now = func() time.Time { return now }
	if err := limiter.Wait(context.Background(), "uid-1"); err != nil {
		t.Fatalf("burst token: %v", err)
	}

	earlier, err := limiter.reserve("uid-1")
	if err != nil || earlier.wait != time.Second {
		t.Fatalf("earlier reservation = %s err=%v, want 1s", earlier.wait, err)
	}
	tip, err := limiter.reserve("uid-1")
	if err != nil || tip.wait != 2*time.Second {
		t.Fatalf("tip reservation = %s err=%v, want 2s", tip.wait, err)
	}

	limiter.cancelReserve(earlier)
	if _, err := limiter.reserve("uid-1"); !errors.Is(err, ErrDataPlaneQueueLimited) {
		t.Fatalf("canceling a non-tip reservation moved later slots: %v", err)
	}

	limiter.cancelReserve(tip)
	restored, err := limiter.reserve("uid-1")
	if err != nil || restored.wait != time.Second {
		t.Fatalf("after tip cancel wait = %s err=%v, want restored 1s refill", restored.wait, err)
	}
}

func TestAccountRateLimiterStaggersConcurrentReservationsAndEnforcesMaxWait(t *testing.T) {
	limiter := newAccountRateLimiter(DataPlaneLimitConfig{RPS: 1, Burst: 1, MaxWait: 2 * time.Second})
	now := time.Unix(1_700_000_000, 0).UTC()
	limiter.now = func() time.Time { return now }
	if err := limiter.Wait(context.Background(), "uid-1"); err != nil {
		t.Fatalf("burst token: %v", err)
	}

	first, err := limiter.reserve("uid-1")
	if err != nil || first.wait != time.Second {
		t.Fatalf("first waiter = %s err=%v, want 1s", first.wait, err)
	}
	second, err := limiter.reserve("uid-1")
	if err != nil || second.wait != 2*time.Second {
		t.Fatalf("second waiter = %s err=%v, want 2s", second.wait, err)
	}
	if _, err := limiter.reserve("uid-1"); !errors.Is(err, ErrDataPlaneQueueLimited) {
		t.Fatalf("third waiter error = %v, want cumulative MaxWait reject", err)
	}
}

func TestCookieHTTPAdapterGetJSONMapsRateLimitAndRiskControl(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr error
	}{
		{name: "too many requests", status: http.StatusTooManyRequests, wantErr: ErrProviderRateLimited},
		{name: "forbidden", status: http.StatusForbidden, wantErr: ErrProviderRiskControl},
		{name: "unavailable", status: http.StatusServiceUnavailable, wantErr: ErrProviderUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
			}))
			defer server.Close()
			adapter := newTestCookieHTTPAdapter(t, server)
			_, err := adapter.SearchBySHA1(context.Background(), fixtureCredential(), FileQuery{SHA1: fixtureSHA1, Size: 1024})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestInitRapidUploadTakesOneTokenPerHTTPRequest(t *testing.T) {
	server := newRapidUploadServer(t, `{"status":2,"statuscode":0}`)
	defer server.Close()
	adapter := newTestRapidUploadAdapter(t, server)
	adapter.limiter = newAccountRateLimiter(DataPlaneLimitConfig{RPS: 1, Burst: 2, MaxWait: time.Millisecond})
	if _, err := adapter.InitRapidUpload(context.Background(), fixtureCredential(), fixtureRapidUploadRequest()); err != nil {
		t.Fatalf("two HTTP calls exhausted three tokens: %v", err)
	}
	if _, err := adapter.SearchBySHA1(context.Background(), fixtureCredential(), FileQuery{SHA1: fixtureSHA1, Size: 1024}); !errors.Is(err, ErrDataPlaneQueueLimited) || errors.Is(err, ErrProviderRateLimited) {
		t.Fatalf("follow-up search error = %v, want local queue limit after two tokens", err)
	}
}

func TestCookieHTTPAdapterLocalQueueLimitDoesNotReach115(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"state":false}`))
	}))
	defer server.Close()
	adapter := newTestCookieHTTPAdapter(t, server)
	adapter.limiter = newAccountRateLimiter(DataPlaneLimitConfig{RPS: 2, Burst: 4, MaxWait: time.Millisecond})
	now := time.Unix(1_700_000_000, 0).UTC()
	adapter.limiter.now = func() time.Time { return now }
	for i := 0; i < 4; i++ {
		if _, err := adapter.SearchBySHA1(context.Background(), fixtureCredential(), FileQuery{SHA1: fixtureSHA1, Size: 1024}); !errors.Is(err, ErrProviderRejected) {
			t.Fatalf("burst token %d error = %v", i, err)
		}
	}
	if _, err := adapter.SearchBySHA1(context.Background(), fixtureCredential(), FileQuery{SHA1: fixtureSHA1, Size: 1024}); !errors.Is(err, ErrDataPlaneQueueLimited) || errors.Is(err, ErrProviderRateLimited) {
		t.Fatalf("overflow error = %v, want local queue limit", err)
	}
	if calls.Load() != 4 {
		t.Fatalf("local queue reject reached 115: calls=%d", calls.Load())
	}
}

func TestCookieHTTPAdapterPacesDataPlaneCallsPerAccount(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"state":false}`))
	}))
	defer server.Close()
	adapter := newTestCookieHTTPAdapter(t, server)
	adapter.limiter = newAccountRateLimiter(DataPlaneLimitConfig{RPS: 1, Burst: 1, MaxWait: time.Millisecond})
	if _, err := adapter.SearchBySHA1(context.Background(), fixtureCredential(), FileQuery{SHA1: fixtureSHA1, Size: 1024}); !errors.Is(err, ErrProviderRejected) {
		t.Fatalf("first search error = %v", err)
	}
	if _, err := adapter.SearchBySHA1(context.Background(), fixtureCredential(), FileQuery{SHA1: fixtureSHA1, Size: 1024}); !errors.Is(err, ErrDataPlaneQueueLimited) || errors.Is(err, ErrProviderRateLimited) {
		t.Fatalf("second search error = %v, want local queue limit", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("local queue reject reached 115: calls=%d", calls.Load())
	}
}

func waitForReservation(t *testing.T, limiter *accountRateLimiter, accountKey string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		limiter.mu.Lock()
		bucket := limiter.buckets[accountKey]
		ready := bucket != nil && bucket.outstanding > 0
		limiter.mu.Unlock()
		if ready {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("Wait did not reserve before cancel")
}
