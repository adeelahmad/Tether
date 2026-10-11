// Package logx builds Tether's slog loggers. Every logger it returns
// redacts secrets: attributes whose key names a credential (token,
// cookie, authorization, password, secret) are replaced, never printed.
package logx

import (
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
)

// Redacted replaces secret values in logs.
const Redacted = "[REDACTED]"

var secretKeys = []string{"token", "cookie", "authorization", "password", "secret", "apikey", "api_key", "set-cookie"}

// IsSecretKey reports whether an attribute or header name carries a secret.
func IsSecretKey(k string) bool {
	k = strings.ToLower(k)
	for _, s := range secretKeys {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// New returns a logger writing to w in "text" or "json" format at level
// ("debug", "info", "warn", "error").
func New(w io.Writer, level, format string) (*slog.Logger, error) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid log level %q", level)
	}
	opts := &slog.HandlerOptions{Level: lv, ReplaceAttr: redact}
	switch strings.ToLower(format) {
	case "", "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	}
	return nil, fmt.Errorf("invalid log format %q (want text or json)", format)
}

func redact(_ []string, a slog.Attr) slog.Attr {
	if IsSecretKey(a.Key) {
		return slog.String(a.Key, Redacted)
	}
	return a
}

// SafeURL renders u for logs without userinfo, query or fragment, which
// routinely carry tokens.
func SafeURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	c := *u
	c.User = nil
	if c.RawQuery != "" {
		c.RawQuery = "…"
	}
	c.Fragment = ""
	c.RawFragment = ""
	return Clean(c.String())
}

// Clean strips CR and LF from a client-supplied value before it is logged,
// so it can never start a new log line whatever handler is in use.
func Clean(s string) string {
	s = strings.ReplaceAll(s, "\n", "")
	return strings.ReplaceAll(s, "\r", "")
}
