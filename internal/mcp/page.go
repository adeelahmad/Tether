package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/adeelahmad/tether/internal/cdp"
)

// Element references are "e" + the node's backend ID, the same scheme the
// WebDriver face uses: stable for the life of the document, so the same
// node always gets the same ref, and a ref from an old page goes stale
// instead of silently pointing somewhere else.
func elementRef(backendNodeID int) string { return "e" + strconv.Itoa(backendNodeID) }

func parseRef(ref string) (int, bool) {
	ref = strings.TrimSpace(ref)
	if !strings.HasPrefix(ref, "e") {
		return 0, false
	}
	n, err := strconv.Atoi(ref[1:])
	return n, err == nil && n > 0
}

const querySelectorJS = `function(sel) { return document.querySelector(sel); }`

// element resolves a ref or CSS selector to a live object handle. The
// caller releases it.
func element(ctx context.Context, tab cdp.Tab, ref, selector string) (string, error) {
	switch {
	case ref != "":
		id, ok := parseRef(ref)
		if !ok {
			return "", badArgs("ref %q is not an element ref from browser_snapshot (they look like e42)", ref)
		}
		oid, err := tab.ResolveNode(ctx, id)
		if err != nil || oid == "" {
			return "", &toolError{code: codeStaleRef, msg: "element " + ref + " is no longer on the page; call browser_snapshot for fresh refs"}
		}
		return oid, nil
	case selector != "":
		r, err := tab.CallFunction(ctx, querySelectorJS, cdp.CallOptions{Args: []cdp.Arg{{Value: selector}}})
		if err != nil {
			var js *cdp.JSError
			if errors.As(err, &js) {
				return "", badArgs("invalid CSS selector %q", selector)
			}
			return "", err
		}
		if r.ObjectID == "" {
			return "", &toolError{code: codeNoElement, msg: "no element matches selector " + selector}
		}
		return r.ObjectID, nil
	}
	return "", badArgs("pass ref (from browser_snapshot) or selector")
}

// callOn runs fn(element, args...) by value and decodes the result.
func callOn(ctx context.Context, tab cdp.Tab, oid, fn string, out any, args ...any) error {
	callArgs := []cdp.Arg{{ObjectID: oid}}
	for _, a := range args {
		callArgs = append(callArgs, cdp.Arg{Value: a})
	}
	r, err := tab.CallFunction(ctx, fn, cdp.CallOptions{Args: callArgs, ByValue: true, Await: true})
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(r.Value, out); err != nil {
		return fmt.Errorf("decoding page result: %w", err)
	}
	return nil
}

// eval runs fn(args...) in the page by value and decodes the result.
func eval(ctx context.Context, tab cdp.Tab, fn string, out any, args ...any) error {
	callArgs := make([]cdp.Arg, 0, len(args))
	for _, a := range args {
		callArgs = append(callArgs, cdp.Arg{Value: a})
	}
	r, err := tab.CallFunction(ctx, fn, cdp.CallOptions{Args: callArgs, ByValue: true, Await: true})
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(r.Value, out); err != nil {
		return fmt.Errorf("decoding page result: %w", err)
	}
	return nil
}

func release(ctx context.Context, tab cdp.Tab, oid string) {
	if oid != "" {
		_ = tab.Release(context.WithoutCancel(ctx), oid)
	}
}

// snapshotJS collects the visible interactive elements and the page text.
// It returns {v: JSON, e: [elements]} so the elements can be mapped to
// backend node IDs. Password values are never read back.
const snapshotJS = `function(maxEls, maxText) {
  const SEL = 'a[href],button,input:not([type=hidden]),select,textarea,summary,' +
    '[role=button],[role=link],[role=checkbox],[role=radio],[role=switch],[role=tab],[role=menuitem],' +
    '[role=menuitemcheckbox],[role=menuitemradio],[role=option],[role=combobox],[role=textbox],' +
    '[role=searchbox],[role=slider],[role=spinbutton],[contenteditable=""],[contenteditable=true],[onclick]';
  const clean = (s, n) => String(s || '').replace(/\s+/g, ' ').trim().slice(0, n || 120);
  const visible = (e) => {
    const r = e.getBoundingClientRect();
    if (r.width === 0 && r.height === 0) return false;
    const s = getComputedStyle(e);
    return s.visibility !== 'hidden' && s.visibility !== 'collapse' && s.display !== 'none' && s.opacity !== '0';
  };
  const role = (e) => {
    const r = e.getAttribute('role');
    if (r) return r;
    const t = e.tagName.toLowerCase();
    switch (t) {
      case 'a': return 'link';
      case 'button': case 'summary': return 'button';
      case 'select': return e.multiple ? 'listbox' : 'combobox';
      case 'textarea': return 'textbox';
      case 'input': {
        const ty = (e.type || 'text').toLowerCase();
        if (['button', 'submit', 'reset', 'image'].includes(ty)) return 'button';
        if (ty === 'checkbox' || ty === 'radio') return ty;
        if (ty === 'range') return 'slider';
        if (ty === 'number') return 'spinbutton';
        if (ty === 'search') return 'searchbox';
        return 'textbox';
      }
    }
    return e.isContentEditable ? 'textbox' : t;
  };
  const name = (e) => {
    const al = e.getAttribute('aria-label');
    if (al && clean(al)) return clean(al);
    const lb = e.getAttribute('aria-labelledby');
    if (lb) {
      const t = lb.split(/\s+/).map(id => document.getElementById(id)).filter(Boolean)
        .map(n => n.innerText || n.textContent).join(' ');
      if (clean(t)) return clean(t);
    }
    if (e.labels && e.labels.length) {
      const t = Array.from(e.labels).map(l => l.innerText || l.textContent).join(' ');
      if (clean(t)) return clean(t);
    }
    if (e.tagName === 'INPUT' && ['button', 'submit', 'reset'].includes(e.type)) return clean(e.value);
    if (e.tagName !== 'SELECT' && e.tagName !== 'TEXTAREA' && e.tagName !== 'INPUT') {
      const tx = clean(e.innerText || e.textContent);
      if (tx) return tx;
    }
    const img = e.querySelector && e.querySelector('img[alt]');
    return clean(e.getAttribute('placeholder') || e.getAttribute('title') || e.getAttribute('alt') ||
      (img && img.alt) || e.getAttribute('name') || '');
  };
  const els = [], items = [];
  let total = 0;
  for (const e of document.querySelectorAll(SEL)) {
    if (!visible(e)) continue;
    total++;
    if (els.length >= maxEls) continue;
    const it = {role: role(e), name: name(e)};
    const t = e.tagName;
    if (t === 'A') it.href = clean(e.getAttribute('href'), 200);
    if (t === 'INPUT' || t === 'TEXTAREA') {
      const ty = (e.type || '').toLowerCase();
      if (ty === 'password') it.value = e.value ? '(hidden)' : '';
      else if (ty === 'checkbox' || ty === 'radio') it.checked = e.checked;
      else it.value = clean(e.value, 200);
      if (e.placeholder) it.placeholder = clean(e.placeholder);
    }
    if (t === 'SELECT') {
      it.value = Array.from(e.selectedOptions).map(o => clean(o.label || o.text, 60)).join(', ');
      it.options = Array.from(e.options).slice(0, 40).map(o => clean(o.label || o.text, 60));
    }
    const ac = e.getAttribute('aria-checked');
    if (ac) it.checked = ac === 'true';
    const ax = e.getAttribute('aria-expanded');
    if (ax) it.expanded = ax === 'true';
    if (e.disabled || e.getAttribute('aria-disabled') === 'true') it.disabled = true;
    const r = e.getBoundingClientRect();
    if (!(r.bottom > 0 && r.right > 0 && r.top < innerHeight && r.left < innerWidth)) it.offscreen = true;
    els.push(e);
    items.push(it);
  }
  let text = '';
  if (maxText > 0 && document.body) text = (document.body.innerText || '').replace(/\n{3,}/g, '\n\n').trim();
  const textTotal = text.length;
  return {v: JSON.stringify({
    title: document.title, url: location.href, items, total,
    text: text.slice(0, maxText), textTotal,
    scrollY: Math.round(scrollY), scrollHeight: document.documentElement ? document.documentElement.scrollHeight : 0,
    viewport: innerHeight,
  }), e: els};
}`

type snapshotItem struct {
	Ref         string   `json:"-"`
	Role        string   `json:"role"`
	Name        string   `json:"name"`
	Href        string   `json:"href"`
	Value       *string  `json:"value"`
	Placeholder string   `json:"placeholder"`
	Options     []string `json:"options"`
	Checked     *bool    `json:"checked"`
	Expanded    *bool    `json:"expanded"`
	Disabled    bool     `json:"disabled"`
	Offscreen   bool     `json:"offscreen"`
}

type snapshot struct {
	Title        string          `json:"title"`
	URL          string          `json:"url"`
	Items        []*snapshotItem `json:"items"`
	Total        int             `json:"total"`
	Text         string          `json:"text"`
	TextTotal    int             `json:"textTotal"`
	ScrollY      int             `json:"scrollY"`
	ScrollHeight int             `json:"scrollHeight"`
	Viewport     int             `json:"viewport"`
}

func takeSnapshot(ctx context.Context, tab cdp.Tab, maxEls, maxText int) (*snapshot, error) {
	holder, err := tab.CallFunction(ctx, snapshotJS, cdp.CallOptions{Args: []cdp.Arg{{Value: maxEls}, {Value: maxText}}})
	if err != nil {
		return nil, err
	}
	defer release(ctx, tab, holder.ObjectID)
	props, err := tab.Properties(ctx, holder.ObjectID)
	if err != nil {
		return nil, err
	}
	var raw, elsID string
	for _, p := range props {
		switch p.Name {
		case "v":
			_ = json.Unmarshal(p.Value.Value, &raw)
		case "e":
			elsID = p.Value.ObjectID
		}
	}
	var snap snapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return nil, fmt.Errorf("decoding snapshot: %w", err)
	}
	if elsID != "" {
		els, err := cdp.ArrayItems(ctx, tab, elsID)
		if err != nil {
			return nil, err
		}
		for i, el := range els {
			if i >= len(snap.Items) {
				break
			}
			id, err := tab.NodeID(ctx, el.ObjectID)
			if err != nil {
				return nil, err
			}
			snap.Items[i].Ref = elementRef(id)
		}
	}
	return &snap, nil
}

// format renders a snapshot as compact text, which models read far more
// reliably (and cheaply) than nested JSON.
func (s *snapshot) format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Page: %s\nURL: %s\n", s.Title, s.URL)
	if s.ScrollHeight > 0 && s.Viewport > 0 {
		fmt.Fprintf(&b, "Scroll: %dpx of %dpx (viewport %dpx)\n", s.ScrollY, s.ScrollHeight, s.Viewport)
	}
	b.WriteString("\nInteractive elements")
	if s.Total > len(s.Items) {
		fmt.Fprintf(&b, " (first %d of %d)", len(s.Items), s.Total)
	}
	b.WriteString(":\n")
	if len(s.Items) == 0 {
		b.WriteString("(none)\n")
	}
	for _, it := range s.Items {
		if it.Ref == "" {
			continue
		}
		fmt.Fprintf(&b, "[%s] %s", it.Ref, it.Role)
		if it.Name != "" {
			fmt.Fprintf(&b, " %q", it.Name)
		}
		if it.Href != "" {
			fmt.Fprintf(&b, " -> %s", it.Href)
		}
		if it.Value != nil && *it.Value != "" {
			fmt.Fprintf(&b, " value=%q", *it.Value)
		}
		if it.Placeholder != "" && (it.Value == nil || *it.Value == "") {
			fmt.Fprintf(&b, " placeholder=%q", it.Placeholder)
		}
		if len(it.Options) > 0 {
			fmt.Fprintf(&b, " options=[%s]", strings.Join(it.Options, " | "))
		}
		if it.Checked != nil {
			fmt.Fprintf(&b, " checked=%t", *it.Checked)
		}
		if it.Expanded != nil {
			fmt.Fprintf(&b, " expanded=%t", *it.Expanded)
		}
		if it.Disabled {
			b.WriteString(" disabled")
		}
		if it.Offscreen {
			b.WriteString(" (offscreen)")
		}
		b.WriteByte('\n')
	}
	if s.Text != "" {
		b.WriteString("\nText")
		if s.TextTotal > len(s.Text) {
			fmt.Fprintf(&b, " (first %d of %d characters; raise max_text for more)", len(s.Text), s.TextTotal)
		}
		b.WriteString(":\n")
		b.WriteString(s.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

const clickPointJS = `function(e) {
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
  if (r.width === 0 || r.height === 0) return {visible: false};
  const x = r.left + r.width / 2, y = r.top + r.height / 2;
  const hit = document.elementFromPoint(x, y);
  return {visible: true, x, y, hit: !!hit && (hit === e || e.contains(hit) || hit.contains(e)),
          blocker: hit ? hit.tagName.toLowerCase() + (hit.id ? '#' + hit.id : '') : ''};
}`

const jsClickJS = `function(e) { e.scrollIntoView({block: 'center', behavior: 'instant'}); e.click(); return true; }`

type clickPoint struct {
	Handled bool    `json:"handled"`
	Visible bool    `json:"visible"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	Hit     bool    `json:"hit"`
	Blocker string  `json:"blocker"`
}

const focusJS = `function(e, clear) {
  if (e.tagName === 'INPUT' && e.type === 'file') return 'file';
  if (e.disabled || e.readOnly) return 'readonly';
  e.scrollIntoView({block: 'center', inline: 'center', behavior: 'instant'});
  e.focus();
  if (clear) {
    if (e.isContentEditable) {
      const r = document.createRange(); r.selectNodeContents(e);
      const s = getSelection(); s.removeAllRanges(); s.addRange(r);
      document.execCommand('delete');
    } else if ('value' in e) {
      const proto = Object.getPrototypeOf(e);
      const desc = Object.getOwnPropertyDescriptor(proto, 'value');
      if (desc && desc.set) desc.set.call(e, ''); else e.value = '';
      e.dispatchEvent(new Event('input', {bubbles: true}));
    }
  }
  if (typeof e.setSelectionRange === 'function' && typeof e.value === 'string') {
    try { e.setSelectionRange(e.value.length, e.value.length); } catch (_) {}
  }
  const a = document.activeElement;
  return (a === e || e.contains(a)) ? 'ok' : 'unfocusable';
}`

const selectOptionJS = `function(e, values) {
  if (e.tagName !== 'SELECT') return {error: 'not a <select> element'};
  const want = values.map(v => String(v).trim());
  const picked = [], missing = [];
  const opts = Array.from(e.options);
  for (const w of want) {
    const o = opts.find(o => o.value === w) || opts.find(o => (o.label || o.text).trim() === w) ||
      opts.find(o => (o.label || o.text).trim().toLowerCase() === w.toLowerCase());
    if (o) picked.push(o); else missing.push(w);
  }
  if (missing.length) return {missing, available: opts.slice(0, 40).map(o => (o.label || o.text).trim())};
  if (!e.multiple && picked.length > 1) return {error: 'this <select> accepts a single value'};
  if (e.multiple) for (const o of opts) o.selected = picked.includes(o);
  else picked[0].selected = true;
  e.dispatchEvent(new Event('input', {bubbles: true}));
  e.dispatchEvent(new Event('change', {bubbles: true}));
  return {selected: picked.map(o => (o.label || o.text).trim())};
}`

const scrollJS = `function(dx, dy, e) {
  if (e) e.scrollIntoView({block: 'center', inline: 'center', behavior: 'instant'});
  else {
    if (dy === 'page') dy = Math.round(innerHeight * 0.8);
    if (dy === '-page') dy = -Math.round(innerHeight * 0.8);
    if (dx === 'page') dx = Math.round(innerWidth * 0.8);
    if (dx === '-page') dx = -Math.round(innerWidth * 0.8);
    window.scrollBy({left: dx, top: dy, behavior: 'instant'});
  }
  return {x: Math.round(scrollX), y: Math.round(scrollY), height: document.documentElement.scrollHeight, viewport: innerHeight};
}`

const scrollIntoViewJS = `function(e) {
  e.scrollIntoView({block: 'center', inline: 'center', behavior: 'instant'});
  return {x: Math.round(scrollX), y: Math.round(scrollY), height: document.documentElement.scrollHeight, viewport: innerHeight};
}`

const waitJS = `function(text, selector, gone) {
  let found = true;
  if (selector) {
    const e = document.querySelector(selector);
    found = !!e && e.getClientRects().length > 0;
  }
  if (found && text) found = !!document.body && document.body.innerText.includes(text);
  return gone ? !found : found;
}`

const elementRectJS = `function(e) {
  e.scrollIntoView({block: 'center', inline: 'center', behavior: 'instant'});
  const r = e.getBoundingClientRect();
  return {x: r.left + scrollX, y: r.top + scrollY, width: r.width, height: r.height};
}`

const pageSizeJS = `function() {
  const d = document.documentElement;
  return {x: 0, y: 0, width: Math.max(d.scrollWidth, innerWidth), height: Math.max(d.scrollHeight, innerHeight)};
}`

// settle gives an action-triggered navigation a moment to start and, if
// one did, waits for the new document to finish loading.
func settle(ctx context.Context, tab cdp.Tab, limit time.Duration) {
	deadline := time.Now().Add(limit)
	select {
	case <-ctx.Done():
		return
	case <-time.After(100 * time.Millisecond):
	}
	for time.Now().Before(deadline) {
		v, err := tab.Evaluate(ctx, "document.readyState", false)
		var state string
		if err == nil && json.Unmarshal(v, &state) == nil && state == "complete" {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}
