package p115

import (
	"container/list"
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
	// minDataPlaneRPS is the slowest positive override. Smaller values overflow
	// time.Duration when converted from seconds and are useless for playback.
	minDataPlaneRPS = 0.01
	maxTimerSeconds = float64(time.Hour/time.Second) * 24
)

// DataPlaneLimitConfig is the conservative per-account 115 API pacing policy.
// Zero RPS disables waiting so tests and emergency bypass stay explicit.
type DataPlaneLimitConfig struct {
	RPS     float64
	Burst   int
	MaxWait time.Duration
}

type accountRateLimiter struct {
	mu           sync.Mutex
	buckets      map[string]*tokenBucket
	config       DataPlaneLimitConfig
	now          func() time.Time
	afterFunc    func(time.Duration, func()) func()
	observeGrant func(time.Time)
}

type tokenBucket struct {
	tokens    float64
	last      time.Time
	waiters   *list.List
	stopTimer func()
}

type dataPlaneWaiter struct {
	grant   chan struct{}
	elem    *list.Element
	granted bool
}

// ParseDataPlaneLimitConfig reads optional env overrides. Invalid values keep
// the conservative defaults rather than disabling protection. RPS=0 remains
// the explicit bypass; Inf/NaN, sub-minimum, negative, and negative-zero RPS
// keep the default. Burst and MaxWait must be positive.
func ParseDataPlaneLimitConfig() DataPlaneLimitConfig {
	config := DataPlaneLimitConfig{
		RPS:     defaultDataPlaneRPS,
		Burst:   defaultDataPlaneBurst,
		MaxWait: defaultDataPlaneMaxWait,
	}
	if value := strings.TrimSpace(os.Getenv("P115_DATA_PLANE_RPS")); value != "" {
		if parsed, err := strconv.ParseFloat(value, 64); err == nil && isFiniteRate(parsed) {
			// Signbit rejects -0 / -0.0, which ParseFloat treats as 0.
			if !math.Signbit(parsed) && (parsed == 0 || parsed >= minDataPlaneRPS) {
				config.RPS = parsed
			}
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

// attachDataPlaneLimiter installs the production per-account limiter.
// Test adapters keep a nil limiter unless a case opts in.
func attachDataPlaneLimiter(adapter *CookieHTTPAdapter) {
	if adapter == nil {
		return
	}
	adapter.limiter = newAccountRateLimiter(ParseDataPlaneLimitConfig())
}

// newAccountRateLimiter builds an empty per-UID FIFO grant map.
func newAccountRateLimiter(config DataPlaneLimitConfig) *accountRateLimiter {
	return &accountRateLimiter{
		buckets:   make(map[string]*tokenBucket),
		config:    config,
		now:       time.Now,
		afterFunc: realAfterFunc,
	}
}

func realAfterFunc(delay time.Duration, fn func()) func() {
	if delay < 0 {
		delay = 0
	}
	timer := time.AfterFunc(delay, fn)
	return func() { timer.Stop() }
}

func (limiter *accountRateLimiter) currentTime() time.Time {
	if limiter != nil && limiter.now != nil {
		return limiter.now()
	}
	return time.Now()
}

func (limiter *accountRateLimiter) scheduleAfter(delay time.Duration, fn func()) func() {
	if limiter != nil && limiter.afterFunc != nil {
		return limiter.afterFunc(delay, fn)
	}
	return realAfterFunc(delay, fn)
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

// Wait takes one token when it is actually available, or joins a FIFO queue.
// Tokens are never pre-assigned to a future wake time. Local queue overflow
// is ErrDataPlaneQueueLimited. Canceled callers receive ctx.Err() and leave
// the queue without occupying a slot.
func (limiter *accountRateLimiter) Wait(ctx context.Context, accountKey string) error {
	if limiter == nil || limiter.config.RPS <= 0 || !isFiniteRate(limiter.config.RPS) || strings.TrimSpace(accountKey) == "" {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	waiter, err := limiter.enqueue(accountKey)
	if err != nil {
		return err
	}
	if waiter == nil {
		return nil
	}
	select {
	case <-waiter.grant:
		return nil
	case <-ctx.Done():
		limiter.leaveQueue(accountKey, waiter)
		select {
		case <-waiter.grant:
			return nil
		default:
			return ctx.Err()
		}
	}
}

// enqueue consumes an immediate token or appends a FIFO waiter. MaxWait is
// decided from float seconds so tiny RPS cannot overflow time.Duration.
func (limiter *accountRateLimiter) enqueue(accountKey string) (*dataPlaneWaiter, error) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	bucket := limiter.bucketLocked(accountKey)
	now := limiter.currentTime()
	limiter.refillLocked(bucket, now)
	limiter.grantReadyLocked(bucket)
	if bucket.tokens >= 1 && bucket.waiters.Len() == 0 {
		limiter.takeTokenLocked(bucket)
		return nil, nil
	}
	waitSeconds := limiter.delaySecondsLocked(bucket, bucket.waiters.Len())
	if limiter.config.MaxWait > 0 && waitSeconds > limiter.config.MaxWait.Seconds() {
		return nil, ErrDataPlaneQueueLimited
	}
	waiter := &dataPlaneWaiter{grant: make(chan struct{})}
	waiter.elem = bucket.waiters.PushBack(waiter)
	limiter.armTimerLocked(accountKey, bucket)
	return waiter, nil
}

// leaveQueue drops an ungranted waiter. The next head inherits the next
// naturally available token; no wake time is rewritten.
func (limiter *accountRateLimiter) leaveQueue(accountKey string, waiter *dataPlaneWaiter) {
	if limiter == nil || waiter == nil {
		return
	}
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if waiter.granted || waiter.elem == nil {
		return
	}
	bucket := limiter.buckets[accountKey]
	if bucket == nil {
		return
	}
	bucket.waiters.Remove(waiter.elem)
	waiter.elem = nil
	limiter.refillLocked(bucket, limiter.currentTime())
	limiter.grantReadyLocked(bucket)
	limiter.armTimerLocked(accountKey, bucket)
}

func (limiter *accountRateLimiter) bucketLocked(accountKey string) *tokenBucket {
	bucket := limiter.buckets[accountKey]
	if bucket != nil {
		return bucket
	}
	bucket = &tokenBucket{
		tokens:  float64(limiter.config.Burst),
		last:    limiter.currentTime(),
		waiters: list.New(),
	}
	limiter.buckets[accountKey] = bucket
	return bucket
}

func (limiter *accountRateLimiter) refillLocked(bucket *tokenBucket, now time.Time) {
	if !now.After(bucket.last) {
		return
	}
	bucket.tokens += now.Sub(bucket.last).Seconds() * limiter.config.RPS
	if bucket.tokens > float64(limiter.config.Burst) {
		bucket.tokens = float64(limiter.config.Burst)
	}
	bucket.last = now
}

func (limiter *accountRateLimiter) grantReadyLocked(bucket *tokenBucket) {
	for bucket.waiters.Len() > 0 && bucket.tokens >= 1 {
		elem := bucket.waiters.Front()
		waiter := elem.Value.(*dataPlaneWaiter)
		bucket.waiters.Remove(elem)
		waiter.elem = nil
		waiter.granted = true
		limiter.takeTokenLocked(bucket)
		close(waiter.grant)
	}
}

func (limiter *accountRateLimiter) takeTokenLocked(bucket *tokenBucket) {
	bucket.tokens--
	if limiter.observeGrant != nil {
		limiter.observeGrant(limiter.currentTime())
	}
}

func (limiter *accountRateLimiter) delaySecondsLocked(bucket *tokenBucket, ahead int) float64 {
	needed := float64(ahead+1) - bucket.tokens
	if needed <= 0 {
		return 0
	}
	return needed / limiter.config.RPS
}

func (limiter *accountRateLimiter) armTimerLocked(accountKey string, bucket *tokenBucket) {
	if bucket.stopTimer != nil {
		bucket.stopTimer()
		bucket.stopTimer = nil
	}
	if bucket.waiters.Len() == 0 {
		return
	}
	waitSeconds := limiter.delaySecondsLocked(bucket, 0)
	if waitSeconds <= 0 {
		limiter.refillLocked(bucket, limiter.currentTime())
		limiter.grantReadyLocked(bucket)
		if bucket.waiters.Len() == 0 {
			return
		}
		waitSeconds = limiter.delaySecondsLocked(bucket, 0)
		if waitSeconds <= 0 {
			return
		}
	}
	delay := durationFromSeconds(waitSeconds)
	bucket.stopTimer = limiter.scheduleAfter(delay, func() {
		limiter.onTokenTimer(accountKey)
	})
}

func (limiter *accountRateLimiter) onTokenTimer(accountKey string) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	bucket := limiter.buckets[accountKey]
	if bucket == nil {
		return
	}
	bucket.stopTimer = nil
	limiter.refillLocked(bucket, limiter.currentTime())
	limiter.grantReadyLocked(bucket)
	limiter.armTimerLocked(accountKey, bucket)
}

func (limiter *accountRateLimiter) queuedWaiters(accountKey string) int {
	if limiter == nil {
		return 0
	}
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	bucket := limiter.buckets[accountKey]
	if bucket == nil {
		return 0
	}
	return bucket.waiters.Len()
}

// durationFromSeconds converts a non-negative second count without wrapping
// to a negative time.Duration when the float product overflows int64.
func durationFromSeconds(seconds float64) time.Duration {
	if seconds <= 0 || math.IsNaN(seconds) {
		return 0
	}
	if math.IsInf(seconds, 0) || seconds >= maxTimerSeconds {
		return time.Duration(maxTimerSeconds * float64(time.Second))
	}
	nanos := seconds * float64(time.Second)
	if nanos >= float64(math.MaxInt64) {
		return time.Duration(math.MaxInt64)
	}
	delay := time.Duration(nanos)
	if delay < 0 {
		return time.Duration(math.MaxInt64)
	}
	return delay
}
