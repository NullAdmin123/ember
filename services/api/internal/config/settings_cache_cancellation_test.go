package config

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/konghang/ember/backend/internal/models"
)

// TestRuntimeSettingsCancellationDoesNotBackOff prevents one request's
// cancellation or deadline from becoming a cached outage for unrelated images.
func TestRuntimeSettingsCancellationDoesNotBackOff(t *testing.T) {
	for _, termination := range []struct {
		name string
		err  error
	}{
		{name: "canceled", err: context.Canceled},
		{name: "deadline", err: fmt.Errorf("fixture query: %w", context.DeadlineExceeded)},
	} {
		for _, keys := range [][]string{{PlaybackGatewayWebEnabledKey}, {PlaybackGatewayWebEnabledKey, LogLevelKey}} {
			t.Run(fmt.Sprintf("%s/%d_keys", termination.name, len(keys)), func(t *testing.T) {
				store := &settingsCacheStore{
					entries: make(map[string]settingsCacheEntry), inflight: make(map[string]*settingsCacheCall), generations: make(map[string]uint64),
				}
				calls := 0
				loader := func(keys []string) (map[string]models.Setting, error) {
					calls++
					if calls == 1 {
						return nil, termination.err
					}
					values := make(map[string]models.Setting)
					for _, key := range keys {
						values[key] = models.Setting{Key: key, Value: "true"}
					}
					return values, nil
				}
				if _, err := store.loadManyWithTTL(context.Background(), keys, time.Minute, loader); !errors.Is(err, termination.err) {
					t.Fatalf("first load error=%v, want request termination", err)
				}
				values, err := store.loadManyWithTTL(context.Background(), keys, time.Minute, loader)
				if err != nil || len(values) != len(keys) || calls != 2 {
					t.Fatalf("healthy retry values=%d error=%v loads=%d; want immediate successful reload", len(values), err, calls)
				}
			})
		}
	}
}

// TestRuntimeSettingsLiveWaiterRetriesCanceledLeader proves that a healthy
// image request refreshes with its own loader after a shared leader is canceled.
func TestRuntimeSettingsLiveWaiterRetriesCanceledLeader(t *testing.T) {
	for _, invalidate := range []bool{false, true} {
		t.Run(fmt.Sprintf("invalidate_%t", invalidate), func(t *testing.T) {
			store := &settingsCacheStore{
				entries: make(map[string]settingsCacheEntry), inflight: make(map[string]*settingsCacheCall), generations: make(map[string]uint64),
			}
			leaderContext, cancelLeader := context.WithCancel(context.Background())
			defer cancelLeader()
			started := make(chan struct{})
			leaderDone := make(chan error, 1)
			var loads atomic.Int32
			go func() {
				_, err := store.loadManyWithTTL(leaderContext, []string{PlaybackGatewayWebEnabledKey}, time.Minute, func([]string) (map[string]models.Setting, error) {
					loads.Add(1)
					close(started)
					<-leaderContext.Done()
					return nil, fmt.Errorf("fixture request: %w", leaderContext.Err())
				})
				leaderDone <- err
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("leader did not start")
			}
			waiterContext := &settingsCacheWaitContext{Context: context.Background(), waiting: make(chan struct{})}
			waiterDone := make(chan error, 1)
			go func() {
				values, err := store.loadManyWithTTL(waiterContext, []string{PlaybackGatewayWebEnabledKey}, time.Minute, func(keys []string) (map[string]models.Setting, error) {
					loads.Add(1)
					return map[string]models.Setting{keys[0]: {Key: keys[0], Value: "true"}}, nil
				})
				if err == nil && values[PlaybackGatewayWebEnabledKey].Value != "true" {
					err = errors.New("healthy waiter did not receive refreshed policy")
				}
				waiterDone <- err
			}()
			select {
			case <-waiterContext.waiting:
			case <-time.After(time.Second):
				t.Fatal("waiter did not join the inflight refresh")
			}
			if invalidate {
				store.invalidate(PlaybackGatewayWebEnabledKey)
			}
			cancelLeader()
			select {
			case err := <-leaderDone:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("leader error=%v, want cancellation", err)
				}
			case <-time.After(time.Second):
				t.Fatal("canceled leader did not return")
			}
			select {
			case err := <-waiterDone:
				if err != nil || loads.Load() != 2 {
					t.Fatalf("healthy waiter error=%v loads=%d, want one successful retry", err, loads.Load())
				}
			case <-time.After(time.Second):
				t.Fatal("healthy waiter did not retry")
			}
		})
	}
}

// TestRuntimeSettingsWaitingCallerCanCancel keeps a follower's cancellation
// independent from the live request that owns a shared database refresh.
func TestRuntimeSettingsWaitingCallerCanCancel(t *testing.T) {
	store := &settingsCacheStore{
		entries: make(map[string]settingsCacheEntry), inflight: make(map[string]*settingsCacheCall), generations: make(map[string]uint64),
	}
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseLeader := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseLeader()
	leaderDone := make(chan error, 1)
	go func() {
		_, err := store.loadManyWithTTL(context.Background(), []string{PlaybackGatewayWebEnabledKey}, time.Minute, func(keys []string) (map[string]models.Setting, error) {
			close(started)
			<-release
			return map[string]models.Setting{keys[0]: {Key: keys[0], Value: "true"}}, nil
		})
		leaderDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("leader did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiterContext := &settingsCacheWaitContext{Context: ctx, waiting: make(chan struct{})}
	waiterDone := make(chan error, 1)
	go func() {
		_, err := store.loadManyWithTTL(waiterContext, []string{PlaybackGatewayWebEnabledKey}, time.Minute, func([]string) (map[string]models.Setting, error) {
			return nil, errors.New("canceled waiter must not start a load")
		})
		waiterDone <- err
	}()
	select {
	case <-waiterContext.waiting:
	case <-time.After(time.Second):
		t.Fatal("waiter did not join")
	}
	cancel()
	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter error=%v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter remained blocked by the leader")
	}
	releaseLeader()
	select {
	case err := <-leaderDone:
		if err != nil {
			t.Fatalf("waiter canceled the leader: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("leader did not complete")
	}
}

// TestRuntimeSettingsCanceledCallerDoesNotUseCache prevents a terminated
// caller from joining or starting a refresh, even when a value is cached.
func TestRuntimeSettingsCanceledCallerDoesNotUseCache(t *testing.T) {
	store := &settingsCacheStore{
		entries:  map[string]settingsCacheEntry{PlaybackGatewayWebEnabledKey: {loadedAt: time.Now(), found: true}},
		inflight: make(map[string]*settingsCacheCall), generations: make(map[string]uint64),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := store.loadManyWithTTL(ctx, []string{PlaybackGatewayWebEnabledKey}, time.Minute, func([]string) (map[string]models.Setting, error) {
		t.Fatal("canceled caller invoked a loader")
		return nil, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled caller error=%v", err)
	}
}

// settingsCacheWaitContext exposes entry into the real wait select so tests
// can cancel a leader or follower without timing sleeps or scheduler guesses.
type settingsCacheWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

// Done records the wait boundary and preserves the embedded context channel.
func (ctx *settingsCacheWaitContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.waiting) })
	return ctx.Context.Done()
}
