package core

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/adeelahmad/tether/internal/cdp"
	"github.com/adeelahmad/tether/internal/cdp/cdptest"
	"github.com/adeelahmad/tether/internal/policy"
	"github.com/adeelahmad/tether/internal/session"
)

func rules(t *testing.T, allow, deny []string) policy.Rules {
	t.Helper()
	r, err := policy.Rules{Allow: allow, Deny: deny}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRulesPolicyLayers(t *testing.T) {
	b := cdptest.NewBrowser()
	m := session.NewManager(b, session.Options{})
	p := NewRulesPolicy(rules(t, nil, []string{"evil.test"}))

	agent := &policy.Principal{Name: "agent", Rules: rules(t, []string{"example.com", "github.com"}, nil)}
	admin := &policy.Principal{Name: "ops", Admin: true}
	agentCtx := policy.WithPrincipal(context.Background(), agent)
	adminCtx := policy.WithPrincipal(context.Background(), admin)

	s, err := m.Create(agentCtx)
	if err != nil {
		t.Fatal(err)
	}
	s.SetRules(rules(t, nil, []string{"gist.github.com"}))

	tests := []struct {
		name string
		ctx  context.Context
		url  string
		want bool
	}{
		{"global deny", adminCtx, "https://evil.test/", false},
		{"token allow", agentCtx, "https://www.example.com/", true},
		{"token rules", agentCtx, "https://other.test/", false},
		{"session deny", agentCtx, "https://gist.github.com/", false},
		{"session leaves rest", agentCtx, "https://github.com/", true},
		// The owner's rules follow the tab, even when an admin drives it.
		{"owner rules follow tab", adminCtx, "https://other.test/", false},
		{"admin within owner rules", adminCtx, "https://example.com/", true},
		{"no principal still bound by tab", context.Background(), "https://other.test/", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := p.Allow(tt.ctx, s, mustURL(tt.url))
			if (err == nil) != tt.want {
				t.Fatalf("Allow(%s) = %v, want allowed=%v", tt.url, err, tt.want)
			}
			if err != nil && !errors.Is(err, ErrForbidden) {
				t.Fatalf("not ErrForbidden: %v", err)
			}
		})
	}

	// The requester's own rules apply too, even on a tab it can use.
	adminTab, _ := m.Create(adminCtx)
	narrow := &policy.Principal{Name: "narrow", Admin: true, Rules: rules(t, []string{"example.com"}, nil)}
	if err := p.Allow(policy.WithPrincipal(context.Background(), narrow), adminTab, mustURL("https://github.com/")); err == nil {
		t.Fatal("requester rules ignored")
	}
	if err := p.Allow(adminCtx, nil, mustURL("https://github.com/")); err != nil {
		t.Fatalf("nil session: %v", err)
	}

	if p.Restricts(adminTab) != true {
		t.Fatal("global rules restrict every tab")
	}
	open := NewRulesPolicy(policy.Rules{})
	if open.Restricts(adminTab) || !open.Restricts(s) {
		t.Fatal("Restricts must follow owner and session rules")
	}
}

func TestPrepareGuardsOnlyRestrictedTabs(t *testing.T) {
	c, b, s := setup(t)
	c.Policy = NewRulesPolicy(policy.Rules{})
	tab := s.Tab.(*cdptest.Tab)
	if _, err := c.Navigate(context.Background(), s, "https://example.com/", cdp.WaitLoad); err != nil {
		t.Fatal(err)
	}
	if tab.Guarded() {
		t.Fatal("unrestricted tab must not be intercepted")
	}

	if err := c.SetSessionRules(context.Background(), s, rules(t, []string{"example.com"}, nil)); err != nil {
		t.Fatal(err)
	}
	if !tab.Guarded() {
		t.Fatal("restricted tab must be guarded")
	}

	// A redirect the core never asked for is stopped in the browser.
	b.Pages["https://example.com/out"] = cdptest.Page{Status: 302, RedirectTo: "https://evil.test/"}
	_, err := c.Navigate(context.Background(), s, "https://example.com/out", cdp.WaitLoad)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("redirect err = %v", err)
	}
	status, code := Classify(err)
	if status != 403 || code != CodeForbidden {
		t.Fatalf("classified as %d %s", status, code)
	}

	// Closing the tab forgets its guard.
	if err := c.Sessions.Close(context.Background(), s.ID, false); err != nil {
		t.Fatal(err)
	}
	c.guards.mu.Lock()
	n := len(c.guards.on)
	c.guards.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d guards left after close", n)
	}
}

func TestFinalURLChecked(t *testing.T) {
	// The final URL is checked even if the browser-side guard missed the
	// redirect (defence in depth).
	c, b, s := setup(t)
	c.Policy = NewRulesPolicy(rules(t, []string{"example.com"}, nil))
	tab := s.Tab.(*cdptest.Tab)
	_ = tab.Guard(context.Background(), nil) // simulate a guard that let it through
	c.guards.on = map[*session.Session]bool{s: true}

	b.Pages["https://example.com/out"] = cdptest.Page{Status: 302, RedirectTo: "https://evil.test/"}
	_, err := c.Do(context.Background(), s, &Request{Method: "GET", URL: mustURL("https://example.com/out")})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v", err)
	}
	if last := tab.Navigations[len(tab.Navigations)-1]; last != "about:blank" {
		t.Fatalf("tab left on forbidden page; last navigation %q", last)
	}

	// fetch() that followed a redirect off the allowlist is refused too.
	tab.SetURL("https://example.com/")
	tab.CallResult = json.RawMessage(`{"status":200,"url":"https://evil.test/","headers":[],"body":""}`)
	_, err = c.Do(context.Background(), s, &Request{Method: "POST", URL: mustURL("https://example.com/api")})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("fetch err = %v", err)
	}
}

func TestEvalNeedsScriptPermission(t *testing.T) {
	c, _, s := setup(t)
	s.Tab.(*cdptest.Tab).EvalResult = json.RawMessage(`1`)
	noScripts := policy.WithPrincipal(context.Background(), &policy.Principal{Name: "agent", Admin: true})
	if _, err := c.Eval(noScripts, s, "1", false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v", err)
	}
	scripts := policy.WithPrincipal(context.Background(), &policy.Principal{Name: "ops", Admin: true, Scripts: true})
	if _, err := c.Eval(scripts, s, "1", false); err != nil {
		t.Fatal(err)
	}
}
