package playbackgateway

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
)

const maxAuthenticationDiagnosticBytes = 16 * 1024

// authenticationRequestObservation copies only the bytes already consumed by
// the proxy. It never pre-reads, rewrites or logs credentials; Close is delegated.
type authenticationRequestObservation struct {
	io.ReadCloser
	mutex           sync.Mutex
	body            []byte
	readBytes       int64
	eof             bool
	readFailed      bool
	contentLength   int64
	contentType     string
	contentEncoding string
}

// observeAuthenticationRequest adds a bounded Debug sidecar without changing
// request headers, ContentLength, streaming order or the original Close method.
func observeAuthenticationRequest(request *http.Request) *authenticationRequestObservation {
	observation := &authenticationRequestObservation{
		ReadCloser:      request.Body,
		contentLength:   request.ContentLength,
		contentType:     request.Header.Get("Content-Type"),
		contentEncoding: request.Header.Get("Content-Encoding"),
	}
	if request.Body != nil && request.Body != http.NoBody {
		request.Body = observation
	} else {
		observation.eof = true
	}
	return observation
}

// Read preserves the underlying reader's bytes and error exactly. The mutex
// permits a response snapshot while a transport finishes consuming the body.
func (observation *authenticationRequestObservation) Read(p []byte) (int, error) {
	n, err := observation.ReadCloser.Read(p)
	observation.mutex.Lock()
	observation.readBytes += int64(n)
	if remaining := maxAuthenticationDiagnosticBytes - len(observation.body); remaining > 0 && n > 0 {
		observation.body = append(observation.body, p[:min(n, remaining)]...)
	}
	observation.eof = observation.eof || err == io.EOF
	observation.readFailed = observation.readFailed || (err != nil && err != io.EOF)
	observation.mutex.Unlock()
	return n, err
}

// logAuthenticationRequestShape reports fixed field states, never names or
// values. Incomplete, encoded, oversized and non-JSON bodies stay opaque.
func (gateway *Gateway) logAuthenticationRequestShape(requestID string, observation *authenticationRequestObservation) {
	observation.mutex.Lock()
	body := append([]byte(nil), observation.body...)
	readBytes, eof, readFailed := observation.readBytes, observation.eof, observation.readFailed
	observation.mutex.Unlock()
	contentType, _, contentTypeErr := mime.ParseMediaType(observation.contentType)
	contentTypeCode := "other"
	switch {
	case observation.contentType == "":
		contentTypeCode = "missing"
	case contentTypeErr != nil:
		contentTypeCode = "invalid"
	case contentType == "application/json":
		contentTypeCode = "json"
	case contentType == "application/x-www-form-urlencoded":
		contentTypeCode = "form"
	case contentType == "application/xml" || contentType == "text/xml":
		contentTypeCode = "xml"
	}
	bodyState := "valid_json"
	usernameState, pwState, passwordState := "unknown", "unknown", "unknown"
	encoding := strings.TrimSpace(observation.contentEncoding)
	switch {
	case readFailed:
		bodyState = "read_failed"
	case readBytes > maxAuthenticationDiagnosticBytes:
		bodyState = "too_large"
	case !eof && (observation.contentLength < 0 || readBytes != observation.contentLength):
		bodyState = "incomplete"
	case readBytes == 0:
		bodyState = "empty"
	case encoding != "" && !strings.EqualFold(encoding, "identity"):
		bodyState = "encoded"
	case contentTypeCode != "json":
		bodyState = "unsupported_media_type"
	default:
		states, valid := authenticationJSONFieldStates(body)
		if !valid {
			bodyState = "invalid_json"
		} else {
			usernameState, pwState, passwordState = states[0], states[1], states[2]
		}
	}
	gateway.debugf("[PlaybackGateway] level=debug code=authentication_request_shape requestId=%s contentType=%s bodyState=%s usernameState=%s pwState=%s passwordState=%s",
		requestID, contentTypeCode, bodyState, usernameState, pwState, passwordState)
}

// authenticationJSONFieldStates retains each top-level key occurrence so exact
// duplicates and case variants cannot be mistaken for an empty credential.
func authenticationJSONFieldStates(body []byte) ([3]string, bool) {
	states := [3]string{"missing", "missing", "missing"}
	if !json.Valid(body) {
		return states, false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	root, err := decoder.Token()
	if err != nil || root != json.Delim('{') {
		return states, false
	}
	var counts [3]int
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return states, false
		}
		key, ok := token.(string)
		if !ok {
			return states, false
		}
		var raw json.RawMessage
		if decoder.Decode(&raw) != nil {
			return states, false
		}
		index := -1
		switch strings.ToLower(key) {
		case "username":
			index = 0
		case "pw":
			index = 1
		case "password":
			index = 2
		}
		if index < 0 {
			continue
		}
		counts[index]++
		if counts[index] > 1 {
			states[index] = "duplicate"
		} else {
			states[index] = authenticationJSONValueState(raw)
		}
	}
	return states, true
}

// authenticationJSONValueState exposes only the type and emptiness of a value.
func authenticationJSONValueState(raw json.RawMessage) string {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "null"
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "invalid_type"
	}
	if value == "" {
		return "empty"
	}
	return "present"
}
