package p115

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"github.com/konghang/ember/backend/internal/logging"
)

// TestRapidUploadUsesOperationObservation locks trace, stage, cancellation and
// redaction while keeping the upload-info request as a separate observation.
func TestRapidUploadUsesOperationObservation(t *testing.T) {
	for _, failure := range []string{"success", "transport", "canceled", "body_canceled"} {
		t.Run(failure, func(t *testing.T) {
			var output bytes.Buffer
			previousOutput, previousDebug := log.Writer(), logging.DebugEnabled()
			log.SetOutput(&output)
			_ = logging.ApplyLevel("debug")
			t.Cleanup(func() {
				log.SetOutput(previousOutput)
				level := "info"
				if previousDebug {
					level = "debug"
				}
				_ = logging.ApplyLevel(level)
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ciphertext := encryptedUploadResponse(t, `{"status":2}`)
			adapter, err := newCookieHTTPAdapter(httpDoerFunc(func(request *http.Request) (*http.Response, error) {
				if request.Method == http.MethodGet {
					return jsonHTTPResponse(http.StatusOK, `{"state":true,"user_id":12345,"userkey":"fixture-key"}`), nil
				}
				trace := httptrace.ContextClientTrace(request.Context())
				if trace == nil {
					t.Error("rapid upload did not install HTTP trace")
				} else {
					trace.DNSStart(httptrace.DNSStartInfo{Host: "fixture-secret"})
					trace.DNSDone(httptrace.DNSDoneInfo{})
					trace.GotFirstResponseByte()
				}
				if failure == "transport" {
					return nil, errors.New("fixture-secret")
				}
				if failure == "canceled" {
					cancel()
					return nil, request.Context().Err()
				}
				response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(ciphertext))}
				if failure == "body_canceled" {
					response.Body = &failingCookieBody{err: errors.New("fixture-secret"), cancel: cancel}
				}
				return response, nil
			}), cookieUploadInfoURL, cookieSHASearchURL, cookieUploadInitURL)
			if err != nil {
				t.Fatal(err)
			}
			adapter.now = func() time.Time { return time.Unix(fixtureUploadTimestamp, 0) }
			result, err := adapter.InitRapidUpload(ctx, fixtureCredential(), fixtureRapidUploadRequest())
			if failure == "success" {
				if err != nil || result.Status != RapidUploadReused {
					t.Fatalf("status=%s error=%v", result.Status, err)
				}
			} else {
				want := ErrProviderUnavailable
				if failure != "transport" {
					want = context.Canceled
				}
				stage, _, _ := FailureDiagnostic(err)
				if !errors.Is(err, want) || stage != "rapid_upload" {
					t.Fatalf("error=%v want=%v stage=%s", err, want, stage)
				}
			}
			if !strings.Contains(output.String(), "stage=rapid_upload ") {
				t.Fatal("missing rapid upload operation log")
			}
			for _, secret := range []string{"fixture-secret", fixtureCookie, "k_ec", string(ciphertext)} {
				if strings.Contains(output.String(), secret) {
					t.Fatal("upload observation leaked request/response material")
				}
			}
		})
	}
}
