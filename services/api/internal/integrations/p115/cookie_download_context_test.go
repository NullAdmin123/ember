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

// TestGetDownloadURLPreservesCallerContext covers cancellation and deadlines
// before HTTP, during headers/body, and while decoding a successful response.
func TestGetDownloadURLPreservesCallerContext(t *testing.T) {
	for _, stage := range []string{"before", "headers", "body", "decrypt"} {
		for _, deadline := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/deadline=%t", stage, deadline), func(t *testing.T) {
				clock := faketime.New(time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))
				ctx, cancel := context.WithCancel(context.Background())
				var requestContext context.Context = ctx
				want := context.Canceled
				if deadline {
					requestContext, cancel = clock.WithTimeout(context.Background(), time.Second)
					want = context.DeadlineExceeded
				}
				defer cancel()
				expire := func() {
					if deadline {
						clock.Advance(time.Second)
					} else {
						cancel()
					}
				}
				if stage == "before" {
					expire()
				}
				calls := 0
				var body *sourceBudgetBody
				adapter, err := newCookieHTTPAdapter(httpDoerFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					if stage == "headers" {
						expire()
						return nil, r.Context().Err()
					}
					response := jsonHTTPResponse(http.StatusOK, `{"state":true,"data":"fixture-response"}`)
					if stage == "body" {
						body = &sourceBudgetBody{ctx: r.Context(), beforeRead: expire}
						response.Body = body
					}
					return response, nil
				}), cookieUploadInfoURL, cookieSHASearchURL, cookieUploadInitURL)
				if err != nil {
					t.Fatal(err)
				}
				adapter.now = func() time.Time { return time.Unix(fixtureUploadTimestamp, 0) }
				adapter.downloadEncrypt = func([]byte) (string, error) { return "fixture-request", nil }
				adapter.downloadDecrypt = func(string) ([]byte, error) {
					if stage == "decrypt" {
						expire()
					}
					return downloadResponseJSON(fixtureDownloadPickCode, fixtureDirectDownloadURL("0", "1", fixtureDownloadExpiry)), nil
				}
				result, err := adapter.GetDownloadURL(requestContext, fixtureCredential(), DownloadURLRequest{PickCode: fixtureDownloadPickCode, UserAgent: fixtureClientUserAgent})
				if !errors.Is(err, want) || result != (DownloadURLResult{}) {
					t.Fatalf("context error=%v want=%v returnedURL=%t", err, want, result.URL != "")
				}
				if stage == "before" && calls != 0 {
					t.Fatal("pre-canceled request reached HTTP")
				}
				if body != nil && !body.closed {
					t.Fatal("canceled response body leaked")
				}
			})
		}
	}
}
