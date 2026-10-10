package webdriver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/adeelahmad/tether/internal/cdp"
	"github.com/adeelahmad/tether/internal/core"
)

func (h *Handler) navigate(ctx context.Context, ws *wdSession, _ *http.Request, body []byte) (any, error) {
	var req struct {
		URL string `json:"url"`
	}
	if err := decodeBody(body, &req); err != nil {
		return nil, err
	}
	if req.URL == "" {
		return nil, wdErr(ErrInvalidArgument, "url is required")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(ws.getTimeouts().PageLoad)*time.Millisecond)
	defer cancel()
	if _, err := h.core.Navigate(ctx, ws.s, req.URL, ws.wait()); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, wdErr(ErrTimeout, "page load timed out")
		}
		return nil, err
	}
	return nil, nil
}

func evalString(ctx context.Context, tab cdp.Tab, expr string) (string, error) {
	v, err := tab.Evaluate(ctx, expr, false)
	if err != nil {
		return "", err
	}
	var s string
	_ = json.Unmarshal(v, &s)
	return s, nil
}

func (h *Handler) currentURL(ctx context.Context, ws *wdSession, _ *http.Request, _ []byte) (any, error) {
	return evalString(ctx, ws.s.Tab, "location.href")
}

func (h *Handler) title(ctx context.Context, ws *wdSession, _ *http.Request, _ []byte) (any, error) {
	return evalString(ctx, ws.s.Tab, "document.title")
}

func (h *Handler) source(ctx context.Context, ws *wdSession, _ *http.Request, _ []byte) (any, error) {
	return evalString(ctx, ws.s.Tab, core.RenderedHTML)
}

func (h *Handler) history(delta int) command {
	return func(ctx context.Context, ws *wdSession, _ *http.Request, _ []byte) (any, error) {
		ctx, cancel := context.WithTimeout(ctx, time.Duration(ws.getTimeouts().PageLoad)*time.Millisecond)
		defer cancel()
		return nil, ws.s.Tab.History(ctx, delta)
	}
}

func (h *Handler) refresh(ctx context.Context, ws *wdSession, _ *http.Request, _ []byte) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(ws.getTimeouts().PageLoad)*time.Millisecond)
	defer cancel()
	return nil, ws.s.Tab.Reload(ctx, ws.wait())
}

// A WebDriver session is exactly one tab, so it has exactly one window.

func (h *Handler) windowHandle(_ context.Context, ws *wdSession, _ *http.Request, _ []byte) (any, error) {
	return ws.s.ID, nil
}

func (h *Handler) windowHandles(_ context.Context, ws *wdSession, _ *http.Request, _ []byte) (any, error) {
	return []string{ws.s.ID}, nil
}

func (h *Handler) switchWindow(_ context.Context, ws *wdSession, _ *http.Request, body []byte) (any, error) {
	var req struct {
		Handle string `json:"handle"`
	}
	if err := decodeBody(body, &req); err != nil {
		return nil, err
	}
	if req.Handle != ws.s.ID {
		return nil, wdErr(ErrNoSuchWindow, "a Tether session owns exactly one tab ("+ws.s.ID+")")
	}
	return nil, nil
}

func (h *Handler) closeWindow(ctx context.Context, ws *wdSession, _ *http.Request, _ []byte) (any, error) {
	h.mu.Lock()
	delete(h.sessions, ws.s.ID)
	h.mu.Unlock()
	if err := h.core.Sessions.Close(context.WithoutCancel(ctx), ws.s.ID, true); err != nil {
		return nil, err
	}
	return []string{}, nil
}

// windowRect reports the window geometry. Resizing a person's real browser
// window is deliberately not supported, so set/maximize are no-ops that
// return the current rect (which keeps maximize_window() scripts working).
func (h *Handler) windowRect(ctx context.Context, ws *wdSession, _ *http.Request, _ []byte) (any, error) {
	v, err := ws.s.Tab.Evaluate(ctx, `({x: window.screenX, y: window.screenY, width: window.outerWidth, height: window.outerHeight})`, false)
	if err != nil {
		return nil, err
	}
	return v, nil
}

func (h *Handler) screenshot(ctx context.Context, ws *wdSession, _ *http.Request, _ []byte) (any, error) {
	png, err := ws.s.Tab.Screenshot(ctx, nil)
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.EncodeToString(png), nil
}

func (h *Handler) execute(async bool) command {
	return func(ctx context.Context, ws *wdSession, _ *http.Request, body []byte) (any, error) {
		var req struct {
			Script *string           `json:"script"`
			Args   []json.RawMessage `json:"args"`
		}
		if err := decodeBody(body, &req); err != nil {
			return nil, err
		}
		if req.Script == nil {
			return nil, wdErr(ErrInvalidArgument, "script is required")
		}
		if t := ws.getTimeouts().Script; t != nil {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, time.Duration(*t)*time.Millisecond)
			defer cancel()
		}
		v, err := runScript(ctx, ws.s.Tab, *req.Script, req.Args, async)
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, wdErr(ErrScriptTimeout, "script did not complete within the script timeout")
		}
		return v, err
	}
}

// W3C cookie serialisation.
type wdCookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Path     string `json:"path,omitempty"`
	Domain   string `json:"domain,omitempty"`
	Secure   bool   `json:"secure"`
	HTTPOnly bool   `json:"httpOnly"`
	Expiry   *int64 `json:"expiry,omitempty"`
	SameSite string `json:"sameSite,omitempty"`
}

func fromCDP(c cdp.Cookie) wdCookie {
	w := wdCookie{Name: c.Name, Value: c.Value, Path: c.Path, Domain: c.Domain, Secure: c.Secure, HTTPOnly: c.HTTPOnly, SameSite: c.SameSite}
	if c.Expires > 0 {
		e := int64(math.Floor(c.Expires))
		w.Expiry = &e
	}
	if w.SameSite == "" {
		w.SameSite = "Lax"
	}
	return w
}

func pageCookies(ctx context.Context, ws *wdSession) ([]wdCookie, string, error) {
	u, err := evalString(ctx, ws.s.Tab, "location.href")
	if err != nil {
		return nil, "", err
	}
	if !strings.HasPrefix(u, "http") {
		return []wdCookie{}, u, nil
	}
	cs, err := ws.s.Tab.Cookies(ctx, []string{u})
	if err != nil {
		return nil, "", err
	}
	out := make([]wdCookie, 0, len(cs))
	for _, c := range cs {
		out = append(out, fromCDP(c))
	}
	return out, u, nil
}

func (h *Handler) getCookies(ctx context.Context, ws *wdSession, _ *http.Request, _ []byte) (any, error) {
	cs, _, err := pageCookies(ctx, ws)
	return cs, err
}

func (h *Handler) getCookie(ctx context.Context, ws *wdSession, r *http.Request, _ []byte) (any, error) {
	cs, _, err := pageCookies(ctx, ws)
	if err != nil {
		return nil, err
	}
	name := r.PathValue("name")
	for _, c := range cs {
		if c.Name == name {
			return c, nil
		}
	}
	return nil, wdErr(ErrNoSuchCookie, "no cookie named "+name)
}

func (h *Handler) addCookie(ctx context.Context, ws *wdSession, _ *http.Request, body []byte) (any, error) {
	var req struct {
		Cookie *wdCookie `json:"cookie"`
	}
	if err := decodeBody(body, &req); err != nil {
		return nil, err
	}
	if req.Cookie == nil || req.Cookie.Name == "" {
		return nil, wdErr(ErrInvalidArgument, "cookie with a name is required")
	}
	u, err := evalString(ctx, ws.s.Tab, "location.href")
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(u, "http") {
		return nil, wdErr(ErrUnableToSetCookie, "cookies need an http(s) page; navigate first")
	}
	c := cdp.Cookie{
		Name: req.Cookie.Name, Value: req.Cookie.Value, Domain: req.Cookie.Domain,
		Path: req.Cookie.Path, Secure: req.Cookie.Secure, HTTPOnly: req.Cookie.HTTPOnly,
		SameSite: req.Cookie.SameSite,
	}
	if c.Domain == "" {
		c.URL = u
	}
	if c.Path == "" {
		c.Path = "/"
	}
	if req.Cookie.Expiry != nil {
		c.Expires = float64(*req.Cookie.Expiry)
	}
	if err := ws.s.Tab.SetCookie(ctx, c); err != nil {
		return nil, wdErr(ErrUnableToSetCookie, err.Error())
	}
	return nil, nil
}

func (h *Handler) deleteCookie(ctx context.Context, ws *wdSession, r *http.Request, _ []byte) (any, error) {
	u, err := evalString(ctx, ws.s.Tab, "location.href")
	if err != nil {
		return nil, err
	}
	return nil, ws.s.Tab.DeleteCookie(ctx, r.PathValue("name"), u)
}

func (h *Handler) deleteAllCookies(ctx context.Context, ws *wdSession, _ *http.Request, _ []byte) (any, error) {
	cs, u, err := pageCookies(ctx, ws)
	if err != nil {
		return nil, err
	}
	for _, c := range cs {
		if err := ws.s.Tab.DeleteCookie(ctx, c.Name, u); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func browserVersion(product string) string {
	if _, v, ok := strings.Cut(product, "/"); ok {
		return v
	}
	return product
}

func platformName() string {
	switch runtime.GOOS {
	case "darwin":
		return "mac"
	case "windows":
		return "windows"
	}
	return runtime.GOOS
}
