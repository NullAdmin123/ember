package p115

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"
)

// TestSourceRangeSlowPreparationLeavesAFullReadWindow reproduces the measured
// control-request delay with short mock budgets, not real 115/CDN calls.
func TestSourceRangeSlowPreparationLeavesAFullReadWindow(t *testing.T) {
	const phaseBudget = 120 * time.Millisecond
	adapter := newTestRangeAdapter(t, "1", func(request *http.Request) (*http.Response, error) {
		deadline, _ := request.Context().Deadline()
		if time.Until(deadline) < 80*time.Millisecond {
			t.Fatalf("slow signed URL consumed CDN read window: remaining=%s", time.Until(deadline))
		}
		select {
		case <-time.After(70 * time.Millisecond):
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
		return rangeHTTPResponse(http.StatusPartialContent, "bytes 0-9/1024", []byte("0123456789")), nil
	})
	adapter.sourceRangePhaseBudget = phaseBudget
	client := adapter.client
	adapter.client = httpDoerFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "range.test" {
			select {
			case <-time.After(70 * time.Millisecond):
			case <-request.Context().Done():
				return nil, request.Context().Err()
			}
		}
		return client.Do(request)
	})
	result, err := adapter.HashFileRange(context.Background(), fixtureCredential(), FileRangeRequest{
		File: File{ID: "789", PickCode: fixtureDownloadPickCode, Size: 1024}, Range: ByteRange{Start: 0, End: 9},
	})
	if err != nil || result.BytesRead != 10 {
		t.Fatalf("slow preparation caused a failed read: bytes=%d error=%v", result.BytesRead, err)
	}
}

type sourceBudgetBody struct {
	ctx    context.Context
	closed bool
}

// Read simulates a stalled response body that honors the HTTP request deadline.
func (body *sourceBudgetBody) Read([]byte) (int, error) {
	<-body.ctx.Done()
	return 0, body.ctx.Err()
}

// Close records cleanup of a failed body read without creating a socket.
func (body *sourceBudgetBody) Close() error { body.closed = true; return nil }

// TestSourceRangeBudgetExhaustionKeepsTheFailedPhase covers headers and bodies
// in both HTTP phases, including early caller cancellation and body cleanup.
func TestSourceRangeBudgetExhaustionKeepsTheFailedPhase(t *testing.T) {
	tests := []struct {
		stage                    string
		bodyRead, callerCanceled bool
	}{
		{"download_url", false, false},
		{"download_url", false, true},
		{"download_url", true, false},
		{"download_url", true, true},
		{"source_range", false, false},
		{"source_range", false, true},
		{"source_range", true, false},
		{"source_range", true, true},
	}
	for _, test := range tests {
		stage, bodyRead, callerCanceled := test.stage, test.bodyRead, test.callerCanceled
		name := stage
		if bodyRead {
			name += "/body"
		} else {
			name += "/headers"
		}
		if callerCanceled {
			name += "/caller_canceled"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var blockedBody *sourceBudgetBody
			var rangeCalls int
			adapter := newTestRangeAdapter(t, "1", func(*http.Request) (*http.Response, error) {
				return rangeHTTPResponse(http.StatusPartialContent, "bytes 0-9/1024", []byte("0123456789")), nil
			})
			adapter.sourceRangePhaseBudget = 10 * time.Millisecond
			client := adapter.client
			adapter.client = httpDoerFunc(func(request *http.Request) (*http.Response, error) {
				isRange := request.URL.Host == "range.test"
				if isRange {
					rangeCalls++
				}
				if isRange != (stage == "source_range") {
					return client.Do(request)
				}
				if callerCanceled {
					cancel()
				}
				if !bodyRead {
					<-request.Context().Done()
					return nil, request.Context().Err()
				}
				response, err := client.Do(request)
				if err != nil {
					return nil, err
				}
				_ = response.Body.Close()
				blockedBody = &sourceBudgetBody{ctx: request.Context()}
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
					t.Fatalf("lost failed phase: stage=%s reason=%s", gotStage, reason)
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

// TestSourceRangeHonorsEarlierCallerDeadline keeps the outer request budget
// authoritative even when each local phase would allow more time.
func TestSourceRangeHonorsEarlierCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	parentDeadline, _ := ctx.Deadline()
	adapter := newTestRangeAdapter(t, "1", func(request *http.Request) (*http.Response, error) {
		deadline, _ := request.Context().Deadline()
		if deadline.After(parentDeadline) {
			t.Fatal("local read extended caller deadline")
		}
		return &http.Response{StatusCode: http.StatusPartialContent, Header: http.Header{
			"Content-Range": {"bytes 0-9/1024"}, "Content-Length": {"10"},
		}, ContentLength: 10, Body: io.NopCloser(&sourceBudgetBody{ctx: request.Context()})}, nil
	})
	_, err := adapter.HashFileRange(ctx, fixtureCredential(), FileRangeRequest{
		File: File{ID: "789", PickCode: fixtureDownloadPickCode, Size: 1024}, Range: ByteRange{Start: 0, End: 9},
	})
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrSourceRangeTimeout) {
		t.Fatalf("caller deadline was reclassified as a local budget: %v", err)
	}
}

// TestSourceRangeStageDeadlinesAreIndependent checks real request deadlines:
// fetching a signed URL must not consume the subsequent CDN read window.
func TestSourceRangeStageDeadlinesAreIndependent(t *testing.T) {
	var downloadDeadline time.Time
	adapter := newTestRangeAdapter(t, "1", func(request *http.Request) (*http.Response, error) {
		deadline, ok := request.Context().Deadline()
		if !ok || !deadline.After(downloadDeadline) || time.Until(deadline) > 5*time.Second {
			t.Fatalf("Range did not receive a fresh bounded deadline: present=%t download=%s range=%s", ok, downloadDeadline, deadline)
		}
		return rangeHTTPResponse(http.StatusPartialContent, "bytes 0-9/1024", []byte("0123456789")), nil
	})
	client := adapter.client
	adapter.client = httpDoerFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "range.test" {
			var ok bool
			downloadDeadline, ok = request.Context().Deadline()
			if !ok || time.Until(downloadDeadline) > 5*time.Second {
				t.Fatalf("signed URL fetch lacks its own 5s bound: present=%t", ok)
			}
		}
		return client.Do(request)
	})
	_, err := adapter.HashFileRange(context.Background(), fixtureCredential(), FileRangeRequest{
		File: File{ID: "789", PickCode: fixtureDownloadPickCode, Size: 1024}, Range: ByteRange{Start: 0, End: 9},
	})
	if err != nil {
		t.Fatalf("HashFileRange error=%v", err)
	}
}
