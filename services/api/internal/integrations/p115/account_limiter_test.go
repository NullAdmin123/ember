package p115

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
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

func TestParseDataPlaneLimitConfigRejectsNonFiniteRPS(t *testing.T) {
	for _, value := range []string{"Inf", "+Inf", "-Inf", "NaN", "nan"} {
		t.Setenv("P115_DATA_PLANE_RPS", value)
		t.Setenv("P115_DATA_PLANE_BURST", "")
		t.Setenv("P115_DATA_PLANE_MAX_WAIT", "")
		config := ParseDataPlaneLimitConfig()
		if config.RPS != defaultDataPlaneRPS {
			t.Fatalf("RPS %q leaked as %v, want default %v", value, config.RPS, defaultDataPlaneRPS)
		}
	}
}

func TestParseDataPlaneLimitConfigRejectsTinyRPS(t *testing.T) {
	t.Setenv("P115_DATA_PLANE_RPS", "1e-10")
	t.Setenv("P115_DATA_PLANE_BURST", "")
	t.Setenv("P115_DATA_PLANE_MAX_WAIT", "")
	config := ParseDataPlaneLimitConfig()
	if config.RPS != defaultDataPlaneRPS {
		t.Fatalf("tiny RPS leaked as %v, want default", config.RPS)
	}
}

func TestParseDataPlaneLimitConfigRejectsNegativeZeroRPS(t *testing.T) {
	for _, value := range []string{"-0", "-0.0", "-0.00"} {
		t.Setenv("P115_DATA_PLANE_RPS", value)
		t.Setenv("P115_DATA_PLANE_BURST", "")
		t.Setenv("P115_DATA_PLANE_MAX_WAIT", "")
		config := ParseDataPlaneLimitConfig()
		if config.RPS != defaultDataPlaneRPS {
			t.Fatalf("RPS %q leaked as %v, want default %v", value, config.RPS, defaultDataPlaneRPS)
		}
	}
}

func TestDurationFromSecondsDoesNotOverflow(t *testing.T) {
	if got := durationFromSeconds(1e10); got <= 0 {
		t.Fatalf("huge seconds overflowed to %s", got)
	}
	if got := durationFromSeconds(math.Inf(1)); got <= 0 {
		t.Fatalf("Inf seconds overflowed to %s", got)
	}
	if got := durationFromSeconds(-1); got != 0 {
		t.Fatalf("negative seconds = %s", got)
	}
}

func TestAccountRateLimiterRejectsWhenWaitExceedsMax(t *testing.T) {
	limiter := newAccountRateLimiter(DataPlaneLimitConfig{RPS: 1, Burst: 1, MaxWait: time.Millisecond})
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

func TestAccountRateLimiterTinyRPSDoesNotBypassMaxWait(t *testing.T) {
	limiter := newAccountRateLimiter(DataPlaneLimitConfig{RPS: 1e-10, Burst: 1, MaxWait: time.Second})
	if err := limiter.Wait(context.Background(), "uid-1"); err != nil {
		t.Fatalf("burst token: %v", err)
	}
	if err := limiter.Wait(context.Background(), "uid-1"); !errors.Is(err, ErrDataPlaneQueueLimited) {
		t.Fatalf("tiny RPS error = %v, want queue limit not instant grant", err)
	}
}

func TestAccountRateLimiterCancelRestoresCapacityForNewArrival(t *testing.T) {
	clock := newManualClock(time.Unix(1_700_000_000, 0).UTC())
	limiter := newManualLimiter(clock, DataPlaneLimitConfig{RPS: 1, Burst: 1, MaxWait: 2 * time.Second})
	if err := limiter.Wait(context.Background(), "uid-1"); err != nil {
		t.Fatalf("burst token: %v", err)
	}

	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- limiter.Wait(ctx, "uid-1") }()
		waitUntilQueued(t, limiter, "uid-1", 1)
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled waiter %d error = %v", i, err)
		}
	}

	done := make(chan error, 1)
	go func() { done <- limiter.Wait(context.Background(), "uid-1") }()
	waitUntilQueued(t, limiter, "uid-1", 1)
	clock.Advance(time.Second)
	if err := <-done; err != nil {
		t.Fatalf("new request after cancels: %v", err)
	}
}

func TestAccountRateLimiterCanceledWaitersLeaveUniqueGrantSlots(t *testing.T) {
	clock := newManualClock(time.Unix(1_700_000_000, 0).UTC())
	limiter := newManualLimiter(clock, DataPlaneLimitConfig{RPS: 1, Burst: 1, MaxWait: 3 * time.Second})
	var grants []time.Time
	limiter.observeGrant = func(at time.Time) {
		grants = append(grants, at)
	}
	if err := limiter.Wait(context.Background(), "uid-1"); err != nil {
		t.Fatalf("burst token: %v", err)
	}

	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	doneA := make(chan error, 1)
	doneB := make(chan error, 1)
	doneC := make(chan error, 1)
	go func() { doneA <- limiter.Wait(ctxA, "uid-1") }()
	waitUntilQueued(t, limiter, "uid-1", 1)
	go func() { doneB <- limiter.Wait(ctxB, "uid-1") }()
	waitUntilQueued(t, limiter, "uid-1", 2)

	cancelA()
	if err := <-doneA; !errors.Is(err, context.Canceled) {
		t.Fatalf("A error = %v", err)
	}
	waitUntilQueued(t, limiter, "uid-1", 1)

	go func() { doneC <- limiter.Wait(context.Background(), "uid-1") }()
	waitUntilQueued(t, limiter, "uid-1", 2)

	clock.Advance(3 * time.Second)
	if err := <-doneB; err != nil {
		t.Fatalf("B error = %v", err)
	}
	if err := <-doneC; err != nil {
		t.Fatalf("C error = %v", err)
	}
	cancelB()

	delayed := grants[1:]
	assertUniqueGrantSpacing(t, delayed, time.Second)
}

func TestAccountRateLimiterStaggersQueueAndEnforcesMaxWait(t *testing.T) {
	clock := newManualClock(time.Unix(1_700_000_000, 0).UTC())
	limiter := newManualLimiter(clock, DataPlaneLimitConfig{RPS: 1, Burst: 1, MaxWait: 2 * time.Second})
	if err := limiter.Wait(context.Background(), "uid-1"); err != nil {
		t.Fatalf("burst token: %v", err)
	}
	doneFirst := make(chan error, 1)
	doneSecond := make(chan error, 1)
	go func() { doneFirst <- limiter.Wait(context.Background(), "uid-1") }()
	waitUntilQueued(t, limiter, "uid-1", 1)
	go func() { doneSecond <- limiter.Wait(context.Background(), "uid-1") }()
	waitUntilQueued(t, limiter, "uid-1", 2)
	if err := limiter.Wait(context.Background(), "uid-1"); !errors.Is(err, ErrDataPlaneQueueLimited) {
		t.Fatalf("third waiter error = %v, want MaxWait reject", err)
	}
	clock.Advance(2 * time.Second)
	if err := <-doneFirst; err != nil {
		t.Fatalf("first waiter: %v", err)
	}
	if err := <-doneSecond; err != nil {
		t.Fatalf("second waiter: %v", err)
	}
}

func TestAccountRateLimiterRandomCancelKeepsGrantSpacing(t *testing.T) {
	const rps = 20.0
	limiter := newAccountRateLimiter(DataPlaneLimitConfig{RPS: rps, Burst: 1, MaxWait: time.Second})
	var mu sync.Mutex
	var grants []time.Time
	limiter.observeGrant = func(at time.Time) {
		mu.Lock()
		grants = append(grants, at)
		mu.Unlock()
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if rand.New(rand.NewSource(seed)).Intn(2) == 0 {
				go func() {
					time.Sleep(time.Duration(rand.New(rand.NewSource(seed+99)).Intn(8)) * time.Millisecond)
					cancel()
				}()
			}
			_ = limiter.Wait(ctx, "uid-1")
		}(int64(i + 1))
	}
	close(start)
	wg.Wait()

	mu.Lock()
	got := append([]time.Time(nil), grants...)
	mu.Unlock()
	if len(got) == 0 {
		t.Fatal("random cancel granted nobody")
	}
	assertUniqueGrantSpacing(t, got, time.Duration(float64(time.Second)/rps)-2*time.Millisecond)
}

func TestMapCookieAPIStatusClassifiesBareStatuses(t *testing.T) {
	tests := []struct {
		status  int
		wantErr error
	}{
		{status: http.StatusOK, wantErr: nil},
		{status: http.StatusTooManyRequests, wantErr: ErrProviderRateLimited},
		{status: http.StatusForbidden, wantErr: ErrProviderUnavailable},
		{status: http.StatusUnauthorized, wantErr: ErrProviderUnavailable},
		{status: http.StatusServiceUnavailable, wantErr: ErrProviderUnavailable},
	}
	for _, test := range tests {
		err := mapCookieAPIStatus(test.status)
		if test.wantErr == nil {
			if err != nil {
				t.Fatalf("status %d error = %v, want nil", test.status, err)
			}
			continue
		}
		if !errors.Is(err, test.wantErr) {
			t.Fatalf("status %d error = %v, want %v", test.status, err, test.wantErr)
		}
		if errors.Is(err, ErrProviderRiskControl) {
			t.Fatalf("status %d must not map to risk control", test.status)
		}
	}
}

func TestCookieHTTPAdapterGetJSONMapsRateLimitAndBareForbidden(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr error
	}{
		{name: "too many requests", status: http.StatusTooManyRequests, wantErr: ErrProviderRateLimited},
		{name: "forbidden", status: http.StatusForbidden, wantErr: ErrProviderUnavailable},
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

type manualClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*manualTimer
}

type manualTimer struct {
	when    time.Time
	fn      func()
	stopped bool
}

func newManualClock(now time.Time) *manualClock {
	return &manualClock{now: now}
}

func (clock *manualClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *manualClock) AfterFunc(delay time.Duration, fn func()) func() {
	clock.mu.Lock()
	timer := &manualTimer{when: clock.now.Add(delay), fn: fn}
	clock.timers = append(clock.timers, timer)
	clock.mu.Unlock()
	return func() {
		clock.mu.Lock()
		timer.stopped = true
		clock.mu.Unlock()
	}
}

func (clock *manualClock) Advance(delay time.Duration) {
	target := clock.Now().Add(delay)
	for {
		timer := clock.nextDue(target)
		if timer == nil {
			clock.mu.Lock()
			clock.now = target
			clock.mu.Unlock()
			return
		}
		clock.mu.Lock()
		clock.now = timer.when
		clock.mu.Unlock()
		timer.fn()
	}
}

func (clock *manualClock) nextDue(target time.Time) *manualTimer {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	var chosen *manualTimer
	chosenIndex := -1
	for i, timer := range clock.timers {
		if timer.stopped || timer.when.After(target) {
			continue
		}
		if chosen == nil || timer.when.Before(chosen.when) {
			chosen = timer
			chosenIndex = i
		}
	}
	if chosenIndex >= 0 {
		clock.timers = append(clock.timers[:chosenIndex], clock.timers[chosenIndex+1:]...)
	}
	return chosen
}

func newManualLimiter(clock *manualClock, config DataPlaneLimitConfig) *accountRateLimiter {
	limiter := newAccountRateLimiter(config)
	limiter.now = clock.Now
	limiter.afterFunc = clock.AfterFunc
	return limiter
}

func waitUntilQueued(t *testing.T, limiter *accountRateLimiter, accountKey string, n int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if limiter.queuedWaiters(accountKey) >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("queued waiters = %d, want >= %d", limiter.queuedWaiters(accountKey), n)
}

func assertUniqueGrantSpacing(t *testing.T, grants []time.Time, minGap time.Duration) {
	t.Helper()
	if len(grants) < 2 {
		return
	}
	sorted := append([]time.Time(nil), grants...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Before(sorted[j]) })
	for i := 1; i < len(sorted); i++ {
		gap := sorted[i].Sub(sorted[i-1])
		if gap < minGap || gap == 0 {
			t.Fatalf("grant gap %s between %s and %s, want >= %s unique slots", gap, sorted[i-1], sorted[i], minGap)
		}
	}
}
