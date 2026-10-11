package directplay

import (
	"context"
	"errors"
	"testing"
	"time"

	p115 "github.com/konghang/ember/backend/internal/integrations/p115"
	"github.com/konghang/ember/backend/internal/services/p115account"
)

type delayedSourceRange struct{ *fakeProvider }

// HashFileRange simulates a stalled read-only source fetch, never a retained write.
func (*delayedSourceRange) HashFileRange(ctx context.Context, _ p115.Credential, _ p115.FileRangeRequest) (p115.FileRangeHash, error) {
	<-ctx.Done()
	return p115.FileRangeHash{}, ctx.Err()
}

// TestSourceReadBudgetFallsBackWithoutAccountFailure covers both source stages
// and caller cancellation, with no actual network or production-duration wait.
func TestSourceReadBudgetFallsBackWithoutAccountFailure(t *testing.T) {
	for _, rangeRead := range []bool{false, true} {
		health := &fakeAccountHealthReporter{}
		provider := newFakeProvider()
		service := newServiceWithDependencies(fakeAccountLoader{health: health}, provider, &fakeTaskStore{}, &fakeTaskLocker{})
		service.sourceReadBudget = time.Millisecond
		if rangeRead {
			service.provider = &delayedSourceRange{provider}
		} else {
			service.provider = &lifecycleProvider{fakeProvider: provider, resolve: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}
		}
		_, err := service.Resolve(context.Background(), fixtureResolveRequest())
		if !errors.Is(err, ErrSourceReadTimeout) {
			t.Fatalf("range=%t error=%v", rangeRead, err)
		}
		if rangeRead {
			if len(health.events) != 1 || health.events[0].outcome != p115account.RuntimeHealthSucceeded {
				t.Fatalf("source observation lost: %+v", health.events)
			}
		} else if len(health.events) != 0 {
			t.Fatal("local budget marked account unhealthy")
		}
		if len(provider.uploadRequests) != 0 {
			t.Fatal("timed out read started retained upload")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(sourceReadResult(ctx, ctx, nil), context.Canceled) {
		t.Fatal("caller cancellation lost")
	}
}

// TestSourceReadBudgetsAlignWithDirectoryFlight locks the production path
// budget to the shared 30s listing flight while range reads stay at 5s.
func TestSourceReadBudgetsAlignWithDirectoryFlight(t *testing.T) {
	service := &Service{}
	if service.sourcePathResolveLimit() != p115.SourceDirectoryResolveTimeout {
		t.Fatalf("path budget=%s, want %s", service.sourcePathResolveLimit(), p115.SourceDirectoryResolveTimeout)
	}
	if service.sourceRangeReadLimit() != sourceReadTimeout {
		t.Fatalf("range budget=%s, want %s", service.sourceRangeReadLimit(), sourceReadTimeout)
	}
	service.sourceReadBudget = time.Millisecond
	if service.sourcePathResolveLimit() != time.Millisecond || service.sourceRangeReadLimit() != time.Millisecond {
		t.Fatal("test seam must still shorten both budgets")
	}
}

// TestSourceDirectoryTimeoutDoesNotWriteAccountHealth treats a large-directory
// resolve budget as a local fallback, not an account-wide 115 outage.
func TestSourceDirectoryTimeoutDoesNotWriteAccountHealth(t *testing.T) {
	health := &fakeAccountHealthReporter{}
	provider := newFakeProvider()
	provider.resolveErr = p115.ErrSourceDirectoryTimeout
	service := newServiceWithDependencies(fakeAccountLoader{health: health}, provider, &fakeTaskStore{}, &fakeTaskLocker{})
	_, err := service.Resolve(context.Background(), fixtureResolveRequest())
	if !errors.Is(err, ErrSourceReadTimeout) {
		t.Fatalf("error=%v, want ErrSourceReadTimeout", err)
	}
	if len(health.events) != 0 {
		t.Fatalf("directory timeout wrote health: %+v", health.events)
	}
}
