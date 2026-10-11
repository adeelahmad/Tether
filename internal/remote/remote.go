// Package remote is Tether's fourth face: an interactive remote-browser
// web app, embedded in the binary and served at /remote/.
//
// A person opens it in any browser, picks a tab of the real Chrome, and
// sees it live: frames come from CDP Page.startScreencast (topped up with
// screenshots while the page is quiet) over a websocket, and mouse,
// wheel, keyboard and paste input go back through Input.dispatch*.
// Navigation from the address bar goes through core.Navigate, so the
// domain policy applies, and every tab is a session of the shared
// session.Manager: a person can watch, or take over, the very tab an agent
// or a WebDriver script is driving.
//
// Authentication is the server's: the bearer token (or the UI login
// cookie the server issues for it) and the loopback guard sit in front of
// this handler. See docs/REMOTE.md.
package remote

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/adeelahmad/tether/internal/cdp"
	"github.com/adeelahmad/tether/internal/core"
	"github.com/adeelahmad/tether/internal/httpx"
	"github.com/adeelahmad/tether/internal/session"
	"github.com/adeelahmad/tether/internal/version"
)

// Prefix is where the remote UI is served.
const Prefix = "/remote"

//go:embed assets
var assets embed.FS

// errNotImplemented marks a browser backend without screencast support.
var errNotImplemented = errors.New("not implemented")

// Options configure the remote UI.
type Options struct {
	// Screencast tunes the video stream (quality, size, keyframe rate).
	Screencast cdp.ScreencastOptions
	// Auth says the server requires a token, so the UI offers sign-out.
	Auth   bool
	Logger *slog.Logger
}

// UI is the remote-browser face.
type UI struct {
	core *core.Core
	hub  *hub
	log  *slog.Logger
	mux  *http.ServeMux
	auth bool

	done      chan struct{}
	closeOnce sync.Once
}

var _ core.Face = (*UI)(nil)

// New returns the remote UI face over c.
func New(c *core.Core, opts Options) *UI {
	lg := opts.Logger
	if lg == nil {
		lg = c.Log
	}
	u := &UI{
		core: c,
		hub:  newHub(opts.Screencast, lg),
		log:  lg,
		mux:  http.NewServeMux(),
		auth: opts.Auth,
		done: make(chan struct{}),
	}
	c.Sessions.OnClose(func(s *session.Session, _ session.CloseReason) { u.hub.end(s.ID) })

	static, err := fs.Sub(assets, "assets")
	if err != nil {
		panic(err) // the embed pattern guarantees the directory
	}
	files := http.StripPrefix(Prefix+"/static/", http.FileServerFS(static))

	u.mux.HandleFunc("GET "+Prefix, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, Prefix+"/", http.StatusMovedPermanently)
	})
	u.mux.HandleFunc("GET "+Prefix+"/{$}", u.index)
	u.mux.Handle("GET "+Prefix+"/static/", files)
	u.mux.HandleFunc("GET "+Prefix+"/api/info", u.info)
	u.mux.HandleFunc("GET "+Prefix+"/api/tabs", u.listTabs)
	u.mux.HandleFunc("POST "+Prefix+"/api/tabs", u.createTab)
	u.mux.HandleFunc("DELETE "+Prefix+"/api/tabs/{id}", u.closeTab)
	u.mux.HandleFunc("GET "+Prefix+"/ws/{id}", u.serveWS)
	u.mux.HandleFunc(Prefix+"/", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
	return u
}

// Name implements core.Face.
func (u *UI) Name() string { return "remote" }

// ServeHTTP implements core.Face.
func (u *UI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	// The UI shows and drives a logged-in browser: never frame it, never
	// leak its URLs, and only ever run its own scripts.
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' blob: data:; style-src 'self'; "+
		"script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	u.mux.ServeHTTP(w, r)
}

// Close disconnects every viewer and stops every screencast. Websockets
// are hijacked connections, so graceful HTTP shutdown alone would not end
// them.
func (u *UI) Close() {
	u.closeOnce.Do(func() {
		close(u.done)
		u.hub.stopAll()
	})
}

func (u *UI) index(w http.ResponseWriter, r *http.Request) {
	b, err := assets.ReadFile("assets/index.html")
	if err != nil {
		u.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func (u *UI) info(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	product, _ := u.core.Browser.Version(ctx)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"version": version.Version,
		"browser": product,
		"auth":    u.auth,
	})
}

// tabView is one Chrome tab as the UI lists it.
type tabView struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	Title   string `json:"title"`
	Session bool   `json:"session"`          // known to Tether (opened or adopted)
	Owned   bool   `json:"owned"`            // opened by Tether
	Domain  string `json:"domain,omitempty"` // default tab for this site
}

// listTabs lists every page in Chrome, not just Tether's sessions: the
// point of the UI is to reach any tab of the real browser.
func (u *UI) listTabs(w http.ResponseWriter, r *http.Request) {
	tabs, err := u.core.Browser.Tabs(r.Context())
	if err != nil {
		u.fail(w, err)
		return
	}
	known := map[string]session.Info{}
	for _, i := range u.core.Sessions.List() {
		known[i.ID] = i
	}
	out := make([]tabView, 0, len(tabs))
	for _, t := range tabs {
		v := tabView{ID: t.ID, URL: t.URL, Title: t.Title}
		if i, ok := known[t.ID]; ok {
			v.Session, v.Owned, v.Domain = true, i.Owned, i.Domain
		}
		out = append(out, v)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"tabs": out})
}

// createTab opens a new owned tab and starts loading url in it (subject to
// policy). It returns once the navigation has started; the viewer watches
// it load.
func (u *UI) createTab(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if err := decodeJSON(r, &req); err != nil {
		u.fail(w, err)
		return
	}
	s, err := u.core.Sessions.Create(r.Context())
	if err != nil {
		u.fail(w, err)
		return
	}
	target := ""
	if req.URL != "" {
		target = normalizeURL(req.URL)
		err := func() error {
			unlock, err := s.Lock(r.Context())
			if err != nil {
				return err
			}
			defer unlock()
			_, err = u.core.Navigate(r.Context(), s, target, cdp.WaitNone)
			return err
		}()
		if err != nil {
			_ = u.core.Sessions.Close(context.WithoutCancel(r.Context()), s.ID, false)
			u.fail(w, err)
			return
		}
	}
	w.Header().Set("X-Tether-Tab-ID", s.ID)
	httpx.WriteJSON(w, http.StatusCreated, tabView{ID: s.ID, URL: target, Session: true, Owned: true})
}

// closeTab ends a session. Tabs Tether did not open are only released
// unless force=true, exactly like the control API.
func (u *UI) closeTab(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	force := r.URL.Query().Get("force") == "true"
	if _, err := u.core.Sessions.Get(r.Context(), id); err != nil {
		u.fail(w, err)
		return
	}
	if err := u.core.Sessions.Close(r.Context(), id, force); err != nil {
		u.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decodeJSON(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		return errors.Join(core.ErrBadRequest, err)
	}
	if len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, v); err != nil {
		return errors.Join(core.ErrBadRequest, err)
	}
	return nil
}

func (u *UI) fail(w http.ResponseWriter, err error) {
	status, code := core.Classify(err)
	if errors.Is(err, errNotImplemented) {
		status, code = http.StatusNotImplemented, core.CodeNotImplemented
	}
	httpx.WriteError(w, status, code, err.Error())
}

// normalizeURL turns what a person types in an address bar into a URL:
// full URLs pass through, host-like input gets a scheme, anything else is
// a search.
func normalizeURL(in string) string {
	s := strings.TrimSpace(in)
	if s == "" {
		return "about:blank"
	}
	if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.Opaque == "" || hasScheme(s) {
		return s
	}
	host := s
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	h := host
	if i := strings.LastIndex(h, ":"); i >= 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	switch {
	case strings.ContainsAny(s, " \t"):
	case h == "localhost" || strings.HasPrefix(h, "127.") || h == "[::1]":
		return "http://" + s
	case strings.Contains(h, ".") && !strings.HasSuffix(h, "."):
		return "https://" + s
	}
	return "https://www.google.com/search?q=" + url.QueryEscape(s)
}

func hasScheme(s string) bool {
	for _, p := range []string{"about:", "data:", "chrome:", "file:", "view-source:"} {
		if strings.HasPrefix(strings.ToLower(s), p) {
			return true
		}
	}
	return false
}
