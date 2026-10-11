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
	"github.com/adeelahmad/tether/internal/oauth"
	"github.com/adeelahmad/tether/internal/policy"
	"github.com/adeelahmad/tether/internal/proxy"
)

// ErrPublicWithoutToken is returned when asked to listen beyond loopback
// with no token and no explicit opt-out.
var ErrPublicWithoutToken = errors.New("refusing to listen on a non-loopback address without --token " +
	"(this exposes a logged-in browser); set a token, or pass --insecure-allow-public if you really mean it")

// CheckBind enforces the exposure rule: loopback is always fine; anything
// else needs a token or an explicit --insecure-allow-public.
func CheckBind(ctx context.Context, addr string, hasToken, insecureAllowPublic bool) error {
	loop, err := IsLoopbackAddr(ctx, addr)
	if err != nil {
		return err
	}
	if loop || hasToken || insecureAllowPublic {
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

// authenticator checks bearer tokens and attaches the matching principal
// to the request context, where the session manager and the policy read
// it. Proxy requests carry the token in Proxy-Authorization (Bearer, or
// Basic with the token as password, which is what
// `curl -x http://tether:TOKEN@host:port` sends); everything else carries
// it in Authorization.
type authenticator struct {
	creds []credential
	// With OAuth, its access tokens also authenticate requests under
	// mcpPrefix (and only there), and 401s there point at its metadata.
	oauth     *oauth.Server
	mcpPrefix string
}

type credential struct {
	sum [32]byte
	p   *policy.Principal
}

func newAuthenticator(creds []policy.Credential) *authenticator {
	if len(creds) == 0 {
		return nil
	}
	a := &authenticator{}
	for _, c := range creds {
		a.creds = append(a.creds, credential{sum: sha256.Sum256([]byte(c.Token)), p: c.Principal})
	}
	return a
}

// match compares candidate against every token in constant time, so the
// response time says nothing about which token, if any, was close.
func (a *authenticator) match(candidate string) *policy.Principal {
	s := sha256.Sum256([]byte(candidate))
	var found *policy.Principal
	for _, c := range a.creds {
		if subtle.ConstantTimeCompare(s[:], c.sum[:]) == 1 {
			found = c.p
		}
	}
	return found
}

func (a *authenticator) check(header string) *policy.Principal {
	scheme, cred, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok {
		return nil
	}
	cred = strings.TrimSpace(cred)
	switch strings.ToLower(scheme) {
	case "bearer":
		return a.match(cred)
	case "basic":
		raw, err := base64.StdEncoding.DecodeString(cred)
		if err != nil {
			return nil
		}
		_, pass, ok := strings.Cut(string(raw), ":")
		if !ok {
			return nil
		}
		return a.match(pass)
	}
	return nil
}

func (a *authenticator) wrap(next http.Handler, uiPrefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isProxy := proxy.IsProxyRequest(r)
		if !isProxy && underPrefix(r.URL.Path, uiPrefix) {
			uiGate(a, uiPrefix, w, r, next)
			return
		}
		if a == nil {
			next.ServeHTTP(w, r)
			return
		}
		var p *policy.Principal
		if isProxy {
			if p = a.check(r.Header.Get("Proxy-Authorization")); p == nil {
				w.Header().Set("Proxy-Authenticate", `Basic realm="tether"`)
				httpx.WriteError(w, http.StatusProxyAuthRequired, core.CodeUnauthorized, "proxy authentication required")
				return
			}
			r.Header.Del("Proxy-Authorization")
		} else if p = a.check(r.Header.Get("Authorization")); p == nil {
			if a.oauth == nil || !underPrefix(r.URL.Path, a.mcpPrefix) {
				a.deny(w)
				return
			}
			if p = a.checkOAuth(r.Header.Get("Authorization")); p == nil {
				w.Header().Set("WWW-Authenticate", a.oauth.Challenge(r))
				httpx.WriteError(w, http.StatusUnauthorized, core.CodeUnauthorized, "missing or invalid bearer token")
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(policy.WithPrincipal(r.Context(), p)))
	})
}

func (a *authenticator) checkOAuth(header string) *policy.Principal {
	scheme, tok, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return nil
	}
	return a.oauth.Verify(strings.TrimSpace(tok))
}

func (a *authenticator) deny(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="tether"`)
	httpx.WriteError(w, http.StatusUnauthorized, core.CodeUnauthorized, "missing or invalid bearer token")
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
