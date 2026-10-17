package directplay

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	p115 "github.com/konghang/ember/backend/internal/integrations/p115"
	"github.com/konghang/ember/backend/internal/models"
	"github.com/konghang/ember/backend/internal/services/p115account"
)

// TestFileFailuresDoNotPoisonPlaybackHealth exercises actual Resolve branches,
// including task failure and candidate validation, rather than just the mapper.
func TestFileFailuresDoNotPoisonPlaybackHealth(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*fakeProvider)
		want      error
		taskFails bool
	}{
		{"file unavailable", func(p *fakeProvider) { p.downloadErr = p115.ErrFileUnavailable }, ErrFileUnavailable, false},
		{"expired link", func(p *fakeProvider) { p.downloadErr = p115.ErrDownloadURLExpired }, ErrFileUnavailable, false},
		{"empty candidate", func(p *fakeProvider) { p.download.URL = "" }, ErrFileUnavailable, false},
		{"expired candidate", func(p *fakeProvider) { p.download.ExpiresAt = time.Time{} }, ErrFileUnavailable, false},
		{"negative concurrency", func(p *fakeProvider) { p.download.ConcurrentOpenLimit = -1 }, ErrFileUnavailable, false},
		{"rapid upload rejected", func(p *fakeProvider) {
			p.searchResults = nil
			p.uploadResults = []p115.RapidUploadResult{{Status: p115.RapidUploadProviderRejected}}
		}, ErrRapidUploadUnavailable, true},
		{"invalid target", func(p *fakeProvider) {
			p.searchResults = nil
			p.targetFile.SHA1 = ""
		}, ErrTargetUnavailable, true},
		{"invalid challenge", func(p *fakeProvider) {
			p.searchResults = nil
			p.uploadResults = []p115.RapidUploadResult{{Status: p115.RapidUploadRangeChallenge}}
		}, ErrProviderProtocol, true},
		{"repeated challenge", func(p *fakeProvider) {
			p.searchResults = nil
			challenge := p115.RapidUploadResult{Status: p115.RapidUploadRangeChallenge, Challenge: &p115.RapidUploadChallenge{SignKey: "fixture-key", Range: p115.ByteRange{Start: 0, End: 99}}}
			p.uploadResults = []p115.RapidUploadResult{challenge, challenge}
		}, ErrProviderProtocol, true},
		{"duplicate target candidates", func(p *fakeProvider) {
			p.searchResults = [][]p115.File{{p.targetFile, p.targetFile}}
		}, ErrProviderProtocol, false},
		{"invalid target candidate", func(p *fakeProvider) {
			p.targetFile.SHA1 = ""
			p.searchResults = [][]p115.File{{p.targetFile}}
		}, ErrProviderProtocol, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := newFakeProvider()
			provider.searchResults = [][]p115.File{{provider.targetFile}}
			test.configure(provider)
			health := &fakeAccountHealthReporter{}
			store := &fakeTaskStore{}
			service := newServiceWithDependencies(fakeAccountLoader{health: health}, provider, store, &fakeTaskLocker{})
			candidate, err := service.Resolve(context.Background(), fixtureResolveRequest())
			if !errors.Is(err, test.want) || candidate.URL != "" {
				t.Fatalf("error=%v want=%v hasCandidate=%t", err, test.want, candidate.URL != "")
			}
			if len(health.events) != 1 || health.events[0].role != models.P115AccountRoleSource || health.events[0].outcome != p115account.RuntimeHealthSucceeded {
				t.Fatalf("file failure wrote playback health or lost valid source observation: %+v", health.events)
			}
			if (store.failedTaskID != "") != test.taskFails {
				t.Fatalf("task failure=%t want=%t", store.failedTaskID != "", test.taskFails)
			}
		})
	}
}

// TestProviderFileFailureMapping isolates file sentinels at every operation and
// preserves operation metadata for fallback diagnostics and failed tasks.
func TestProviderFileFailureMapping(t *testing.T) {
	for _, operation := range []string{failureOperationResolveSourcePath, failureOperationHashSourcePreID, failureOperationHashSourceChallenge,
		failureOperationRapidUpload, failureOperationRapidUploadRetry, failureOperationVerifyPlaybackTarget, failureOperationSearchPlaybackTarget, failureOperationGetDownloadURL} {
		failure := fmt.Errorf("fixture wrapper: %w", p115.ErrFileUnavailable)
		if _, ok := runtimeHealthOutcome(operation, failure); ok {
			t.Fatalf("%s wrote file failure to account health", operation)
		}
		mapped := mapProviderFailure(operation, failure)
		if !errors.Is(mapped, ErrFileUnavailable) || InspectFailure(mapped).ProviderOperation != operation {
			t.Fatalf("%s lost file scope or diagnostics: %v", operation, mapped)
		}
	}
	if code := providerFailureCode(p115.ErrFileUnavailable); code != "file_unavailable" {
		t.Fatalf("file failure task code=%s", code)
	}
}

// TestDownloadAccountFailuresRetainHealthOutcomes keeps account-wide errors on
// their existing health paths while file failures are excluded separately.
func TestDownloadAccountFailuresRetainHealthOutcomes(t *testing.T) {
	for _, test := range []struct {
		failure error
		want    p115account.RuntimeHealthOutcome
	}{
		{p115.ErrProviderUnavailable, p115account.RuntimeHealthProviderUnavailable},
		{p115.ErrProviderRateLimited, p115account.RuntimeHealthProviderRateLimited},
		{p115.ErrCredentialRejected, p115account.RuntimeHealthCredentialRejected},
		{p115.ErrProviderProtocol, p115account.RuntimeHealthProviderProtocol},
	} {
		if outcome, ok := runtimeHealthOutcome(failureOperationGetDownloadURL, test.failure); !ok || outcome != test.want {
			t.Fatalf("confirmed account error=%v outcome=%s reported=%t", test.failure, outcome, ok)
		}
	}
}
