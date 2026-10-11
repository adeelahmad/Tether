package webdriver

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/adeelahmad/tether/internal/cdp"
)

// Scripts run through a wrapper that revives element arguments and
// serialises the result, collecting returned elements so they can be
// mapped to stable element references (backend node IDs).
const wrapperHead = `function(__tArgs, ...__tEls) {
  const __revive = (v) => {
    if (Array.isArray(v)) return v.map(__revive);
    if (v && typeof v === 'object') {
      if (Object.prototype.hasOwnProperty.call(v, '__tetherEl')) {
        const el = __tEls[v.__tetherEl];
        if (!el || !el.isConnected) {
          const e = new Error(v.s ? 'detached shadow root' : 'stale element reference');
          e.name = v.s ? 'DetachedShadowRoot' : 'StaleElementReference';
          throw e;
        }
        return el;
      }
      const o = {};
      for (const k of Object.keys(v)) o[k] = __revive(v[k]);
      return o;
    }
    return v;
  };
  const __args = __revive(__tArgs);
  const __collect = (r) => {
    const els = [];
    const seen = new Set();
    const ser = (v) => {
      if (v === undefined || v === null || typeof v === 'function' || typeof v === 'symbol') return null;
      if (typeof v === 'bigint') return v.toString();
      if (typeof v === 'number') return isFinite(v) ? v : null;
      if (typeof v !== 'object') return v;
      if (typeof Element !== 'undefined' && v instanceof Element) { els.push(v); return {__tetherEl: els.length - 1}; }
      if (typeof ShadowRoot !== 'undefined' && v instanceof ShadowRoot) { els.push(v); return {__tetherEl: els.length - 1, s: 1}; }
      if (typeof Node !== 'undefined' && v instanceof Node) return null;
      if (v === window) return null;
      if (seen.has(v)) throw new Error('cyclic object value');
      seen.add(v);
      try {
        if (Array.isArray(v) || (typeof NodeList !== 'undefined' && v instanceof NodeList) ||
            (typeof HTMLCollection !== 'undefined' && v instanceof HTMLCollection)) return Array.from(v, ser);
        if (typeof v.toJSON === 'function') return ser(v.toJSON());
        const o = {};
        for (const k of Object.keys(v)) o[k] = ser(v[k]);
        return o;
      } finally { seen.delete(v); }
    };
    const value = ser(r);
    return {v: JSON.stringify(value === undefined ? null : value), e: els};
  };
`

func syncWrapper(script string) string {
	return wrapperHead + "  return Promise.resolve((function() {\n" + script + "\n}).apply(null, __args)).then(__collect);\n}"
}

func asyncWrapper(script string) string {
	return wrapperHead + `  return new Promise((resolve, reject) => {
    __args.push(resolve);
    try { (function() {
` + script + `
}).apply(null, __args); } catch (e) { reject(e); }
  }).then(__collect);
}`
}

// encodeArgs replaces element references in args with placeholders and
// resolves each element to a live object handle.
func encodeArgs(ctx context.Context, tab cdp.Tab, args []json.RawMessage) (any, []cdp.Arg, error) {
	var handles []cdp.Arg
	var walk func(v any) (any, error)
	walk = func(v any) (any, error) {
		switch t := v.(type) {
		case []any:
			for i := range t {
				nv, err := walk(t[i])
				if err != nil {
					return nil, err
				}
				t[i] = nv
			}
			return t, nil
		case map[string]any:
			if ref, ok := t[ElementKey].(string); ok {
				oid, err := resolveElement(ctx, tab, ref)
				if err != nil {
					return nil, err
				}
				handles = append(handles, cdp.Arg{ObjectID: oid})
				return map[string]any{"__tetherEl": len(handles) - 1}, nil
			}
			if ref, ok := t[ShadowKey].(string); ok {
				oid, err := resolveShadow(ctx, tab, ref)
				if err != nil {
					return nil, err
				}
				handles = append(handles, cdp.Arg{ObjectID: oid})
				return map[string]any{"__tetherEl": len(handles) - 1, "s": 1}, nil
			}
			for k := range t {
				nv, err := walk(t[k])
				if err != nil {
					return nil, err
				}
				t[k] = nv
			}
			return t, nil
		}
		return v, nil
	}
	out := make([]any, len(args))
	for i, raw := range args {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, nil, wdErr(ErrInvalidArgument, "bad script argument")
		}
		nv, err := walk(v)
		if err != nil {
			return nil, nil, err
		}
		out[i] = nv
	}
	return out, handles, nil
}

// runScript executes a function body with W3C argument and return-value
// semantics.
func runScript(ctx context.Context, tab cdp.Tab, script string, args []json.RawMessage, async bool) (any, error) {
	argv, handles, err := encodeArgs(ctx, tab, args)
	if err != nil {
		return nil, err
	}
	decl := syncWrapper(script)
	if async {
		decl = asyncWrapper(script)
	}
	defer func() {
		for _, h := range handles {
			_ = tab.Release(context.WithoutCancel(ctx), h.ObjectID)
		}
	}()
	callArgs := append([]cdp.Arg{{Value: argv}}, handles...)
	holder, err := tab.CallFunction(ctx, decl, cdp.CallOptions{Args: callArgs, Await: true})
	if err != nil {
		var js *cdp.JSError
		if errors.As(err, &js) && strings.Contains(js.Text, "StaleElementReference") {
			return nil, wdErr(ErrStaleElement, "element is no longer attached to the page")
		}
		if errors.As(err, &js) && strings.Contains(js.Text, "DetachedShadowRoot") {
			return nil, wdErr(ErrDetachedShadowRoot, "shadow root is no longer attached to the page")
		}
		var pe *cdp.ProtocolError
		if errors.As(err, &pe) && strings.Contains(pe.Message, "same JavaScript world") {
			return nil, wdErr(ErrNoSuchElement, "element belongs to another frame; switch to its frame first")
		}
		return nil, err
	}
	defer func() { _ = tab.Release(context.WithoutCancel(ctx), holder.ObjectID) }()
	props, err := tab.Properties(ctx, holder.ObjectID)
	if err != nil {
		return nil, err
	}
	var valueJSON string
	var elsID string
	for _, p := range props {
		switch p.Name {
		case "v":
			_ = json.Unmarshal(p.Value.Value, &valueJSON)
		case "e":
			elsID = p.Value.ObjectID
		}
	}
	var ids []int
	if elsID != "" {
		items, err := cdp.ArrayItems(ctx, tab, elsID)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			id, err := tab.NodeID(ctx, it.ObjectID)
			if err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
	}
	var v any
	if err := json.Unmarshal([]byte(valueJSON), &v); err != nil {
		return nil, wdErr(ErrJavaScript, "result is not serialisable")
	}
	return replacePlaceholders(v, ids), nil
}

// replacePlaceholders swaps the wrapper's {__tetherEl: i[, s: 1]} markers
// for web element or shadow root references.
func replacePlaceholders(v any, ids []int) any {
	switch t := v.(type) {
	case []any:
		for i := range t {
			t[i] = replacePlaceholders(t[i], ids)
		}
		return t
	case map[string]any:
		if idx, ok := t["__tetherEl"].(float64); ok && int(idx) < len(ids) {
			_, shadow := t["s"]
			if len(t) == 2 && shadow {
				return map[string]any{ShadowKey: shadowRef(ids[int(idx)])}
			}
			if len(t) == 1 {
				return map[string]any{ElementKey: elementRef(ids[int(idx)])}
			}
		}
		for k := range t {
			t[k] = replacePlaceholders(t[k], ids)
		}
		return t
	}
	return v
}

// Element references are the node's backend ID, which is stable for the
// life of the document, so finding the same node twice yields the same
// reference.
func elementRef(backendNodeID int) string { return "e" + strconv.Itoa(backendNodeID) }

func parseElementRef(ref string) (int, bool) { return parseRef(ref, "e") }

// Shadow root references work the same way, with their own prefix.
func shadowRef(backendNodeID int) string { return "s" + strconv.Itoa(backendNodeID) }

func parseShadowRef(ref string) (int, bool) { return parseRef(ref, "s") }

func parseRef(ref, prefix string) (int, bool) {
	if !strings.HasPrefix(ref, prefix) {
		return 0, false
	}
	n, err := strconv.Atoi(ref[len(prefix):])
	return n, err == nil && n > 0
}

func resolveShadow(ctx context.Context, tab cdp.Tab, ref string) (string, error) {
	id, ok := parseShadowRef(ref)
	if !ok {
		return "", wdErr(ErrNoSuchShadowRoot, "unknown shadow root reference "+ref)
	}
	oid, err := tab.ResolveNode(ctx, id)
	if err != nil {
		return "", wdErr(ErrDetachedShadowRoot, "shadow root "+ref+" is no longer attached to the page")
	}
	return oid, nil
}

func resolveElement(ctx context.Context, tab cdp.Tab, ref string) (string, error) {
	id, ok := parseElementRef(ref)
	if !ok {
		return "", wdErr(ErrNoSuchElement, "unknown element reference "+ref)
	}
	oid, err := tab.ResolveNode(ctx, id)
	if err != nil {
		return "", wdErr(ErrStaleElement, "element "+ref+" is no longer attached to the page")
	}
	return oid, nil
}

// elementArg is a script argument referring to element ref.
func elementArg(ref string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{ElementKey: ref})
	return b
}

// shadowArg is a script argument referring to shadow root ref.
func shadowArg(ref string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{ShadowKey: ref})
	return b
}

func stringArg(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}
