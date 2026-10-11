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

	mu     sync.Mutex
	hooks  []func(id string)
	closed bool
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
	b := &Chrome{c: c, log: log}
	c.subscribe("", b.onEvent)
	if err := c.call(ctx, "", "Target.setDiscoverTargets", map[string]any{"discover": true}, nil); err != nil {
		_ = c.close()
		return nil, err
	}
	return b, nil
}

func (b *Chrome) onEvent(e Event) {
	if e.Method != "Target.targetDestroyed" {
		return
	}
	var p struct {
		TargetID string `json:"targetId"`
	}
	if json.Unmarshal(e.Params, &p) != nil {
		return
	}
	b.mu.Lock()
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
	for _, m := range []string{"Page.enable", "Network.enable"} {
		if err := t.call(ctx, m, nil, nil); err != nil {
			return nil, err
		}
	}
	return t, nil
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
