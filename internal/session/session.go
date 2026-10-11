// Package session maps Tether sessions onto real Chrome tabs.
//
// A session is exactly one CDP page target, keyed by its target (tab) ID.
// Every face — proxy, WebDriver, and later MCP and the remote UI — resolves
// requests to a Session through the same Manager, so a tab opened by one
// face can be driven by another.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/adeelahmad/tether/internal/cdp"
	"github.com/adeelahmad/tether/internal/logx"
)

// ErrTabGone is returned when a client references a tab that is closed,
// expired, or was never known. Faces map it to a well-defined error
// (HTTP 410 / WebDriver "invalid session id").
var ErrTabGone = errors.New("tab is closed or unknown")

// Source says how a request was bound to its session.
type Source string

const (
	SourceHeader Source = "header"
	SourceCookie Source = "cookie"
	SourceDomain Source = "domain"
	SourceNew    Source = "new"
)

// CloseReason says why a session ended.
type CloseReason string

const (
	ReasonExplicit CloseReason = "closed"
	ReasonIdle     CloseReason = "idle"
	ReasonBrowser  CloseReason = "tab-destroyed"
	ReasonShutdown CloseReason = "shutdown"
)

// Session is one live Chrome tab.
type Session struct {
	ID      string
	Domain  string // registrable domain this tab is the default for, if any
	Owned   bool   // opened by Tether (closed on idle/shutdown) vs adopted
	Tab     cdp.Tab
	Created time.Time

	sem      chan struct{}
	lastUsed atomic.Int64
	closed   atomic.Bool
}

// Lock serialises work on the tab: one navigation or command at a time.
// It fails if ctx ends first or the session is closed.
func (s *Session) Lock(ctx context.Context) (unlock func(), err error) {
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if s.closed.Load() {
		<-s.sem
		return nil, ErrTabGone
	}
	s.Touch()
	var once sync.Once
	return func() { once.Do(func() { s.Touch(); <-s.sem }) }, nil
}

// Touch marks the session as used now.
func (s *Session) Touch() { s.lastUsed.Store(time.Now().UnixNano()) }

// LastUsed is the last time the session was used.
func (s *Session) LastUsed() time.Time { return time.Unix(0, s.lastUsed.Load()) }

// Closed reports whether the session has ended.
func (s *Session) Closed() bool { return s.closed.Load() }

// Info is a serialisable view of a session.
type Info struct {
	ID       string    `json:"id"`
	Domain   string    `json:"domain,omitempty"`
	Owned    bool      `json:"owned"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"lastUsed"`
}

// Info returns a snapshot of the session.
func (s *Session) Info() Info {
	return Info{ID: s.ID, Domain: s.Domain, Owned: s.Owned, Created: s.Created, LastUsed: s.LastUsed()}
}

// Hints are what a request carries to identify its session, in precedence
// order: an explicit tab ID (header), a session cookie, then the host.
type Hints struct {
	TabID       string
	CookieTabID string
	Host        string
}

// Options configure a Manager.
type Options struct {
	// IdleTimeout closes owned tabs unused for this long (0 = never).
	IdleTimeout time.Duration
	Logger      *slog.Logger
}

// Manager owns the set of live sessions.
type Manager struct {
	browser cdp.Browser
	idle    time.Duration
	log     *slog.Logger

	mu       sync.Mutex
	byID     map[string]*Session
	byDomain map[string]string
	pending  map[string]*pendingCreate
	hooks    []func(*Session, CloseReason)
}

type pendingCreate struct {
	done chan struct{}
	s    *Session
	err  error
}

// NewManager returns a Manager over browser.
func NewManager(browser cdp.Browser, opts Options) *Manager {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{
		browser:  browser,
		idle:     opts.IdleTimeout,
		log:      log,
		byID:     map[string]*Session{},
		byDomain: map[string]string{},
		pending:  map[string]*pendingCreate{},
	}
	browser.OnTabClosed(func(id string) { m.forget(id, ReasonBrowser) })
	return m
}

// OnClose registers fn to run whenever a session ends.
func (m *Manager) OnClose(fn func(*Session, CloseReason)) {
	m.mu.Lock()
	m.hooks = append(m.hooks, fn)
	m.mu.Unlock()
}

// Resolve finds or creates the session for a request.
//
// Precedence: explicit tab ID → cookie → the default tab for the host's
// registrable domain, creating one on first contact. A tab ID or cookie
// that names a dead tab yields ErrTabGone rather than silently falling
// through, so clients learn their handle is stale.
func (m *Manager) Resolve(ctx context.Context, h Hints) (*Session, Source, error) {
	if h.TabID != "" {
		s, err := m.Get(ctx, h.TabID)
		return s, SourceHeader, err
	}
	if h.CookieTabID != "" {
		s, err := m.Get(ctx, h.CookieTabID)
		return s, SourceCookie, err
	}
	domain := RegistrableDomain(h.Host)
	if domain == "" {
		return nil, "", errors.New("session: request has no host")
	}

	m.mu.Lock()
	if id, ok := m.byDomain[domain]; ok {
		if s := m.byID[id]; s != nil && !s.Closed() {
			m.mu.Unlock()
			return s, SourceDomain, nil
		}
		delete(m.byDomain, domain)
	}
	if p, ok := m.pending[domain]; ok {
		m.mu.Unlock()
		select {
		case <-p.done:
			return p.s, SourceDomain, p.err
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}
	p := &pendingCreate{done: make(chan struct{})}
	m.pending[domain] = p
	m.mu.Unlock()

	p.s, p.err = m.create(ctx, domain)

	m.mu.Lock()
	delete(m.pending, domain)
	if p.err == nil {
		m.byDomain[domain] = p.s.ID
	}
	m.mu.Unlock()
	close(p.done)
	return p.s, SourceNew, p.err
}

// Create opens a new owned tab that is not bound to any domain.
func (m *Manager) Create(ctx context.Context) (*Session, error) {
	return m.create(ctx, "")
}

func (m *Manager) create(ctx context.Context, domain string) (*Session, error) {
	tab, err := m.browser.NewTab(ctx)
	if err != nil {
		return nil, fmt.Errorf("session: open tab: %w", err)
	}
	s := m.track(tab, domain, true)
	m.log.Info("tab opened", "tab", logx.Clean(s.ID), "domain", logx.Clean(domain))
	return s, nil
}

func (m *Manager) track(tab cdp.Tab, domain string, owned bool) *Session {
	s := &Session{
		ID:      tab.ID(),
		Domain:  domain,
		Owned:   owned,
		Tab:     tab,
		Created: time.Now(),
		sem:     make(chan struct{}, 1),
	}
	s.Touch()
	m.mu.Lock()
	m.byID[s.ID] = s
	m.mu.Unlock()
	return s
}

// Get returns the session for a tab ID. A tab that Tether did not open but
// that exists in Chrome is adopted (attached, never auto-closed), which is
// how a client drives a tab the user already has open.
func (m *Manager) Get(ctx context.Context, id string) (*Session, error) {
	m.mu.Lock()
	s := m.byID[id]
	m.mu.Unlock()
	if s != nil && !s.Closed() {
		return s, nil
	}
	tab, err := m.browser.AttachTab(ctx, id)
	if err != nil {
		if errors.Is(err, cdp.ErrNoSuchTab) {
			return nil, fmt.Errorf("%w: %s", ErrTabGone, id)
		}
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.byID[id]; s != nil && !s.Closed() { // lost a race; keep the first
		return s, nil
	}
	s = &Session{ID: id, Tab: tab, Created: time.Now(), sem: make(chan struct{}, 1)}
	s.Touch()
	m.byID[id] = s
	m.log.Info("tab adopted", "tab", logx.Clean(id))
	return s, nil
}

// Close ends a session. Owned tabs are closed in Chrome; adopted tabs are
// only released, never closed, unless closeAdopted is set.
func (m *Manager) Close(ctx context.Context, id string, closeAdopted bool) error {
	m.mu.Lock()
	s := m.byID[id]
	m.mu.Unlock()
	if s == nil || s.Closed() {
		return fmt.Errorf("%w: %s", ErrTabGone, id)
	}
	return m.end(ctx, s, ReasonExplicit, s.Owned || closeAdopted)
}

func (m *Manager) end(ctx context.Context, s *Session, why CloseReason, closeTab bool) error {
	if !m.forget(s.ID, why) {
		return nil
	}
	if closeTab {
		if err := m.browser.CloseTab(ctx, s.ID); err != nil && !errors.Is(err, cdp.ErrNoSuchTab) {
			return err
		}
	}
	return nil
}

// forget drops a session from the index and runs close hooks once.
func (m *Manager) forget(id string, why CloseReason) bool {
	m.mu.Lock()
	s := m.byID[id]
	if s == nil || !s.closed.CompareAndSwap(false, true) {
		m.mu.Unlock()
		return false
	}
	delete(m.byID, id)
	if s.Domain != "" && m.byDomain[s.Domain] == id {
		delete(m.byDomain, s.Domain)
	}
	hooks := append([]func(*Session, CloseReason){}, m.hooks...)
	m.mu.Unlock()
	m.log.Info("tab session ended", "tab", id, "reason", string(why))
	for _, h := range hooks {
		h(s, why)
	}
	return true
}

// List returns all live sessions, oldest first.
func (m *Manager) List() []Info {
	m.mu.Lock()
	out := make([]Info, 0, len(m.byID))
	for _, s := range m.byID {
		out = append(out, s.Info())
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// Reap ends sessions idle for longer than the idle timeout. Owned tabs are
// closed; adopted tabs are just released. Busy sessions are never reaped.
func (m *Manager) Reap(ctx context.Context, now time.Time) int {
	if m.idle <= 0 {
		return 0
	}
	m.mu.Lock()
	var idle []*Session
	for _, s := range m.byID {
		if now.Sub(s.LastUsed()) >= m.idle {
			idle = append(idle, s)
		}
	}
	m.mu.Unlock()
	n := 0
	for _, s := range idle {
		select {
		case s.sem <- struct{}{}: // not busy
		default:
			continue
		}
		if now.Sub(s.LastUsed()) >= m.idle {
			if err := m.end(ctx, s, ReasonIdle, s.Owned); err != nil {
				m.log.Warn("closing idle tab", "tab", s.ID, "err", err)
			}
			n++
		}
		<-s.sem
	}
	return n
}

// Run reaps idle sessions until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	if m.idle <= 0 {
		<-ctx.Done()
		return
	}
	every := m.idle / 4
	if every > time.Minute {
		every = time.Minute
	}
	if every < time.Second {
		every = time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			m.Reap(ctx, now)
		}
	}
}

// Shutdown ends every session, closing the tabs Tether opened.
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	all := make([]*Session, 0, len(m.byID))
	for _, s := range m.byID {
		all = append(all, s)
	}
	m.mu.Unlock()
	for _, s := range all {
		if err := m.end(ctx, s, ReasonShutdown, s.Owned); err != nil {
			m.log.Warn("closing tab on shutdown", "tab", s.ID, "err", err)
		}
	}
}
