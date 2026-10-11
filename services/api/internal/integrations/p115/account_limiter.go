package p115

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultDataPlaneRPS     = 2.0
	defaultDataPlaneBurst   = 4
	defaultDataPlaneMaxWait = 2 * time.Second
)

// DataPlaneLimitConfig is the conservative per-account 115 API pacing policy.
// Zero RPS disables waiting so tests and emergency bypass stay explicit.
type DataPlaneLimitConfig struct {
	RPS     float64
	Burst   int
	MaxWait time.Duration
}

type accountRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	config  DataPlaneLimitConfig
	now     func() time.Time
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

// ParseDataPlaneLimitConfig reads optional env overrides. Invalid values keep
// the conservative defaults rather than disabling protection.
func ParseDataPlaneLimitConfig() DataPlaneLimitConfig {
	config := DataPlaneLimitConfig{
		RPS:     defaultDataPlaneRPS,
		Burst:   defaultDataPlaneBurst,
		MaxWait: defaultDataPlaneMaxWait,
	}
	if value := strings.TrimSpace(os.Getenv("P115_DATA_PLANE_RPS")); value != "" {
		if parsed, err := strconv.ParseFloat(value, 64); err == nil && parsed >= 0 {
			config.RPS = parsed
		}
	}
	if value := strings.TrimSpace(os.Getenv("P115_DATA_PLANE_BURST")); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed >= 0 {
			config.Burst = parsed
		}
	}
	if value := strings.TrimSpace(os.Getenv("P115_DATA_PLANE_MAX_WAIT")); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil && parsed >= 0 {
			config.MaxWait = parsed
		}
	}
	if config.Burst == 0 && config.RPS > 0 {
		config.Burst = 1
	}
	return config
}

// attachDataPlaneLimiter installs the production per-account token bucket.
// Test adapters keep a nil limiter unless a case opts in.
func attachDataPlaneLimiter(adapter *CookieHTTPAdapter) {
	if adapter == nil {
		return
	}
	adapter.limiter = newAccountRateLimiter(ParseDataPlaneLimitConfig())
}

// newAccountRateLimiter builds an empty per-UID token-bucket map.
func newAccountRateLimiter(config DataPlaneLimitConfig) *accountRateLimiter {
	return &accountRateLimiter{
		buckets: make(map[string]*tokenBucket),
		config:  config,
		now:     time.Now,
	}
}

// waitDataPlane paces Cookie/Web API calls for one Provider UID. Missing or
// disabled limiters do not invent a quota. Context cancellation stays the
// caller's timeout, not a rate-limit health event.
func (a *CookieHTTPAdapter) waitDataPlane(ctx context.Context, credential Credential) error {
	if a == nil || a.limiter == nil {
		return nil
	}
	providerUserID, err := validateCookieHTTPCredential(credential)
	if err != nil {
		return err
	}
	return a.limiter.Wait(ctx, providerUserID)
}

// Wait reserves one token for the account key or fails closed when the wait
// would exceed MaxWait. Canceled callers receive the context error.
func (limiter *accountRateLimiter) Wait(ctx context.Context, accountKey string) error {
	if limiter == nil || limiter.config.RPS <= 0 || strings.TrimSpace(accountKey) == "" {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	wait, err := limiter.reserve(accountKey)
	if err != nil {
		return err
	}
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// reserve deducts one token or reports how long the caller must wait.
// Future reservations accumulate from max(now, last) so concurrent waiters
// are spaced at 1/RPS and MaxWait applies to the caller's total delay.
func (limiter *accountRateLimiter) reserve(accountKey string) (time.Duration, error) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	now := limiter.now()
	if limiter.now == nil {
		now = time.Now()
	}
	bucket := limiter.buckets[accountKey]
	if bucket == nil {
		bucket = &tokenBucket{tokens: float64(limiter.config.Burst), last: now}
		limiter.buckets[accountKey] = bucket
	}
	cursor := bucket.last
	if now.After(cursor) {
		bucket.tokens += now.Sub(cursor).Seconds() * limiter.config.RPS
		if bucket.tokens > float64(limiter.config.Burst) {
			bucket.tokens = float64(limiter.config.Burst)
		}
		cursor = now
	}
	if bucket.tokens >= 1 {
		bucket.tokens--
		bucket.last = cursor
		return 0, nil
	}
	needed := (1 - bucket.tokens) / limiter.config.RPS
	next := cursor.Add(time.Duration(needed * float64(time.Second)))
	wait := next.Sub(now)
	if wait < 0 {
		wait = 0
	}
	if limiter.config.MaxWait > 0 && wait > limiter.config.MaxWait {
		return 0, ErrProviderRateLimited
	}
	bucket.tokens = 0
	bucket.last = next
	return wait, nil
}
