package core

import (
	"context"
	"errors"
	"net/http"

	"github.com/adeelahmad/tether/internal/cdp"
	"github.com/adeelahmad/tether/internal/session"
)

// Error codes shared by the HTTP faces. They appear in JSON error bodies
// and in the X-Tether-Error response header.
const (
	CodeTabGone        = "tab_gone"
	CodeForbidden      = "forbidden"
	CodeBadRequest     = "bad_request"
	CodeNavigation     = "navigation_failed"
	CodeTimeout        = "timeout"
	CodeJavaScript     = "javascript_error"
	CodeBrowserGone    = "browser_disconnected"
	CodeInternal       = "internal_error"
	CodeUnauthorized   = "unauthorized"
	CodeNotImplemented = "not_implemented"
)

// Classify maps an error from the core to an HTTP status and a stable code.
func Classify(err error) (status int, code string) {
	var nav *cdp.NavigationError
	var js *cdp.JSError
	switch {
	case errors.Is(err, session.ErrTabGone), errors.Is(err, cdp.ErrNoSuchTab):
		return http.StatusGone, CodeTabGone
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden, CodeForbidden
	case errors.Is(err, ErrBadRequest):
		return http.StatusBadRequest, CodeBadRequest
	case errors.As(err, &nav):
		return http.StatusBadGateway, CodeNavigation
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, CodeTimeout
	case errors.As(err, &js):
		return http.StatusUnprocessableEntity, CodeJavaScript
	case errors.Is(err, cdp.ErrDisconnected):
		return http.StatusServiceUnavailable, CodeBrowserGone
	}
	return http.StatusInternalServerError, CodeInternal
}
