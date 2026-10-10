package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/adeelahmad/tether/internal/core"
	"github.com/adeelahmad/tether/internal/httpx"
	"github.com/adeelahmad/tether/internal/proxy"
)

// ErrPublicWithoutToken is returned when asked to listen beyond loopback
// with no token and no explicit opt-out.
var ErrPublicWithoutToken = errors.New("refusing to listen on a non-loopback address without --token " +
	"(this exposes a logged-in browser); set a token, or pass --insecure-allow-public if you really mean it")

// CheckBind enforces the exposure rule: loopback is always fine; anything
// else needs a token or an explicit --insecure-allow-public.
func CheckBind(ctx context.Context, addr, token string, insecureAllowPublic bool) error {
	loop, err := IsLoopbackAddr(ctx, addr)
	if err != nil {
		return err
	}
	if loop || token != "" || insecureAllowPublic {
		return nil
	}
	return ErrPublicWithoutToken
}

// IsLoopbackAddr reports whether every address host resolves to is
// loopback. An empty host (":8787") means all interfaces, so it is not.
func IsLoopbackAddr(ctx context.Context, addr string) (bool, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false, fmt.Errorf("invalid --addr %q: %w", addr, err)
	}
	if host == "" {
		return false, nil
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback(), nil
	}
	if strings.EqualFold(host, "localhost") {
		return true, nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return false, fmt.Errorf("resolving --addr host %q: %w", host, err)
	}
	for _, ip := range ips {
		if !ip.IP.IsLoopback() {
			return false, nil
		}
	}
	return len(ips) > 0, nil
}

// authenticator checks the shared bearer token. Proxy requests carry it in
// Proxy-Authorization (Bearer, or Basic with the token as password, which
// is what `curl -x http://tether:TOKEN@host:port` sends); everything else
// carries it in Authorization.
type authenticator struct {
	sum [32]byte
}

func newAuthenticator(token string) *authenticator {
	if token == "" {
		return nil
	}
	return &authenticator{sum: sha256.Sum256([]byte(token))}
}

func (a *authenticator) match(candidate string) bool {
	s := sha256.Sum256([]byte(candidate))
	return subtle.ConstantTimeCompare(s[:], a.sum[:]) == 1
}

func (a *authenticator) check(header string) bool {
	scheme, cred, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok {
		return false
	}
	cred = strings.TrimSpace(cred)
	switch strings.ToLower(scheme) {
	case "bearer":
		return a.match(cred)
	case "basic":
		raw, err := base64.StdEncoding.DecodeString(cred)
		if err != nil {
			return false
		}
		_, pass, ok := strings.Cut(string(raw), ":")
		return ok && a.match(pass)
	}
	return false
}

func (a *authenticator) wrap(next http.Handler) http.Handler {
	if a == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if proxy.IsProxyRequest(r) {
			if !a.check(r.Header.Get("Proxy-Authorization")) {
				w.Header().Set("Proxy-Authenticate", `Basic realm="tether"`)
				httpx.WriteError(w, http.StatusProxyAuthRequired, core.CodeUnauthorized, "proxy authentication required")
				return
			}
			r.Header.Del("Proxy-Authorization")
		} else if !a.check(r.Header.Get("Authorization")) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="tether"`)
			httpx.WriteError(w, http.StatusUnauthorized, core.CodeUnauthorized, "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// localGuard protects token-less loopback deployments from the browser
// itself: web pages can send requests to 127.0.0.1, and DNS rebinding can
// make them same-origin. With no token, Tether's own endpoints only
// answer requests whose Host is a loopback name or IP and that carry no
// cross-site Origin. (With a token, browsers cannot authenticate anyway.)
func localGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if proxy.IsProxyRequest(r) {
			next.ServeHTTP(w, r)
			return
		}
		if !isLoopbackHost(r.Host) {
			httpx.WriteError(w, http.StatusForbidden, core.CodeForbidden, "host not allowed; use 127.0.0.1 or localhost, or set --token")
			return
		}
		if o := r.Header.Get("Origin"); o != "" && o != "null" {
			if u, err := url.Parse(o); err != nil || !isLoopbackHost(u.Host) {
				httpx.WriteError(w, http.StatusForbidden, core.CodeForbidden, "cross-origin requests are not allowed")
				return
			}
		}
		if sf := r.Header.Get("Sec-Fetch-Site"); sf == "cross-site" {
			httpx.WriteError(w, http.StatusForbidden, core.CodeForbidden, "cross-site requests are not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
