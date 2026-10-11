package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/adeelahmad/tether/internal/cdp"
	"github.com/adeelahmad/tether/internal/core"
	"github.com/adeelahmad/tether/internal/session"
)

// tool is one MCP tool: its advertised definition and its handler.
type tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations *annotations   `json:"annotations,omitempty"`

	run func(ctx context.Context, a *agentSession, args json.RawMessage) (*toolResult, error)
}

// annotations are MCP tool hints that let clients decide what to confirm
// with the user.
type annotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint"`
	DestructiveHint bool   `json:"destructiveHint"`
	IdempotentHint  bool   `json:"idempotentHint"`
	OpenWorldHint   bool   `json:"openWorldHint"`
}

type toolResult struct {
	Content []content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

type content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

func textResult(format string, a ...any) *toolResult {
	return &toolResult{Content: []content{{Type: "text", Text: fmt.Sprintf(format, a...)}}}
}

// Tool error codes beyond the core's.
const (
	codeInvalidArgs = "invalid_arguments"
	codeNoElement   = "no_such_element"
	codeStaleRef    = "stale_element"
	codeNoTab       = "no_current_tab"
	codeNotActable  = "element_not_interactable"
)

// toolError is a failure the agent can act on.
type toolError struct {
	code string
	msg  string
}

func (e *toolError) Error() string { return e.msg }

func badArgs(format string, a ...any) error {
	return &toolError{code: codeInvalidArgs, msg: fmt.Sprintf(format, a...)}
}

// errorCode is the stable code reported for err.
func errorCode(err error) string {
	var te *toolError
	if errors.As(err, &te) {
		return te.code
	}
	_, code := core.Classify(err)
	return code
}

var hints = map[string]string{
	core.CodeTabGone:     " The tab was closed; use browser_tabs, browser_select_tab or browser_new_tab.",
	core.CodeForbidden:   " Tether's domain allowlist blocks this; ask the user to extend --allow-domains if it is intended.",
	core.CodeTimeout:     " The page did not respond in time; try browser_snapshot to see its current state.",
	core.CodeBrowserGone: " Tether lost its connection to Chrome.",
}

func errorResult(err error) *toolResult {
	code := errorCode(err)
	return &toolResult{IsError: true, Content: []content{{Type: "text", Text: "Error (" + code + "): " + err.Error() + hints[code]}}}
}

// decode parses tool arguments strictly so typos surface as errors.
func decode(args json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badArgs("invalid arguments: %v", err)
	}
	return nil
}

// JSON Schema helpers.
func object(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}
func integer(desc string, minimum, maximum int) map[string]any {
	return map[string]any{"type": "integer", "description": desc, "minimum": minimum, "maximum": maximum}
}
func enum(desc string, values ...string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": values}
}

var (
	refProp      = str("Element ref from browser_snapshot, e.g. e42. Preferred over selector.")
	selectorProp = str("CSS selector, used when no ref is given. The first match is used.")
	waitProp     = enum("When to consider the page loaded (default load).", "none", "domcontentloaded", "load", "networkidle")
	tabProp      = str("Tab ID from browser_tabs.")
)

func (s *Server) toolset() []*tool {
	return []*tool{
		{
			Name:  "browser_tabs",
			Title: "List tabs",
			Description: "List the open tabs in the user's Chrome, including ones the user opened. " +
				"Marks this session's current tab. Use browser_select_tab to work in one.",
			InputSchema: object(map[string]any{}),
			Annotations: &annotations{ReadOnlyHint: true, IdempotentHint: true},
			run:         s.tabs,
		},
		{
			Name:        "browser_new_tab",
			Title:       "Open a new tab",
			Description: "Open a new tab, make it this session's current tab, and optionally load a URL in it.",
			InputSchema: object(map[string]any{"url": str("URL to open (optional)."), "wait": waitProp}),
			Annotations: &annotations{OpenWorldHint: true},
			run:         s.newTab,
		},
		{
			Name:  "browser_select_tab",
			Title: "Switch tab",
			Description: "Make an existing tab this session's current tab. Works for tabs the user already has open " +
				"(they stay open when this session ends).",
			InputSchema: object(map[string]any{"tab_id": tabProp}, "tab_id"),
			Annotations: &annotations{IdempotentHint: true},
			run:         s.selectTab,
		},
		{
			Name:  "browser_close_tab",
			Title: "Close a tab",
			Description: "Close a tab (default: the current tab). Tabs Tether opened are closed; tabs the user opened " +
				"are only released, never closed.",
			InputSchema: object(map[string]any{"tab_id": str("Tab ID from browser_tabs (default: the current tab).")}),
			Annotations: &annotations{DestructiveHint: true, IdempotentHint: true},
			run:         s.closeTab,
		},
		{
			Name:  "browser_navigate",
			Title: "Navigate",
			Description: "Load a URL in the current tab (opening one if needed) with the user's real cookies and logins. " +
				"Then call browser_snapshot to read the page.",
			InputSchema: object(map[string]any{"url": str("Absolute URL to load."), "wait": waitProp}, "url"),
			Annotations: &annotations{OpenWorldHint: true},
			run:         s.navigate,
		},
		{
			Name:        "browser_history",
			Title:       "Back, forward or reload",
			Description: "Go back, go forward, or reload the current tab.",
			InputSchema: object(map[string]any{"action": enum("What to do.", "back", "forward", "reload")}, "action"),
			Annotations: &annotations{OpenWorldHint: true},
			run:         s.history,
		},
		{
			Name:  "browser_snapshot",
			Title: "Read the page",
			Description: "Read the current tab: title, URL, visible interactive elements with refs (e.g. [e42] button \"Sign in\") " +
				"and the page text. Pass refs to browser_click, browser_type and browser_select_option. " +
				"Refs go stale when the page changes; snapshot again after navigating. Main frame only.",
			InputSchema: object(map[string]any{
				"max_elements": integer("Most interactive elements to list (default 200).", 0, 2000),
				"max_text":     integer("Most characters of page text to include (default 8000; 0 omits text).", 0, 200000),
			}),
			Annotations: &annotations{ReadOnlyHint: true, IdempotentHint: true},
			run:         s.snapshot,
		},
		{
			Name:  "browser_click",
			Title: "Click",
			Description: "Click an element with a real mouse event, waiting for any navigation it starts. " +
				"Fails if another element covers it, unless force is set.",
			InputSchema: object(map[string]any{
				"ref": refProp, "selector": selectorProp,
				"force": boolean("Click via element.click() even if the element is covered or has no size."),
			}),
			Annotations: &annotations{DestructiveHint: true, OpenWorldHint: true},
			run:         s.click,
		},
		{
			Name:  "browser_type",
			Title: "Type text",
			Description: "Focus an input, textarea or editable element and type text with real key events. " +
				"Replaces the existing value unless clear is false. Set submit to press Enter afterwards.",
			InputSchema: object(map[string]any{
				"ref": refProp, "selector": selectorProp,
				"text":   str("Text to type. Newlines press Enter."),
				"clear":  boolean("Clear the current value first (default true)."),
				"submit": boolean("Press Enter after typing (default false)."),
			}, "text"),
			Annotations: &annotations{DestructiveHint: true, OpenWorldHint: true},
			run:         s.typeText,
		},
		{
			Name:  "browser_press_key",
			Title: "Press a key",
			Description: "Press a key or chord in the current tab, sent to the focused element: " +
				"e.g. Enter, Escape, Tab, ArrowDown, PageDown, Control+A, Shift+Tab, Meta+K.",
			InputSchema: object(map[string]any{"key": str("Key or chord, modifiers joined with +.")}, "key"),
			Annotations: &annotations{DestructiveHint: true, OpenWorldHint: true},
			run:         s.pressKey,
		},
		{
			Name:        "browser_select_option",
			Title:       "Choose in a dropdown",
			Description: "Choose option(s) in a <select> element by value or visible label.",
			InputSchema: object(map[string]any{
				"ref": refProp, "selector": selectorProp,
				"values": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1,
					"description": "Option values or labels to select."},
			}, "values"),
			Annotations: &annotations{DestructiveHint: true, IdempotentHint: true},
			run:         s.selectOption,
		},
		{
			Name:  "browser_scroll",
			Title: "Scroll",
			Description: "Scroll the page by a page or a number of pixels, or scroll an element into view. " +
				"Useful for pages that load more content as you scroll.",
			InputSchema: object(map[string]any{
				"direction": enum("Direction (default down).", "up", "down", "left", "right"),
				"pixels":    integer("Distance in CSS pixels (default: most of a screen).", 1, 100000),
				"ref":       str("Scroll this element (from browser_snapshot) into view instead."),
			}),
			Annotations: &annotations{IdempotentHint: false},
			run:         s.scroll,
		},
		{
			Name:  "browser_wait_for",
			Title: "Wait for content",
			Description: "Wait until text appears in the page and/or an element matching a CSS selector is visible " +
				"(or, with gone, until it disappears).",
			InputSchema: object(map[string]any{
				"text":       str("Text to wait for."),
				"selector":   str("CSS selector to wait for."),
				"gone":       boolean("Wait for the text/element to disappear instead."),
				"timeout_ms": integer("How long to wait (default 10000).", 1, 120000),
			}),
			Annotations: &annotations{ReadOnlyHint: true, IdempotentHint: true},
			run:         s.waitFor,
		},
		{
			Name:  "browser_eval",
			Title: "Run JavaScript",
			Description: "Evaluate a JavaScript expression in the current tab's page (Runtime.evaluate) and return its " +
				"JSON value. Promises are awaited. Example: document.querySelectorAll('tr').length",
			InputSchema: object(map[string]any{
				"expression":    str("JavaScript expression."),
				"await_promise": boolean("Await a returned promise (default true)."),
			}, "expression"),
			Annotations: &annotations{DestructiveHint: true, OpenWorldHint: true},
			run:         s.eval,
		},
		{
			Name:        "browser_screenshot",
			Title:       "Screenshot",
			Description: "Take a PNG screenshot of the viewport, the full page, or one element.",
			InputSchema: object(map[string]any{
				"ref":       str("Screenshot only this element (from browser_snapshot)."),
				"full_page": boolean("Capture the whole scrollable page instead of the viewport."),
			}),
			Annotations: &annotations{ReadOnlyHint: true, IdempotentHint: true},
			run:         s.screenshot,
		},
		{
			Name:  "browser_fetch",
			Title: "HTTP request as the user",
			Description: "Make an HTTP request from inside the current tab with fetch(), so it carries the user's real " +
				"cookies and session. Good for JSON APIs behind a login. If the tab is on another origin it is first " +
				"moved to the target origin. Response Set-Cookie headers are never returned.",
			InputSchema: object(map[string]any{
				"url":       str("Absolute URL."),
				"method":    str("HTTP method (default GET)."),
				"headers":   map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "Request headers."},
				"body":      str("Request body (text)."),
				"max_chars": integer("Most characters of response body to return (default 20000).", 0, 1000000),
			}, "url"),
			Annotations: &annotations{DestructiveHint: true, OpenWorldHint: true},
			run:         s.fetch,
		},
	}
}

// tab returns the session's current tab. With create, a new tab is opened
// when there is none.
func (s *Server) tab(ctx context.Context, a *agentSession, create bool) (*session.Session, error) {
	id := a.currentTab()
	if id == "" {
		if !create {
			return nil, &toolError{code: codeNoTab, msg: "no current tab; call browser_navigate, browser_new_tab or browser_select_tab first"}
		}
		t, err := s.core.Sessions.Create(ctx)
		if err != nil {
			return nil, err
		}
		a.setTab(t.ID, true)
		return t, nil
	}
	t, err := s.core.Sessions.Get(ctx, id)
	if errors.Is(err, session.ErrTabGone) {
		a.mu.Lock()
		if a.tab == id {
			a.tab = ""
		}
		a.mu.Unlock()
		return nil, fmt.Errorf("the current tab %s: %w", id, session.ErrTabGone)
	}
	return t, err
}

// lockPage resolves the current tab, locks it, and checks that the page it
// is showing is allowed by policy, so an agent cannot act on (or read) a
// page outside its allowlist even if it got there by clicking a link.
func (s *Server) lockPage(ctx context.Context, a *agentSession) (*session.Session, func(), error) {
	t, err := s.tab(ctx, a, false)
	if err != nil {
		return nil, nil, err
	}
	unlock, err := t.Lock(ctx)
	if err != nil {
		return nil, nil, err
	}
	if err := s.allowed(ctx, t); err != nil {
		unlock()
		return nil, nil, err
	}
	return t, unlock, nil
}

func (s *Server) allowed(ctx context.Context, t *session.Session) error {
	info, err := t.Tab.Info(ctx)
	if err != nil {
		return err
	}
	if info.URL == "" {
		return nil // nothing loaded yet
	}
	u, err := url.Parse(info.URL)
	if err != nil {
		return fmt.Errorf("%w: cannot parse the page URL", core.ErrForbidden) // fail closed
	}
	return s.core.Policy.Allow(ctx, t, u)
}

func (s *Server) tabs(ctx context.Context, a *agentSession, args json.RawMessage) (*toolResult, error) {
	if err := decode(args, &struct{}{}); err != nil {
		return nil, err
	}
	all, err := s.core.Browser.Tabs(ctx)
	if err != nil {
		return nil, err
	}
	tracked := map[string]session.Info{}
	for _, i := range s.core.Sessions.List() {
		tracked[i.ID] = i
	}
	cur := a.currentTab()
	a.mu.Lock()
	mine := map[string]bool{}
	for id := range a.opened {
		mine[id] = true
	}
	a.mu.Unlock()
	sort.SliceStable(all, func(i, j int) bool { return all[i].ID == cur && all[j].ID != cur })

	var b strings.Builder
	fmt.Fprintf(&b, "%d open tab(s):\n", len(all))
	for _, t := range all {
		marker := " "
		if t.ID == cur {
			marker = "*"
		}
		title := t.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(&b, "%s %s  %s\n    %s", marker, t.ID, title, t.URL)
		var tags []string
		switch {
		case mine[t.ID]:
			tags = append(tags, "opened by you")
		case tracked[t.ID].Owned:
			tags = append(tags, "opened by Tether")
		default:
			tags = append(tags, "user's tab")
		}
		if t.ID == cur {
			tags = append(tags, "current")
		}
		fmt.Fprintf(&b, "  [%s]\n", strings.Join(tags, ", "))
	}
	if cur == "" {
		b.WriteString("\nNo current tab yet: browser_navigate opens one, browser_select_tab picks one of these.\n")
	}
	return textResult("%s", b.String()), nil
}

func (s *Server) load(ctx context.Context, t *session.Session, rawURL, wait string) (*toolResult, error) {
	w, err := cdp.ParseWait(wait)
	if err != nil {
		return nil, badArgs("%v", err)
	}
	unlock, err := t.Lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	doc, err := s.core.Navigate(ctx, t, rawURL, w)
	if err != nil {
		return nil, err
	}
	title := ""
	if v, err := t.Tab.Evaluate(ctx, "document.title", false); err == nil {
		_ = json.Unmarshal(v, &title)
	}
	status := ""
	if doc.Status > 0 {
		status = fmt.Sprintf(" (HTTP %d)", doc.Status)
	}
	return textResult("Loaded %s%s in tab %s.\nTitle: %s\nCall browser_snapshot to read the page.", doc.URL, status, t.ID, title), nil
}

func (s *Server) newTab(ctx context.Context, a *agentSession, args json.RawMessage) (*toolResult, error) {
	var p struct {
		URL  string `json:"url"`
		Wait string `json:"wait"`
	}
	if err := decode(args, &p); err != nil {
		return nil, err
	}
	t, err := s.core.Sessions.Create(ctx)
	if err != nil {
		return nil, err
	}
	a.setTab(t.ID, true)
	if p.URL == "" {
		return textResult("Opened tab %s (blank). It is now the current tab.", t.ID), nil
	}
	return s.load(ctx, t, p.URL, p.Wait)
}

func (s *Server) selectTab(ctx context.Context, a *agentSession, args json.RawMessage) (*toolResult, error) {
	var p struct {
		TabID string `json:"tab_id"`
	}
	if err := decode(args, &p); err != nil {
		return nil, err
	}
	if p.TabID == "" {
		return nil, badArgs("tab_id is required; see browser_tabs")
	}
	t, err := s.core.Sessions.Get(ctx, p.TabID)
	if err != nil {
		return nil, err
	}
	if err := s.allowed(ctx, t); err != nil {
		return nil, err
	}
	a.setTab(t.ID, false)
	info, _ := t.Tab.Info(ctx)
	return textResult("Tab %s is now the current tab: %s %s", t.ID, info.Title, info.URL), nil
}

func (s *Server) closeTab(ctx context.Context, a *agentSession, args json.RawMessage) (*toolResult, error) {
	var p struct {
		TabID string `json:"tab_id"`
	}
	if err := decode(args, &p); err != nil {
		return nil, err
	}
	id := p.TabID
	if id == "" {
		if id = a.currentTab(); id == "" {
			return nil, &toolError{code: codeNoTab, msg: "no current tab to close"}
		}
	}
	t, err := s.core.Sessions.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	owned := t.Owned
	if err := s.core.Sessions.Close(ctx, id, false); err != nil {
		return nil, err
	}
	a.mu.Lock()
	if a.tab == id {
		a.tab = ""
	}
	delete(a.opened, id)
	a.mu.Unlock()
	if !owned {
		return textResult("Released tab %s. It is the user's own tab, so it was left open.", id), nil
	}
	return textResult("Closed tab %s.", id), nil
}

func (s *Server) navigate(ctx context.Context, a *agentSession, args json.RawMessage) (*toolResult, error) {
	var p struct {
		URL  string `json:"url"`
		Wait string `json:"wait"`
	}
	if err := decode(args, &p); err != nil {
		return nil, err
	}
	if p.URL == "" {
		return nil, badArgs("url is required")
	}
	t, err := s.tab(ctx, a, true)
	if err != nil {
		return nil, err
	}
	return s.load(ctx, t, p.URL, p.Wait)
}

func (s *Server) history(ctx context.Context, a *agentSession, args json.RawMessage) (*toolResult, error) {
	var p struct {
		Action string `json:"action"`
	}
	if err := decode(args, &p); err != nil {
		return nil, err
	}
	t, unlock, err := s.lockPage(ctx, a)
	if err != nil {
		return nil, err
	}
	defer unlock()
	switch p.Action {
	case "back":
		err = t.Tab.History(ctx, -1)
	case "forward":
		err = t.Tab.History(ctx, 1)
	case "reload":
		err = t.Tab.Reload(ctx, cdp.WaitLoad)
	default:
		return nil, badArgs("action must be back, forward or reload")
	}
	if err != nil {
		return nil, err
	}
	info, _ := t.Tab.Info(ctx)
	return textResult("Now at %s\nTitle: %s", info.URL, info.Title), nil
}

func (s *Server) snapshot(ctx context.Context, a *agentSession, args json.RawMessage) (*toolResult, error) {
	var p struct {
		MaxElements *int `json:"max_elements"`
		MaxText     *int `json:"max_text"`
	}
	if err := decode(args, &p); err != nil {
		return nil, err
	}
	maxEls, maxText := 200, 8000
	if p.MaxElements != nil {
		maxEls = clamp(*p.MaxElements, 0, 2000)
	}
	if p.MaxText != nil {
		maxText = clamp(*p.MaxText, 0, 200000)
	}
	t, unlock, err := s.lockPage(ctx, a)
	if err != nil {
		return nil, err
	}
	defer unlock()
	snap, err := takeSnapshot(ctx, t.Tab, maxEls, maxText)
	if err != nil {
		return nil, err
	}
	return textResult("%s", snap.format()), nil
}

func (s *Server) click(ctx context.Context, a *agentSession, args json.RawMessage) (*toolResult, error) {
	var p struct {
		Ref      string `json:"ref"`
		Selector string `json:"selector"`
		Force    bool   `json:"force"`
	}
	if err := decode(args, &p); err != nil {
		return nil, err
	}
	t, unlock, err := s.lockPage(ctx, a)
	if err != nil {
		return nil, err
	}
	defer unlock()
	oid, err := element(ctx, t.Tab, p.Ref, p.Selector)
	if err != nil {
		return nil, err
	}
	defer release(ctx, t.Tab, oid)

	if p.Force {
		if err := callOn(ctx, t.Tab, oid, jsClickJS, nil); err != nil {
			return nil, err
		}
	} else {
		var pt clickPoint
		if err := callOn(ctx, t.Tab, oid, clickPointJS, &pt); err != nil {
			return nil, err
		}
		switch {
		case pt.Handled:
		case !pt.Visible:
			return nil, &toolError{code: codeNotActable, msg: "element has no size or is not rendered; scroll to it, or retry with force"}
		case !pt.Hit:
			return nil, &toolError{code: codeNotActable, msg: "element is covered by <" + pt.Blocker + ">; close the overlay first, or retry with force"}
		default:
			if err := t.Tab.Click(ctx, pt.X, pt.Y); err != nil {
				return nil, err
			}
		}
	}
	settle(ctx, t.Tab, 10*time.Second)
	info, _ := t.Tab.Info(ctx)
	return textResult("Clicked. Now at %s\nTitle: %s", info.URL, info.Title), nil
}

const maxTypeChars = 20000

func (s *Server) typeText(ctx context.Context, a *agentSession, args json.RawMessage) (*toolResult, error) {
	var p struct {
		Ref      string  `json:"ref"`
		Selector string  `json:"selector"`
		Text     *string `json:"text"`
		Clear    *bool   `json:"clear"`
		Submit   bool    `json:"submit"`
	}
	if err := decode(args, &p); err != nil {
		return nil, err
	}
	if p.Text == nil {
		return nil, badArgs("text is required (use \"\" to just clear the field)")
	}
	text := *p.Text
	if utf8.RuneCountInString(text) > maxTypeChars {
		return nil, badArgs("text is longer than %d characters", maxTypeChars)
	}
	clearFirst := p.Clear == nil || *p.Clear
	t, unlock, err := s.lockPage(ctx, a)
	if err != nil {
		return nil, err
	}
	defer unlock()
	oid, err := element(ctx, t.Tab, p.Ref, p.Selector)
	if err != nil {
		return nil, err
	}
	defer release(ctx, t.Tab, oid)
	var state string
	if err := callOn(ctx, t.Tab, oid, focusJS, &state, clearFirst); err != nil {
		return nil, err
	}
	switch state {
	case "file":
		return nil, &toolError{code: codeNotActable, msg: "file inputs cannot be filled by typing"}
	case "readonly":
		return nil, &toolError{code: codeNotActable, msg: "element is disabled or read-only"}
	case "unfocusable":
		return nil, &toolError{code: codeNotActable, msg: "element cannot receive keyboard focus"}
	}
	if err := typeText(ctx, t.Tab, text); err != nil {
		return nil, err
	}
	msg := fmt.Sprintf("Typed %d character(s).", utf8.RuneCountInString(text))
	if p.Submit {
		if err := pressChord(ctx, t.Tab, "Enter"); err != nil {
			return nil, err
		}
		settle(ctx, t.Tab, 10*time.Second)
		info, _ := t.Tab.Info(ctx)
		msg += fmt.Sprintf(" Pressed Enter. Now at %s\nTitle: %s", info.URL, info.Title)
	}
	return textResult("%s", msg), nil
}

func (s *Server) pressKey(ctx context.Context, a *agentSession, args json.RawMessage) (*toolResult, error) {
	var p struct {
		Key string `json:"key"`
	}
	if err := decode(args, &p); err != nil {
		return nil, err
	}
	if _, _, _, err := parseChord(p.Key); err != nil {
		return nil, err
	}
	t, unlock, err := s.lockPage(ctx, a)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := pressChord(ctx, t.Tab, p.Key); err != nil {
		return nil, err
	}
	settle(ctx, t.Tab, 5*time.Second)
	return textResult("Pressed %s.", p.Key), nil
}

func (s *Server) selectOption(ctx context.Context, a *agentSession, args json.RawMessage) (*toolResult, error) {
	var p struct {
		Ref      string   `json:"ref"`
		Selector string   `json:"selector"`
		Values   []string `json:"values"`
	}
	if err := decode(args, &p); err != nil {
		return nil, err
	}
	if len(p.Values) == 0 {
		return nil, badArgs("values must name at least one option")
	}
	t, unlock, err := s.lockPage(ctx, a)
	if err != nil {
		return nil, err
	}
	defer unlock()
	oid, err := element(ctx, t.Tab, p.Ref, p.Selector)
	if err != nil {
		return nil, err
	}
	defer release(ctx, t.Tab, oid)
	var r struct {
		Error     string   `json:"error"`
		Missing   []string `json:"missing"`
		Available []string `json:"available"`
		Selected  []string `json:"selected"`
	}
	if err := callOn(ctx, t.Tab, oid, selectOptionJS, &r, p.Values); err != nil {
		return nil, err
	}
	switch {
	case r.Error != "":
		return nil, &toolError{code: codeNotActable, msg: r.Error}
	case len(r.Missing) > 0:
		return nil, &toolError{code: codeNoElement, msg: fmt.Sprintf("no option matches %q; available: %s",
			r.Missing, strings.Join(r.Available, " | "))}
	}
	return textResult("Selected %s.", strings.Join(r.Selected, ", ")), nil
}

func (s *Server) scroll(ctx context.Context, a *agentSession, args json.RawMessage) (*toolResult, error) {
	var p struct {
		Direction string `json:"direction"`
		Pixels    int    `json:"pixels"`
		Ref       string `json:"ref"`
	}
	if err := decode(args, &p); err != nil {
		return nil, err
	}
	t, unlock, err := s.lockPage(ctx, a)
	if err != nil {
		return nil, err
	}
	defer unlock()
	var pos struct {
		X, Y, Height, Viewport int
	}
	if p.Ref != "" {
		oid, err := element(ctx, t.Tab, p.Ref, "")
		if err != nil {
			return nil, err
		}
		defer release(ctx, t.Tab, oid)
		if err := callOn(ctx, t.Tab, oid, scrollIntoViewJS, &pos); err != nil {
			return nil, err
		}
	} else {
		var dist any = "page"
		if p.Pixels > 0 {
			dist = p.Pixels
		}
		neg := func(v any) any {
			if n, ok := v.(int); ok {
				return -n
			}
			return "-page"
		}
		var dx, dy any = 0, dist
		switch p.Direction {
		case "", "down":
		case "up":
			dy = neg(dist)
		case "right":
			dx, dy = dist, 0
		case "left":
			dx, dy = neg(dist), 0
		default:
			return nil, badArgs("direction must be up, down, left or right")
		}
		if err := eval(ctx, t.Tab, scrollJS, &pos, dx, dy, nil); err != nil {
			return nil, err
		}
	}
	settle(ctx, t.Tab, 2*time.Second)
	return textResult("Scrolled to x=%d y=%d (page height %dpx, viewport %dpx).", pos.X, pos.Y, pos.Height, pos.Viewport), nil
}

func (s *Server) waitFor(ctx context.Context, a *agentSession, args json.RawMessage) (*toolResult, error) {
	var p struct {
		Text      string `json:"text"`
		Selector  string `json:"selector"`
		Gone      bool   `json:"gone"`
		TimeoutMS int    `json:"timeout_ms"`
	}
	if err := decode(args, &p); err != nil {
		return nil, err
	}
	if p.Text == "" && p.Selector == "" {
		return nil, badArgs("pass text, selector, or both")
	}
	timeout := 10 * time.Second
	if p.TimeoutMS > 0 {
		timeout = time.Duration(clamp(p.TimeoutMS, 1, 120000)) * time.Millisecond
	}
	t, err := s.tab(ctx, a, false)
	if err != nil {
		return nil, err
	}
	what := strings.TrimSpace(fmt.Sprintf("%s %s", quoteIf(p.Text), p.Selector))
	deadline := time.Now().Add(timeout)
	for {
		// Lock per poll so other tools are not starved while waiting.
		unlock, err := t.Lock(ctx)
		if err != nil {
			return nil, err
		}
		var ok bool
		err = s.allowed(ctx, t)
		if err == nil {
			err = eval(ctx, t.Tab, waitJS, &ok, p.Text, p.Selector, p.Gone)
		}
		unlock()
		if err != nil {
			var js *cdp.JSError
			if errors.As(err, &js) {
				return nil, badArgs("invalid CSS selector %q", p.Selector)
			}
			return nil, err
		}
		if ok {
			if p.Gone {
				return textResult("%s is gone.", what), nil
			}
			return textResult("Found %s.", what), nil
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("waiting %s for %s: %w", timeout, what, context.DeadlineExceeded)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func quoteIf(s string) string {
	if s == "" {
		return ""
	}
	return fmt.Sprintf("%q", s)
}

const maxEvalChars = 100000

func (s *Server) eval(ctx context.Context, a *agentSession, args json.RawMessage) (*toolResult, error) {
	var p struct {
		Expression   string `json:"expression"`
		AwaitPromise *bool  `json:"await_promise"`
	}
	if err := decode(args, &p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Expression) == "" {
		return nil, badArgs("expression is required")
	}
	t, unlock, err := s.lockPage(ctx, a)
	if err != nil {
		return nil, err
	}
	unlock() // core.Eval takes the tab lock itself
	v, err := s.core.Eval(ctx, t, p.Expression, p.AwaitPromise == nil || *p.AwaitPromise)
	if err != nil {
		return nil, err
	}
	out := string(v)
	if out == "" {
		out = "undefined"
	}
	return textResult("%s", truncate(out, maxEvalChars)), nil
}

const maxShotHeight = 16384

func (s *Server) screenshot(ctx context.Context, a *agentSession, args json.RawMessage) (*toolResult, error) {
	var p struct {
		Ref      string `json:"ref"`
		FullPage bool   `json:"full_page"`
	}
	if err := decode(args, &p); err != nil {
		return nil, err
	}
	t, unlock, err := s.lockPage(ctx, a)
	if err != nil {
		return nil, err
	}
	defer unlock()
	var clip *cdp.Rect
	switch {
	case p.Ref != "":
		oid, err := element(ctx, t.Tab, p.Ref, "")
		if err != nil {
			return nil, err
		}
		defer release(ctx, t.Tab, oid)
		clip = &cdp.Rect{}
		if err := callOn(ctx, t.Tab, oid, elementRectJS, clip); err != nil {
			return nil, err
		}
		if clip.Width == 0 || clip.Height == 0 {
			return nil, &toolError{code: codeNotActable, msg: "element has no size"}
		}
	case p.FullPage:
		clip = &cdp.Rect{}
		if err := eval(ctx, t.Tab, pageSizeJS, clip); err != nil {
			return nil, err
		}
	}
	if clip != nil && clip.Height > maxShotHeight {
		clip.Height = maxShotHeight
	}
	png, err := t.Tab.Screenshot(ctx, clip)
	if err != nil {
		return nil, err
	}
	info, _ := t.Tab.Info(ctx)
	return &toolResult{Content: []content{
		{Type: "image", Data: base64.StdEncoding.EncodeToString(png), MimeType: "image/png"},
		{Type: "text", Text: "Screenshot of " + info.URL},
	}}, nil
}

func (s *Server) fetch(ctx context.Context, a *agentSession, args json.RawMessage) (*toolResult, error) {
	var p struct {
		URL      string            `json:"url"`
		Method   string            `json:"method"`
		Headers  map[string]string `json:"headers"`
		Body     string            `json:"body"`
		MaxChars *int              `json:"max_chars"`
	}
	if err := decode(args, &p); err != nil {
		return nil, err
	}
	u, err := url.Parse(p.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, badArgs("url must be an absolute http(s) URL")
	}
	method := strings.ToUpper(strings.TrimSpace(p.Method))
	if method == "" {
		method = http.MethodGet
	}
	if method == http.MethodConnect || method == "TRACE" || strings.ContainsAny(method, " \t\r\n") {
		return nil, badArgs("method %q is not allowed", method)
	}
	limit := 20000
	if p.MaxChars != nil {
		limit = clamp(*p.MaxChars, 0, 1000000)
	}
	h := http.Header{}
	for k, v := range p.Headers {
		h.Set(k, v)
	}
	t, err := s.tab(ctx, a, true)
	if err != nil {
		return nil, err
	}
	resp, err := s.core.Do(ctx, t, &core.Request{Method: method, URL: u, Header: h, Body: []byte(p.Body), Mode: core.ModeFetch})
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP %d", resp.Status)
	if resp.FinalURL != "" && resp.FinalURL != u.String() {
		fmt.Fprintf(&b, " (after redirects: %s)", resp.FinalURL)
	}
	b.WriteByte('\n')
	keys := make([]string, 0, len(resp.Header))
	for k := range resp.Header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %s\n", k, strings.Join(resp.Header[k], ", "))
	}
	b.WriteByte('\n')
	switch {
	case len(resp.Body) == 0:
		b.WriteString("(empty body)")
	case !isText(resp.Header.Get("Content-Type"), resp.Body):
		fmt.Fprintf(&b, "(binary body, %d bytes)", len(resp.Body))
	default:
		b.WriteString(truncate(string(resp.Body), limit))
	}
	return textResult("%s", b.String()), nil
}

func isText(contentType string, body []byte) bool {
	ct := strings.ToLower(contentType)
	for _, t := range []string{"text/", "json", "xml", "javascript", "x-www-form-urlencoded", "csv", "yaml"} {
		if strings.Contains(ct, t) {
			return true
		}
	}
	if ct != "" && !strings.HasPrefix(ct, "application/octet-stream") {
		return false
	}
	sample := body
	if len(sample) > 1024 {
		sample = sample[:1024]
	}
	return utf8.Valid(sample) && !bytes.ContainsRune(sample, 0)
}

func truncate(s string, n int) string {
	if n <= 0 {
		return fmt.Sprintf("(%d characters omitted)", utf8.RuneCountInString(s))
	}
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n… [truncated: showing %d of %d bytes]", cut, len(s))
}

func clamp(v, lo, hi int) int {
	return max(lo, min(v, hi))
}
