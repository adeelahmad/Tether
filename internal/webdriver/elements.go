package webdriver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/adeelahmad/tether/internal/cdp"
)

const findScript = `const [using, value, root] = arguments;
const scope = root || document;
const links = () => Array.from(scope.querySelectorAll('a'));
const text = (a) => (a.innerText || a.textContent || '').trim();
try {
  switch (using) {
    case 'css selector': return Array.from(scope.querySelectorAll(value));
    case 'tag name': return Array.from(scope.getElementsByTagName ? scope.getElementsByTagName(value) : scope.querySelectorAll(value));
    case 'link text': return links().filter(a => text(a) === value);
    case 'partial link text': return links().filter(a => text(a).includes(value));
    case 'xpath': {
      const r = document.evaluate(value, scope, null, XPathResult.ORDERED_NODE_SNAPSHOT_TYPE, null);
      const out = [];
      for (let i = 0; i < r.snapshotLength; i++) {
        const n = r.snapshotItem(i);
        if (n.nodeType === 1) out.push(n);
      }
      return out;
    }
  }
} catch (e) {
  const err = new Error('InvalidSelector: ' + e.message);
  throw err;
}
return [];`

var locators = map[string]bool{
	"css selector": true, "link text": true, "partial link text": true, "tag name": true, "xpath": true,
}

// locate finds elements from the document, or from root (an element or
// shadow root argument) when it is non-nil.
func (h *Handler) locate(ctx context.Context, ws *wdSession, body []byte, root json.RawMessage) ([]any, error) {
	var req struct {
		Using string `json:"using"`
		Value string `json:"value"`
	}
	if err := decodeBody(body, &req); err != nil {
		return nil, err
	}
	if !locators[req.Using] {
		return nil, wdErr(ErrInvalidArgument, "unsupported locator strategy "+req.Using)
	}
	if root == nil {
		root = json.RawMessage("null")
	}
	args := []json.RawMessage{stringArg(req.Using), stringArg(req.Value), root}
	deadline := time.Now().Add(time.Duration(ws.getTimeouts().Implicit) * time.Millisecond)
	for {
		v, err := runScript(ctx, ws.tab(), findScript, args, false)
		if err != nil {
			var js *cdp.JSError
			if errors.As(err, &js) && strings.Contains(js.Text, "InvalidSelector") {
				return nil, wdErr(ErrInvalidSelector, js.Text)
			}
			return nil, err
		}
		found, _ := v.([]any)
		if len(found) > 0 || !time.Now().Before(deadline) {
			return found, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func elementRoot(r *http.Request, fromElement bool) json.RawMessage {
	if fromElement {
		return elementArg(r.PathValue("eid"))
	}
	return nil
}

func (h *Handler) findElement(fromElement bool) command {
	return func(ctx context.Context, ws *wdSession, r *http.Request, body []byte) (any, error) {
		return firstFound(h.locate(ctx, ws, body, elementRoot(r, fromElement)))
	}
}

func (h *Handler) findElements(fromElement bool) command {
	return func(ctx context.Context, ws *wdSession, r *http.Request, body []byte) (any, error) {
		return allFound(h.locate(ctx, ws, body, elementRoot(r, fromElement)))
	}
}

func firstFound(found []any, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, wdErr(ErrNoSuchElement, "no element matches the locator")
	}
	return found[0], nil
}

func allFound(found []any, err error) (any, error) {
	if found == nil && err == nil {
		found = []any{}
	}
	return found, err
}

func (h *Handler) activeElement(ctx context.Context, ws *wdSession, _ *http.Request, _ []byte) (any, error) {
	v, err := runScript(ctx, ws.tab(), "return document.activeElement;", nil, false)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, wdErr(ErrNoSuchElement, "no active element")
	}
	return v, nil
}

// Element property scripts. arguments[0] is the element.
const (
	scriptText      = `return (arguments[0].innerText ?? arguments[0].textContent ?? '').replace(/ /g, ' ').trim();`
	scriptTagName   = `return arguments[0].tagName.toLowerCase();`
	scriptEnabled   = `return !arguments[0].disabled;`
	scriptSelected  = `const e = arguments[0]; return !!(e.checked || e.selected);`
	scriptProperty  = `const v = arguments[0][arguments[1]]; return v === undefined ? null : v;`
	scriptCSS       = `return getComputedStyle(arguments[0]).getPropertyValue(arguments[1]);`
	scriptRect      = `const r = arguments[0].getBoundingClientRect(); return {x: r.left + scrollX, y: r.top + scrollY, width: r.width, height: r.height};`
	scriptDisplayed = `const e = arguments[0];
if (!e.isConnected) return false;
for (let n = e; n && n.nodeType === 1; n = n.parentElement) {
  const s = getComputedStyle(n);
  if (s.display === 'none') return false;
  if (n === e && (s.visibility === 'hidden' || s.visibility === 'collapse')) return false;
  if (s.opacity === '0' && n === e) return false;
}
if (e.tagName === 'INPUT' && e.type === 'hidden') return false;
return e.getClientRects().length > 0 || e.tagName === 'OPTION';`
	scriptAttribute = `const [e, name] = arguments;
const booleans = ['allowfullscreen','async','autofocus','autoplay','checked','controls','default','defer',
  'disabled','formnovalidate','hidden','inert','ismap','itemscope','loop','multiple','muted','nomodule',
  'novalidate','open','playsinline','readonly','required','reversed','selected'];
if (booleans.includes(name.toLowerCase())) return e.hasAttribute(name) ? 'true' : null;
return e.getAttribute(name);`
)

func (h *Handler) elementScript(script string, pathParams ...string) command {
	return func(ctx context.Context, ws *wdSession, r *http.Request, _ []byte) (any, error) {
		args := []json.RawMessage{elementArg(r.PathValue("eid"))}
		for _, p := range pathParams {
			args = append(args, stringArg(r.PathValue(p)))
		}
		return runScript(ctx, ws.tab(), script, args, false)
	}
}

const scriptClickPoint = `const e = arguments[0];
if (e.tagName === 'OPTION') {
  const sel = e.closest('select');
  if (sel) {
    if (sel.multiple) e.selected = !e.selected; else e.selected = true;
    sel.dispatchEvent(new Event('input', {bubbles: true}));
    sel.dispatchEvent(new Event('change', {bubbles: true}));
  }
  return {handled: true};
}
e.scrollIntoView({block: 'center', inline: 'center', behavior: 'instant'});
const rects = e.getClientRects();
if (!rects.length) return {visible: false};
const r = rects[0];
const x = r.left + r.width / 2, y = r.top + r.height / 2;
if (r.width === 0 || r.height === 0) return {visible: false};
const hit = document.elementFromPoint(x, y);
return {visible: true, x, y, hit: !!hit && (hit === e || e.contains(hit) || hit.contains(e)),
        blocker: hit ? hit.tagName.toLowerCase() + (hit.id ? '#' + hit.id : '') : ''};`

func (h *Handler) click(ctx context.Context, ws *wdSession, r *http.Request, _ []byte) (any, error) {
	if err := revealFrames(ctx, ws); err != nil {
		return nil, err
	}
	v, err := runScript(ctx, ws.tab(), scriptClickPoint, []json.RawMessage{elementArg(r.PathValue("eid"))}, false)
	if err != nil {
		return nil, err
	}
	var p struct {
		Handled bool    `json:"handled"`
		Visible bool    `json:"visible"`
		X       float64 `json:"x"`
		Y       float64 `json:"y"`
		Hit     bool    `json:"hit"`
		Blocker string  `json:"blocker"`
	}
	if err := remarshal(v, &p); err != nil {
		return nil, err
	}
	if p.Handled {
		return nil, nil
	}
	if !p.Visible {
		return nil, wdErr(ErrElementNotInteractable, "element has no size or is not rendered")
	}
	if !p.Hit {
		return nil, wdErr(ErrElementClickIntercepted, "element is obscured by <"+p.Blocker+">")
	}
	ox, oy, err := frameOffset(ctx, ws)
	if err != nil {
		return nil, err
	}
	ref := elementArg(r.PathValue("eid"))
	inFrame := len(ws.frames) > 0
	if inFrame {
		if _, err := runScript(ctx, ws.tab(), scriptArmClick, []json.RawMessage{ref}, false); err != nil {
			return nil, err
		}
	}
	if err := ws.s.Tab.Click(ctx, p.X+ox, p.Y+oy); err != nil {
		if errors.Is(err, cdp.ErrDialogOpen) {
			return nil, nil // the click opened a user prompt
		}
		return nil, err
	}
	if inFrame {
		if err := confirmFrameClick(ctx, ws, ref); err != nil {
			return nil, err
		}
	}
	settle(ctx, ws)
	return nil, nil
}

// Chrome routes input into a frame asynchronously, so a click in a frame
// is confirmed by a one-shot listener on the element. If the real click
// does not arrive (a cross-site frame Chrome could not hit-test), the
// element is clicked from script instead.
const (
	scriptArmClick = `const e = arguments[0], k = Symbol.for('tether.clicked');
e[k] = false;
e.addEventListener('click', () => { e[k] = true; }, {capture: true, once: true});`
	scriptClicked   = `return arguments[0][Symbol.for('tether.clicked')] !== false;`
	scriptClickJS   = `const e = arguments[0]; delete e[Symbol.for('tether.clicked')]; e.click();`
	frameClickGrace = time.Second
)

func confirmFrameClick(ctx context.Context, ws *wdSession, ref json.RawMessage) error {
	deadline := time.Now().Add(frameClickGrace)
	for {
		v, err := runScript(ctx, ws.tab(), scriptClicked, []json.RawMessage{ref}, false)
		if err != nil {
			// The click navigated the frame or removed the element: it landed.
			if errors.Is(err, cdp.ErrDialogOpen) || isGone(err) {
				return nil
			}
			return err
		}
		if v == true {
			return nil
		}
		if !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	_, err := runScript(ctx, ws.tab(), scriptClickJS, []json.RawMessage{ref}, false)
	if errors.Is(err, cdp.ErrDialogOpen) {
		return nil
	}
	return err
}

func isGone(err error) bool {
	var we *Error
	if errors.As(err, &we) {
		return we.Code == ErrStaleElement || we.Code == ErrNoSuchElement
	}
	return errors.Is(err, cdp.ErrNoSuchFrame) || cdp.IsStaleObject(err)
}

// settle gives a click-triggered navigation a moment to start and, if one
// did, waits for it like chromedriver does.
func settle(ctx context.Context, ws *wdSession) {
	if ws.strategy == "none" {
		return
	}
	limit := time.Now().Add(time.Duration(ws.getTimeouts().PageLoad) * time.Millisecond)
	select {
	case <-ctx.Done():
		return
	case <-time.After(75 * time.Millisecond):
	}
	for time.Now().Before(limit) {
		state, err := evalString(ctx, ws.s.Tab, "document.readyState")
		if err == nil && (state == "complete" || (ws.strategy == "eager" && state == "interactive")) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

const scriptFocus = `const e = arguments[0];
if (e.tagName === 'INPUT' && e.type === 'file') return e.multiple ? 'files' : 'file';
e.scrollIntoView({block: 'center', inline: 'center', behavior: 'instant'});
e.focus();
if (typeof e.setSelectionRange === 'function' && typeof e.value === 'string') {
  try { e.setSelectionRange(e.value.length, e.value.length); } catch (_) {}
}
const a = document.activeElement;
return (a === e || e.contains(a)) ? 'ok' : 'unfocusable';`

func (h *Handler) sendKeys(ctx context.Context, ws *wdSession, r *http.Request, body []byte) (any, error) {
	var req struct {
		Text *string `json:"text"`
	}
	if err := decodeBody(body, &req); err != nil {
		return nil, err
	}
	if req.Text == nil {
		return nil, wdErr(ErrInvalidArgument, "text is required")
	}
	ref := r.PathValue("eid")
	v, err := runScript(ctx, ws.tab(), scriptFocus, []json.RawMessage{elementArg(ref)}, false)
	if err != nil {
		return nil, err
	}
	switch v {
	case "file", "files":
		return nil, setFiles(ctx, ws, ref, *req.Text, v == "files")
	case "unfocusable":
		return nil, wdErr(ErrElementNotInteractable, "element cannot receive keyboard focus")
	}
	if err := typeText(ctx, ws.s.Tab, *req.Text); err != nil {
		if errors.Is(err, cdp.ErrDialogOpen) {
			return nil, nil // a key opened a user prompt
		}
		return nil, err
	}
	if strings.ContainsAny(*req.Text, "\uE006\uE007") {
		settle(ctx, ws) // Enter may submit a form
	}
	return nil, nil
}

const scriptClear = `const e = arguments[0];
if (e.isContentEditable) { e.innerHTML = ''; }
else if ('value' in e) {
  if (e.readOnly || e.disabled) return false;
  e.focus(); e.value = '';
} else return false;
e.dispatchEvent(new Event('input', {bubbles: true}));
e.dispatchEvent(new Event('change', {bubbles: true}));
e.blur();
return true;`

func (h *Handler) clear(ctx context.Context, ws *wdSession, r *http.Request, _ []byte) (any, error) {
	v, err := runScript(ctx, ws.tab(), scriptClear, []json.RawMessage{elementArg(r.PathValue("eid"))}, false)
	if err != nil {
		return nil, err
	}
	if v != true {
		return nil, wdErr(ErrElementNotInteractable, "element is not editable")
	}
	return nil, nil
}

// scriptViewportRect scrolls an element into view and returns its rect
// relative to its frame's viewport.
const scriptViewportRect = `const e = arguments[0];
e.scrollIntoView({block: 'center', behavior: 'instant'});
const r = e.getBoundingClientRect();
return {x: r.left, y: r.top, width: r.width, height: r.height};`

func (h *Handler) elementScreenshot(ctx context.Context, ws *wdSession, r *http.Request, _ []byte) (any, error) {
	if err := revealFrames(ctx, ws); err != nil {
		return nil, err
	}
	v, err := runScript(ctx, ws.tab(), scriptViewportRect, []json.RawMessage{elementArg(r.PathValue("eid"))}, false)
	if err != nil {
		return nil, err
	}
	var rect cdp.Rect
	if err := remarshal(v, &struct {
		X      *float64 `json:"x"`
		Y      *float64 `json:"y"`
		Width  *float64 `json:"width"`
		Height *float64 `json:"height"`
	}{&rect.X, &rect.Y, &rect.Width, &rect.Height}); err != nil {
		return nil, err
	}
	if rect.Width == 0 || rect.Height == 0 {
		return nil, wdErr(ErrElementNotInteractable, "element has no size")
	}
	// The clip is in top-level document coordinates.
	ox, oy, err := frameOffset(ctx, ws)
	if err != nil {
		return nil, err
	}
	scroll, err := ws.s.Tab.Evaluate(ctx, "({x: scrollX, y: scrollY})", false)
	if err != nil {
		return nil, err
	}
	var sc struct{ X, Y float64 }
	_ = json.Unmarshal(scroll, &sc)
	rect.X += ox + sc.X
	rect.Y += oy + sc.Y
	png, err := ws.s.Tab.Screenshot(ctx, &rect)
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.EncodeToString(png), nil
}

func remarshal(in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
