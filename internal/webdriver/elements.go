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
    case 'tag name': return Array.from(scope.getElementsByTagName(value));
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

func (h *Handler) locate(ctx context.Context, ws *wdSession, r *http.Request, body []byte, fromElement bool) ([]any, error) {
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
	root := json.RawMessage("null")
	if fromElement {
		root = elementArg(r.PathValue("eid"))
	}
	args := []json.RawMessage{stringArg(req.Using), stringArg(req.Value), root}
	deadline := time.Now().Add(time.Duration(ws.getTimeouts().Implicit) * time.Millisecond)
	for {
		v, err := runScript(ctx, ws.s.Tab, findScript, args, false)
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

func (h *Handler) findElement(fromElement bool) command {
	return func(ctx context.Context, ws *wdSession, r *http.Request, body []byte) (any, error) {
		found, err := h.locate(ctx, ws, r, body, fromElement)
		if err != nil {
			return nil, err
		}
		if len(found) == 0 {
			return nil, wdErr(ErrNoSuchElement, "no element matches the locator")
		}
		return found[0], nil
	}
}

func (h *Handler) findElements(fromElement bool) command {
	return func(ctx context.Context, ws *wdSession, r *http.Request, body []byte) (any, error) {
		found, err := h.locate(ctx, ws, r, body, fromElement)
		if found == nil && err == nil {
			found = []any{}
		}
		return found, err
	}
}

func (h *Handler) activeElement(ctx context.Context, ws *wdSession, _ *http.Request, _ []byte) (any, error) {
	v, err := runScript(ctx, ws.s.Tab, "return document.activeElement;", nil, false)
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
		return runScript(ctx, ws.s.Tab, script, args, false)
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
	v, err := runScript(ctx, ws.s.Tab, scriptClickPoint, []json.RawMessage{elementArg(r.PathValue("eid"))}, false)
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
	if err := ws.s.Tab.Click(ctx, p.X, p.Y); err != nil {
		return nil, err
	}
	settle(ctx, ws)
	return nil, nil
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
if (e.tagName === 'INPUT' && e.type === 'file') return 'file';
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
	v, err := runScript(ctx, ws.s.Tab, scriptFocus, []json.RawMessage{elementArg(r.PathValue("eid"))}, false)
	if err != nil {
		return nil, err
	}
	switch v {
	case "file":
		return nil, wdErr(ErrUnsupportedOperation, "file uploads are not supported over Tether")
	case "unfocusable":
		return nil, wdErr(ErrElementNotInteractable, "element cannot receive keyboard focus")
	}
	if err := typeText(ctx, ws.s.Tab, *req.Text); err != nil {
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
	v, err := runScript(ctx, ws.s.Tab, scriptClear, []json.RawMessage{elementArg(r.PathValue("eid"))}, false)
	if err != nil {
		return nil, err
	}
	if v != true {
		return nil, wdErr(ErrElementNotInteractable, "element is not editable")
	}
	return nil, nil
}

func (h *Handler) elementScreenshot(ctx context.Context, ws *wdSession, r *http.Request, _ []byte) (any, error) {
	v, err := runScript(ctx, ws.s.Tab, `arguments[0].scrollIntoView({block: 'center', behavior: 'instant'});`+scriptRect,
		[]json.RawMessage{elementArg(r.PathValue("eid"))}, false)
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
