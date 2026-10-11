package session

import (
	"context"
	"errors"
	"testing"

	"github.com/adeelahmad/tether/internal/cdp/cdptest"
	"github.com/adeelahmad/tether/internal/policy"
)

func TestOwnership(t *testing.T) {
	b := cdptest.NewBrowser()
	b.AddTab("USER", "https://mail.example.com/")
	m := NewManager(b, Options{})

	alice := policy.WithPrincipal(context.Background(), &policy.Principal{Name: "alice"})
	bob := policy.WithPrincipal(context.Background(), &policy.Principal{Name: "bob"})
	admin := policy.WithPrincipal(context.Background(), &policy.Principal{Name: "ops", Admin: true})

	s, err := m.Create(alice)
	if err != nil {
		t.Fatal(err)
	}
	if s.Owner != "alice" || s.OwnerPrincipal().Name != "alice" || s.Info().Owner != "alice" {
		t.Fatalf("owner not recorded: %+v", s.Info())
	}
	if got, err := m.Get(alice, s.ID); err != nil || got != s {
		t.Fatalf("owner Get: %v", err)
	}
	if _, err := m.Get(admin, s.ID); err != nil {
		t.Fatalf("admin Get: %v", err)
	}
	if _, err := m.Get(context.Background(), s.ID); err != nil {
		t.Fatalf("unauthenticated Get: %v", err)
	}
	if _, err := m.Get(bob, s.ID); !errors.Is(err, ErrTabGone) {
		t.Fatalf("other token Get = %v, want ErrTabGone", err)
	}
	if _, _, err := m.Resolve(bob, Hints{TabID: s.ID, Host: "example.com"}); !errors.Is(err, ErrTabGone) {
		t.Fatalf("other token Resolve by header = %v", err)
	}
	if _, _, err := m.Resolve(bob, Hints{CookieTabID: s.ID, Host: "example.com"}); !errors.Is(err, ErrTabGone) {
		t.Fatalf("other token Resolve by cookie = %v", err)
	}
	if err := m.Close(bob, s.ID, false); !errors.Is(err, ErrTabGone) {
		t.Fatalf("other token Close = %v", err)
	}
	if !Visible(alice, s) || Visible(bob, s) || !Visible(admin, s) {
		t.Fatal("Visible")
	}

	// Adopting the user's own tabs is for admins only.
	if _, err := m.Get(bob, "USER"); !errors.Is(err, ErrTabGone) {
		t.Fatalf("non-admin adopt = %v", err)
	}
	u, err := m.Get(admin, "USER")
	if err != nil {
		t.Fatalf("admin adopt: %v", err)
	}
	if _, err := m.Get(bob, u.ID); !errors.Is(err, ErrTabGone) {
		t.Fatalf("non-admin use of adopted tab = %v", err)
	}

	if err := m.Close(alice, s.ID, false); err != nil {
		t.Fatalf("owner Close: %v", err)
	}
}

func TestDomainTabsArePerPrincipal(t *testing.T) {
	b := cdptest.NewBrowser()
	m := NewManager(b, Options{})
	alice := policy.WithPrincipal(context.Background(), &policy.Principal{Name: "alice"})
	bob := policy.WithPrincipal(context.Background(), &policy.Principal{Name: "bob"})

	a1, src, err := m.Resolve(alice, Hints{Host: "www.example.com"})
	if err != nil || src != SourceNew {
		t.Fatal(src, err)
	}
	a2, src, _ := m.Resolve(alice, Hints{Host: "api.example.com"})
	if a2 != a1 || src != SourceDomain {
		t.Fatalf("alice should reuse her domain tab: %v %v", a2.ID, src)
	}
	b1, src, _ := m.Resolve(bob, Hints{Host: "example.com"})
	if b1 == a1 || src != SourceNew || b1.Owner != "bob" {
		t.Fatalf("bob must get his own tab, got %s (%s)", b1.ID, src)
	}
	anon, _, _ := m.Resolve(context.Background(), Hints{Host: "example.com"})
	if anon == a1 || anon == b1 {
		t.Fatal("unauthenticated requests share no tab with tokens")
	}

	// Closing alice's tab frees only her binding.
	if err := m.Close(alice, a1.ID, false); err != nil {
		t.Fatal(err)
	}
	if again, src, _ := m.Resolve(bob, Hints{Host: "example.com"}); again != b1 || src != SourceDomain {
		t.Fatal("bob's binding survived")
	}
	if fresh, src, _ := m.Resolve(alice, Hints{Host: "example.com"}); fresh == a1 || src != SourceNew {
		t.Fatal("alice gets a new tab")
	}
}

func TestSessionRules(t *testing.T) {
	b := cdptest.NewBrowser()
	m := NewManager(b, Options{})
	s, _ := m.Create(context.Background())
	if !s.Rules().Empty() || s.Info().Rules != nil {
		t.Fatal("no rules by default")
	}
	s.SetRules(policy.Rules{Allow: []string{"example.com"}})
	if s.Rules().Allow[0] != "example.com" || s.Info().Rules == nil {
		t.Fatal("rules not stored")
	}
	s.SetRules(policy.Rules{})
	if !s.Rules().Empty() || s.Info().Rules != nil {
		t.Fatal("rules not cleared")
	}
}
