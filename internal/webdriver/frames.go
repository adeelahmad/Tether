package webdriver

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"

	"github.com/adeelahmad/tether/internal/cdp"
)

const scriptIsFrame = `const e = arguments[0]; return e.tagName === 'IFRAME' || e.tagName === 'FRAME';`

const scriptFrameAt = `return Array.from(document.querySelectorAll('iframe, frame'))[arguments[0]] || null;`

// switchFrame implements Switch To Frame: null is the top-level context, a
// number indexes the current document's frames, an element must be an
// <iframe> or <frame>.
func (h *Handler) switchFrame(ctx context.Context, ws *wdSession, _ *http.Request, body []byte) (any, error) {
	var req struct {
		ID json.RawMessage `json:"id"`
	}
	if err := decodeBody(body, &req); err != nil {
		return nil, err
	}
	if len(req.ID) == 0 || string(req.ID) == "null" {
		ws.frames = nil
		return nil, nil
	}
	var ref string
	var n float64
	var el map[string]any
	switch {
	case json.Unmarshal(req.ID, &n) == nil:
		if n < 0 || n > math.MaxUint16 || n != math.Trunc(n) {
			return nil, wdErr(ErrInvalidArgument, "frame index must be an integer from 0 to 65535")
		}
		v, err := runScript(ctx, ws.tab(), scriptFrameAt, []json.RawMessage{req.ID}, false)
		if err != nil {
			return nil, err
		}
		m, _ := v.(map[string]any)
		ref, _ = m[ElementKey].(string)
		if ref == "" {
			return nil, wdErr(ErrNoSuchFrame, "no frame at that index")
		}
	case json.Unmarshal(req.ID, &el) == nil:
		ref, _ = el[ElementKey].(string)
		if ref == "" {
			return nil, wdErr(ErrInvalidArgument, "id must be null, a frame index or a web element")
		}
		v, err := runScript(ctx, ws.tab(), scriptIsFrame, []json.RawMessage{elementArg(ref)}, false)
		if err != nil {
			return nil, err
		}
		if v != true {
			return nil, wdErr(ErrNoSuchFrame, "element is not a frame")
		}
	default:
		return nil, wdErr(ErrInvalidArgument, "id must be null, a frame index or a web element")
	}
	id, _ := parseElementRef(ref)
	path := append(append([]int(nil), ws.frames...), id)
	if _, err := ws.s.Tab.InFrame(path).Evaluate(ctx, "1", false); err != nil {
		if errors.Is(err, cdp.ErrNoSuchFrame) {
			return nil, wdErr(ErrNoSuchFrame, "frame has no document Tether can reach: "+err.Error())
		}
		return nil, err
	}
	ws.frames = path
	return nil, nil
}

func (h *Handler) parentFrame(_ context.Context, ws *wdSession, _ *http.Request, _ []byte) (any, error) {
	if len(ws.frames) > 0 {
		ws.frames = ws.frames[:len(ws.frames)-1]
	}
	return nil, nil
}

// scriptFrameOrigin is where a frame element's content starts, relative
// to its own document's viewport.
const scriptFrameOrigin = `const e = arguments[0], r = e.getBoundingClientRect(), s = getComputedStyle(e);
return {x: r.left + e.clientLeft + parseFloat(s.paddingLeft), y: r.top + e.clientTop + parseFloat(s.paddingTop)};`

// revealFrames scrolls each frame of the current path into view in its
// parent, top down. Scrolling an element inside a cross-site frame does
// not scroll the documents around it.
func revealFrames(ctx context.Context, ws *wdSession) error {
	for i, id := range ws.frames {
		_, err := runScript(ctx, ws.s.Tab.InFrame(ws.frames[:i]),
			`arguments[0].scrollIntoView({block: 'nearest', inline: 'nearest', behavior: 'instant'});`,
			[]json.RawMessage{elementArg(elementRef(id))}, false)
		if err != nil {
			return err
		}
	}
	return nil
}

// frameOffset is the position of the current frame's viewport within the
// top-level viewport, for turning frame coordinates into input events and
// screenshot clips.
func frameOffset(ctx context.Context, ws *wdSession) (x, y float64, err error) {
	for i, id := range ws.frames {
		v, err := runScript(ctx, ws.s.Tab.InFrame(ws.frames[:i]), scriptFrameOrigin, []json.RawMessage{elementArg(elementRef(id))}, false)
		if err != nil {
			return 0, 0, err
		}
		var p struct{ X, Y float64 }
		if err := remarshal(v, &p); err != nil {
			return 0, 0, err
		}
		x, y = x+p.X, y+p.Y
	}
	return x, y, nil
}

// shadowRoot implements Get Element Shadow Root. Closed shadow roots are
// returned too: DevTools can see them even though page script cannot.
func (h *Handler) shadowRoot(ctx context.Context, ws *wdSession, r *http.Request, _ []byte) (any, error) {
	ref := r.PathValue("eid")
	tab := ws.tab()
	if _, err := resolveElement(ctx, tab, ref); err != nil {
		return nil, err
	}
	id, _ := parseElementRef(ref)
	sr, err := tab.ShadowRoot(ctx, id)
	if err != nil {
		return nil, err
	}
	if sr == 0 {
		return nil, wdErr(ErrNoSuchShadowRoot, "element has no shadow root")
	}
	return map[string]any{ShadowKey: shadowRef(sr)}, nil
}

func (h *Handler) findFromShadow(all bool) command {
	return func(ctx context.Context, ws *wdSession, r *http.Request, body []byte) (any, error) {
		var req struct {
			Using string `json:"using"`
		}
		if err := decodeBody(body, &req); err != nil {
			return nil, err
		}
		if req.Using == "xpath" {
			return nil, wdErr(ErrInvalidArgument, "xpath cannot search a shadow root")
		}
		ref := r.PathValue("shid")
		if _, err := resolveShadow(ctx, ws.tab(), ref); err != nil {
			return nil, err
		}
		found, err := h.locate(ctx, ws, body, shadowArg(ref))
		if all {
			return allFound(found, err)
		}
		return firstFound(found, err)
	}
}
