package cdp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type chromeTab struct {
	b       *Chrome
	id      string
	session string

	guardMu    sync.Mutex // serialises Guard calls
	guardCheck atomic.Pointer[func(string) error]
	guardUnsub func()
	logs       *logBuffer // nil when capture is off
}

var _ Tab = (*chromeTab)(nil)

func newTab(b *Chrome, id, session string) *chromeTab {
	return &chromeTab{b: b, id: id, session: session}
}

func (t *chromeTab) ID() string { return t.id }

func (t *chromeTab) Logs(after uint64) []LogEntry {
	if t.logs == nil {
		return nil
	}
	return t.logs.since(after)
}

func (t *chromeTab) call(ctx context.Context, method string, params, out any) error {
	return t.b.c.call(ctx, t.session, method, params, out)
}

func (t *chromeTab) Info(ctx context.Context) (TabInfo, error) {
	var res struct {
		TargetInfo struct {
			URL   string `json:"url"`
			Title string `json:"title"`
		} `json:"targetInfo"`
	}
	if err := t.b.c.call(ctx, "", "Target.getTargetInfo", map[string]any{"targetId": t.id}, &res); err != nil {
		return TabInfo{}, ErrNoSuchTab
	}
	return TabInfo{ID: t.id, URL: res.TargetInfo.URL, Title: res.TargetInfo.Title}, nil
}

// navWatch tracks the page events of one navigation. It is fed from the
// dispatcher goroutine, so it only records state and signals.
type navWatch struct {
	frameID string

	mu       sync.Mutex
	docs     map[string]*Document // by loaderId
	inflight map[string]struct{}
	quietAt  time.Time

	dcl, load, navigated chan struct{}
	dclOnce, loadOnce    sync.Once
	navOnce              sync.Once
}

func newNavWatch(frameID string) *navWatch {
	return &navWatch{
		frameID:   frameID,
		docs:      map[string]*Document{},
		inflight:  map[string]struct{}{},
		quietAt:   time.Now(),
		dcl:       make(chan struct{}),
		load:      make(chan struct{}),
		navigated: make(chan struct{}),
	}
}

type rawResponse struct {
	URL        string            `json:"url"`
	Status     int               `json:"status"`
	StatusText string            `json:"statusText"`
	Headers    map[string]string `json:"headers"`
	MimeType   string            `json:"mimeType"`
}

func (w *navWatch) handle(e Event) {
	switch e.Method {
	case "Page.domContentEventFired":
		w.dclOnce.Do(func() { close(w.dcl) })
	case "Page.loadEventFired":
		w.loadOnce.Do(func() { close(w.load) })
	case "Page.frameNavigated", "Page.navigatedWithinDocument":
		var p struct {
			FrameID string `json:"frameId"`
			Frame   struct {
				ID       string `json:"id"`
				ParentID string `json:"parentId"`
			} `json:"frame"`
		}
		if json.Unmarshal(e.Params, &p) == nil && (p.FrameID == w.frameID || (p.Frame.ID != "" && p.Frame.ParentID == "")) {
			w.navOnce.Do(func() { close(w.navigated) })
		}
	case "Network.requestWillBeSent":
		var p struct {
			RequestID string `json:"requestId"`
			Type      string `json:"type"`
		}
		if json.Unmarshal(e.Params, &p) == nil && countsForIdle(p.Type) {
			w.mu.Lock()
			w.inflight[p.RequestID] = struct{}{}
			w.mu.Unlock()
		}
	case "Network.loadingFinished", "Network.loadingFailed":
		var p struct {
			RequestID string `json:"requestId"`
		}
		if json.Unmarshal(e.Params, &p) == nil {
			w.mu.Lock()
			if _, ok := w.inflight[p.RequestID]; ok {
				delete(w.inflight, p.RequestID)
				w.quietAt = time.Now()
			}
			w.mu.Unlock()
		}
	case "Network.responseReceived":
		var p struct {
			RequestID string      `json:"requestId"`
			LoaderID  string      `json:"loaderId"`
			Type      string      `json:"type"`
			FrameID   string      `json:"frameId"`
			Response  rawResponse `json:"response"`
		}
		if json.Unmarshal(e.Params, &p) != nil || p.Type != "Document" || p.FrameID != w.frameID {
			return
		}
		d := &Document{
			URL:        p.Response.URL,
			Status:     p.Response.Status,
			StatusText: p.Response.StatusText,
			MimeType:   p.Response.MimeType,
			RequestID:  p.RequestID,
			Header:     http.Header{},
		}
		for k, v := range p.Response.Headers {
			// Chrome joins repeated headers with newlines.
			for _, line := range strings.Split(v, "\n") {
				d.Header.Add(k, line)
			}
		}
		w.mu.Lock()
		w.docs[p.LoaderID] = d
		w.mu.Unlock()
	}
}

func countsForIdle(resourceType string) bool {
	switch resourceType {
	case "WebSocket", "EventSource", "Media", "Image", "Font":
		return false
	}
	return true
}

func (w *navWatch) waitIdle(ctx context.Context) error {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		w.mu.Lock()
		idle := len(w.inflight) == 0 && time.Since(w.quietAt) >= networkIdleQuiet
		w.mu.Unlock()
		if idle {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func (w *navWatch) wait(ctx context.Context, mode Wait) error {
	var ch chan struct{}
	switch mode {
	case WaitNone:
		return nil
	case WaitDOMContentLoaded:
		ch = w.dcl
	default:
		ch = w.load
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ch:
	}
	if mode == WaitNetworkIdle {
		return w.waitIdle(ctx)
	}
	return nil
}

func (t *chromeTab) Navigate(ctx context.Context, url string, wait Wait) (*Document, error) {
	w := newNavWatch(t.id)
	unsub := t.b.c.subscribe(t.session, w.handle)
	defer unsub()

	var res struct {
		LoaderID  string `json:"loaderId"`
		ErrorText string `json:"errorText"`
	}
	if err := t.call(ctx, "Page.navigate", map[string]any{"url": url}, &res); err != nil {
		return nil, err
	}
	if res.ErrorText != "" {
		return nil, &NavigationError{URL: url, Text: res.ErrorText}
	}
	if res.LoaderID == "" {
		// Same-document navigation (fragment change): nothing to wait for.
		return &Document{URL: url, Status: http.StatusOK, Header: http.Header{}}, nil
	}
	if err := w.wait(ctx, wait); err != nil {
		return nil, err
	}
	w.mu.Lock()
	doc := w.docs[res.LoaderID]
	w.mu.Unlock()
	if doc == nil {
		// about:, data: and similar URLs have no network response.
		doc = &Document{URL: url, Status: http.StatusOK, Header: http.Header{}}
	}
	if doc.StatusText == "" {
		doc.StatusText = http.StatusText(doc.Status)
	}
	return doc, nil
}

func (t *chromeTab) Reload(ctx context.Context, wait Wait) error {
	w := newNavWatch(t.id)
	unsub := t.b.c.subscribe(t.session, w.handle)
	defer unsub()
	if err := t.call(ctx, "Page.reload", nil, nil); err != nil {
		return err
	}
	return w.wait(ctx, wait)
}

// History moves through session history (delta -1 = back, +1 = forward).
// Back/forward may be served from the bfcache, which fires no load event,
// so it waits for the frame to commit and then for readyState.
func (t *chromeTab) History(ctx context.Context, delta int) error {
	var h struct {
		CurrentIndex int `json:"currentIndex"`
		Entries      []struct {
			ID int `json:"id"`
		} `json:"entries"`
	}
	if err := t.call(ctx, "Page.getNavigationHistory", nil, &h); err != nil {
		return err
	}
	idx := h.CurrentIndex + delta
	if idx < 0 || idx >= len(h.Entries) {
		return nil // nothing to do, like the browser buttons
	}
	w := newNavWatch(t.id)
	unsub := t.b.c.subscribe(t.session, w.handle)
	defer unsub()
	if err := t.call(ctx, "Page.navigateToHistoryEntry", map[string]any{"entryId": h.Entries[idx].ID}, nil); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.navigated:
	}
	return t.waitReady(ctx)
}

func (t *chromeTab) waitReady(ctx context.Context) error {
	for {
		v, err := t.Evaluate(ctx, "document.readyState", false)
		if err == nil && string(v) == `"complete"` {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

type remoteObject struct {
	Type                string          `json:"type"`
	Subtype             string          `json:"subtype"`
	Value               json.RawMessage `json:"value"`
	UnserializableValue string          `json:"unserializableValue"`
	ObjectID            string          `json:"objectId"`
	Description         string          `json:"description"`
}

func (r remoteObject) toRemote() *Remote {
	v := r.Value
	if r.UnserializableValue != "" {
		// NaN, Infinity, -0, bigints: surface as strings.
		v, _ = json.Marshal(r.UnserializableValue)
	}
	if len(v) == 0 {
		v = json.RawMessage("null")
	}
	return &Remote{ObjectID: r.ObjectID, Type: r.Type, Subtype: r.Subtype, Value: v}
}

type exceptionDetails struct {
	Text      string        `json:"text"`
	Exception *remoteObject `json:"exception"`
}

func (e *exceptionDetails) err() error {
	if e == nil {
		return nil
	}
	return &JSError{Text: e.message()}
}

func (e *exceptionDetails) message() string {
	if e.Exception != nil && e.Exception.Description != "" {
		return e.Exception.Description
	}
	return e.Text
}

func (t *chromeTab) Evaluate(ctx context.Context, expr string, awaitPromise bool) (json.RawMessage, error) {
	var res struct {
		Result           remoteObject      `json:"result"`
		ExceptionDetails *exceptionDetails `json:"exceptionDetails"`
	}
	err := t.call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    expr,
		"returnByValue": true,
		"awaitPromise":  awaitPromise,
		"userGesture":   true,
	}, &res)
	if err != nil {
		return nil, err
	}
	if err := res.ExceptionDetails.err(); err != nil {
		return nil, err
	}
	return res.Result.toRemote().Value, nil
}

func (t *chromeTab) CallFunction(ctx context.Context, decl string, opts CallOptions) (*Remote, error) {
	this := opts.This
	if this == "" {
		var g struct {
			Result remoteObject `json:"result"`
		}
		if err := t.call(ctx, "Runtime.evaluate", map[string]any{"expression": "globalThis"}, &g); err != nil {
			return nil, err
		}
		this = g.Result.ObjectID
		defer func() { _ = t.Release(context.WithoutCancel(ctx), this) }()
	}
	args := make([]map[string]any, len(opts.Args))
	for i, a := range opts.Args {
		if a.ObjectID != "" {
			args[i] = map[string]any{"objectId": a.ObjectID}
		} else {
			args[i] = map[string]any{"value": a.Value}
		}
	}
	var res struct {
		Result           remoteObject      `json:"result"`
		ExceptionDetails *exceptionDetails `json:"exceptionDetails"`
	}
	err := t.call(ctx, "Runtime.callFunctionOn", map[string]any{
		"functionDeclaration": decl,
		"objectId":            this,
		"arguments":           args,
		"returnByValue":       opts.ByValue,
		"awaitPromise":        opts.Await,
		"userGesture":         true,
	}, &res)
	if err != nil {
		return nil, err
	}
	if err := res.ExceptionDetails.err(); err != nil {
		return nil, err
	}
	return res.Result.toRemote(), nil
}

func (t *chromeTab) Properties(ctx context.Context, objectID string) ([]Property, error) {
	var res struct {
		Result []struct {
			Name  string        `json:"name"`
			Value *remoteObject `json:"value"`
		} `json:"result"`
	}
	if err := t.call(ctx, "Runtime.getProperties", map[string]any{
		"objectId":      objectID,
		"ownProperties": true,
	}, &res); err != nil {
		return nil, err
	}
	out := make([]Property, 0, len(res.Result))
	for _, p := range res.Result {
		if p.Value == nil {
			continue
		}
		out = append(out, Property{Name: p.Name, Value: p.Value.toRemote()})
	}
	return out, nil
}

// ArrayItems returns the elements of a remote array, in index order.
func ArrayItems(ctx context.Context, t Tab, objectID string) ([]*Remote, error) {
	props, err := t.Properties(ctx, objectID)
	if err != nil {
		return nil, err
	}
	type item struct {
		i int
		r *Remote
	}
	var items []item
	for _, p := range props {
		i, err := strconv.Atoi(p.Name)
		if err != nil {
			continue
		}
		items = append(items, item{i, p.Value})
	}
	sort.Slice(items, func(a, b int) bool { return items[a].i < items[b].i })
	out := make([]*Remote, len(items))
	for i, it := range items {
		out[i] = it.r
	}
	return out, nil
}

func (t *chromeTab) Release(ctx context.Context, objectID string) error {
	if objectID == "" {
		return nil
	}
	return t.call(ctx, "Runtime.releaseObject", map[string]any{"objectId": objectID}, nil)
}

func (t *chromeTab) NodeID(ctx context.Context, objectID string) (int, error) {
	var res struct {
		Node struct {
			BackendNodeID int `json:"backendNodeId"`
		} `json:"node"`
	}
	if err := t.call(ctx, "DOM.describeNode", map[string]any{"objectId": objectID}, &res); err != nil {
		return 0, err
	}
	return res.Node.BackendNodeID, nil
}

func (t *chromeTab) ResolveNode(ctx context.Context, backendNodeID int) (string, error) {
	var res struct {
		Object remoteObject `json:"object"`
	}
	if err := t.call(ctx, "DOM.resolveNode", map[string]any{"backendNodeId": backendNodeID}, &res); err != nil {
		return "", err
	}
	return res.Object.ObjectID, nil
}

func (t *chromeTab) ResponseBody(ctx context.Context, requestID string) ([]byte, error) {
	if requestID == "" {
		return nil, errors.New("cdp: no response to read")
	}
	var res struct {
		Body          string `json:"body"`
		Base64Encoded bool   `json:"base64Encoded"`
	}
	if err := t.call(ctx, "Network.getResponseBody", map[string]any{"requestId": requestID}, &res); err != nil {
		return nil, err
	}
	if res.Base64Encoded {
		return base64.StdEncoding.DecodeString(res.Body)
	}
	return []byte(res.Body), nil
}

func (t *chromeTab) Screenshot(ctx context.Context, clip *Rect) ([]byte, error) {
	params := map[string]any{"format": "png"}
	if clip != nil {
		params["clip"] = map[string]any{
			"x": clip.X, "y": clip.Y, "width": clip.Width, "height": clip.Height, "scale": 1,
		}
		params["captureBeyondViewport"] = true
	}
	var res struct {
		Data []byte `json:"data"` // base64 in JSON
	}
	if err := t.call(ctx, "Page.captureScreenshot", params, &res); err != nil {
		return nil, err
	}
	return res.Data, nil
}

func (t *chromeTab) Cookies(ctx context.Context, urls []string) ([]Cookie, error) {
	var res struct {
		Cookies []Cookie `json:"cookies"`
	}
	params := map[string]any{}
	if len(urls) > 0 {
		params["urls"] = urls
	}
	if err := t.call(ctx, "Network.getCookies", params, &res); err != nil {
		return nil, err
	}
	return res.Cookies, nil
}

func (t *chromeTab) SetCookie(ctx context.Context, c Cookie) error {
	var res struct {
		Success *bool `json:"success"`
	}
	if err := t.call(ctx, "Network.setCookie", c, &res); err != nil {
		return err
	}
	if res.Success != nil && !*res.Success {
		return errors.New("cdp: cookie rejected by browser")
	}
	return nil
}

func (t *chromeTab) DeleteCookie(ctx context.Context, name, url string) error {
	return t.call(ctx, "Network.deleteCookies", map[string]any{"name": name, "url": url}, nil)
}

func (t *chromeTab) Click(ctx context.Context, x, y float64) error {
	for _, typ := range []string{"mouseMoved", "mousePressed", "mouseReleased"} {
		p := map[string]any{"type": typ, "x": x, "y": y}
		if typ != "mouseMoved" {
			p["button"] = "left"
			p["clickCount"] = 1
		}
		if err := t.call(ctx, "Input.dispatchMouseEvent", p, nil); err != nil {
			return err
		}
	}
	return nil
}

func (t *chromeTab) Key(ctx context.Context, k KeyEvent) error {
	return t.call(ctx, "Input.dispatchKeyEvent", k, nil)
}
