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
// budget to the shared 30s listing flight and the range's two bounded HTTP phases.
func TestSourceReadBudgetsAlignWithDirectoryFlight(t *testing.T) {
	service := &Service{}
	if service.sourcePathResolveLimit() != p115.SourceDirectoryResolveTimeout {
		t.Fatalf("path budget=%s, want %s", service.sourcePathResolveLimit(), p115.SourceDirectoryResolveTimeout)
	}
	if service.sourceRangeReadLimit() != p115.SourceRangeReadTimeout {
		t.Fatalf("range budget=%s, want %s", service.sourceRangeReadLimit(), p115.SourceRangeReadTimeout)
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

type localSourceRangeTimeout struct{ *fakeProvider }

// HashFileRange returns the adapter's local phase timeout before the outer
// service context expires, as with a stalled CDN or signed-URL request.
func (*localSourceRangeTimeout) HashFileRange(context.Context, p115.Credential, p115.FileRangeRequest) (p115.FileRangeHash, error) {
	return p115.FileRangeHash{}, p115.ErrSourceRangeTimeout
}

// TestAdapterSourceRangeTimeoutDoesNotRetryTransfers covers both preID and
// challenge paths and does not convert a local deadline into account failure.
func TestAdapterSourceRangeTimeoutDoesNotRetryTransfers(t *testing.T) {
	for _, challenge := range []bool{false, true} {
		health := &fakeAccountHealthReporter{}
		base := newFakeProvider()
		service := newServiceWithDependencies(fakeAccountLoader{health: health}, base, &fakeTaskStore{}, &fakeTaskLocker{})
		if challenge {
			base.uploadResults = []p115.RapidUploadResult{{Status: p115.RapidUploadRangeChallenge,
				Challenge: &p115.RapidUploadChallenge{SignKey: "fixture-key", Range: p115.ByteRange{Start: 0, End: 9}}}}
			service.provider = &localChallengeTimeout{fakeProvider: base}
		} else {
			service.provider = &localSourceRangeTimeout{fakeProvider: base}
		}
		_, err := service.Resolve(context.Background(), fixtureResolveRequest())
		if !errors.Is(err, ErrSourceReadTimeout) {
			t.Fatalf("challenge=%t error=%v", challenge, err)
		}
		wantOperation := failureOperationHashSourcePreID
		if challenge {
			wantOperation = failureOperationHashSourceChallenge
		}
		if InspectFailure(err).ProviderOperation != wantOperation {
			t.Fatalf("lost failed read operation: got=%s want=%s", InspectFailure(err).ProviderOperation, wantOperation)
		}
		if len(health.events) != 1 || health.events[0].outcome != p115account.RuntimeHealthSucceeded {
			t.Fatalf("local timeout poisoned source health: %+v", health.events)
		}
		wantUploads := 0
		if challenge {
			wantUploads = 1
		}
		if len(base.uploadRequests) != wantUploads {
			t.Fatalf("read timeout started extra retained writes: calls=%d want=%d", len(base.uploadRequests), wantUploads)
		}
	}
}

type localChallengeTimeout struct {
	*fakeProvider
	reads int
}

// HashFileRange permits preID and fails the challenge without retrying upload.
func (provider *localChallengeTimeout) HashFileRange(ctx context.Context, credential p115.Credential, request p115.FileRangeRequest) (p115.FileRangeHash, error) {
	provider.reads++
	if provider.reads == 2 {
		return p115.FileRangeHash{}, p115.ErrSourceRangeTimeout
	}
	return provider.fakeProvider.HashFileRange(ctx, credential, request)
}
