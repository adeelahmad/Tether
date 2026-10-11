package webdriver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/adeelahmad/tether/internal/cdp"
	"github.com/adeelahmad/tether/internal/core"
	"github.com/adeelahmad/tether/internal/session"
)

// W3C WebDriver error codes (https://www.w3.org/TR/webdriver2/#errors).
const (
	ErrElementClickIntercepted = "element click intercepted"
	ErrElementNotInteractable  = "element not interactable"
	ErrInvalidArgument         = "invalid argument"
	ErrInvalidSelector         = "invalid selector"
	ErrInvalidSessionID        = "invalid session id"
	ErrJavaScript              = "javascript error"
	ErrDetachedShadowRoot      = "detached shadow root"
	ErrNoSuchAlert             = "no such alert"
	ErrNoSuchCookie            = "no such cookie"
	ErrNoSuchElement           = "no such element"
	ErrNoSuchFrame             = "no such frame"
	ErrNoSuchShadowRoot        = "no such shadow root"
	ErrNoSuchWindow            = "no such window"
	ErrScriptTimeout           = "script timeout"
	ErrSessionNotCreated       = "session not created"
	ErrStaleElement            = "stale element reference"
	ErrTimeout                 = "timeout"
	ErrUnableToSetCookie       = "unable to set cookie"
	ErrUnexpectedAlertOpen     = "unexpected alert open"
	ErrUnknownCommand          = "unknown command"
	ErrUnknownError            = "unknown error"
	ErrUnsupportedOperation    = "unsupported operation"
)

var statusFor = map[string]int{
	ErrElementClickIntercepted: http.StatusBadRequest,
	ErrElementNotInteractable:  http.StatusBadRequest,
	ErrInvalidArgument:         http.StatusBadRequest,
	ErrInvalidSelector:         http.StatusBadRequest,
	ErrInvalidSessionID:        http.StatusNotFound,
	ErrJavaScript:              http.StatusInternalServerError,
	ErrDetachedShadowRoot:      http.StatusNotFound,
	ErrNoSuchAlert:             http.StatusNotFound,
	ErrNoSuchCookie:            http.StatusNotFound,
	ErrNoSuchElement:           http.StatusNotFound,
	ErrNoSuchFrame:             http.StatusNotFound,
	ErrNoSuchShadowRoot:        http.StatusNotFound,
	ErrNoSuchWindow:            http.StatusNotFound,
	ErrScriptTimeout:           http.StatusInternalServerError,
	ErrSessionNotCreated:       http.StatusInternalServerError,
	ErrStaleElement:            http.StatusNotFound,
	ErrTimeout:                 http.StatusInternalServerError,
	ErrUnableToSetCookie:       http.StatusInternalServerError,
	ErrUnexpectedAlertOpen:     http.StatusInternalServerError,
	ErrUnknownCommand:          http.StatusNotFound,
	ErrUnknownError:            http.StatusInternalServerError,
	ErrUnsupportedOperation:    http.StatusInternalServerError,
}

// Error is a WebDriver error with a W3C error code.
type Error struct {
	Code    string
	Message string
	Data    map[string]any // optional, e.g. the text of an unexpected alert
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func wdErr(code, msg string) *Error { return &Error{Code: code, Message: msg} }

// toError maps any error onto a W3C error.
func toError(err error) *Error {
	var we *Error
	var js *cdp.JSError
	var nav *cdp.NavigationError
	switch {
	case errors.As(err, &we):
		return we
	case errors.Is(err, session.ErrTabGone), errors.Is(err, cdp.ErrNoSuchTab):
		return wdErr(ErrNoSuchWindow, "the tab was closed: "+err.Error())
	case errors.Is(err, cdp.ErrNoSuchFrame):
		return wdErr(ErrNoSuchWindow, "the current frame is gone; switch to another frame: "+err.Error())
	case errors.Is(err, cdp.ErrDialogOpen):
		return wdErr(ErrUnexpectedAlertOpen, "a user prompt is open")
	case cdp.IsStaleObject(err):
		return wdErr(ErrStaleElement, "element is no longer attached to the page")
	case errors.As(err, &js):
		return wdErr(ErrJavaScript, js.Text)
	case errors.As(err, &nav):
		return wdErr(ErrUnknownError, nav.Text)
	case errors.Is(err, context.DeadlineExceeded):
		return wdErr(ErrTimeout, "operation timed out")
	case errors.Is(err, core.ErrForbidden):
		return wdErr(ErrUnsupportedOperation, err.Error())
	case errors.Is(err, core.ErrBadRequest):
		return wdErr(ErrInvalidArgument, err.Error())
	}
	return wdErr(ErrUnknownError, err.Error())
}

func writeValue(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"value": v})
}

func writeError(w http.ResponseWriter, err error) {
	e := toError(err)
	status, ok := statusFor[e.Code]
	if !ok {
		status = http.StatusInternalServerError
	}
	v := map[string]any{"error": e.Code, "message": e.Message, "stacktrace": ""}
	if e.Data != nil {
		v["data"] = e.Data
	}
	writeValue(w, status, v)
}
