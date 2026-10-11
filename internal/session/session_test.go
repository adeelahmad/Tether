package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/adeelahmad/tether/internal/cdp/cdptest"
)

func TestRegistrableDomain(t *testing.T) {
	tests := []struct {
		host, want string
	}{
		{"example.com", "example.com"},
		{"www.example.com", "example.com"},
		{"a.b.c.example.com", "example.com"},
		{"WWW.Example.COM.", "example.com"},
		{"shop.example.co.uk", "example.co.uk"},
		{"example.co.uk", "example.co.uk"},
		{"foo.github.io", "foo.github.io"},
		{"bar.foo.github.io", "foo.github.io"},
		{"mail.google.com:443", "google.com"},
		{"localhost", "localhost"},
		{"localhost:8080", "localhost"},
		{"127.0.0.1", "127.0.0.1"},
		{"127.0.0.1:8999", "127.0.0.1"},
		{"[::1]:80", "::1"},
		{"co.uk", "co.uk"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			if got := RegistrableDomain(tt.host); got != tt.want {
				t.Errorf("RegistrableDomain(%q) = %q, want %q", tt.host, got, tt.want)
			}
		})
	}
}

func newManager(t *testing.T, idle time.Duration) (*Manager, *cdptest.Browser) {
	t.Helper()
	b := cdptest.NewBrowser()
	return NewManager(b, Options{IdleTimeout: idle}), b
}

func TestResolvePrecedence(t *testing.T) {
	ctx := context.Background()
	m, b := newManager(t, 0)
	b.AddTab("USER", "https://mail.example.com/")

	byDomain, src, err := m.Resolve(ctx, Hints{Host: "www.example.com"})
	if err != nil || src != SourceNew {
		t.Fatalf("first contact: src=%s err=%v", src, err)
	}
	other, _, _ := m.Resolve(ctx, Hints{Host: "other.org"})

	tests := []struct {
		name    string
		hints   Hints
		wantID  string
		wantSrc Source
		wantErr error
	}{
		{"domain reuse", Hints{Host: "api.example.com"}, byDomain.ID, SourceDomain, nil},
		{"cookie beats domain", Hints{CookieTabID: other.ID, Host: "example.com"}, other.ID, SourceCookie, nil},
		{"header beats cookie", Hints{TabID: byDomain.ID, CookieTabID: other.ID, Host: "x.org"}, byDomain.ID, SourceHeader, nil},
		{"header adopts user tab", Hints{TabID: "USER"}, "USER", SourceHeader, nil},
		{"dead header is an error", Hints{TabID: "NOPE", Host: "example.com"}, "", SourceHeader, ErrTabGone},
		{"dead cookie is an error", Hints{CookieTabID: "NOPE", Host: "example.com"}, "", SourceCookie, ErrTabGone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, src, err := m.Resolve(ctx, tt.hints)
			if src != tt.wantSrc {
				t.Errorf("source = %s, want %s", src, tt.wantSrc)
			}
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if s.ID != tt.wantID {
				t.Errorf("tab = %s, want %s", s.ID, tt.wantID)
			}
		})
	}
	if s, _ := m.Get(ctx, "USER"); s.Owned {
		t.Error("adopted tab must not be owned")
	}
}

func TestResolveCreatesOneTabPerDomainConcurrently(t *testing.T) {
	m, b := newManager(t, 0)
	var wg sync.WaitGroup
	ids := make(chan string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, _, err := m.Resolve(context.Background(), Hints{Host: "example.com"})
			if err != nil {
				t.Error(err)
				return
			}
			ids <- s.ID
		}()
	}
	wg.Wait()
	close(ids)
	first := <-ids
	for id := range ids {
		if id != first {
			t.Fatalf("got tabs %s and %s for one domain", first, id)
		}
	}
	if n := len(b.Open()); n != 1 {
		t.Fatalf("browser has %d tabs, want 1", n)
	}
}

func TestCloseAndBrowserDestroy(t *testing.T) {
	ctx := context.Background()
	m, b := newManager(t, 0)
	var mu sync.Mutex
	var reasons []CloseReason
	m.OnClose(func(_ *Session, r CloseReason) { mu.Lock(); reasons = append(reasons, r); mu.Unlock() })

	s, _, _ := m.Resolve(ctx, Hints{Host: "a.com"})
	if err := m.Close(ctx, s.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(ctx, s.ID, false); !errors.Is(err, ErrTabGone) {
		t.Fatalf("second close: %v", err)
	}
	if _, err := s.Lock(ctx); !errors.Is(err, ErrTabGone) {
		t.Fatalf("lock on closed session: %v", err)
	}

	s2, _, _ := m.Resolve(ctx, Hints{Host: "a.com"})
	if s2.ID == s.ID {
		t.Fatal("closed tab was reused")
	}
	b.Destroy(s2.ID) // the user closes the tab in Chrome
	if _, err := m.Get(ctx, s2.ID); !errors.Is(err, ErrTabGone) {
		t.Fatalf("get destroyed tab: %v", err)
	}
	s3, src, _ := m.Resolve(ctx, Hints{Host: "a.com"})
	if src != SourceNew || s3.ID == s2.ID {
		t.Fatalf("expected a fresh tab after destroy, got %s (%s)", s3.ID, src)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reasons) != 2 || reasons[0] != ReasonExplicit || reasons[1] != ReasonBrowser {
		t.Fatalf("close reasons = %v", reasons)
	}
}

func TestAdoptedTabsAreNeverClosed(t *testing.T) {
	ctx := context.Background()
	m, b := newManager(t, time.Minute)
	b.AddTab("USER", "https://example.com")
	if _, err := m.Get(ctx, "USER"); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(ctx, "USER", false); err != nil {
		t.Fatal(err)
	}
	if len(b.Open()) != 1 {
		t.Fatal("releasing an adopted tab closed it in Chrome")
	}
	m.Get(ctx, "USER")
	m.Reap(ctx, time.Now().Add(2*time.Minute))
	if len(b.Open()) != 1 {
		t.Fatal("reaping an adopted tab closed it in Chrome")
	}
	if len(m.List()) != 0 {
		t.Fatal("adopted idle tab was not released")
	}
}

func TestReap(t *testing.T) {
	ctx := context.Background()
	m, b := newManager(t, time.Minute)
	idle, _, _ := m.Resolve(ctx, Hints{Host: "idle.com"})
	busy, _, _ := m.Resolve(ctx, Hints{Host: "busy.com"})
	unlock, err := busy.Lock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	if n := m.Reap(ctx, time.Now()); n != 0 {
		t.Fatalf("reaped %d fresh sessions", n)
	}
	if n := m.Reap(ctx, time.Now().Add(2*time.Minute)); n != 1 {
		t.Fatalf("reaped %d, want 1 (busy sessions are skipped)", n)
	}
	if !idle.Closed() || busy.Closed() {
		t.Fatalf("idle closed=%v busy closed=%v", idle.Closed(), busy.Closed())
	}
	if len(b.Open()) != 1 {
		t.Fatalf("browser tabs = %d, want 1", len(b.Open()))
	}
}

func TestShutdownClosesOwnedTabs(t *testing.T) {
	ctx := context.Background()
	m, b := newManager(t, 0)
	b.AddTab("USER", "https://example.com")
	m.Resolve(ctx, Hints{Host: "a.com"})
	m.Resolve(ctx, Hints{Host: "b.com"})
	m.Get(ctx, "USER")
	m.Shutdown(ctx)
	if open := b.Open(); len(open) != 1 || open[0] != "USER" {
		t.Fatalf("after shutdown open tabs = %v, want [USER]", open)
	}
}

func TestLockSerialisesAndHonoursContext(t *testing.T) {
	m, _ := newManager(t, 0)
	s, _ := m.Create(context.Background())
	unlock, err := s.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := s.Lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock: %v", err)
	}
	unlock()
	unlock() // idempotent
	u2, err := s.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	u2()
}

func TestCreateError(t *testing.T) {
	m, b := newManager(t, 0)
	b.NewErr = errors.New("boom")
	if _, _, err := m.Resolve(context.Background(), Hints{Host: "a.com"}); err == nil {
		t.Fatal("expected error")
	}
	b.NewErr = nil
	if _, src, err := m.Resolve(context.Background(), Hints{Host: "a.com"}); err != nil || src != SourceNew {
		t.Fatalf("retry after failure: %s %v", src, err)
	}
}

func TestResolveRequiresHost(t *testing.T) {
	m, _ := newManager(t, 0)
	if _, _, err := m.Resolve(context.Background(), Hints{}); err == nil {
		t.Fatal("expected error for empty hints")
	}
}
