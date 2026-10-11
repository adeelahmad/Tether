// Package policy holds Tether's permission model: domain rules, the
// principals (tokens) that requests act as, and the policy file that
// defines them.
//
// Rules are layered. A navigation is allowed only if every layer allows
// it: the global rules (--allow-domains, --deny-domains, the policy file's
// top level), the rules of the token making the request, the rules of the
// token that owns the tab, and the tab's own session rules. A layer can
// only narrow what the layers above it permit; deny always wins.
//
// The package is a leaf: session and core both build on it.
package policy

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// ErrForbidden is returned when a rule rejects a request.
var ErrForbidden = errors.New("forbidden by policy")

// Rules are one layer of domain rules.
//
// A pattern is a host name. "example.com" matches example.com and every
// subdomain; "*.example.com" matches subdomains only; "*" matches every
// host. Allow empty means "everything not denied". Whenever a layer has
// any rule, only http, https, about: and data: URLs pass it.
type Rules struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

// Normalize lowercases and trims patterns, drops empties and duplicates,
// and rejects patterns that are not host names.
func (r Rules) Normalize() (Rules, error) {
	allow, err := normalizeList(r.Allow)
	if err != nil {
		return Rules{}, err
	}
	deny, err := normalizeList(r.Deny)
	if err != nil {
		return Rules{}, err
	}
	return Rules{Allow: allow, Deny: deny}, nil
}

func normalizeList(in []string) ([]string, error) {
	var out []string
	for _, p := range in {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "*." {
			return nil, fmt.Errorf("invalid domain pattern %q", p)
		}
		p = strings.TrimSuffix(p, ".")
		p = strings.TrimPrefix(p, ".") // ".example.com" is the old cookie-style spelling
		if p == "" {
			continue
		}
		if err := validPattern(p); err != nil {
			return nil, err
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out, nil
}

func validPattern(p string) error {
	if p == "*" {
		return nil
	}
	host := strings.TrimPrefix(p, "*.")
	if host == "" || strings.ContainsAny(host, "*/:@?#[] \t") {
		return fmt.Errorf("invalid domain pattern %q: use a host name like example.com or *.example.com", p)
	}
	return nil
}

// Empty reports whether the layer has no rules at all.
func (r Rules) Empty() bool { return len(r.Allow) == 0 && len(r.Deny) == 0 }

// Check returns nil if u passes this layer, or an error wrapping
// ErrForbidden that says why not. Patterns are expected to be normalized.
func (r Rules) Check(u *url.URL) error {
	if r.Empty() {
		return nil
	}
	switch u.Scheme {
	case "http", "https":
	case "about", "data":
		return nil
	default:
		return fmt.Errorf("%w: scheme %q", ErrForbidden, u.Scheme)
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	for _, p := range r.Deny {
		if Match(p, host) {
			return fmt.Errorf("%w: %s is denied by %q", ErrForbidden, host, p)
		}
	}
	if len(r.Allow) == 0 {
		return nil
	}
	for _, p := range r.Allow {
		if Match(p, host) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s is not in the allowlist", ErrForbidden, host)
}

// Match reports whether host matches pattern.
func Match(pattern, host string) bool {
	switch {
	case pattern == "*":
		return true
	case strings.HasPrefix(pattern, "*."):
		return strings.HasSuffix(host, pattern[1:])
	default:
		return host == pattern || strings.HasSuffix(host, "."+pattern)
	}
}

// Principal is who a request acts as: the token it authenticated with.
type Principal struct {
	// Name identifies the token in logs, tab listings and errors. It is
	// never the secret itself.
	Name string `json:"name"`
	// Admin principals see and drive every tab, including tabs other
	// tokens opened and tabs the user already had open. Others see only
	// the tabs they opened.
	Admin bool `json:"admin"`
	// Scripts allows arbitrary JavaScript (the eval endpoint and
	// WebDriver execute script).
	Scripts bool `json:"scripts"`
	// Rules restrict where this token may navigate. They follow the tabs
	// it opens, whoever drives them later.
	Rules Rules `json:"rules"`
}

// CanUse reports whether p may use a tab owned by owner. A nil principal
// (no authentication configured) can use everything.
func (p *Principal) CanUse(owner string) bool {
	return p == nil || p.Admin || p.Name == owner
}

// CanAdopt reports whether p may attach to tabs Tether did not open.
func (p *Principal) CanAdopt() bool { return p == nil || p.Admin }

// CanScript reports whether p may run arbitrary JavaScript.
func (p *Principal) CanScript() bool { return p == nil || p.Scripts }

// OwnerName is the owner recorded on tabs p opens ("" for nil).
func (p *Principal) OwnerName() string {
	if p == nil {
		return ""
	}
	return p.Name
}

// RulesOrEmpty returns p's rules, or none for a nil principal.
func (p *Principal) RulesOrEmpty() Rules {
	if p == nil {
		return Rules{}
	}
	return p.Rules
}

type ctxKey struct{}

// WithPrincipal returns ctx carrying p.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the principal ctx carries, or nil when the request
// was not authenticated (no tokens configured, or internal work).
func FromContext(ctx context.Context) *Principal {
	p, _ := ctx.Value(ctxKey{}).(*Principal)
	return p
}
