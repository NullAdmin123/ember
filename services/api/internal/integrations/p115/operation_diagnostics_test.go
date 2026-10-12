package p115

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/konghang/ember/backend/internal/logging"
)

// TestSourceRangeTraceDoesNotIncludeSignedURLFetch keeps a successful control
// request's TLS/first-byte timing out of a failed CDN request's diagnostics.
func TestSourceRangeTraceDoesNotIncludeSignedURLFetch(t *testing.T) {
	previousOutput := log.Writer()
	previousDebug := logging.DebugEnabled()
	var output bytes.Buffer
	log.SetOutput(&output)
	if err := logging.ApplyLevel("debug"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		log.SetOutput(previousOutput)
		level := "info"
		if previousDebug {
			level = "debug"
		}
		_ = logging.ApplyLevel(level)
	})
	adapter := newTestRangeAdapter(t, "1", func(request *http.Request) (*http.Response, error) {
		httptrace.ContextClientTrace(request.Context()).WroteRequest(httptrace.WroteRequestInfo{})
		return nil, errors.New("fixture transport failure")
	})
	client := adapter.client
	adapter.client = httpDoerFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "range.test" {
			trace := httptrace.ContextClientTrace(request.Context())
			trace.TLSHandshakeStart()
			time.Sleep(3 * time.Millisecond)
			trace.TLSHandshakeDone(tls.ConnectionState{}, nil)
			trace.GotFirstResponseByte()
		}
		return client.Do(request)
	})
	_, err := adapter.HashFileRange(context.Background(), fixtureCredential(), FileRangeRequest{
		File: File{ID: "789", PickCode: fixtureDownloadPickCode, Size: 1024}, Range: ByteRange{Start: 0, End: 9},
	})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("error=%v", err)
	}
	var sourceLog string
	for _, line := range strings.Split(output.String(), "\n") {
		if strings.Contains(line, "code=provider_operation_completed stage=source_range ") {
			sourceLog = line
		}
	}
	for _, want := range []string{"networkPhase=waiting_headers", "statusCode=0", "tlsMs=0", "firstByteMs=0"} {
		if !strings.Contains(sourceLog, want) {
			t.Errorf("Range diagnostics include control-request timing: missing %s in %s", want, sourceLog)
		}
	}
	for _, secret := range []string{fixtureCredential().Cookie, "range.test", "provider.test", "encrypted-request"} {
		if strings.Contains(output.String(), secret) {
			t.Errorf("diagnostic log exposed fixture secret")
		}
	}
}

// TestRangeDiagnosticIdentifiesValidationFailure preserves exact bounded Range
// validation and reports its failure without exposing signed URLs or bodies.
func TestRangeDiagnosticIdentifiesValidationFailure(t *testing.T) {
	for _, test := range []struct {
		status         int
		header, reason string
	}{
		{200, "bytes 0-9/1024", "range_response_status_invalid"},
		{206, "bytes 1-10/1024", "range_response_content_range_mismatch"},
		{206, "", "range_response_content_range_missing"},
	} {
		adapter := newTestRangeAdapter(t, "1", func(*http.Request) (*http.Response, error) {
			return rangeHTTPResponse(test.status, test.header, []byte("0123456789")), nil
		})
		_, err := adapter.HashFileRange(context.Background(), fixtureCredential(), FileRangeRequest{File: File{ID: "789", PickCode: fixtureDownloadPickCode, Size: 1024}, Range: ByteRange{Start: 0, End: 9}})
		stage, reason, status := FailureDiagnostic(err)
		if !errors.Is(err, ErrProviderProtocol) || stage != "source_range" || reason != test.reason || status != test.status {
			t.Fatalf("stage=%s reason=%s status=%d error=%v", stage, reason, status, err)
		}
	}
}

// TestOperationDiagnosticRedactsNetworkErrors never forwards URL/DNS error text.
func TestOperationDiagnosticRedactsNetworkErrors(t *testing.T) {
	_, o := observeOperation(context.Background(), "source_parent")
	request, _ := http.NewRequest(http.MethodGet, "https://fixture.invalid/?secret=fixture-secret", nil)
	_, err := o.do(diagnosticHTTPFunc(func(*http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Get", URL: request.URL.String(), Err: &net.DNSError{Name: "fixture-secret", Err: "fixture-secret"}}
	}), request)
	err = o.finish(err)
	stage, reason, _ := FailureDiagnostic(err)
	if stage != "source_parent" || reason != "dns_error" || strings.Contains(err.Error(), "fixture-secret") {
		t.Fatalf("unsafe diagnostic: %s %s %v", stage, reason, err)
	}
}

type diagnosticHTTPFunc func(*http.Request) (*http.Response, error)

// Do provides an in-memory HTTP boundary; no external request is possible.
func (f diagnosticHTTPFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

// TestNestedOperationFailureKeepsDownloadStage distinguishes decrypt/JSON
// failures from failures in the subsequent source Range request.
func TestNestedOperationFailureKeepsDownloadStage(t *testing.T) {
	_, download := observeOperation(context.Background(), "download_url")
	_, source := observeOperation(context.Background(), "source_range")
	err := source.finish(download.finish(protocolError("download response decrypt failed")))
	stage, reason, _ := FailureDiagnostic(err)
	if stage != "download_url" || reason != "download_response_decrypt_failed" {
		t.Fatalf("lost deepest stage: %s %s", stage, reason)
	}
}

// TestOperationTraceRecordsOnlyTiming exercises transport hooks without a socket.
func TestOperationTraceRecordsOnlyTiming(t *testing.T) {
	ctx, o := observeOperation(context.Background(), "source_parent")
	trace := httptrace.ContextClientTrace(ctx)
	// Synchronous fake hooks can share one clock tick. Model elapsed time
	// explicitly so this checks measured durations rather than clock resolution.
	const elapsed = time.Millisecond
	o.started = o.started.Add(-elapsed)
	trace.DNSStart(httptrace.DNSStartInfo{Host: "fixture-secret"})
	o.dnsStarted = o.dnsStarted.Add(-elapsed)
	trace.DNSDone(httptrace.DNSDoneInfo{})
	trace.ConnectStart("tcp", "fixture-secret")
	o.connectStarted = o.connectStarted.Add(-elapsed)
	trace.ConnectDone("tcp", "fixture-secret", nil)
	trace.TLSHandshakeStart()
	o.tlsStarted = o.tlsStarted.Add(-elapsed)
	trace.TLSHandshakeDone(tls.ConnectionState{}, nil)
	trace.GotConn(httptrace.GotConnInfo{Reused: true})
	trace.WroteRequest(httptrace.WroteRequestInfo{})
	trace.GotFirstResponseByte()
	if o.firstByte <= 0 || o.dns <= 0 || o.connect <= 0 || o.tls <= 0 || !o.reused || o.phase != "response" {
		t.Fatalf("missing trace timing: dns=%s connect=%s tls=%s firstByte=%s reused=%t phase=%s", o.dns, o.connect, o.tls, o.firstByte, o.reused, o.phase)
	}
	if err := o.finish(nil); err != nil {
		t.Fatal(err)
	}
}

// TestDownloadPolicyDiagnosticDoesNotLeakTarget preserves the exact rejection
// category while excluding even sanitized hostname evidence from playback logs.
func TestDownloadPolicyDiagnosticDoesNotLeakTarget(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{&DownloadURLPolicyError{Reason: DownloadURLPolicyHostNotAllowed, Host: "fixture-secret"}, "download_host_not_allowed"},
		{&DownloadURLPolicyError{Reason: "fixture-secret", Host: "fixture-secret"}, "download_policy_rejected"},
		{ErrDownloadURLExpired, "download_url_expired"},
		{ErrDownloadURLIncompatible, "download_headers_incompatible"},
		{ErrDataPlaneQueueLimited, "data_plane_queue_limited"},
		{ErrProviderRateLimited, "provider_rate_limited"},
		{ErrProviderRiskControl, "provider_risk_control"},
		{ErrSourceDirectoryTimeout, "source_directory_timeout"},
		{ErrSourceRangeTimeout, "source_range_timeout"},
		{ErrProviderUnavailable, "provider_unavailable"},
	} {
		_, o := observeOperation(context.Background(), "download_url")
		err := o.finish(test.err)
		_, reason, _ := FailureDiagnostic(err)
		if reason != test.want || strings.Contains(err.Error(), "fixture-secret") {
			t.Fatalf("unsafe/missing reason: %s %v", reason, err)
		}
	}
}
