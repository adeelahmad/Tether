// Package cdptest provides in-memory fakes of cdp.Browser and cdp.Tab for
// unit tests of the layers above the DevTools transport.
package cdptest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/adeelahmad/tether/internal/cdp"
)

// Page is a canned navigation result.
type Page struct {
	Status   int
	MimeType string
	Header   http.Header
	HTML     string // returned for rendered HTML
	Body     []byte // returned by ResponseBody
	Err      error
	// RedirectTo makes the navigation end on another URL, as an HTTP
	// redirect would. The browser-side guard sees it too.
	RedirectTo string
}

// Browser is a fake cdp.Browser.
type Browser struct {
	mu     sync.Mutex
	next   int
	tabs   map[string]*Tab
	hooks  []func(string)
	done   chan struct{}
	Pages  map[string]Page // by URL
	NewErr error
	// Windows maps tab IDs to window IDs; unlisted tabs share window 1.
	Windows map[string]int
	// Bounds are the windows' current geometry, by window ID.
	Bounds map[int]cdp.Window
}

var _ cdp.Browser = (*Browser)(nil)

// NewBrowser returns an empty fake browser.
func NewBrowser() *Browser {
	return &Browser{
		tabs: map[string]*Tab{}, done: make(chan struct{}), Pages: map[string]Page{},
		Windows: map[string]int{}, Bounds: map[int]cdp.Window{},
	}
}

// AddTab registers a pre-existing tab, as if the user had opened it.
func (b *Browser) AddTab(id, url string) *Tab {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := &Tab{b: b, id: id, url: url}
	b.tabs[id] = t
	return t
}

// AddPopup registers a tab opened by the page in tab opener.
func (b *Browser) AddPopup(id, url, opener string) *Tab {
	t := b.AddTab(id, url)
	t.opener = opener
	return t
}

// Open returns the IDs of open tabs.
func (b *Browser) Open() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var ids []string
	for id := range b.tabs {
		ids = append(ids, id)
	}
	return ids
}

// Destroy simulates the user closing a tab.
func (b *Browser) Destroy(id string) {
	b.mu.Lock()
	delete(b.tabs, id)
	hooks := append([]func(string){}, b.hooks...)
	b.mu.Unlock()
	for _, h := range hooks {
		h(id)
	}
}

func (b *Browser) NewTab(context.Context) (cdp.Tab, error) {
	if b.NewErr != nil {
		return nil, b.NewErr
	}
	b.mu.Lock()
	b.next++
	t := &Tab{b: b, id: fmt.Sprintf("TAB%d", b.next), url: "about:blank"}
	b.tabs[t.id] = t
	b.mu.Unlock()
	return t, nil
}

// NewWindow opens a tab in a window of its own.
func (b *Browser) NewWindow(ctx context.Context) (cdp.Tab, error) {
	t, err := b.NewTab(ctx)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.Windows[t.ID()] = 100 + b.next
	b.mu.Unlock()
	return t, nil
}

func (b *Browser) windowOf(id string) int {
	if w, ok := b.Windows[id]; ok {
		return w
	}
	return 1
}

func (b *Browser) Window(_ context.Context, id string) (cdp.Window, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.tabs[id]; !ok {
		return cdp.Window{}, cdp.ErrNoSuchTab
	}
	wid := b.windowOf(id)
	w, ok := b.Bounds[wid]
	if !ok {
		w = cdp.Window{Left: 10, Top: 20, Width: 1280, Height: 800, State: "normal"}
	}
	w.ID = wid
	return w, nil
}

func (b *Browser) SetWindow(ctx context.Context, id string, wb cdp.WindowBounds) (cdp.Window, error) {
	w, err := b.Window(ctx, id)
	if err != nil {
		return w, err
	}
	if wb.State != "" && wb.State != "normal" {
		w.State = wb.State
	} else {
		w.State = "normal"
		for _, f := range []struct {
			dst *int
			src *int
		}{{&w.Left, wb.Left}, {&w.Top, wb.Top}, {&w.Width, wb.Width}, {&w.Height, wb.Height}} {
			if f.src != nil {
				*f.dst = *f.src
			}
		}
	}
	b.mu.Lock()
	b.Bounds[w.ID] = w
	b.mu.Unlock()
	return w, nil
}

func (b *Browser) AttachTab(_ context.Context, id string) (cdp.Tab, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.tabs[id]
	if !ok {
		return nil, cdp.ErrNoSuchTab
	}
	return t, nil
}

func (b *Browser) CloseTab(_ context.Context, id string) error {
	b.mu.Lock()
	_, ok := b.tabs[id]
	b.mu.Unlock()
	if !ok {
		return cdp.ErrNoSuchTab
	}
	b.Destroy(id)
	return nil
}

func (b *Browser) Tabs(context.Context) ([]cdp.TabInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []cdp.TabInfo
	for _, t := range b.tabs {
		out = append(out, cdp.TabInfo{ID: t.id, URL: t.url, OpenerID: t.opener})
	}
	return out, nil
}

func (b *Browser) OnTabClosed(fn func(string)) {
	b.mu.Lock()
	b.hooks = append(b.hooks, fn)
	b.mu.Unlock()
}

func (b *Browser) Version(context.Context) (string, error) { return "FakeChrome/1.0", nil }
func (b *Browser) Done() <-chan struct{}                   { return b.done }
func (b *Browser) Close() error                            { return nil }

// Call records one CallFunction invocation.
type Call struct {
	Decl string
	Args []cdp.Arg
}

// Tab is a fake cdp.Tab. Navigate serves Browser.Pages; CallFunction
// answers with CallResult.
type Tab struct {
	b      *Browser
	id     string
	mu     sync.Mutex
	url    string
	opener string
	dialog *cdp.Dialog

	Navigations []string
	Calls       []Call
	logs        []cdp.LogEntry
	logSeq      uint64
	CallResult  json.RawMessage
	EvalResult  json.RawMessage
	// ScriptResult, when set, is returned as the "v" property of the
	// object CallFunction yields, the shape the WebDriver script wrapper
	// produces (a JSON-encoded value).
	ScriptResult any
	// Input records every Key, Mouse and Touch event, in order, as
	// cdp.KeyEvent, cdp.MouseEvent or cdp.TouchEvent values.
	Input []any
	// Frame is the frame path of the last InFrame view that made a call.
	Frame []int
	// Shadow is what ShadowRoot returns.
	Shadow int
	// Files records SetFiles calls by backend node ID.
	Files map[int][]string
	// PDFs records PrintPDF calls.
	PDFs []cdp.PDFOptions
	// Answers records HandleDialog calls.
	Answers []DialogAnswer

	guard func(string) error
}

// DialogAnswer is one HandleDialog call.
type DialogAnswer struct {
	Accept bool
	Text   *string
}

var _ cdp.Tab = (*Tab)(nil)

func (t *Tab) ID() string { return t.id }

func (t *Tab) Info(context.Context) (cdp.TabInfo, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return cdp.TabInfo{ID: t.id, URL: t.url}, nil
}

func (t *Tab) page() Page {
	t.b.mu.Lock()
	defer t.b.mu.Unlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.b.Pages[t.url]
	if !ok {
		return Page{Status: 200, MimeType: "text/html", HTML: "<html></html>"}
	}
	return p
}

func (t *Tab) Navigate(_ context.Context, url string, _ cdp.Wait) (*cdp.Document, error) {
	t.mu.Lock()
	if t.guard != nil && t.guard(url) != nil {
		t.mu.Unlock()
		return nil, &cdp.NavigationError{URL: url, Text: cdp.BlockedByClient}
	}
	t.url = url
	t.Navigations = append(t.Navigations, url)
	t.mu.Unlock()
	p := t.page()
	if p.Err != nil {
		return nil, p.Err
	}
	if p.RedirectTo != "" {
		t.mu.Lock()
		if t.guard != nil && t.guard(p.RedirectTo) != nil {
			t.mu.Unlock()
			return nil, &cdp.NavigationError{URL: p.RedirectTo, Text: cdp.BlockedByClient}
		}
		t.url = p.RedirectTo
		t.mu.Unlock()
		url = p.RedirectTo
	}
	h := p.Header
	if h == nil {
		h = http.Header{"Content-Type": {p.MimeType}}
	}
	return &cdp.Document{URL: url, Status: p.Status, Header: h.Clone(), MimeType: p.MimeType, RequestID: "req-" + url}, nil
}

func (t *Tab) Reload(context.Context, cdp.Wait) error { return nil }
func (t *Tab) History(context.Context, int) error     { return nil }

func (t *Tab) Evaluate(_ context.Context, expr string, _ bool) (json.RawMessage, error) {
	if t.EvalResult != nil {
		return t.EvalResult, nil
	}
	if expr == "location.href" {
		return json.Marshal(t.url)
	}
	return json.Marshal(t.page().HTML)
}

func (t *Tab) CallFunction(_ context.Context, decl string, opts cdp.CallOptions) (*cdp.Remote, error) {
	t.mu.Lock()
	t.Calls = append(t.Calls, Call{Decl: decl, Args: opts.Args})
	res := t.CallResult
	script := t.ScriptResult
	t.mu.Unlock()
	if script != nil {
		return &cdp.Remote{ObjectID: "script-result", Type: "object"}, nil
	}
	if res == nil {
		res = json.RawMessage("null")
	}
	return &cdp.Remote{ObjectID: "result", Type: "object", Value: res}, nil
}

// Properties answers the WebDriver script wrapper: for ScriptResult it
// yields {"v": <json>}; otherwise it lists the fields of CallResult when it
// is a JSON object, so a CallResult of {"v": "<json>", "e": null} answers
// with the value <json>.
func (t *Tab) Properties(_ context.Context, objectID string) ([]cdp.Property, error) {
	t.mu.Lock()
	res, script := t.CallResult, t.ScriptResult
	t.mu.Unlock()
	switch {
	case objectID == "script-result" && script != nil:
		inner, err := json.Marshal(script)
		if err != nil {
			return nil, err
		}
		v, _ := json.Marshal(string(inner))
		return []cdp.Property{{Name: "v", Value: &cdp.Remote{Type: "string", Value: v}}}, nil
	case objectID != "result":
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(res, &fields); err != nil {
		return nil, nil //nolint:nilerr // a non-object result has no properties
	}
	var out []cdp.Property
	for k, v := range fields {
		if string(v) == "null" {
			continue
		}
		out = append(out, cdp.Property{Name: k, Value: &cdp.Remote{Value: v}})
	}
	return out, nil
}
func (t *Tab) Release(context.Context, string) error            { return nil }
func (t *Tab) NodeID(context.Context, string) (int, error)      { return 1, nil }
func (t *Tab) ResolveNode(context.Context, int) (string, error) { return "obj", nil }

func (t *Tab) ResponseBody(context.Context, string) ([]byte, error) {
	return t.page().Body, nil
}

func (t *Tab) Screenshot(context.Context, *cdp.Rect) ([]byte, error) {
	return []byte("\x89PNG fake"), nil
}

func (t *Tab) Cookies(context.Context, []string) ([]cdp.Cookie, error) { return nil, nil }
func (t *Tab) SetCookie(context.Context, cdp.Cookie) error             { return nil }
func (t *Tab) DeleteCookie(context.Context, string, string) error      { return nil }
func (t *Tab) Click(context.Context, float64, float64) error           { return nil }

func (t *Tab) Key(_ context.Context, k cdp.KeyEvent) error     { return t.record(k) }
func (t *Tab) Mouse(_ context.Context, m cdp.MouseEvent) error { return t.record(m) }
func (t *Tab) Touch(_ context.Context, e cdp.TouchEvent) error { return t.record(e) }

func (t *Tab) record(ev any) error {
	t.mu.Lock()
	t.Input = append(t.Input, ev)
	t.mu.Unlock()
	return nil
}

// Guard records check; Navigate refuses URLs it rejects, like the real
// browser-side guard.
func (t *Tab) Guard(_ context.Context, check func(string) error) error {
	t.mu.Lock()
	t.guard = check
	t.mu.Unlock()
	return nil
}

// Events returns a copy of the recorded input events.
func (t *Tab) Events() []any {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]any(nil), t.Input...)
}

// Guarded reports whether a navigation guard is installed.
func (t *Tab) Guarded() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.guard != nil
}

// SetURL moves the tab without a navigation, as page script or a redirect
// would.
func (t *Tab) SetURL(url string) {
	t.mu.Lock()
	t.url = url
	t.mu.Unlock()
}

// Log appends a log entry, as if the page had printed it, and returns its
// sequence number.
func (t *Tab) Log(level, text string) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.logSeq++
	e := cdp.LogEntry{Seq: t.logSeq, Time: time.Now(), Source: "console", Level: level, Text: text}
	t.logs = append(t.logs, e)
	return e.Seq
}

func (t *Tab) Logs(after uint64) []cdp.LogEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []cdp.LogEntry
	for _, e := range t.logs {
		if e.Seq > after {
			out = append(out, e)
		}
	}
	return out
}

func (t *Tab) InFrame(path []int) cdp.Tab {
	if len(path) == 0 {
		return t
	}
	return &FrameTab{Tab: t, Path: append([]int(nil), path...)}
}

func (t *Tab) ShadowRoot(context.Context, int) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.Shadow, nil
}

func (t *Tab) SetFiles(_ context.Context, id int, files []string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.Files == nil {
		t.Files = map[int][]string{}
	}
	t.Files[id] = files
	return nil
}

func (t *Tab) PrintPDF(_ context.Context, o cdp.PDFOptions) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.PDFs = append(t.PDFs, o)
	return []byte("%PDF-1.4 fake"), nil
}

// OpenDialog simulates the page opening a JavaScript dialog.
func (t *Tab) OpenDialog(d cdp.Dialog) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dialog = &d
}

func (t *Tab) Dialog() *cdp.Dialog {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dialog == nil {
		return nil
	}
	d := *t.dialog
	return &d
}

func (t *Tab) HandleDialog(_ context.Context, accept bool, text *string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dialog == nil {
		return fmt.Errorf("no dialog")
	}
	t.dialog = nil
	t.Answers = append(t.Answers, DialogAnswer{Accept: accept, Text: text})
	return nil
}

// FrameTab is the fake's InFrame view: it behaves like its Tab and
// records its path there on every script call.
type FrameTab struct {
	*Tab
	Path []int
}

func (f *FrameTab) InFrame(path []int) cdp.Tab {
	return f.Tab.InFrame(append(append([]int(nil), f.Path...), path...))
}

func (f *FrameTab) mark() {
	f.mu.Lock()
	f.Frame = f.Path
	f.mu.Unlock()
}

func (f *FrameTab) Evaluate(ctx context.Context, expr string, await bool) (json.RawMessage, error) {
	f.mark()
	return f.Tab.Evaluate(ctx, expr, await)
}

func (f *FrameTab) CallFunction(ctx context.Context, decl string, opts cdp.CallOptions) (*cdp.Remote, error) {
	f.mark()
	return f.Tab.CallFunction(ctx, decl, opts)
}
