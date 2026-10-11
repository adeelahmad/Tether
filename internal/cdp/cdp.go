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
	// NewWindow is NewTab in a new browser window of its own.
	NewWindow(ctx context.Context) (Tab, error)
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
	// Window describes the browser window that holds a tab.
	Window(ctx context.Context, tabID string) (Window, error)
	// SetWindow changes the state or bounds of the window that holds a tab
	// and returns the result.
	SetWindow(ctx context.Context, tabID string, b WindowBounds) (Window, error)
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

	// InFrame returns a view of the tab whose script and DOM calls run in
	// a child frame. path lists the backend node IDs of the <iframe> or
	// <frame> elements from the top document down. Each step is resolved
	// on every call, so the view follows navigations inside the frame and
	// works for out-of-process (cross-site) frames. Page-level calls
	// (navigation, input, screenshots, cookies, dialogs, PDF) still go to
	// the tab. A step that is not, or is no longer, a frame yields
	// ErrNoSuchFrame.
	InFrame(path []int) Tab
	// ShadowRoot returns the backend node ID of an element's open or
	// closed shadow root, or 0 if it has none.
	ShadowRoot(ctx context.Context, backendNodeID int) (int, error)
	// SetFiles sets the selected files of an <input type=file>.
	SetFiles(ctx context.Context, backendNodeID int, files []string) error
	// PrintPDF renders the page as a PDF.
	PrintPDF(ctx context.Context, opts PDFOptions) ([]byte, error)

	// Dialog is the JavaScript dialog (alert, confirm, prompt,
	// beforeunload) open in the tab, or nil. While one is open, script
	// calls fail fast with ErrDialogOpen instead of hanging, and calls
	// that open one (a click, a navigation) return ErrDialogOpen.
	Dialog() *Dialog
	// HandleDialog accepts or dismisses the open dialog, answering a
	// prompt with promptText when it is non-nil.
	HandleDialog(ctx context.Context, accept bool, promptText *string) error
}

// TabInfo describes a page target.
type TabInfo struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Title string `json:"title"`
	// OpenerID is the tab that opened this one (window.open, a
	// target=_blank link), if any.
	OpenerID string `json:"openerId,omitempty"`
}

// Window is a browser window's geometry, in screen pixels.
type Window struct {
	ID     int    `json:"windowId"`
	Left   int    `json:"left"`
	Top    int    `json:"top"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	State  string `json:"windowState"` // normal, minimized, maximized, fullscreen
}

// WindowBounds is a change to a window. A State other than "" or
// "normal" ignores the bounds; nil bounds are left as they are.
type WindowBounds struct {
	Left, Top, Width, Height *int
	State                    string
}

// Dialog is an open JavaScript dialog.
type Dialog struct {
	Type          string `json:"type"` // alert, confirm, prompt, beforeunload
	Message       string `json:"message"`
	DefaultPrompt string `json:"defaultPrompt"`
	URL           string `json:"url"`
}

// PDFOptions configure PrintPDF. Lengths are in inches; zero values take
// Chrome's defaults.
type PDFOptions struct {
	Landscape       bool    `json:"landscape,omitempty"`
	PrintBackground bool    `json:"printBackground,omitempty"`
	Scale           float64 `json:"scale,omitempty"`
	PaperWidth      float64 `json:"paperWidth,omitempty"`
	PaperHeight     float64 `json:"paperHeight,omitempty"`
	MarginTop       float64 `json:"marginTop"`
	MarginBottom    float64 `json:"marginBottom"`
	MarginLeft      float64 `json:"marginLeft"`
	MarginRight     float64 `json:"marginRight"`
	PageRanges      string  `json:"pageRanges,omitempty"`
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

// ErrNoSuchFrame means a frame path no longer leads to a frame.
var ErrNoSuchFrame = errors.New("cdp: no such frame")

// ErrDialogOpen means a JavaScript dialog is open (or opened during the
// call) and is blocking the page.
var ErrDialogOpen = errors.New("cdp: a JavaScript dialog is open")

// networkIdleQuiet is how long the network must be quiet for WaitNetworkIdle.
const networkIdleQuiet = 500 * time.Millisecond
