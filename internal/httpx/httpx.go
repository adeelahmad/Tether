// Package httpx holds small HTTP helpers shared by Tether's faces.
package httpx

import (
	"encoding/json"
	"net/http"
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
