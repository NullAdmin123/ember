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

func TestAccountRateLimiterRejectsWhenWaitExceedsMax(t *testing.T) {
	limiter := newAccountRateLimiter(DataPlaneLimitConfig{RPS: 1, Burst: 1, MaxWait: time.Millisecond})
	now := time.Now()
	limiter.now = func() time.Time { return now }
	if err := limiter.Wait(context.Background(), "uid-1"); err != nil {
		t.Fatalf("first token: %v", err)
	}
	if err := limiter.Wait(context.Background(), "uid-1"); !errors.Is(err, ErrProviderRateLimited) {
		t.Fatalf("second token error = %v, want rate limited", err)
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
	if _, err := adapter.SearchBySHA1(context.Background(), fixtureCredential(), FileQuery{SHA1: fixtureSHA1, Size: 1024}); !errors.Is(err, ErrProviderRateLimited) {
		t.Fatalf("second search error = %v, want local rate limit", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("rate-limited call reached 115: calls=%d", calls.Load())
	}
}
