package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// autoAttachParams attach to out-of-process iframes only, without pausing
// them, over the same flattened connection.
var autoAttachParams = map[string]any{
	"autoAttach":             true,
	"waitForDebuggerOnStart": false,
	"flatten":                true,
	"filter":                 []map[string]any{{"type": "iframe", "exclude": false}, {"exclude": true}},
}

func (t *chromeTab) owns(session string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessions[session]
}

// onEvent tracks the tab's iframe targets and JavaScript dialogs. It runs
// on the dispatcher goroutine, so it must not block or call Chrome.
func (t *chromeTab) onEvent(e Event) {
	switch e.Method {
	case "Target.attachedToTarget":
		if !t.owns(e.SessionID) {
			return
		}
		var p struct {
			SessionID  string `json:"sessionId"`
			TargetInfo struct {
				TargetID string `json:"targetId"`
				Type     string `json:"type"`
			} `json:"targetInfo"`
			Waiting bool `json:"waitingForDebugger"`
		}
		if json.Unmarshal(e.Params, &p) != nil || p.TargetInfo.Type != "iframe" {
			return
		}
		t.mu.Lock()
		t.children[p.TargetInfo.TargetID] = p.SessionID
		t.sessions[p.SessionID] = true
		t.mu.Unlock()
		go t.initChild(p.SessionID, p.Waiting)
	case "Target.detachedFromTarget":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(e.Params, &p) != nil || !t.owns(p.SessionID) {
			return
		}
		if p.SessionID == t.session {
			t.stop()
			return
		}
		t.mu.Lock()
		delete(t.sessions, p.SessionID)
		for frame, s := range t.children {
			if s == p.SessionID {
				delete(t.children, frame)
			}
		}
		t.mu.Unlock()
	case "Page.javascriptDialogOpening":
		if !t.owns(e.SessionID) {
			return
		}
		var d Dialog
		if json.Unmarshal(e.Params, &d) != nil {
			return
		}
		t.mu.Lock()
		if t.dialog == nil {
			close(t.dialogCh)
		}
		t.dialog, t.dialogOn = &d, e.SessionID
		t.mu.Unlock()
	case "Page.javascriptDialogClosed":
		if t.owns(e.SessionID) {
			t.clearDialog()
		}
	}
}

func (t *chromeTab) initChild(session string, waiting bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Nested cross-site frames attach through their parent frame.
	_ = t.b.c.call(ctx, session, "Target.setAutoAttach", autoAttachParams, nil)
	_ = t.b.c.call(ctx, session, "Page.enable", nil, nil)
	if waiting {
		_ = t.b.c.call(ctx, session, "Runtime.runIfWaitingForDebugger", nil, nil)
	}
}

// stop drops the tab's event subscription once its target is gone.
func (t *chromeTab) stop() {
	t.mu.Lock()
	unsub := t.unsub
	t.unsub = nil
	t.mu.Unlock()
	if unsub != nil {
		unsub()
	}
}

func (t *chromeTab) clearDialog() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dialog != nil {
		t.dialog, t.dialogOn = nil, ""
		t.dialogCh = make(chan struct{})
	}
}

func (t *chromeTab) Dialog() *Dialog {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dialog == nil {
		return nil
	}
	d := *t.dialog
	return &d
}

func (t *chromeTab) HandleDialog(ctx context.Context, accept bool, promptText *string) error {
	t.mu.Lock()
	open, session := t.dialog != nil, t.dialogOn
	t.mu.Unlock()
	if !open {
		return errors.New("cdp: no dialog is open")
	}
	params := map[string]any{"accept": accept}
	if promptText != nil {
		params["promptText"] = *promptText
	}
	if err := t.b.c.call(ctx, session, "Page.handleJavaScriptDialog", params, nil); err != nil {
		return err
	}
	t.clearDialog()
	return nil
}

// guard runs fn, giving up with ErrDialogOpen as soon as a JavaScript
// dialog is open: the page is blocked until someone answers it, so
// waiting would only hang.
func (t *chromeTab) guard(ctx context.Context, fn func(ctx context.Context) error) error {
	t.mu.Lock()
	ch, open := t.dialogCh, t.dialog != nil
	t.mu.Unlock()
	if open {
		return ErrDialogOpen
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()
	select {
	case err := <-done:
		return err
	case <-ch:
		return ErrDialogOpen
	}
}

// A realm is where a frame's JavaScript and DOM live: a CDP session, and
// for a same-process frame, the backend node of its document.
type realm struct {
	session string
	doc     int // 0 = the session's main frame
}

func (t *chromeTab) top() realm { return realm{session: t.session} }

// resolve walks a frame path from the top document.
func (t *chromeTab) resolve(ctx context.Context, path []int) (realm, error) {
	r := t.top()
	for _, id := range path {
		var res struct {
			Node struct {
				FrameID         string `json:"frameId"`
				ContentDocument *struct {
					BackendNodeID int `json:"backendNodeId"`
				} `json:"contentDocument"`
			} `json:"node"`
		}
		if err := t.b.c.call(ctx, r.session, "DOM.describeNode", map[string]any{"backendNodeId": id}, &res); err != nil {
			if ctx.Err() != nil {
				return realm{}, ctx.Err()
			}
			return realm{}, fmt.Errorf("%w: %w", ErrNoSuchFrame, err)
		}
		if cd := res.Node.ContentDocument; cd != nil && cd.BackendNodeID != 0 {
			r = realm{session: r.session, doc: cd.BackendNodeID}
			continue
		}
		if res.Node.FrameID == "" {
			return realm{}, fmt.Errorf("%w: node %d is not a frame", ErrNoSuchFrame, id)
		}
		t.mu.Lock()
		s, ok := t.children[res.Node.FrameID]
		t.mu.Unlock()
		if !ok {
			return realm{}, fmt.Errorf("%w: frame %s is not attached", ErrNoSuchFrame, res.Node.FrameID)
		}
		r = realm{session: s}
	}
	return r, nil
}

func (t *chromeTab) callIn(ctx context.Context, r realm, method string, params, out any) error {
	return t.b.c.call(ctx, r.session, method, params, out)
}

// global returns a handle to the realm's globalThis, which the caller
// must release.
func (t *chromeTab) global(ctx context.Context, r realm) (string, error) {
	var res struct {
		Result           remoteObject      `json:"result"`
		ExceptionDetails *exceptionDetails `json:"exceptionDetails"`
	}
	if r.doc == 0 {
		if err := t.callIn(ctx, r, "Runtime.evaluate", map[string]any{"expression": "globalThis"}, &res); err != nil {
			return "", err
		}
		return res.Result.ObjectID, nil
	}
	doc, err := t.resolveIn(ctx, r, r.doc)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoSuchFrame, err)
	}
	defer func() { _ = t.releaseIn(context.WithoutCancel(ctx), r, doc) }()
	if err := t.callIn(ctx, r, "Runtime.callFunctionOn", map[string]any{
		"functionDeclaration": "function() { return globalThis; }",
		"objectId":            doc,
	}, &res); err != nil {
		return "", err
	}
	if err := res.ExceptionDetails.err(); err != nil {
		return "", err
	}
	return res.Result.ObjectID, nil
}

func (t *chromeTab) evaluateIn(ctx context.Context, r realm, expr string, awaitPromise bool) (json.RawMessage, error) {
	if r.doc != 0 {
		// Runtime.evaluate only targets a frame by execution context, which
		// needs the Runtime domain enabled; calling a function on the
		// frame's global object reaches the same main world without it.
		rem, err := t.callFunctionIn(ctx, r, "function() { return (\n"+expr+"\n); }", CallOptions{Await: awaitPromise, ByValue: true})
		if err != nil {
			return nil, err
		}
		return rem.Value, nil
	}
	var res struct {
		Result           remoteObject      `json:"result"`
		ExceptionDetails *exceptionDetails `json:"exceptionDetails"`
	}
	err := t.callIn(ctx, r, "Runtime.evaluate", map[string]any{
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

func (t *chromeTab) callFunctionIn(ctx context.Context, r realm, decl string, opts CallOptions) (*Remote, error) {
	this := opts.This
	if this == "" {
		g, err := t.global(ctx, r)
		if err != nil {
			return nil, err
		}
		this = g
		defer func() { _ = t.releaseIn(context.WithoutCancel(ctx), r, g) }()
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
	err := t.callIn(ctx, r, "Runtime.callFunctionOn", map[string]any{
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

func (t *chromeTab) propertiesIn(ctx context.Context, r realm, objectID string) ([]Property, error) {
	var res struct {
		Result []struct {
			Name  string        `json:"name"`
			Value *remoteObject `json:"value"`
		} `json:"result"`
	}
	if err := t.callIn(ctx, r, "Runtime.getProperties", map[string]any{
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

func (t *chromeTab) releaseIn(ctx context.Context, r realm, objectID string) error {
	if objectID == "" {
		return nil
	}
	return t.callIn(ctx, r, "Runtime.releaseObject", map[string]any{"objectId": objectID}, nil)
}

func (t *chromeTab) nodeIDIn(ctx context.Context, r realm, objectID string) (int, error) {
	var res struct {
		Node struct {
			BackendNodeID int `json:"backendNodeId"`
		} `json:"node"`
	}
	if err := t.callIn(ctx, r, "DOM.describeNode", map[string]any{"objectId": objectID}, &res); err != nil {
		return 0, err
	}
	return res.Node.BackendNodeID, nil
}

func (t *chromeTab) resolveIn(ctx context.Context, r realm, backendNodeID int) (string, error) {
	var res struct {
		Object remoteObject `json:"object"`
	}
	if err := t.callIn(ctx, r, "DOM.resolveNode", map[string]any{"backendNodeId": backendNodeID}, &res); err != nil {
		return "", err
	}
	return res.Object.ObjectID, nil
}

func (t *chromeTab) shadowRootIn(ctx context.Context, r realm, backendNodeID int) (int, error) {
	var res struct {
		Node struct {
			ShadowRoots []struct {
				BackendNodeID  int    `json:"backendNodeId"`
				ShadowRootType string `json:"shadowRootType"`
			} `json:"shadowRoots"`
		} `json:"node"`
	}
	if err := t.callIn(ctx, r, "DOM.describeNode", map[string]any{"backendNodeId": backendNodeID}, &res); err != nil {
		return 0, err
	}
	for _, sr := range res.Node.ShadowRoots {
		if sr.ShadowRootType == "open" || sr.ShadowRootType == "closed" { // not the browser's own (user-agent) roots
			return sr.BackendNodeID, nil
		}
	}
	return 0, nil
}

func (t *chromeTab) setFilesIn(ctx context.Context, r realm, backendNodeID int, files []string) error {
	return t.callIn(ctx, r, "DOM.setFileInputFiles", map[string]any{"files": files, "backendNodeId": backendNodeID}, nil)
}

// Tab methods on the top frame.

func (t *chromeTab) Evaluate(ctx context.Context, expr string, awaitPromise bool) (json.RawMessage, error) {
	var v json.RawMessage
	err := t.guard(ctx, func(ctx context.Context) (err error) {
		v, err = t.evaluateIn(ctx, t.top(), expr, awaitPromise)
		return err
	})
	return v, err
}

func (t *chromeTab) CallFunction(ctx context.Context, decl string, opts CallOptions) (*Remote, error) {
	var v *Remote
	err := t.guard(ctx, func(ctx context.Context) (err error) {
		v, err = t.callFunctionIn(ctx, t.top(), decl, opts)
		return err
	})
	return v, err
}

func (t *chromeTab) Properties(ctx context.Context, objectID string) ([]Property, error) {
	return t.propertiesIn(ctx, t.top(), objectID)
}

func (t *chromeTab) Release(ctx context.Context, objectID string) error {
	return t.releaseIn(ctx, t.top(), objectID)
}

func (t *chromeTab) NodeID(ctx context.Context, objectID string) (int, error) {
	return t.nodeIDIn(ctx, t.top(), objectID)
}

func (t *chromeTab) ResolveNode(ctx context.Context, backendNodeID int) (string, error) {
	return t.resolveIn(ctx, t.top(), backendNodeID)
}

func (t *chromeTab) ShadowRoot(ctx context.Context, backendNodeID int) (int, error) {
	return t.shadowRootIn(ctx, t.top(), backendNodeID)
}

func (t *chromeTab) SetFiles(ctx context.Context, backendNodeID int, files []string) error {
	return t.setFilesIn(ctx, t.top(), backendNodeID, files)
}

func (t *chromeTab) InFrame(path []int) Tab {
	if len(path) == 0 {
		return t
	}
	return &frameTab{chromeTab: t, path: append([]int(nil), path...)}
}

// frameTab is a chromeTab whose script and DOM calls run in a child frame.
type frameTab struct {
	*chromeTab
	path []int
}

func (f *frameTab) InFrame(path []int) Tab {
	return f.chromeTab.InFrame(append(append([]int(nil), f.path...), path...))
}

func (f *frameTab) Evaluate(ctx context.Context, expr string, awaitPromise bool) (json.RawMessage, error) {
	var v json.RawMessage
	err := f.guard(ctx, func(ctx context.Context) error {
		r, err := f.resolve(ctx, f.path)
		if err != nil {
			return err
		}
		v, err = f.evaluateIn(ctx, r, expr, awaitPromise)
		return err
	})
	return v, err
}

func (f *frameTab) CallFunction(ctx context.Context, decl string, opts CallOptions) (*Remote, error) {
	var v *Remote
	err := f.guard(ctx, func(ctx context.Context) error {
		r, err := f.resolve(ctx, f.path)
		if err != nil {
			return err
		}
		v, err = f.callFunctionIn(ctx, r, decl, opts)
		return err
	})
	return v, err
}

func (f *frameTab) Properties(ctx context.Context, objectID string) ([]Property, error) {
	r, err := f.resolve(ctx, f.path)
	if err != nil {
		return nil, err
	}
	return f.propertiesIn(ctx, r, objectID)
}

func (f *frameTab) Release(ctx context.Context, objectID string) error {
	r, err := f.resolve(ctx, f.path)
	if err != nil {
		return err
	}
	return f.releaseIn(ctx, r, objectID)
}

func (f *frameTab) NodeID(ctx context.Context, objectID string) (int, error) {
	r, err := f.resolve(ctx, f.path)
	if err != nil {
		return 0, err
	}
	return f.nodeIDIn(ctx, r, objectID)
}

func (f *frameTab) ResolveNode(ctx context.Context, backendNodeID int) (string, error) {
	r, err := f.resolve(ctx, f.path)
	if err != nil {
		return "", err
	}
	return f.resolveIn(ctx, r, backendNodeID)
}

func (f *frameTab) ShadowRoot(ctx context.Context, backendNodeID int) (int, error) {
	r, err := f.resolve(ctx, f.path)
	if err != nil {
		return 0, err
	}
	return f.shadowRootIn(ctx, r, backendNodeID)
}

func (f *frameTab) SetFiles(ctx context.Context, backendNodeID int, files []string) error {
	r, err := f.resolve(ctx, f.path)
	if err != nil {
		return err
	}
	return f.setFilesIn(ctx, r, backendNodeID, files)
}
