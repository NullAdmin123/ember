package p115

import (
	"container/list"
	"context"
	"log"
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
	// maxDataPlaneRPS keeps refill delay meaningful. A huge finite RPS makes
	// needed/RPS ≈ 0 and refills the bucket on any elapsed tick.
	maxDataPlaneRPS = 100
	// maxDataPlaneBurst stays well below the 2^53 integer-exact range of
	// float64 tokens. Near-MaxInt burst makes tokens-- a no-op.
	maxDataPlaneBurst = 1000
	// maxDataPlaneMaxWait is the longest local queue a playback request may
	// sit before falling back. Multi-hour ParseDuration values would hang.
	maxDataPlaneMaxWait = time.Minute
	maxTimerSeconds     = float64(time.Hour/time.Second) * 24
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
	// timerGen identifies the currently armed handle. A callback that
	// started before stopTimer/re-arm must not clear or replace the
	// successor; only the matching generation may reschedule.
	timerGen uint64
}

type dataPlaneWaiter struct {
	grant      chan struct{}
	expired    chan struct{}
	expireOnce sync.Once
	elem       *list.Element
	granted    bool
	deadline   time.Time
	stopExpire func()
}

// ParseDataPlaneLimitConfig reads optional env overrides. Invalid values keep
// the conservative defaults rather than disabling protection. RPS=0 remains
// the explicit bypass; Inf/NaN, sub-minimum, negative, and negative-zero RPS
// keep the default. Burst and MaxWait must be positive. Values above the
// business caps (RPS 100, Burst 1000, MaxWait 1m) also keep the default and
// log, because oversized Burst/RPS silently disable pacing and a huge MaxWait
// holds playback instead of falling back.
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
				if parsed > maxDataPlaneRPS {
					logOversizedDataPlaneOverride("P115_DATA_PLANE_RPS", value, maxDataPlaneRPS, defaultDataPlaneRPS)
				} else {
					config.RPS = parsed
				}
			}
		}
	}
	if value := strings.TrimSpace(os.Getenv("P115_DATA_PLANE_BURST")); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
			if parsed > maxDataPlaneBurst {
				logOversizedDataPlaneOverride("P115_DATA_PLANE_BURST", value, maxDataPlaneBurst, defaultDataPlaneBurst)
			} else {
				config.Burst = parsed
			}
		}
	}
	if value := strings.TrimSpace(os.Getenv("P115_DATA_PLANE_MAX_WAIT")); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil && parsed > 0 {
			if parsed > maxDataPlaneMaxWait {
				logOversizedDataPlaneOverride("P115_DATA_PLANE_MAX_WAIT", value, maxDataPlaneMaxWait, defaultDataPlaneMaxWait)
			} else {
				config.MaxWait = parsed
			}
		}
	}
	return config
}

// logOversizedDataPlaneOverride records a rejected over-cap override. The
// caller keeps the conservative default, matching other invalid env values.
func logOversizedDataPlaneOverride(env, value string, max, keepDefault any) {
	log.Printf("[P115] 忽略超量数据面限流覆盖 env=%s value=%q max=%v keepDefault=%v", env, value, max, keepDefault)
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
// is ErrDataPlaneQueueLimited, including a real MaxWait deadline after
// enqueue. Canceled callers receive ctx.Err() and leave the queue without
// occupying a slot. A grant that races with expiry or cancel is used; an
// ungranted expiry returns the token to the bucket.
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
	return limiter.awaitWaiter(ctx, accountKey, waiter)
}

func (limiter *accountRateLimiter) awaitWaiter(ctx context.Context, accountKey string, waiter *dataPlaneWaiter) error {
	if waiter.expired == nil {
		select {
		case <-waiter.grant:
			waiter.stopDeadline()
			return nil
		case <-ctx.Done():
			return limiter.resolveWait(accountKey, waiter, ctx.Err())
		}
	}
	select {
	case <-waiter.grant:
		waiter.stopDeadline()
		return nil
	case <-ctx.Done():
		return limiter.resolveWait(accountKey, waiter, ctx.Err())
	case <-waiter.expired:
		return limiter.resolveWait(accountKey, waiter, ErrDataPlaneQueueLimited)
	}
}

// enqueue consumes an immediate token or appends a FIFO waiter. Theoretical
// wait above MaxWait is rejected immediately; accepted waiters also get a
// real deadline so a late token timer cannot hold them past MaxWait.
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
	if limiter.config.MaxWait > 0 {
		waiter.deadline = now.Add(limiter.config.MaxWait)
		waiter.expired = make(chan struct{})
	}
	waiter.elem = bucket.waiters.PushBack(waiter)
	limiter.armTimerLocked(accountKey, bucket)
	if waiter.expired != nil {
		waiter.stopExpire = limiter.scheduleAfter(limiter.config.MaxWait, waiter.signalExpired)
	}
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
		waiter.stopDeadline()
		return
	}
	waiter.stopDeadline()
	bucket := limiter.buckets[accountKey]
	if bucket == nil {
		waiter.elem = nil
		return
	}
	bucket.waiters.Remove(waiter.elem)
	waiter.elem = nil
	limiter.refillLocked(bucket, limiter.currentTime())
	limiter.grantReadyLocked(bucket)
	limiter.armTimerLocked(accountKey, bucket)
}

// resolveWait finishes a canceled or MaxWait-expired waiter. A concurrent
// grant keeps the token; otherwise the waiter leaves and the token stays.
func (limiter *accountRateLimiter) resolveWait(accountKey string, waiter *dataPlaneWaiter, err error) error {
	if limiter == nil || waiter == nil {
		return err
	}
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	waiter.stopDeadline()
	if waiter.granted {
		return nil
	}
	if waiter.elem == nil {
		return err
	}
	bucket := limiter.buckets[accountKey]
	if bucket == nil {
		waiter.elem = nil
		return err
	}
	bucket.waiters.Remove(waiter.elem)
	waiter.elem = nil
	limiter.refillLocked(bucket, limiter.currentTime())
	limiter.grantReadyLocked(bucket)
	limiter.armTimerLocked(accountKey, bucket)
	return err
}

func (waiter *dataPlaneWaiter) signalExpired() {
	if waiter == nil || waiter.expired == nil {
		return
	}
	waiter.expireOnce.Do(func() { close(waiter.expired) })
}

func (waiter *dataPlaneWaiter) stopDeadline() {
	if waiter == nil || waiter.stopExpire == nil {
		return
	}
	waiter.stopExpire()
	waiter.stopExpire = nil
}

func (waiter *dataPlaneWaiter) deadlineExpired(now time.Time) bool {
	return waiter != nil && !waiter.deadline.IsZero() && now.After(waiter.deadline)
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
	now := limiter.currentTime()
	for bucket.waiters.Len() > 0 && bucket.tokens >= 1 {
		elem := bucket.waiters.Front()
		waiter := elem.Value.(*dataPlaneWaiter)
		if waiter.deadlineExpired(now) {
			bucket.waiters.Remove(elem)
			waiter.elem = nil
			waiter.stopDeadline()
			waiter.signalExpired()
			continue
		}
		bucket.waiters.Remove(elem)
		waiter.elem = nil
		waiter.granted = true
		waiter.stopDeadline()
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

// disarmTimerLocked invalidates any in-flight callback and stops the
// current handle. AfterFunc Stop does not wait, so generation is the
// only safe identity once a callback may already be queued on mu.
func (limiter *accountRateLimiter) disarmTimerLocked(bucket *tokenBucket) {
	bucket.timerGen++
	if bucket.stopTimer != nil {
		bucket.stopTimer()
		bucket.stopTimer = nil
	}
}

func (limiter *accountRateLimiter) armTimerLocked(accountKey string, bucket *tokenBucket) {
	limiter.disarmTimerLocked(bucket)
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
	gen := bucket.timerGen
	bucket.stopTimer = limiter.scheduleAfter(delay, func() {
		limiter.onTokenTimer(accountKey, gen)
	})
}

func (limiter *accountRateLimiter) onTokenTimer(accountKey string, gen uint64) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	bucket := limiter.buckets[accountKey]
	if bucket == nil || bucket.timerGen != gen {
		return
	}
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
