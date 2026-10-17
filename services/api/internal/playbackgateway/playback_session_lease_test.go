package playbackgateway

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/konghang/ember/backend/internal/services/directplay"
	"github.com/konghang/ember/backend/internal/services/p115quota"
)

type fakePlaybackSessionService struct {
	mu     sync.Mutex
	events []directplay.PlaybackSessionEvent
	result directplay.PlaybackSessionEventResult
	err    error
}

type playbackSessionServiceFunc func(context.Context, directplay.PlaybackSessionEvent) (directplay.PlaybackSessionEventResult, error)

// HandlePlaybackSessionEvent lets a test observe the exact persistence context.
func (f playbackSessionServiceFunc) HandlePlaybackSessionEvent(ctx context.Context, event directplay.PlaybackSessionEvent) (directplay.PlaybackSessionEventResult, error) {
	return f(ctx, event)
}

// TestGatewayStoppedReleasesLeaseAfterRequestCancellation covers both an
// already-canceled update and disconnect after successful Emby forwarding.
func TestGatewayStoppedReleasesLeaseAfterRequestCancellation(t *testing.T) {
	for _, directUpdate := range []bool{false, true} {
		t.Run(fmt.Sprintf("directUpdate=%t", directUpdate), func(t *testing.T) {
			keys, err := p115quota.NewKeyDeriver("fixture-root-key")
			if err != nil {
				t.Fatal(err)
			}
			account, _ := keys.PlaybackAccountKey("100")
			h := &leaseIdentityHarness{keys: keys, account: account, store: p115quota.NewMemoryLeaseStore(), now: time.Now()}
			principal := fixturePrincipal()
			_, err = h.ResolveMediaPath(context.Background(), directplay.MediaPathResolveRequest{
				UserID: principal.User.ID, MappingID: principal.MappingID, DeviceID: principal.DeviceID, PlaySessionID: "session-1",
			})
			if err != nil {
				t.Fatal(err)
			}
			type contextKey struct{}
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), contextKey{}, "retained"))
			defer cancel()
			gateway := newVideoTestGateway(t, "http://emby.invalid", &fakeTokenService{principal: principal}, nil, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				cancel()
				return transferIntentHTTPResponse(r, http.StatusNoContent, ""), nil
			}), &bytes.Buffer{})
			var updateContext context.Context
			gateway.playbackSessionService = playbackSessionServiceFunc(func(ctx context.Context, event directplay.PlaybackSessionEvent) (directplay.PlaybackSessionEventResult, error) {
				updateContext = ctx
				deadline, bounded := ctx.Deadline()
				if ctx.Err() != nil || !bounded || time.Until(deadline) > 2*time.Second || ctx.Value(contextKey{}) != "retained" {
					t.Error("lease update did not preserve values with an independent two-second budget")
				}
				return h.HandlePlaybackSessionEvent(ctx, event)
			})
			if directUpdate {
				cancel()
				gateway.updatePlaybackSessionLease(ctx, principal, playbackSessionEventSnapshot{kind: playbackSessionEventStop, snapshotState: "recorded", itemID: "item-1", mediaSourceID: "source-1", playSessionID: "session-1"})
			} else {
				request := httptest.NewRequest(http.MethodPost, "/Sessions/Playing/Stopped", bytes.NewBufferString(`{"ItemId":"item-1","MediaSourceId":"source-1","PlaySessionId":"session-1"}`)).WithContext(ctx)
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set(accessTokenHeader, fixtureAccessToken)
				response := httptest.NewRecorder()
				gateway.ServeHTTP(response, request)
				if response.Code != http.StatusNoContent {
					t.Fatalf("forwarded stop status=%d", response.Code)
				}
			}
			usage, _ := h.store.AccountUsage(context.Background(), account, h.now)
			if !h.result.Found || usage.OccupiedStreams != 0 || updateContext == nil || updateContext.Err() != context.Canceled {
				t.Fatalf("stop found=%t occupied=%d; update context must be canceled on completion", h.result.Found, usage.OccupiedStreams)
			}
		})
	}
}

func (service *fakePlaybackSessionService) HandlePlaybackSessionEvent(
	_ context.Context,
	event directplay.PlaybackSessionEvent,
) (directplay.PlaybackSessionEventResult, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.events = append(service.events, event)
	return service.result, service.err
}

func (service *fakePlaybackSessionService) snapshot() []directplay.PlaybackSessionEvent {
	service.mu.Lock()
	defer service.mu.Unlock()
	return append([]directplay.PlaybackSessionEvent(nil), service.events...)
}

func TestGatewayUpdatesPlaybackLeaseOnlyAfterSuccessfulEmbyEvent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/emby/Sessions/Playing/Progress" {
			t.Fatalf("unexpected upstream path %s", request.URL.Path)
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	var logs bytes.Buffer
	gateway := newVideoTestGateway(t, upstream.URL, &fakeTokenService{principal: fixturePrincipal()}, nil, nil, &logs)
	leases := &fakePlaybackSessionService{result: directplay.PlaybackSessionEventResult{Found: true}}
	gateway.playbackSessionService = leases
	request := httptest.NewRequest(http.MethodPost, "/Sessions/Playing/Progress", bytes.NewBufferString(
		`{"ItemId":"item-1","MediaSourceId":"source-1","PlaySessionId":"session-1","PositionTicks":42,"IsPaused":true}`,
	))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(accessTokenHeader, fixtureAccessToken)
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("response = %d", response.Code)
	}
	events := leases.snapshot()
	if len(events) != 1 || events[0].UserID != "user-1" || events[0].MappingID != "mapping-1" ||
		events[0].DeviceID != "device-1" || events[0].PlaySessionID != "session-1" || !events[0].IsProgress || !events[0].IsPaused || events[0].Stopped {
		t.Fatalf("events = %+v", events)
	}
}

func TestGatewayDoesNotUpdatePlaybackLeaseAfterFailedEmbyEvent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadGateway)
	}))
	defer upstream.Close()

	gateway := newVideoTestGateway(t, upstream.URL, &fakeTokenService{principal: fixturePrincipal()}, nil, nil, &bytes.Buffer{})
	leases := &fakePlaybackSessionService{}
	gateway.playbackSessionService = leases
	request := httptest.NewRequest(http.MethodPost, "/Sessions/Playing/Stopped", bytes.NewBufferString(
		`{"ItemId":"item-1","MediaSourceId":"source-1","PlaySessionId":"session-1"}`,
	))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(accessTokenHeader, fixtureAccessToken)
	gateway.ServeHTTP(httptest.NewRecorder(), request)

	if events := leases.snapshot(); len(events) != 0 {
		t.Fatalf("failed upstream updated leases: %+v", events)
	}
}

func TestGatewayLeaseUpdateFailureDoesNotChangeSuccessfulEmbyResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	var logs bytes.Buffer
	gateway := newVideoTestGateway(t, upstream.URL, &fakeTokenService{principal: fixturePrincipal()}, nil, nil, &logs)
	gateway.logger = log.New(&logs, "", 0)
	gateway.playbackSessionService = &fakePlaybackSessionService{err: directplay.ErrRedisUnavailable}
	request := httptest.NewRequest(http.MethodPost, "/Sessions/Playing", bytes.NewBufferString(
		`{"ItemId":"item-1","MediaSourceId":"source-1","PlaySessionId":"session-1"}`,
	))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(accessTokenHeader, fixtureAccessToken)
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent || !bytes.Contains(logs.Bytes(), []byte("reasonCode=redis_unavailable")) {
		t.Fatalf("response=%d logs=%q", response.Code, logs.String())
	}
}
