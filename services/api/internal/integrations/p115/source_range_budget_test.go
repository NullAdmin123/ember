package p115

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/konghang/ember/backend/internal/testutil/faketime"
)

// TestSourceRangeSlowPreparationLeavesAFullReadWindow advances a fake clock
// through two full 5s windows; no outer timer may steal the failed-stage detail.
func TestSourceRangeSlowPreparationLeavesAFullReadWindow(t *testing.T) {
	clock := faketime.New(time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))
	var downloadDeadline time.Time
	adapter := newTestRangeAdapter(t, "1", func(request *http.Request) (*http.Response, error) {
		deadline, ok := request.Context().Deadline()
		if !ok || deadline.Sub(clock.Now()) != 5*time.Second || !deadline.After(downloadDeadline) {
			t.Fatal("Range did not receive a fresh independent 5s window")
		}
		clock.Advance(5 * time.Second)
		return nil, request.Context().Err()
	})
	adapter.sourceReadTimeout = clock.WithTimeout
	client := adapter.client
	adapter.client = httpDoerFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "range.test" {
			downloadDeadline, _ = request.Context().Deadline()
			if downloadDeadline.Sub(clock.Now()) != 5*time.Second {
				t.Fatal("download URL lacks its own 5s window")
			}
			clock.Advance(4*time.Second + 999*time.Millisecond)
		}
		return client.Do(request)
	})
	_, err := adapter.HashFileRange(context.Background(), fixtureCredential(), FileRangeRequest{
		File: File{ID: "789", PickCode: fixtureDownloadPickCode, Size: 1024}, Range: ByteRange{Start: 0, End: 9},
	})
	stage, reason, _ := FailureDiagnostic(err)
	if !errors.Is(err, ErrSourceRangeTimeout) || stage != "source_range" || reason != "request_deadline" {
		t.Fatalf("lost independent Range deadline: error=%v stage=%s reason=%s", err, stage, reason)
	}
}

type sourceBudgetBody struct {
	ctx        context.Context
	beforeRead func()
	closed     bool
}

// Read advances a test-owned clock before observing the request deadline.
func (body *sourceBudgetBody) Read([]byte) (int, error) {
	if body.beforeRead != nil {
		body.beforeRead()
	}
	<-body.ctx.Done()
	return 0, body.ctx.Err()
}

// Close records cleanup of a failed body read without creating a socket.
func (body *sourceBudgetBody) Close() error { body.closed = true; return nil }

// TestSourceRangeBudgetExhaustionKeepsTheFailedPhase covers both stages,
// headers/bodies, and caller cancellation without wall-clock deadlines.
func TestSourceRangeBudgetExhaustionKeepsTheFailedPhase(t *testing.T) {
	for _, stage := range []string{"download_url", "source_range"} {
		for _, bodyRead := range []bool{false, true} {
			for _, callerCanceled := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/body=%t/canceled=%t", stage, bodyRead, callerCanceled), func(t *testing.T) {
					clock := faketime.New(time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					expire := func() {
						if callerCanceled {
							cancel()
						} else {
							clock.Advance(5 * time.Second)
						}
					}
					var blockedBody *sourceBudgetBody
					rangeCalls := 0
					adapter := newTestRangeAdapter(t, "1", func(*http.Request) (*http.Response, error) {
						return rangeHTTPResponse(http.StatusPartialContent, "bytes 0-9/1024", []byte("0123456789")), nil
					})
					adapter.sourceReadTimeout = clock.WithTimeout
					client := adapter.client
					adapter.client = httpDoerFunc(func(request *http.Request) (*http.Response, error) {
						isRange := request.URL.Host == "range.test"
						if isRange {
							rangeCalls++
						}
						if isRange != (stage == "source_range") {
							return client.Do(request)
						}
						if !bodyRead {
							expire()
							return nil, request.Context().Err()
						}
						response, err := client.Do(request)
						if err != nil {
							return nil, err
						}
						_ = response.Body.Close()
						blockedBody = &sourceBudgetBody{ctx: request.Context(), beforeRead: expire}
						response.Body = blockedBody
						return response, nil
					})
					result, err := adapter.HashFileRange(ctx, fixtureCredential(), FileRangeRequest{
						File: File{ID: "789", PickCode: fixtureDownloadPickCode, Size: 1024}, Range: ByteRange{Start: 0, End: 9},
					})
					want := ErrSourceRangeTimeout
					if callerCanceled {
						want = context.Canceled
					}
					if !errors.Is(err, want) || result != (FileRangeHash{}) {
						t.Fatalf("result=%+v error=%v want=%v", result, err, want)
					}
					if !callerCanceled {
						gotStage, reason, _ := FailureDiagnostic(err)
						if gotStage != stage || reason != "request_deadline" {
							t.Fatalf("lost phase: %s/%s", gotStage, reason)
						}
					}
					if stage == "download_url" && rangeCalls != 0 {
						t.Fatal("failed URL fetch reached CDN")
					}
					if blockedBody != nil && !blockedBody.closed {
						t.Fatal("failed body was not closed")
					}
				})
			}
		}
	}
}

// TestSourceRangeHonorsEarlierCallerDeadline leaves caller expiry authoritative
// even when each local stage would otherwise allow another five seconds.
func TestSourceRangeHonorsEarlierCallerDeadline(t *testing.T) {
	clock := faketime.New(time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))
	ctx, cancel := clock.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	parentDeadline, _ := ctx.Deadline()
	adapter := newTestRangeAdapter(t, "1", func(request *http.Request) (*http.Response, error) {
		deadline, _ := request.Context().Deadline()
		if !deadline.Equal(parentDeadline) {
			t.Fatal("local read extended caller deadline")
		}
		clock.Advance(3 * time.Second)
		return nil, request.Context().Err()
	})
	adapter.sourceReadTimeout = clock.WithTimeout
	_, err := adapter.HashFileRange(ctx, fixtureCredential(), FileRangeRequest{
		File: File{ID: "789", PickCode: fixtureDownloadPickCode, Size: 1024}, Range: ByteRange{Start: 0, End: 9},
	})
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrSourceRangeTimeout) {
		t.Fatalf("caller deadline reclassified: %v", err)
	}
}
