// Package httpx holds small HTTP helpers shared by Tether's faces.
package httpx

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// ErrorBody is the JSON shape of every Tether (non-WebDriver) error.
type ErrorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// WriteJSON writes v as JSON with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// WriteError writes a JSON error and the X-Tether-Error header.
func WriteError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("X-Tether-Error", code)
	WriteJSON(w, status, ErrorBody{Error: code, Message: msg})
}

// IsHTTPS reports whether the browser reached us over HTTPS, directly or
// through a TLS-terminating proxy such as `tailscale serve`. Cookies are
// only marked Secure then: Tether usually serves plain HTTP on loopback or
// a tailnet, where browsers refuse to store Secure cookies at all.
func IsHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// SameOrigin reports whether a form post came from a page on this server
// (directly or via a reverse proxy that forwards the host). Modern browsers
// say so in Sec-Fetch-Site; otherwise the Origin must match. A request with
// neither (curl, very old browsers) cannot be a cross-site form post from a
// modern browser.
func SameOrigin(r *http.Request) bool {
	if sf := r.Header.Get("Sec-Fetch-Site"); sf != "" {
		return sf == "same-origin" || sf == "none"
	}
	o := r.Header.Get("Origin")
	if o == "" || o == "null" {
		return o == ""
	}
	u, err := url.Parse(o)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host) ||
		(r.Header.Get("X-Forwarded-Host") != "" && strings.EqualFold(u.Host, r.Header.Get("X-Forwarded-Host")))
}
