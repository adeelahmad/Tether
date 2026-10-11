// Package webdriver is Tether's W3C WebDriver face. It implements the
// common command set — enough that most Selenium scripts just work — on
// top of the same tab sessions the proxy uses. A WebDriver session ID is
// the Chrome tab ID, so a tab can be driven by WebDriver and the proxy
// (X-Tether-Tab-ID) interchangeably.
//
// The covered surface is listed in docs/WEBDRIVER.md. Anything else
// answers "unknown command".
package webdriver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/adeelahmad/tether/internal/cdp"
	"github.com/adeelahmad/tether/internal/core"
	"github.com/adeelahmad/tether/internal/logx"
	"github.com/adeelahmad/tether/internal/policy"
	"github.com/adeelahmad/tether/internal/session"
	"github.com/adeelahmad/tether/internal/version"
)

// ElementKey is the W3C web element identifier key.
const ElementKey = "element-6066-11e4-a52e-4f735466cecf"

// Handler serves the WebDriver endpoints at / and /wd/hub.
type Handler struct {
	core *core.Core
	log  *slog.Logger
	mux  *http.ServeMux

	mu       sync.Mutex
	sessions map[string]*wdSession
}

var _ core.Face = (*Handler)(nil)

// Timeouts are the W3C session timeouts, in milliseconds.
type Timeouts struct {
	Script   *int64 `json:"script"`
	PageLoad int64  `json:"pageLoad"`
	Implicit int64  `json:"implicit"`
}

type wdSession struct {
	s        *session.Session
	keepTab  bool
	strategy string // pageLoadStrategy

	mu       sync.Mutex
	timeouts Timeouts
}

func (ws *wdSession) getTimeouts() Timeouts {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.timeouts
}

func (ws *wdSession) wait() cdp.Wait {
	switch ws.strategy {
	case "eager":
		return cdp.WaitDOMContentLoaded
	case "none":
		return cdp.WaitNone
	}
	return cdp.WaitLoad
}

// New returns the WebDriver face.
func New(c *core.Core, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	h := &Handler{core: c, log: log.With("face", "webdriver"), mux: http.NewServeMux(), sessions: map[string]*wdSession{}}
	c.Sessions.OnClose(func(s *session.Session, _ session.CloseReason) {
		h.mu.Lock()
		delete(h.sessions, s.ID)
		h.mu.Unlock()
	})
	h.routes()
	return h
}

// Name implements core.Face.
func (h *Handler) Name() string { return "webdriver" }

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

type command func(ctx context.Context, ws *wdSession, r *http.Request, body []byte) (any, error)

func (h *Handler) routes() {
	for _, p := range []string{"", "/wd/hub"} {
		h.mux.HandleFunc("GET "+p+"/status", h.status)
		h.mux.HandleFunc("POST "+p+"/session", h.newSession)
		h.mux.HandleFunc("DELETE "+p+"/session/{sid}", h.deleteSession)

		c := func(pattern string, fn command) { h.mux.Handle(withPrefix(pattern, p), h.cmd(fn)) }
		c("GET /session/{sid}/timeouts", h.getTimeouts)
		c("POST /session/{sid}/timeouts", h.setTimeouts)

		c("POST /session/{sid}/url", h.navigate)
		c("GET /session/{sid}/url", h.currentURL)
		c("POST /session/{sid}/back", h.history(-1))
		c("POST /session/{sid}/forward", h.history(+1))
		c("POST /session/{sid}/refresh", h.refresh)
		c("GET /session/{sid}/title", h.title)

		c("GET /session/{sid}/window", h.windowHandle)
		c("GET /session/{sid}/window/handles", h.windowHandles)
		c("POST /session/{sid}/window", h.switchWindow)
		c("DELETE /session/{sid}/window", h.closeWindow)
		c("GET /session/{sid}/window/rect", h.windowRect)
		c("POST /session/{sid}/window/rect", h.windowRect)
		c("POST /session/{sid}/window/maximize", h.windowRect)

		c("POST /session/{sid}/element", h.findElement(false))
		c("POST /session/{sid}/elements", h.findElements(false))
		c("POST /session/{sid}/element/{eid}/element", h.findElement(true))
		c("POST /session/{sid}/element/{eid}/elements", h.findElements(true))
		c("GET /session/{sid}/element/active", h.activeElement)
		c("POST /session/{sid}/element/{eid}/click", h.click)
		c("POST /session/{sid}/element/{eid}/clear", h.clear)
		c("POST /session/{sid}/element/{eid}/value", h.sendKeys)
		c("GET /session/{sid}/element/{eid}/text", h.elementScript(scriptText))
		c("GET /session/{sid}/element/{eid}/name", h.elementScript(scriptTagName))
		c("GET /session/{sid}/element/{eid}/rect", h.elementScript(scriptRect))
		c("GET /session/{sid}/element/{eid}/enabled", h.elementScript(scriptEnabled))
		c("GET /session/{sid}/element/{eid}/selected", h.elementScript(scriptSelected))
		c("GET /session/{sid}/element/{eid}/displayed", h.elementScript(scriptDisplayed))
		c("GET /session/{sid}/element/{eid}/attribute/{name}", h.elementScript(scriptAttribute, "name"))
		c("GET /session/{sid}/element/{eid}/property/{name}", h.elementScript(scriptProperty, "name"))
		c("GET /session/{sid}/element/{eid}/css/{name}", h.elementScript(scriptCSS, "name"))
		c("GET /session/{sid}/element/{eid}/screenshot", h.elementScreenshot)

		c("GET /session/{sid}/source", h.source)
		c("POST /session/{sid}/execute/sync", h.execute(false))
		c("POST /session/{sid}/execute/async", h.execute(true))

		c("GET /session/{sid}/cookie", h.getCookies)
		c("GET /session/{sid}/cookie/{name}", h.getCookie)
		c("POST /session/{sid}/cookie", h.addCookie)
		c("DELETE /session/{sid}/cookie/{name}", h.deleteCookie)
		c("DELETE /session/{sid}/cookie", h.deleteAllCookies)

		c("GET /session/{sid}/screenshot", h.screenshot)
	}
	h.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, wdErr(ErrUnknownCommand, r.Method+" "+r.URL.Path+" is not supported by Tether; see docs/WEBDRIVER.md"))
	})
}

func withPrefix(pattern, prefix string) string {
	method, path, _ := cutSpace(pattern)
	return method + " " + prefix + path
}

func cutSpace(s string) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' {
			return s[:i], s[i+1:], true
		}
	}
	return "", s, false
}

func (h *Handler) lookup(ctx context.Context, id string) (*wdSession, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ws, ok := h.sessions[id]
	if !ok || ws.s.Closed() || !session.Visible(ctx, ws.s) {
		return nil, wdErr(ErrInvalidSessionID, "no active session with ID "+id)
	}
	return ws, nil
}

func (h *Handler) cmd(fn command) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := h.lookup(r.Context(), r.PathValue("sid"))
		if err != nil {
			writeError(w, err)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
		if err != nil {
			writeError(w, wdErr(ErrInvalidArgument, err.Error()))
			return
		}
		limit := time.Duration(ws.getTimeouts().PageLoad) * time.Millisecond
		if limit < h.core.Timeout {
			limit = h.core.Timeout
		}
		ctx, cancel := context.WithTimeout(r.Context(), limit+5*time.Second)
		defer cancel()
		unlock, err := ws.s.Lock(ctx)
		if err != nil {
			if errors.Is(err, session.ErrTabGone) {
				err = wdErr(ErrInvalidSessionID, "session ended: the tab was closed")
			}
			writeError(w, err)
			return
		}
		defer unlock()
		v, err := fn(ctx, ws, r, body)
		if err != nil {
			h.log.Debug("command failed", "path", logx.Clean(r.URL.Path), "err", logx.Clean(err.Error()))
			writeError(w, err)
			return
		}
		writeValue(w, http.StatusOK, v)
	})
}

func decodeBody(body []byte, v any) error {
	if len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, v); err != nil {
		return wdErr(ErrInvalidArgument, "malformed JSON body: "+err.Error())
	}
	return nil
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	_, err := h.core.Browser.Version(ctx)
	msg := "Tether " + version.Version + " is ready"
	if err != nil {
		msg = "browser unavailable: " + err.Error()
	}
	writeValue(w, http.StatusOK, map[string]any{"ready": err == nil, "message": msg})
}

// TetherOptions are Tether's vendor capabilities ("tether:options").
type TetherOptions struct {
	// TabID attaches the session to an existing tab instead of opening one.
	TabID string `json:"tabId"`
	// KeepTab leaves the tab open when the session is deleted. It defaults
	// to true for attached tabs and false for new ones.
	KeepTab *bool `json:"keepTab"`
	// Policy sets session rules on the tab: domains it may (allow) and may
	// not (deny) navigate to, on top of the global and token rules.
	Policy *policy.Rules `json:"policy"`
}

func mergeCapabilities(body []byte) (map[string]any, error) {
	var req struct {
		Capabilities struct {
			AlwaysMatch map[string]any   `json:"alwaysMatch"`
			FirstMatch  []map[string]any `json:"firstMatch"`
		} `json:"capabilities"`
	}
	if err := decodeBody(body, &req); err != nil {
		return nil, err
	}
	caps := map[string]any{}
	for k, v := range req.Capabilities.AlwaysMatch {
		caps[k] = v
	}
	if len(req.Capabilities.FirstMatch) > 0 {
		for k, v := range req.Capabilities.FirstMatch[0] {
			if _, dup := caps[k]; dup {
				return nil, wdErr(ErrInvalidArgument, "capability "+k+" is in both alwaysMatch and firstMatch")
			}
			caps[k] = v
		}
	}
	if bn, ok := caps["browserName"].(string); ok {
		switch bn {
		case "", "chrome", "chromium", "tether", "MicrosoftEdge", "msedge":
		default:
			return nil, wdErr(ErrSessionNotCreated, "Tether drives Chrome; browserName "+bn+" is not available")
		}
	}
	return caps, nil
}

func (h *Handler) newSession(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	caps, err := mergeCapabilities(body)
	if err != nil {
		writeError(w, err)
		return
	}
	var opts TetherOptions
	if raw, ok := caps["tether:options"]; ok {
		b, _ := json.Marshal(raw)
		if err := json.Unmarshal(b, &opts); err != nil {
			writeError(w, wdErr(ErrInvalidArgument, "tether:options: "+err.Error()))
			return
		}
	}
	var rules policy.Rules
	if opts.Policy != nil {
		if rules, err = opts.Policy.Normalize(); err != nil {
			writeError(w, wdErr(ErrInvalidArgument, "tether:options.policy: "+err.Error()))
			return
		}
	}
	timeouts := Timeouts{Script: ptr(int64(30000)), PageLoad: 300000, Implicit: 0}
	if raw, ok := caps["timeouts"]; ok {
		b, _ := json.Marshal(raw)
		if err := applyTimeouts(&timeouts, b); err != nil {
			writeError(w, err)
			return
		}
	}
	strategy, _ := caps["pageLoadStrategy"].(string)
	switch strategy {
	case "":
		strategy = "normal"
	case "normal", "eager", "none":
	default:
		writeError(w, wdErr(ErrInvalidArgument, "invalid pageLoadStrategy "+strategy))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var s *session.Session
	keep := false
	if opts.TabID != "" {
		s, err = h.core.Sessions.Get(ctx, opts.TabID)
		keep = true
	} else {
		s, err = h.core.Sessions.Create(ctx)
	}
	if err != nil {
		writeError(w, wdErr(ErrSessionNotCreated, err.Error()))
		return
	}
	if opts.Policy != nil || opts.TabID == "" {
		err = h.core.SetSessionRules(ctx, s, rules)
	} else {
		err = h.core.Prepare(ctx, s)
	}
	if err != nil {
		if opts.TabID == "" {
			_ = h.core.Sessions.Close(context.WithoutCancel(ctx), s.ID, false)
		}
		writeError(w, wdErr(ErrSessionNotCreated, err.Error()))
		return
	}
	if opts.KeepTab != nil {
		keep = *opts.KeepTab
	}
	ws := &wdSession{s: s, keepTab: keep, strategy: strategy, timeouts: timeouts}
	h.mu.Lock()
	if _, exists := h.sessions[s.ID]; exists {
		h.mu.Unlock()
		writeError(w, wdErr(ErrSessionNotCreated, "tab "+s.ID+" already has a WebDriver session"))
		return
	}
	h.sessions[s.ID] = ws
	h.mu.Unlock()

	product, _ := h.core.Browser.Version(ctx)
	h.log.Info("webdriver session created", "tab", logx.Clean(s.ID), "attached", opts.TabID != "")
	writeValue(w, http.StatusOK, map[string]any{
		"sessionId": s.ID,
		"capabilities": map[string]any{
			"browserName":               "chrome",
			"browserVersion":            browserVersion(product),
			"platformName":              platformName(),
			"acceptInsecureCerts":       false,
			"pageLoadStrategy":          strategy,
			"setWindowRect":             false,
			"strictFileInteractability": false,
			"unhandledPromptBehavior":   "dismiss and notify",
			"timeouts":                  timeouts,
			"tether:options":            map[string]any{"tabId": s.ID, "keepTab": keep, "policy": s.Rules()},
		},
	})
}

func (h *Handler) deleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sid")
	h.mu.Lock()
	ws, ok := h.sessions[id]
	if ok && !session.Visible(r.Context(), ws.s) {
		h.mu.Unlock()
		writeError(w, wdErr(ErrInvalidSessionID, "no active session with ID "+id))
		return
	}
	delete(h.sessions, id)
	h.mu.Unlock()
	if ok && !ws.keepTab {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if err := h.core.Sessions.Close(ctx, id, true); err != nil && !errors.Is(err, session.ErrTabGone) {
			h.log.Warn("closing tab for deleted session", "tab", id, "err", err)
		}
	}
	writeValue(w, http.StatusOK, nil)
}

func (h *Handler) getTimeouts(_ context.Context, ws *wdSession, _ *http.Request, _ []byte) (any, error) {
	return ws.getTimeouts(), nil
}

func (h *Handler) setTimeouts(_ context.Context, ws *wdSession, _ *http.Request, body []byte) (any, error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	t := ws.timeouts
	if err := applyTimeouts(&t, body); err != nil {
		return nil, err
	}
	ws.timeouts = t
	return nil, nil
}

func applyTimeouts(t *Timeouts, body []byte) error {
	var raw map[string]json.RawMessage
	if err := decodeBody(body, &raw); err != nil {
		return err
	}
	for k, v := range raw {
		var n *int64
		if err := json.Unmarshal(v, &n); err != nil || (n != nil && *n < 0) {
			return wdErr(ErrInvalidArgument, "timeout "+k+" must be a non-negative integer")
		}
		switch k {
		case "script":
			t.Script = n
		case "pageLoad":
			if n == nil {
				return wdErr(ErrInvalidArgument, "pageLoad timeout cannot be null")
			}
			t.PageLoad = *n
		case "implicit":
			if n == nil {
				return wdErr(ErrInvalidArgument, "implicit timeout cannot be null")
			}
			t.Implicit = *n
		default:
			return wdErr(ErrInvalidArgument, "unknown timeout "+k)
		}
	}
	return nil
}

func ptr[T any](v T) *T { return &v }
