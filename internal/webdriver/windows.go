package webdriver

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"os"
	"slices"

	"github.com/adeelahmad/tether/internal/cdp"
	"github.com/adeelahmad/tether/internal/core"
	"github.com/adeelahmad/tether/internal/session"
)

// enterWindow locks the current window for a command and, when prompts
// apply, deals with an open user prompt first.
func (h *Handler) enterWindow(ctx context.Context, ws *wdSession, prompts bool) (func(), error) {
	cur := ws.s
	if cur == nil || cur.Closed() {
		return nil, wdErr(ErrNoSuchWindow, "the current window was closed; switch to another window")
	}
	unlock, err := cur.Lock(ctx)
	if err != nil {
		if errors.Is(err, session.ErrTabGone) {
			err = wdErr(ErrNoSuchWindow, "the current window was closed; switch to another window")
		}
		return nil, err
	}
	if prompts {
		if err := h.handlePrompt(ctx, ws); err != nil {
			unlock()
			return nil, err
		}
	}
	return unlock, nil
}

// W3C unhandledPromptBehavior values.
var promptBehaviors = []string{"dismiss", "accept", "dismiss and notify", "accept and notify", "ignore"}

// promptBehavior validates the unhandledPromptBehavior capability. The
// newer per-prompt-type map form is accepted through its "default" entry.
func promptBehavior(v any) (string, error) {
	switch t := v.(type) {
	case nil:
		return "dismiss and notify", nil
	case string:
		if slices.Contains(promptBehaviors, t) {
			return t, nil
		}
	case map[string]any:
		return promptBehavior(t["default"])
	}
	return "", wdErr(ErrInvalidArgument, "invalid unhandledPromptBehavior")
}

// handlePrompt applies unhandledPromptBehavior to a dialog that is open
// when a command arrives.
func (h *Handler) handlePrompt(ctx context.Context, ws *wdSession) error {
	d := ws.s.Tab.Dialog()
	if d == nil {
		return nil
	}
	if ws.prompt != "ignore" {
		accept := ws.prompt == "accept" || ws.prompt == "accept and notify"
		if err := ws.s.Tab.HandleDialog(ctx, accept, nil); err != nil {
			return err
		}
		ws.promptText = nil
		if ws.prompt == "accept" || ws.prompt == "dismiss" {
			return nil
		}
	}
	return &Error{Code: ErrUnexpectedAlertOpen, Message: "unexpected alert open: " + d.Message, Data: map[string]any{"text": d.Message}}
}

// dropWindow forgets a closed tab and returns how many windows remain.
func (ws *wdSession) dropWindow(id string) int {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.handles = slices.DeleteFunc(ws.handles, func(h string) bool { return h == id })
	return len(ws.handles)
}

func (ws *wdSession) windowList() []string {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return slices.Clone(ws.handles)
}

func (ws *wdSession) hasWindow(id string) bool {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return slices.Contains(ws.handles, id)
}

func (ws *wdSession) addWindow(id string) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if !slices.Contains(ws.handles, id) {
		ws.handles = append(ws.handles, id)
	}
}

func (ws *wdSession) addUpload(dir string) {
	ws.mu.Lock()
	ws.uploads = append(ws.uploads, dir)
	ws.mu.Unlock()
}

func (ws *wdSession) removeUploads() {
	ws.mu.Lock()
	dirs := ws.uploads
	ws.uploads = nil
	ws.mu.Unlock()
	for _, d := range dirs {
		_ = os.RemoveAll(d)
	}
}

// refreshHandles syncs the session's windows with Chrome: closed tabs
// drop out, and tabs opened by a session window (window.open, a
// target=_blank link), directly or through another popup, join.
func (h *Handler) refreshHandles(ctx context.Context, ws *wdSession) {
	tabs, err := h.core.Browser.Tabs(ctx)
	if err != nil {
		return
	}
	open := map[string]bool{}
	for _, t := range tabs {
		open[t.ID] = true
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.handles = slices.DeleteFunc(ws.handles, func(id string) bool { return !open[id] })
	mine := map[string]bool{}
	for _, id := range ws.handles {
		mine[id] = true
	}
	for grew := true; grew; {
		grew = false
		for _, t := range tabs {
			if !mine[t.ID] && !ws.ignore[t.ID] && t.OpenerID != "" && mine[t.OpenerID] {
				ws.handles = append(ws.handles, t.ID)
				mine[t.ID] = true
				grew = true
			}
		}
	}
}

// closeTab closes a session window, whether or not Tether has attached to
// it yet.
func (h *Handler) closeTab(ctx context.Context, id string) {
	err := h.core.Sessions.Close(ctx, id, true)
	if errors.Is(err, session.ErrTabGone) {
		err = h.core.Browser.CloseTab(ctx, id)
		if errors.Is(err, cdp.ErrNoSuchTab) {
			err = nil
		}
	}
	if err != nil {
		h.log.Warn("closing session window", "tab", id, "err", err)
	}
}

func (h *Handler) windowHandle(_ context.Context, ws *wdSession, _ *http.Request, _ []byte) (any, error) {
	return ws.s.ID, nil
}

func (h *Handler) windowHandles(ctx context.Context, ws *wdSession, _ *http.Request, _ []byte) (any, error) {
	h.refreshHandles(ctx, ws)
	return ws.windowList(), nil
}

func (h *Handler) switchWindow(ctx context.Context, ws *wdSession, _ *http.Request, body []byte) (any, error) {
	var req struct {
		Handle *string `json:"handle"`
	}
	if err := decodeBody(body, &req); err != nil {
		return nil, err
	}
	if req.Handle == nil {
		return nil, wdErr(ErrInvalidArgument, "handle is required")
	}
	h.refreshHandles(ctx, ws)
	if !ws.hasWindow(*req.Handle) {
		return nil, wdErr(ErrNoSuchWindow, "no window with handle "+*req.Handle+" in this session")
	}
	// A popup is attached on first use, as the session's own tab under
	// its owner and session rules.
	s, err := h.core.AdoptPopup(ctx, *req.Handle, ws.s)
	if err != nil {
		if errors.Is(err, core.ErrForbidden) {
			ws.dropWindow(*req.Handle)
			return nil, err
		}
		return nil, wdErr(ErrNoSuchWindow, err.Error())
	}
	ws.s, ws.frames, ws.promptText = s, nil, nil
	return nil, nil
}

// closeWindow closes the current window. Closing the last one ends the
// session.
func (h *Handler) closeWindow(ctx context.Context, ws *wdSession, _ *http.Request, _ []byte) (any, error) {
	h.refreshHandles(ctx, ws)
	h.closeTab(context.WithoutCancel(ctx), ws.s.ID)
	ws.dropWindow(ws.s.ID)
	ws.frames = nil
	return ws.windowList(), nil
}

func (h *Handler) newWindow(ctx context.Context, ws *wdSession, _ *http.Request, body []byte) (any, error) {
	var req struct {
		Type string `json:"type"`
	}
	if err := decodeBody(body, &req); err != nil {
		return nil, err
	}
	var s *session.Session
	var err error
	typ := "tab"
	if req.Type == "window" {
		typ = "window"
		s, err = h.core.Sessions.CreateWindow(ctx)
	} else {
		s, err = h.core.Sessions.Create(ctx)
	}
	if err != nil {
		return nil, err
	}
	// The new window lives under the same session rules as the session.
	if err := h.core.SetSessionRules(ctx, s, ws.s.Rules()); err != nil {
		h.closeTab(context.WithoutCancel(ctx), s.ID)
		return nil, err
	}
	ws.addWindow(s.ID)
	return map[string]any{"handle": s.ID, "type": typ}, nil
}

func rectOf(w cdp.Window) map[string]any {
	return map[string]any{"x": w.Left, "y": w.Top, "width": w.Width, "height": w.Height}
}

func (h *Handler) windowRect(ctx context.Context, ws *wdSession, _ *http.Request, _ []byte) (any, error) {
	w, err := h.core.Browser.Window(ctx, ws.s.ID)
	if err != nil {
		return nil, err
	}
	return rectOf(w), nil
}

// ownsWindow reports whether every tab in the current window belongs to
// this session or was opened by Tether. Tether never resizes, minimises
// or fullscreens a window holding tabs the person is using; there the
// window commands leave it alone and report its rect.
func (h *Handler) ownsWindow(ctx context.Context, ws *wdSession) (bool, error) {
	w, err := h.core.Browser.Window(ctx, ws.s.ID)
	if err != nil {
		return false, err
	}
	tabs, err := h.core.Browser.Tabs(ctx)
	if err != nil {
		return false, err
	}
	for _, t := range tabs {
		if ws.hasWindow(t.ID) {
			continue
		}
		if s := h.core.Sessions.Lookup(t.ID); s != nil && s.Owned {
			continue
		}
		if tw, err := h.core.Browser.Window(ctx, t.ID); err == nil && tw.ID == w.ID {
			return false, nil
		}
	}
	return true, nil
}

func (h *Handler) changeWindow(ctx context.Context, ws *wdSession, b cdp.WindowBounds) (any, error) {
	own, err := h.ownsWindow(ctx, ws)
	if err != nil {
		return nil, err
	}
	if !own {
		h.log.Debug("window shared with other tabs; leaving it as is", "tab", ws.s.ID)
		return h.windowRect(ctx, ws, nil, nil)
	}
	w, err := h.core.Browser.SetWindow(ctx, ws.s.ID, b)
	if err != nil {
		return nil, err
	}
	return rectOf(w), nil
}

func (h *Handler) setWindowRect(ctx context.Context, ws *wdSession, _ *http.Request, body []byte) (any, error) {
	var req map[string]json.RawMessage
	if err := decodeBody(body, &req); err != nil {
		return nil, err
	}
	var b cdp.WindowBounds
	for _, f := range []struct {
		name string
		dst  **int
		min  float64
	}{{"x", &b.Left, math.MinInt32}, {"y", &b.Top, math.MinInt32}, {"width", &b.Width, 0}, {"height", &b.Height, 0}} {
		raw, ok := req[f.name]
		if !ok || string(raw) == "null" {
			continue
		}
		var n float64
		if err := json.Unmarshal(raw, &n); err != nil || n != math.Trunc(n) || n < f.min || n > math.MaxInt32 {
			return nil, wdErr(ErrInvalidArgument, f.name+" must be an integer in range")
		}
		v := int(n)
		*f.dst = &v
	}
	return h.changeWindow(ctx, ws, b)
}

func (h *Handler) windowState(state string) command {
	return func(ctx context.Context, ws *wdSession, _ *http.Request, _ []byte) (any, error) {
		return h.changeWindow(ctx, ws, cdp.WindowBounds{State: state})
	}
}
