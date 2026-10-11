package core

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/adeelahmad/tether/internal/policy"
	"github.com/adeelahmad/tether/internal/session"
)

// ErrForbidden is returned when a Policy rejects a request.
var ErrForbidden = policy.ErrForbidden

// Policy decides whether a session may load a URL. It is enforced in the
// core request path, so every face (proxy, WebDriver, MCP, remote UI) is
// subject to it. The principal making the request, if any, is in ctx
// (policy.FromContext); s may be nil when no tab is involved yet.
type Policy interface {
	Allow(ctx context.Context, s *session.Session, u *url.URL) error
}

// Restricter is implemented by policies that can say up front whether a
// tab is restricted at all. Unrestricted tabs get no browser-side
// navigation guard; a Policy without it is assumed to restrict everything.
type Restricter interface {
	Restricts(s *session.Session) bool
}

// PolicyFunc adapts a function to Policy.
type PolicyFunc func(ctx context.Context, s *session.Session, u *url.URL) error

// Allow implements Policy.
func (f PolicyFunc) Allow(ctx context.Context, s *session.Session, u *url.URL) error {
	return f(ctx, s, u)
}

// AllowAll permits everything, ignoring token and session rules too. The
// core's default is NewRulesPolicy with no global rules, not AllowAll.
var AllowAll Policy = allowAll{}

type allowAll struct{}

func (allowAll) Allow(context.Context, *session.Session, *url.URL) error { return nil }
func (allowAll) Restricts(*session.Session) bool                         { return false }

// RulesPolicy is Tether's layered domain policy. A URL must pass, in order:
// the global rules, the requesting token's rules, the rules of the token
// that owns the tab, and the tab's own session rules.
type RulesPolicy struct {
	Global policy.Rules
}

// NewRulesPolicy returns the layered policy over global rules. The rules
// must be normalized (policy.Rules.Normalize).
func NewRulesPolicy(global policy.Rules) *RulesPolicy { return &RulesPolicy{Global: global} }

// Allow implements Policy.
func (p *RulesPolicy) Allow(ctx context.Context, s *session.Session, u *url.URL) error {
	if err := p.Global.Check(u); err != nil {
		return fmt.Errorf("%w (global rules)", err)
	}
	req := policy.FromContext(ctx)
	if err := req.RulesOrEmpty().Check(u); err != nil {
		return fmt.Errorf("%w (rules of token %q)", err, req.Name)
	}
	if s == nil {
		return nil
	}
	if owner := s.OwnerPrincipal(); owner != req {
		if err := owner.RulesOrEmpty().Check(u); err != nil {
			return fmt.Errorf("%w (rules of token %q, which opened this tab)", err, owner.Name)
		}
	}
	if err := s.Rules().Check(u); err != nil {
		return fmt.Errorf("%w (this tab's session rules)", err)
	}
	return nil
}

// Restricts implements Restricter.
func (p *RulesPolicy) Restricts(s *session.Session) bool {
	return !p.Global.Empty() || (s != nil && (!s.OwnerPrincipal().RulesOrEmpty().Empty() || !s.Rules().Empty()))
}

// DomainAllowlist permits only the listed domains and their subdomains.
// An empty list allows everything. Internal pages (about:blank, data:) are
// always allowed. Token and session rules still apply on top of it.
func DomainAllowlist(domains []string) Policy {
	r, err := policy.Rules{Allow: domains}.Normalize()
	if err != nil {
		// Keep the old lenient behaviour for odd entries: drop them.
		r = policy.Rules{}
		for _, d := range domains {
			if n, err := (policy.Rules{Allow: []string{d}}).Normalize(); err == nil {
				r.Allow = append(r.Allow, n.Allow...)
			}
		}
	}
	return NewRulesPolicy(r)
}

// guards tracks which tabs carry a browser-side navigation guard.
type guards struct {
	mu sync.Mutex
	on map[*session.Session]bool
}

// Prepare readies s for work under the current policy: when the tab is
// restricted at all, it installs a browser-side guard so that navigations
// the core never sees (redirects, link clicks, page script) are held to
// the same rules. It is cheap to call repeatedly. Faces call it when they
// hand a tab to a client; Do, Navigate and Eval call it themselves.
func (c *Core) Prepare(ctx context.Context, s *session.Session) error {
	if r, ok := c.Policy.(Restricter); ok && !r.Restricts(s) {
		return nil
	}
	c.guards.mu.Lock()
	on := c.guards.on[s]
	c.guards.mu.Unlock()
	if on {
		return nil
	}
	// Guard is idempotent per tab, so a concurrent Prepare is harmless.
	err := s.Tab.Guard(ctx, func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			return fmt.Errorf("%w: unparseable URL", ErrForbidden)
		}
		// No principal in this context: the tab's owner and session
		// rules apply to whoever is driving it.
		return c.Policy.Allow(context.Background(), s, u)
	})
	if err != nil {
		return fmt.Errorf("installing navigation guard: %w", err)
	}
	c.guards.mu.Lock()
	if c.guards.on == nil {
		c.guards.on = map[*session.Session]bool{}
	}
	if !s.Closed() {
		c.guards.on[s] = true
	}
	c.guards.mu.Unlock()
	return nil
}

// dropGuard runs as a session close hook, possibly on the CDP event loop,
// so it never blocks.
func (c *Core) dropGuard(s *session.Session, why session.CloseReason) {
	c.guards.mu.Lock()
	on := c.guards.on[s]
	delete(c.guards.on, s)
	c.guards.mu.Unlock()
	if on && !s.Owned && why != session.ReasonBrowser {
		// An adopted tab goes back to the user: stop intercepting it.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = s.Tab.Guard(ctx, nil)
		}()
	}
}

// SetSessionRules replaces a tab's session rules and makes sure the tab is
// guarded under them. rules must be normalized.
func (c *Core) SetSessionRules(ctx context.Context, s *session.Session, rules policy.Rules) error {
	s.SetRules(rules)
	return c.Prepare(ctx, s)
}

// AdoptPopup takes over a tab that opener's page opened, under opener's
// owner and session rules. The popup loaded before Tether could guard it,
// so a popup sitting on a page the rules forbid is closed and refused.
func (c *Core) AdoptPopup(ctx context.Context, id string, opener *session.Session) (*session.Session, error) {
	s, err := c.Sessions.Child(ctx, id, opener)
	if err != nil {
		return nil, err
	}
	if err := c.Prepare(ctx, s); err != nil {
		return nil, err
	}
	info, err := s.Tab.Info(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.checkFinal(ctx, s, info.URL, false); err != nil {
		_ = c.Sessions.Close(context.WithoutCancel(ctx), s.ID, true)
		return nil, err
	}
	return s, nil
}

// AllowScripts returns ErrForbidden unless the principal in ctx may run
// arbitrary JavaScript.
func AllowScripts(ctx context.Context) error {
	if p := policy.FromContext(ctx); !p.CanScript() {
		return fmt.Errorf("%w: token %q may not run scripts", ErrForbidden, p.Name)
	}
	return nil
}
