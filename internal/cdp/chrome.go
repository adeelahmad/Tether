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
func (b *Chrome) NewTab(ctx context.Context) (Tab, error) { return b.newTarget(ctx, false) }

// NewWindow implements Browser.
func (b *Chrome) NewWindow(ctx context.Context) (Tab, error) { return b.newTarget(ctx, true) }

func (b *Chrome) newTarget(ctx context.Context, newWindow bool) (Tab, error) {
	var res struct {
		TargetID string `json:"targetId"`
	}
	params := map[string]any{"url": "about:blank", "background": true}
	if newWindow {
		params["newWindow"] = true
	}
	err := b.c.call(ctx, "", "Target.createTarget", params, &res)
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
			t.stop()
			return nil, err
		}
	}
	// Tether's tabs live in the background. Emulating focus makes Chrome
	// route input to them (and into their cross-site iframes) promptly,
	// as it does for the active tab, without stealing the person's focus.
	if err := t.call(ctx, "Emulation.setFocusEmulationEnabled", map[string]any{"enabled": true}, nil); err != nil {
		b.log.Debug("focus emulation unavailable", "err", err)
	}
	// Out-of-process (cross-site) iframes are separate targets; attach to
	// them so frames work regardless of process. Best effort: without it
	// only same-process frames can be entered.
	if err := t.call(ctx, "Target.setAutoAttach", autoAttachParams, nil); err != nil {
		b.log.Debug("auto-attach to iframes unavailable", "err", err)
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
			OpenerID string `json:"openerId"`
		} `json:"targetInfos"`
	}
	if err := b.c.call(ctx, "", "Target.getTargets", nil, &res); err != nil {
		return nil, err
	}
	var out []TabInfo
	for _, t := range res.TargetInfos {
		if t.Type == "page" {
			out = append(out, TabInfo{ID: t.TargetID, URL: t.URL, Title: t.Title, OpenerID: t.OpenerID})
		}
	}
	return out, nil
}

// Window implements Browser.
func (b *Chrome) Window(ctx context.Context, tabID string) (Window, error) {
	var res struct {
		WindowID int    `json:"windowId"`
		Bounds   Window `json:"bounds"`
	}
	if err := b.c.call(ctx, "", "Browser.getWindowForTarget", map[string]any{"targetId": tabID}, &res); err != nil {
		return Window{}, err
	}
	w := res.Bounds
	w.ID = res.WindowID
	return w, nil
}

// SetWindow implements Browser. Chrome only resizes a window in the
// normal state, so a maximised, minimised or fullscreen window is
// restored first.
func (b *Chrome) SetWindow(ctx context.Context, tabID string, wb WindowBounds) (Window, error) {
	w, err := b.Window(ctx, tabID)
	if err != nil {
		return Window{}, err
	}
	set := func(bounds map[string]any) error {
		return b.c.call(ctx, "", "Browser.setWindowBounds", map[string]any{"windowId": w.ID, "bounds": bounds}, nil)
	}
	if wb.State != "" && wb.State != "normal" {
		if w.State != wb.State {
			if w.State != "normal" {
				if err := set(map[string]any{"windowState": "normal"}); err != nil {
					return Window{}, err
				}
			}
			if err := set(map[string]any{"windowState": wb.State}); err != nil {
				return Window{}, err
			}
		}
		return b.Window(ctx, tabID)
	}
	if w.State != "normal" {
		if err := set(map[string]any{"windowState": "normal"}); err != nil {
			return Window{}, err
		}
	}
	bounds := map[string]any{}
	for k, v := range map[string]*int{"left": wb.Left, "top": wb.Top, "width": wb.Width, "height": wb.Height} {
		if v != nil {
			bounds[k] = *v
		}
	}
	if len(bounds) > 0 {
		if err := set(bounds); err != nil {
			return Window{}, err
		}
	}
	return b.Window(ctx, tabID)
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
