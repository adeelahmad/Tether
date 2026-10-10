package core

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/adeelahmad/tether/internal/session"
)

// ErrForbidden is returned when a Policy rejects a request.
var ErrForbidden = errors.New("forbidden by policy")

// Policy decides whether a session may load a URL. It is enforced in the
// core request path, so every face (proxy, WebDriver, MCP, remote UI) is
// subject to it. It is the seam for per-agent permissioning: a Policy can
// inspect the session and the target and say no.
type Policy interface {
	Allow(ctx context.Context, s *session.Session, u *url.URL) error
}

// PolicyFunc adapts a function to Policy.
type PolicyFunc func(ctx context.Context, s *session.Session, u *url.URL) error

// Allow implements Policy.
func (f PolicyFunc) Allow(ctx context.Context, s *session.Session, u *url.URL) error {
	return f(ctx, s, u)
}

// AllowAll permits everything.
var AllowAll Policy = PolicyFunc(func(context.Context, *session.Session, *url.URL) error { return nil })

// DomainAllowlist permits only the listed domains and their subdomains.
// An empty list allows everything. Internal pages (about:blank, data:) are
// always allowed.
func DomainAllowlist(domains []string) Policy {
	var allowed []string
	for _, d := range domains {
		d = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(d)), ".")
		if d != "" {
			allowed = append(allowed, d)
		}
	}
	if len(allowed) == 0 {
		return AllowAll
	}
	return PolicyFunc(func(_ context.Context, _ *session.Session, u *url.URL) error {
		if u.Scheme != "http" && u.Scheme != "https" {
			if u.Scheme == "about" || u.Scheme == "data" {
				return nil
			}
			return fmt.Errorf("%w: scheme %q", ErrForbidden, u.Scheme)
		}
		host := strings.ToLower(u.Hostname())
		for _, d := range allowed {
			if host == d || strings.HasSuffix(host, "."+d) {
				return nil
			}
		}
		return fmt.Errorf("%w: %s is not in the domain allowlist", ErrForbidden, host)
	})
}
