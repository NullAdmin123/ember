package directplay

import (
	"context"
	"errors"
	"testing"
	"time"

	p115 "github.com/konghang/ember/backend/internal/integrations/p115"
	"github.com/konghang/ember/backend/internal/services/p115account"
	"github.com/konghang/ember/backend/internal/testutil/faketime"
)

// TestSourceReadBudgetFallsBackWithoutAccountFailure expires a directory
// budget on a fake clock without starting a retained upload or poisoning health.
func TestSourceReadBudgetFallsBackWithoutAccountFailure(t *testing.T) {
	clock := faketime.New(time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))
	health := &fakeAccountHealthReporter{}
	base := newFakeProvider()
	provider := &lifecycleProvider{fakeProvider: base, resolve: func(ctx context.Context) error {
		clock.Advance(30 * time.Second)
		return ctx.Err()
	}}
	service := newServiceWithDependencies(fakeAccountLoader{health: health}, provider, &fakeTaskStore{}, &fakeTaskLocker{})
	service.sourceReadTimeout = clock.WithTimeout
	_, err := service.Resolve(context.Background(), fixtureResolveRequest())
	if !errors.Is(err, ErrSourceReadTimeout) || len(health.events) != 0 || len(base.uploadRequests) != 0 {
		t.Fatalf("directory timeout=%v healthEvents=%d uploads=%d", err, len(health.events), len(base.uploadRequests))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(sourceReadResult(ctx, ctx, nil), context.Canceled) {
		t.Fatal("caller cancellation lost")
	}
}

// TestSourceReadBudgetsAlignWithDirectoryFlight locks the production path
// budget to the shared 30s listing flight; range budgets belong to the Provider.
func TestSourceReadBudgetsAlignWithDirectoryFlight(t *testing.T) {
	service := &Service{}
	if service.sourcePathResolveLimit() != p115.SourceDirectoryResolveTimeout {
		t.Fatalf("path budget=%s, want %s", service.sourcePathResolveLimit(), p115.SourceDirectoryResolveTimeout)
	}
	service.sourceReadBudget = time.Millisecond
	if service.sourcePathResolveLimit() != time.Millisecond {
		t.Fatal("test seam must still shorten the directory budget")
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

type inspectSourceRangeContext struct {
	*fakeProvider
	t             *testing.T
	reads, failAt int
}

// HashFileRange requires the Provider to own its stage budgets, then simulates
// a local expiry so the service must preserve its sentinel through mapping.
func (provider *inspectSourceRangeContext) HashFileRange(ctx context.Context, credential p115.Credential, request p115.FileRangeRequest) (p115.FileRangeHash, error) {
	if _, competing := ctx.Deadline(); competing {
		provider.t.Error("service added an outer source-range deadline")
	}
	provider.reads++
	if provider.reads == provider.failAt {
		return p115.FileRangeHash{}, p115.ErrSourceRangeTimeout
	}
	return provider.fakeProvider.HashFileRange(ctx, credential, request)
}

// TestSourceRangeDoesNotRaceProviderPhaseDeadlines locks both preID and
// challenge paths to the Provider-owned deadline and its original failure scope.
func TestSourceRangeDoesNotRaceProviderPhaseDeadlines(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		base := newFakeProvider()
		if failAt == 2 {
			base.uploadResults = []p115.RapidUploadResult{{Status: p115.RapidUploadRangeChallenge,
				Challenge: &p115.RapidUploadChallenge{SignKey: "fixture-key", Range: p115.ByteRange{Start: 0, End: 9}}}}
		}
		provider := &inspectSourceRangeContext{fakeProvider: base, t: t, failAt: failAt}
		service := newServiceWithDependencies(fakeAccountLoader{}, provider, &fakeTaskStore{}, &fakeTaskLocker{})
		_, err := service.Resolve(context.Background(), fixtureResolveRequest())
		if !errors.Is(err, ErrSourceReadTimeout) || provider.reads != failAt {
			t.Fatalf("read=%d error=%v", provider.reads, err)
		}
	}
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
