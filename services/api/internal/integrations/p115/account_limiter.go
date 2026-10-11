package p115

import (
	"context"
	"math"
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
	tokens      float64
	last        time.Time
	outstanding int
}

// dataPlaneReservation is one delayed token slot. Immediate tokens do not
// create a reservation that later Wait cancellation can roll back.
type dataPlaneReservation struct {
	accountKey string
	readyAt    time.Time
	wait       time.Duration
}

// ParseDataPlaneLimitConfig reads optional env overrides. Invalid values keep
// the conservative defaults rather than disabling protection. RPS=0 remains
// the explicit bypass; Inf/NaN and negative RPS keep the default. Burst and
// MaxWait must be positive.
func ParseDataPlaneLimitConfig() DataPlaneLimitConfig {
	config := DataPlaneLimitConfig{
		RPS:     defaultDataPlaneRPS,
		Burst:   defaultDataPlaneBurst,
		MaxWait: defaultDataPlaneMaxWait,
	}
	if value := strings.TrimSpace(os.Getenv("P115_DATA_PLANE_RPS")); value != "" {
		if parsed, err := strconv.ParseFloat(value, 64); err == nil && parsed >= 0 && isFiniteRate(parsed) {
			config.RPS = parsed
		}
	}
	if value := strings.TrimSpace(os.Getenv("P115_DATA_PLANE_BURST")); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
			config.Burst = parsed
		}
	}
	if value := strings.TrimSpace(os.Getenv("P115_DATA_PLANE_MAX_WAIT")); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil && parsed > 0 {
			config.MaxWait = parsed
		}
	}
	return config
}

// isFiniteRate rejects Inf/NaN so ParseFloat cannot silently disable pacing.
func isFiniteRate(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
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

func (limiter *accountRateLimiter) currentTime() time.Time {
	if limiter != nil && limiter.now != nil {
		return limiter.now()
	}
	return time.Now()
}

// waitDataPlane paces Cookie/Web API calls for one Provider UID. Missing or
// disabled limiters do not invent a quota. Context cancellation and local
// MaxWait overflow stay request-scoped and are not 429 health events.
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
// would exceed MaxWait. Local queue overflow is ErrDataPlaneQueueLimited, not
// an upstream 429. Canceled callers receive the context error and roll back
// an unused delayed reservation so leftover queue cannot trip MaxWait.
func (limiter *accountRateLimiter) Wait(ctx context.Context, accountKey string) error {
	if limiter == nil || limiter.config.RPS <= 0 || strings.TrimSpace(accountKey) == "" {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	reserved, err := limiter.reserve(accountKey)
	if err != nil {
		return err
	}
	if reserved.wait <= 0 {
		return nil
	}
	timer := time.NewTimer(reserved.wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		limiter.cancelReserve(reserved)
		return ctx.Err()
	case <-timer.C:
		limiter.completeReserve(reserved)
		return nil
	}
}

// reserve deducts one token or reports how long the caller must wait.
// Future reservations accumulate from max(now, last) so concurrent waiters
// are spaced at 1/RPS and MaxWait applies to the caller's total delay.
func (limiter *accountRateLimiter) reserve(accountKey string) (dataPlaneReservation, error) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	now := limiter.currentTime()
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
		return dataPlaneReservation{accountKey: accountKey}, nil
	}
	needed := (1 - bucket.tokens) / limiter.config.RPS
	next := cursor.Add(time.Duration(needed * float64(time.Second)))
	wait := next.Sub(now)
	if wait < 0 {
		wait = 0
	}
	if limiter.config.MaxWait > 0 && wait > limiter.config.MaxWait {
		return dataPlaneReservation{}, ErrDataPlaneQueueLimited
	}
	bucket.tokens = 0
	bucket.last = next
	bucket.outstanding++
	return dataPlaneReservation{accountKey: accountKey, readyAt: next, wait: wait}, nil
}

// cancelReserve reclaims one future slot for any canceled delayed reservation,
// including non-tip waiters. Later waiters may oversleep; new arrivals must
// not inherit a hole that pushes them past MaxWait. When no delayed waiters
// remain, unused future queue is dropped.
func (limiter *accountRateLimiter) cancelReserve(reserved dataPlaneReservation) {
	if limiter == nil || reserved.wait <= 0 || reserved.accountKey == "" {
		return
	}
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	bucket := limiter.buckets[reserved.accountKey]
	if bucket == nil || bucket.outstanding <= 0 {
		return
	}
	bucket.outstanding--
	now := limiter.currentTime()
	if bucket.outstanding == 0 {
		if bucket.last.After(now) {
			bucket.last = now
		}
		return
	}
	rewound := bucket.last.Add(-limiter.tokenInterval())
	if rewound.Before(now) {
		rewound = now
	}
	bucket.last = rewound
}

// completeReserve marks a delayed waiter as having consumed its slot.
func (limiter *accountRateLimiter) completeReserve(reserved dataPlaneReservation) {
	if limiter == nil || reserved.wait <= 0 || reserved.accountKey == "" {
		return
	}
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	bucket := limiter.buckets[reserved.accountKey]
	if bucket == nil || bucket.outstanding <= 0 {
		return
	}
	bucket.outstanding--
}

func (limiter *accountRateLimiter) tokenInterval() time.Duration {
	if limiter == nil || limiter.config.RPS <= 0 {
		return 0
	}
	return time.Duration(float64(time.Second) / limiter.config.RPS)
}
