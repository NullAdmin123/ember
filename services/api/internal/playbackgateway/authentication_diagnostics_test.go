package playbackgateway

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGatewayAuthenticationDiagnosticsPreserveExchange locks Debug-only login
// observations to request shape while preserving upstream request/response bytes.
func TestGatewayAuthenticationDiagnosticsPreserveExchange(t *testing.T) {
	for _, test := range []struct {
		name, contentType, body string
		want                    []string
	}{
		{
			name: "plaintext password field", contentType: "application/json; charset=utf-8",
			body: `{"Username":"fixture-login-identity","Pw":"fixture-login-secret","Extension":"fixture-extension-secret"}`,
			want: []string{"bodyState=valid_json", "usernameState=present", "pwState=present", "passwordState=missing"},
		},
		{
			name: "legacy password field", contentType: "application/json",
			body: `{"Username":"fixture-login-identity","Password":"fixture-login-secret"}`,
			want: []string{"bodyState=valid_json", "usernameState=present", "pwState=missing", "passwordState=present"},
		},
		{
			name: "empty password", contentType: "application/json",
			body: `{"Username":"fixture-login-identity","Pw":""}`,
			want: []string{"bodyState=valid_json", "pwState=empty"},
		},
		{
			name: "ambiguous and invalid field types", contentType: "application/json",
			body: `{"Username":null,"Pw":123,"Password":"fixture-login-secret","password":"other-fixture-secret"}`,
			want: []string{"bodyState=valid_json", "usernameState=null", "pwState=invalid_type", "passwordState=duplicate"},
		},
		{
			name: "exact duplicate password keys", contentType: "application/json",
			body: `{"Username":"fixture-login-identity","Username":"other-fixture-secret","Pw":"fixture-login-secret","Pw":"","Password":"","Password":"fixture-login-secret"}`,
			want: []string{"bodyState=valid_json", "usernameState=duplicate", "pwState=duplicate", "passwordState=duplicate"},
		},
		{
			name: "nested names are not top level fields", contentType: "application/json",
			body: `{"Username":"fixture-login-identity","Extension":{"Username":"other-fixture-secret","Pw":"fixture-login-secret"}}`,
			want: []string{"bodyState=valid_json", "usernameState=present", "pwState=missing"},
		},
		{
			name: "empty body", contentType: "application/json", body: "",
			want: []string{"bodyState=empty", "pwState=unknown"},
		},
		{
			name: "invalid json", contentType: "application/json", body: `{"Pw":"fixture-login-secret"`,
			want: []string{"bodyState=invalid_json", "pwState=unknown"},
		},
		{
			name: "form remains opaque", contentType: "application/x-www-form-urlencoded",
			body: "Username=fixture-login-identity&Pw=fixture-login-secret",
			want: []string{"bodyState=unsupported_media_type", "pwState=unknown"},
		},
		{
			name: "bounded capture", contentType: "application/json",
			body: `{"Pw":"` + strings.Repeat("x", 32*1024) + `"}`,
			want: []string{"bodyState=too_large", "pwState=unknown"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			const responseBody = "fixture-upstream-authentication-response"
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != test.body || r.Header.Get("Content-Type") != test.contentType || r.URL.RawQuery != "fixture=keep" {
					t.Error("login request changed while collecting diagnostics")
				}
				w.Header().Set("X-Upstream", "preserved")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, responseBody)
			}))
			defer upstream.Close()
			var logs bytes.Buffer
			gateway := newTestGateway(t, upstream.URL, &fakeTokenService{}, &logs)
			request := httptest.NewRequest(http.MethodPost, authenticationPath+"?fixture=keep", strings.NewReader(test.body))
			request.Header.Set("Content-Type", test.contentType)
			response := httptest.NewRecorder()
			gateway.ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized || response.Body.String() != responseBody || response.Header().Get("X-Upstream") != "preserved" {
				t.Fatal("login response changed while collecting diagnostics")
			}
			for _, want := range append(test.want, "code=authentication_request_shape", "code=authentication_upstream_response", "statusCode=401") {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("missing diagnostic %s", want)
				}
			}
			assertSecretsAbsent(t, logs.String(), "fixture-login-identity", "fixture-login-secret", "fixture-extension-secret", "other-fixture-secret", responseBody)
		})
	}
}

// TestAuthenticationObservationPreservesReadAndCloseErrors prevents the
// diagnostic wrapper from hiding the transport's original failure semantics.
func TestAuthenticationObservationPreservesReadAndCloseErrors(t *testing.T) {
	readErr, closeErr := errors.New("fixture-read-secret"), errors.New("fixture-close-secret")
	body := &authenticationDiagnosticErrorBody{readErr: readErr, closeErr: closeErr}
	request := newAuthenticationRequest("")
	request.Body = body
	request.ContentLength = -1
	observation := observeAuthenticationRequest(request)
	buffer := make([]byte, 8)
	n, err := request.Body.Read(buffer)
	if n != 1 || buffer[0] != '{' || err != readErr {
		t.Fatal("diagnostic wrapper changed bytes or the read error")
	}
	if request.Body.Close() != closeErr || !body.closed {
		t.Fatal("diagnostic wrapper changed Close semantics")
	}
	var logs bytes.Buffer
	gateway := newTestGateway(t, "http://fixture.invalid", &fakeTokenService{}, &logs)
	gateway.logAuthenticationRequestShape("fixture-request", observation)
	if !strings.Contains(logs.String(), "bodyState=read_failed") {
		t.Fatal("failed body was interpreted as a complete credential DTO")
	}
	assertSecretsAbsent(t, logs.String(), readErr.Error(), closeErr.Error())
}

// TestAuthenticationObservationKeepsPartialAndEncodedBodiesOpaque guards the
// bounded observation path when the transport has not consumed a complete DTO.
func TestAuthenticationObservationKeepsPartialAndEncodedBodiesOpaque(t *testing.T) {
	for _, test := range []struct{ name, encoding, want string }{
		{name: "incomplete", want: "incomplete"},
		{name: "encoded", encoding: "gzip", want: "encoded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := newAuthenticationRequest(`{"Pw":"fixture-login-secret"}`)
			request.Header.Set("Content-Encoding", test.encoding)
			observation := observeAuthenticationRequest(request)
			if test.encoding != "" {
				_, _ = io.Copy(io.Discard, request.Body)
			}
			var logs bytes.Buffer
			gateway := newTestGateway(t, "http://fixture.invalid", &fakeTokenService{}, &logs)
			gateway.logAuthenticationRequestShape("fixture-request", observation)
			if !strings.Contains(logs.String(), "bodyState="+test.want) || !strings.Contains(logs.String(), "pwState=unknown") {
				t.Fatal("unavailable body was interpreted as a credential DTO")
			}
			assertSecretsAbsent(t, logs.String(), "fixture-login-secret")
		})
	}
}

// authenticationDiagnosticErrorBody supplies distinct read and close failures.
type authenticationDiagnosticErrorBody struct {
	readErr, closeErr error
	closed            bool
}

// Read returns a byte together with its error to exercise partial-read replay.
func (body *authenticationDiagnosticErrorBody) Read(p []byte) (int, error) {
	p[0] = '{'
	return 1, body.readErr
}

// Close records delegation and retains the exact fixture error.
func (body *authenticationDiagnosticErrorBody) Close() error {
	body.closed = true
	return body.closeErr
}

// TestGatewayAuthenticationDiagnosticsHonorDebug prevents request-body capture
// and extra authentication diagnostics when the runtime Debug policy is off.
func TestGatewayAuthenticationDiagnosticsHonorDebug(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()
	var logs bytes.Buffer
	gateway := newTestGateway(t, upstream.URL, &fakeTokenService{}, &logs)
	gateway.debugEnabled = func() bool { return false }
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, newAuthenticationRequest(`{"Username":"fixture-login-identity","Pw":"fixture-login-secret"}`))
	if response.Code != http.StatusUnauthorized || strings.Contains(logs.String(), "authentication_request_shape") || strings.Contains(logs.String(), "authentication_upstream_response") {
		t.Fatal("Debug-disabled login changed status or emitted diagnostics")
	}
}
