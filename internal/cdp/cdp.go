// Package cdp is Tether's thin, purpose-built layer over the Chrome DevTools
// Protocol. It connects to an existing Chrome (or launches one), opens and
// attaches to page targets, and exposes the handful of primitives the
// faces need: navigate, evaluate, call functions on remote objects, read
// response bodies, input, cookies and screenshots.
//
// The rest of Tether depends only on the Browser and Tab interfaces, so it
// can be tested with fakes and so the CDP transport can change without
// touching the faces.
package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// Browser is a connected Chrome instance.
type Browser interface {
	// NewTab opens a fresh tab (about:blank) and attaches to it.
	NewTab(ctx context.Context) (Tab, error)
	// AttachTab attaches to an existing page target. It returns
	// ErrNoSuchTab if the target does not exist or is not a page.
	AttachTab(ctx context.Context, id string) (Tab, error)
	// CloseTab closes the page target.
	CloseTab(ctx context.Context, id string) error
	// Tabs lists the open page targets.
	Tabs(ctx context.Context) ([]TabInfo, error)
	// OnTabClosed registers fn to be called (from the event goroutine) when
	// a page target is destroyed, for any reason.
	OnTabClosed(fn func(id string))
	// Version is the browser product string, e.g. "Chrome/141.0…".
	Version(ctx context.Context) (string, error)
	// Done is closed when the DevTools connection drops.
	Done() <-chan struct{}
	// Close disconnects. It never closes the user's browser.
	Close() error
}

// Tab is one attached page target.
type Tab interface {
	ID() string
	Info(ctx context.Context) (TabInfo, error)

	// Navigate loads url in the tab and waits according to wait. The
	// returned Document describes the main-frame response.
	Navigate(ctx context.Context, url string, wait Wait) (*Document, error)
	Reload(ctx context.Context, wait Wait) error
	History(ctx context.Context, delta int) error

	// Evaluate runs a JavaScript expression in the page's main world and
	// returns its JSON-serialised value.
	Evaluate(ctx context.Context, expr string, awaitPromise bool) (json.RawMessage, error)
	// CallFunction calls a function declaration with the given arguments.
	CallFunction(ctx context.Context, decl string, opts CallOptions) (*Remote, error)
	// Properties lists the own properties of a remote object.
	Properties(ctx context.Context, objectID string) ([]Property, error)
	// Release frees a remote object handle.
	Release(ctx context.Context, objectID string) error
	// NodeID returns the stable backend node ID of a DOM node handle.
	NodeID(ctx context.Context, objectID string) (int, error)
	// ResolveNode returns a fresh handle for a backend node ID. It fails
	// once the node's document is gone.
	ResolveNode(ctx context.Context, backendNodeID int) (string, error)

	// ResponseBody returns the decoded body of a network response the tab
	// has seen.
	ResponseBody(ctx context.Context, requestID string) ([]byte, error)

	Screenshot(ctx context.Context, clip *Rect) ([]byte, error)

	Cookies(ctx context.Context, urls []string) ([]Cookie, error)
	SetCookie(ctx context.Context, c Cookie) error
	DeleteCookie(ctx context.Context, name, url string) error

	Click(ctx context.Context, x, y float64) error
	Key(ctx context.Context, k KeyEvent) error

	// Guard enforces check on every top-level document request the tab
	// makes — navigations, redirects, link clicks, script-driven location
	// changes — by pausing it in the browser (Fetch domain) and failing it
	// with net::ERR_BLOCKED_BY_CLIENT when check returns an error. Calling
	// Guard again replaces check; a nil check removes the guard. check runs
	// on the event loop and must not block.
	Guard(ctx context.Context, check func(url string) error) error

	// Logs returns the buffered console messages, uncaught exceptions and
	// browser log entries with Seq > after, oldest first. Capture starts
	// when Tether attaches; the buffer keeps the most recent entries only.
	Logs(after uint64) []LogEntry
}

// TabInfo describes a page target.
type TabInfo struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Title string `json:"title"`
}

// Wait selects how long Navigate blocks.
type Wait string

const (
	WaitNone             Wait = "none"
	WaitDOMContentLoaded Wait = "domcontentloaded"
	WaitLoad             Wait = "load"
	WaitNetworkIdle      Wait = "networkidle"
)

// ParseWait validates a wait mode; "" means WaitLoad.
func ParseWait(s string) (Wait, error) {
	switch w := Wait(s); w {
	case "":
		return WaitLoad, nil
	case WaitNone, WaitDOMContentLoaded, WaitLoad, WaitNetworkIdle:
		return w, nil
	}
	return "", errors.New("wait must be one of none, domcontentloaded, load, networkidle")
}

// Document is the main-frame response of a navigation.
type Document struct {
	URL        string
	Status     int
	StatusText string
	Header     http.Header
	MimeType   string
	RequestID  string
}

// CallOptions configure CallFunction.
type CallOptions struct {
	// This binds `this` to a remote object; empty means globalThis.
	This string
	Args []Arg
	// Await resolves a returned promise before returning.
	Await bool
	// ByValue returns the JSON value instead of an object handle.
	ByValue bool
}

// Arg is a CallFunction argument: either a JSON value or a remote object.
type Arg struct {
	Value    any
	ObjectID string
}

// Remote is a value or handle returned from the page.
type Remote struct {
	ObjectID string
	Type     string
	Subtype  string
	Value    json.RawMessage
}

// Property is one own property of a remote object.
type Property struct {
	Name  string
	Value *Remote
}

// Rect is a page-coordinate rectangle in CSS pixels.
type Rect struct {
	X, Y, Width, Height float64
}

// Cookie is a browser cookie.
type Cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain,omitempty"`
	Path     string  `json:"path,omitempty"`
	URL      string  `json:"url,omitempty"`
	Secure   bool    `json:"secure,omitempty"`
	HTTPOnly bool    `json:"httpOnly,omitempty"`
	SameSite string  `json:"sameSite,omitempty"`
	Expires  float64 `json:"expires,omitempty"`
}

// KeyEvent is a single Input.dispatchKeyEvent.
type KeyEvent struct {
	Type      string `json:"type"` // keyDown, keyUp, char, rawKeyDown
	Key       string `json:"key,omitempty"`
	Code      string `json:"code,omitempty"`
	Text      string `json:"text,omitempty"`
	KeyCode   int    `json:"windowsVirtualKeyCode,omitempty"`
	Modifiers int    `json:"modifiers,omitempty"`
}

// JSError is an exception thrown by page JavaScript.
type JSError struct {
	Text string
}

func (e *JSError) Error() string { return "javascript error: " + e.Text }

// NavigationError is a navigation Chrome could not complete (DNS failure,
// connection refused, blocked, …).
type NavigationError struct {
	URL  string
	Text string
}

func (e *NavigationError) Error() string { return "navigation to " + e.URL + " failed: " + e.Text }

// ErrNoSuchTab means the requested target does not exist.
var ErrNoSuchTab = errors.New("cdp: no such tab")

// networkIdleQuiet is how long the network must be quiet for WaitNetworkIdle.
const networkIdleQuiet = 500 * time.Millisecond
