package p115

import (
	"fmt"
	"net/http"

	"github.com/konghang/ember/backend/internal/common/upstream"
)

// mapCookieAPIStatus classifies 115 Cookie/Web API HTTP statuses without
// inspecting response bodies. CDN Range reads do not use this mapper.
func mapCookieAPIStatus(status int) error {
	if status >= http.StatusOK && status < http.StatusMultipleChoices {
		return nil
	}
	detail := upstream.SafeUpstreamHTTPError("p115", status)
	switch status {
	case http.StatusTooManyRequests:
		return fmt.Errorf("%w: %v", ErrProviderRateLimited, detail)
	case http.StatusForbidden:
		return fmt.Errorf("%w: %v", ErrProviderRiskControl, detail)
	default:
		return fmt.Errorf("%w: %v", ErrProviderUnavailable, detail)
	}
}
