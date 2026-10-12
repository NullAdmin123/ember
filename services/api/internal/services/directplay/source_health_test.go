package directplay

import (
	"context"
	"errors"
	"testing"
	"time"

	p115 "github.com/konghang/ember/backend/internal/integrations/p115"
	"github.com/konghang/ember/backend/internal/services/p115account"
)

type failingSourceRangeProvider struct {
	*fakeProvider
	failAt       int
	invalid      bool
	callsToRange int
}

// HashFileRange fails only the selected source read, after path resolution
// succeeded, to test that subsequent failure wins over the earlier observation.
func (p *failingSourceRangeProvider) HashFileRange(ctx context.Context, credential p115.Credential, request p115.FileRangeRequest) (p115.FileRangeHash, error) {
	p.callsToRange++
	if p.callsToRange == p.failAt {
		if p.invalid {
			return p115.FileRangeHash{}, nil
		}
		return p115.FileRangeHash{}, p115.ErrProviderUnavailable
	}
	return p.fakeProvider.HashFileRange(ctx, credential, request)
}

type failingTerminalStore struct{ fakeTaskStore }

// MarkFailed simulates loss of task persistence after a real source failure.
func (*failingTerminalStore) MarkFailed(context.Context, string, string, string, time.Time) error {
	return errors.New("fixture store failure")
}

// TestSourceRecoveryDoesNotOverwriteLaterSourceFailure keeps real transport
// failures authoritative while isolating invalid file hashes, even if task persistence fails.
func TestSourceRecoveryDoesNotOverwriteLaterSourceFailure(t *testing.T) {
	for _, stage := range []int{1, 2} {
		for _, invalid := range []bool{false, true} {
			for _, failStore := range []bool{false, true} {
				health := &fakeAccountHealthReporter{}
				provider := &failingSourceRangeProvider{fakeProvider: newFakeProvider(), failAt: stage, invalid: invalid}
				if stage == 2 {
					provider.uploadResults = []p115.RapidUploadResult{{Status: p115.RapidUploadRangeChallenge, Challenge: &p115.RapidUploadChallenge{SignKey: "fixture-key", Range: p115.ByteRange{Start: 0, End: 99}}}}
				}
				service := newServiceWithDependencies(fakeAccountLoader{health: health}, provider, &fakeTaskStore{}, &fakeTaskLocker{})
				if failStore {
					service.store = &failingTerminalStore{}
				}
				_, err := service.Resolve(context.Background(), fixtureResolveRequest())
				want := p115account.RuntimeHealthProviderUnavailable
				if invalid {
					want = p115account.RuntimeHealthSucceeded
				}
				if err == nil || len(health.events) != 1 || health.events[0].outcome != want {
					t.Fatalf("stage=%d invalid=%t storeFailure=%t error=%v events=%+v", stage, invalid, failStore, err, health.events)
				}
			}
		}
	}
}

// TestSourceRangeHealthClassification rejects account-wide conclusions from
// file/CDN protocol errors but preserves explicit credential and transport failures.
func TestSourceRangeHealthClassification(t *testing.T) {
	for _, operation := range []string{failureOperationHashSourcePreID, failureOperationHashSourceChallenge} {
		for _, failure := range []error{p115.ErrProviderProtocol, p115.ErrProviderRejected, p115.ErrSourceRangeTimeout, ErrSourceReadTimeout, context.Canceled} {
			if _, ok := runtimeHealthOutcome(operation, failure); ok {
				t.Fatalf("file error poisoned account: %s %v", operation, failure)
			}
		}
		for _, failure := range []error{p115.ErrCredentialRejected, p115.ErrProviderUnavailable} {
			if _, ok := runtimeHealthOutcome(operation, failure); !ok {
				t.Fatalf("account failure ignored: %s %v", operation, failure)
			}
		}
	}
}

// TestRateLimitAndRiskControlStayAccountWideButNeverExpire covers sentinel
// mapping: real 429 and reserved risk-control stay brief cooling, never
// expired+disabled, including source path. Bare HTTP 403 is unavailable.
func TestRateLimitAndRiskControlStayAccountWideButNeverExpire(t *testing.T) {
	tests := []struct {
		err     error
		outcome p115account.RuntimeHealthOutcome
	}{
		{p115.ErrProviderRateLimited, p115account.RuntimeHealthProviderRateLimited},
		{p115.ErrProviderRiskControl, p115account.RuntimeHealthProviderRiskControl},
	}
	for _, operation := range []string{
		failureOperationResolveSourcePath, failureOperationHashSourcePreID,
		failureOperationRapidUpload, failureOperationGetDownloadURL,
	} {
		for _, test := range tests {
			outcome, ok := runtimeHealthOutcome(operation, test.err)
			if !ok || outcome != test.outcome {
				t.Fatalf("%s %v outcome=%s ok=%t", operation, test.err, outcome, ok)
			}
			mapped := mapProviderFailure(operation, test.err)
			if !errors.Is(mapped, ErrProviderUnavailable) {
				t.Fatalf("%s mapped=%v, want public unavailable fallback", operation, mapped)
			}
			if code := providerFailureCode(test.err); code != string(test.outcome) {
				t.Fatalf("failure code=%s, want %s", code, test.outcome)
			}
		}
	}
	if _, ok := runtimeHealthOutcome(failureOperationResolveSourcePath, p115.ErrSourceDirectoryTimeout); ok {
		t.Fatal("directory timeout must not write account health")
	}
	if code := providerFailureCode(p115.ErrSourceDirectoryTimeout); code != "source_directory_timeout" {
		t.Fatalf("directory timeout task code=%s, want source_directory_timeout", code)
	}
}

// TestLocalQueueLimitDoesNotWriteHealthOrShareRateLimitCode keeps MaxWait
// overflow off the account cooldown path while still allowing Emby fallback.
func TestLocalQueueLimitDoesNotWriteHealthOrShareRateLimitCode(t *testing.T) {
	for _, operation := range []string{
		failureOperationResolveSourcePath, failureOperationHashSourcePreID,
		failureOperationRapidUpload, failureOperationGetDownloadURL,
	} {
		if _, ok := runtimeHealthOutcome(operation, p115.ErrDataPlaneQueueLimited); ok {
			t.Fatalf("%s local queue overflow wrote account health", operation)
		}
		mapped := mapProviderFailure(operation, p115.ErrDataPlaneQueueLimited)
		if !errors.Is(mapped, ErrProviderUnavailable) {
			t.Fatalf("%s mapped=%v, want public unavailable fallback", operation, mapped)
		}
		if code := providerFailureCode(p115.ErrDataPlaneQueueLimited); code != "data_plane_queue_limited" {
			t.Fatalf("failure code=%s, want data_plane_queue_limited", code)
		}
		if code := providerFailureCode(p115.ErrDataPlaneQueueLimited); code == "provider_rate_limited" {
			t.Fatal("local queue overflow reused upstream 429 task code")
		}
	}
}
