package cdp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
)

// Chrome is a Browser backed by a live DevTools websocket.
type Chrome struct {
	c   *conn
	log *slog.Logger

	mu        sync.Mutex
	hooks     []func(id string)
	closed    bool
	logSize   int
	logUnsubs map[string]logSub // by CDP session ID
}

type logSub struct {
	target string
	cancel func()
}

var _ Browser = (*Chrome)(nil)

// Connect dials the browser-level DevTools websocket (see ResolveEndpoint).
func Connect(ctx context.Context, wsURL string, log *slog.Logger) (*Chrome, error) {
	if log == nil {
		log = slog.Default()
	}
	c, err := dial(ctx, wsURL)
	if err != nil {
		return nil, err
	}
	b := &Chrome{c: c, log: log, logSize: DefaultLogBuffer, logUnsubs: map[string]logSub{}}
	c.subscribe("", b.onEvent)
	if err := c.call(ctx, "", "Target.setDiscoverTargets", map[string]any{"discover": true}, nil); err != nil {
		_ = c.close()
		return nil, err
	}
	return b, nil
}

// SetLogBuffer sets how many console/log entries each tab attached from now
// on keeps. Zero or less turns capture off, which also leaves the Runtime
// and Log domains disabled.
func (b *Chrome) SetLogBuffer(n int) {
	b.mu.Lock()
	b.logSize = n
	b.mu.Unlock()
}

func (b *Chrome) onEvent(e Event) {
	if e.SessionID != "" {
		return
	}
	switch e.Method {
	case "Target.targetDestroyed", "Target.detachedFromTarget":
	default:
		return
	}
	var p struct {
		TargetID  string `json:"targetId"`
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(e.Params, &p) != nil {
		return
	}
	b.mu.Lock()
	for sid, s := range b.logUnsubs {
		if sid == p.SessionID || (p.SessionID == "" && s.target == p.TargetID) {
			s.cancel()
			delete(b.logUnsubs, sid)
		}
	}
	if e.Method != "Target.targetDestroyed" {
		b.mu.Unlock()
		return
	}
	hooks := append([]func(string){}, b.hooks...)
	b.mu.Unlock()
	for _, h := range hooks {
		h(p.TargetID)
	}
}

// OnTabClosed implements Browser.
func (b *Chrome) OnTabClosed(fn func(id string)) {
	b.mu.Lock()
	b.hooks = append(b.hooks, fn)
	b.mu.Unlock()
}

// Version implements Browser.
func (b *Chrome) Version(ctx context.Context) (string, error) {
	var v struct {
		Product string `json:"product"`
	}
	if err := b.c.call(ctx, "", "Browser.getVersion", nil, &v); err != nil {
		return "", err
	}
	return v.Product, nil
}

// NewTab implements Browser. Tabs open in the background so Tether never
// steals focus from the person using the browser.
func (b *Chrome) NewTab(ctx context.Context) (Tab, error) {
	var res struct {
		TargetID string `json:"targetId"`
	}
	err := b.c.call(ctx, "", "Target.createTarget", map[string]any{
		"url":        "about:blank",
		"background": true,
	}, &res)
	if err != nil {
		return nil, err
	}
	t, err := b.attach(ctx, res.TargetID)
	if err != nil {
		_ = b.CloseTab(context.WithoutCancel(ctx), res.TargetID)
		return nil, err
	}
	return t, nil
}

// AttachTab implements Browser.
func (b *Chrome) AttachTab(ctx context.Context, id string) (Tab, error) {
	var info struct {
		TargetInfo struct {
			Type string `json:"type"`
		} `json:"targetInfo"`
	}
	if err := b.c.call(ctx, "", "Target.getTargetInfo", map[string]any{"targetId": id}, &info); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNoSuchTab, id)
	}
	if info.TargetInfo.Type != "page" {
		return nil, fmt.Errorf("%w: %s is a %s", ErrNoSuchTab, id, info.TargetInfo.Type)
	}
	return b.attach(ctx, id)
}

func (b *Chrome) attach(ctx context.Context, id string) (*chromeTab, error) {
	var res struct {
		SessionID string `json:"sessionId"`
	}
	if err := b.c.call(ctx, "", "Target.attachToTarget", map[string]any{
		"targetId": id,
		"flatten":  true,
	}, &res); err != nil {
		return nil, err
	}
	t := newTab(b, id, res.SessionID)
	domains := []string{"Page.enable", "Network.enable"}
	b.mu.Lock()
	if b.logSize > 0 {
		// Subscribe before enabling so entries Log.enable replays are kept.
		t.logs = newLogBuffer(b.logSize)
		b.logUnsubs[res.SessionID] = logSub{target: id, cancel: b.c.subscribe(res.SessionID, t.logs.handle)}
		domains = append(domains, "Runtime.enable", "Log.enable")
	}
	b.mu.Unlock()
	for _, m := range domains {
		if err := t.call(ctx, m, nil, nil); err != nil {
			b.dropLogs(res.SessionID)
			return nil, err
		}
	}
	return t, nil
}

func (b *Chrome) dropLogs(sessionID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.logUnsubs[sessionID]; ok {
		s.cancel()
		delete(b.logUnsubs, sessionID)
	}
}

// CloseTab implements Browser.
func (b *Chrome) CloseTab(ctx context.Context, id string) error {
	err := b.c.call(ctx, "", "Target.closeTarget", map[string]any{"targetId": id}, nil)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrNoSuchTab, id)
	}
	return nil
}

// Tabs implements Browser.
func (b *Chrome) Tabs(ctx context.Context) ([]TabInfo, error) {
	var res struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
			Title    string `json:"title"`
			URL      string `json:"url"`
		} `json:"targetInfos"`
	}
	if err := b.c.call(ctx, "", "Target.getTargets", nil, &res); err != nil {
		return nil, err
	}
	var out []TabInfo
	for _, t := range res.TargetInfos {
		if t.Type == "page" {
			out = append(out, TabInfo{ID: t.TargetID, URL: t.URL, Title: t.Title})
		}
	}
	return out, nil
}

// Quit asks Chrome itself to exit. Only for browsers Tether launched.
func (b *Chrome) Quit(ctx context.Context) error {
	return b.c.call(ctx, "", "Browser.close", nil, nil)
}

// Done implements Browser.
func (b *Chrome) Done() <-chan struct{} { return b.c.done }

// Close implements Browser. It drops the websocket; Chrome keeps running.
func (b *Chrome) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	return b.c.close()
}
