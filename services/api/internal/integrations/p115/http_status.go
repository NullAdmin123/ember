package p115

import (
	"fmt"
	"net/http"

	"github.com/konghang/ember/backend/internal/common/upstream"
)

// mapCookieAPIStatus classifies 115 Cookie/Web API HTTP statuses without
// inspecting response bodies. CDN Range reads do not use this mapper.
//
// HTTP 429 maps to ErrProviderRateLimited because RFC 6585 / RFC 9110
// define 429 as Too Many Requests. Bare HTTP 403 is Forbidden for many
// reasons (file ACL, request authorization) and stays a generic upstream
// failure. ErrProviderRiskControl is reserved until a verified 115
// structured risk-control code exists; this mapper does not produce it.
func mapCookieAPIStatus(status int) error {
	if status >= http.StatusOK && status < http.StatusMultipleChoices {
		return nil
	}
	detail := upstream.SafeUpstreamHTTPError("p115", status)
	if status == http.StatusTooManyRequests {
		return fmt.Errorf("%w: %v", ErrProviderRateLimited, detail)
	}
	return fmt.Errorf("%w: %v", ErrProviderUnavailable, detail)
}
