package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"
)

// Screen is implemented by tabs a person can watch and drive: a live
// screencast, text input and dialog notifications, on top of the mouse,
// key and dialog calls every Tab has. It is separate from Tab so faces
// that never need video (proxy, WebDriver) do not depend on it; the remote
// UI type-asserts a Tab to Screen.
type Screen interface {
	// Screencast streams frames of the tab's viewport to fn until ctx ends.
	// fn runs on one goroutine and should not block for long: the next
	// frame is only requested once it returns.
	Screencast(ctx context.Context, opts ScreencastOptions, fn func(Frame)) error
	// InsertText types text into the focused element as one input event,
	// the way a paste or an IME commit does.
	InsertText(ctx context.Context, text string) error
	// BringToFront activates the tab in its window. Chrome paints
	// background tabs lazily, so this is how a person gets smooth video of
	// a tab that is not the one showing on the machine running Chrome.
	BringToFront(ctx context.Context) error
	// WatchDialogs calls fn whenever the page opens a JavaScript dialog
	// (alert, confirm, prompt, beforeunload) and, with Closed set, when one
	// closes. fn runs on the event goroutine and must not block. Dialogs
	// are drawn by Chrome, not the page, so they never appear in frames.
	WatchDialogs(fn func(Dialog)) (cancel func())
}

// ScreencastOptions configure Screencast.
type ScreencastOptions struct {
	// Quality is the JPEG quality, 1–100 (default 70).
	Quality int
	// MaxWidth and MaxHeight bound the frame size in device pixels
	// (default 1920×1080). Chrome scales frames down to fit.
	MaxWidth, MaxHeight int
	// Keyframe is how often a still frame is captured when the page is
	// not repainting (default 1s). Chrome's screencast only emits frames on
	// paint, and paints background tabs rarely or never, so periodic
	// captures keep a quiet or hidden tab visible.
	Keyframe time.Duration
	// Refresh, when signalled, says someone just interacted with the tab:
	// for the next few seconds quiet periods are filled with captures four
	// times a second, so a background tab still answers typing and clicks
	// promptly.
	Refresh <-chan struct{}
}

const (
	activeFor      = 3 * time.Second
	activeKeyframe = 250 * time.Millisecond
)

func (o ScreencastOptions) withDefaults() ScreencastOptions {
	if o.Quality <= 0 || o.Quality > 100 {
		o.Quality = 70
	}
	if o.MaxWidth <= 0 {
		o.MaxWidth = 1920
	}
	if o.MaxHeight <= 0 {
		o.MaxHeight = 1080
	}
	if o.Keyframe <= 0 {
		o.Keyframe = time.Second
	}
	return o
}

// Frame is one JPEG image of the viewport.
type Frame struct {
	Data []byte    `json:"-"`
	Meta FrameMeta `json:"meta"`
}

// FrameMeta describes how a frame maps onto the page. Input coordinates
// are CSS pixels in a DeviceWidth × DeviceHeight viewport.
type FrameMeta struct {
	DeviceWidth     float64 `json:"deviceWidth"`
	DeviceHeight    float64 `json:"deviceHeight"`
	OffsetTop       float64 `json:"offsetTop"`
	PageScaleFactor float64 `json:"pageScaleFactor"`
	ScrollOffsetX   float64 `json:"scrollOffsetX"`
	ScrollOffsetY   float64 `json:"scrollOffsetY"`
	Timestamp       float64 `json:"timestamp,omitempty"`
}

// Validate rejects mouse events Chrome would refuse or that make no
// sense; faces that take raw events from clients call it.
func (e MouseEvent) Validate() error {
	switch e.Type {
	case "mousePressed", "mouseReleased", "mouseMoved", "mouseWheel":
	default:
		return errors.New("mouse type must be mousePressed, mouseReleased, mouseMoved or mouseWheel")
	}
	switch e.Button {
	case "", "none", "left", "middle", "right", "back", "forward":
	default:
		return errors.New("invalid mouse button")
	}
	for _, f := range []float64{e.X, e.Y, e.DeltaX, e.DeltaY} {
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return errors.New("mouse coordinates must be finite")
		}
	}
	if e.ClickCount < 0 || e.ClickCount > 3 {
		return errors.New("clickCount must be 0–3")
	}
	return nil
}

var _ Screen = (*chromeTab)(nil)

// Screencast implements Screen with Page.startScreencast, topped up with
// Page.captureScreenshot keyframes whenever the screencast goes quiet.
func (t *chromeTab) Screencast(ctx context.Context, opts ScreencastOptions, fn func(Frame)) error {
	opts = opts.withDefaults()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type rawFrame struct {
		Data      []byte    `json:"data"` // base64 in JSON
		Metadata  FrameMeta `json:"metadata"`
		SessionID int       `json:"sessionId"`
	}
	// The dispatcher goroutine must never block, so it only parks the
	// newest frame here; older unread frames are simply superseded.
	latest := make(chan rawFrame, 1)
	// A main-frame navigation can swap the page's renderer, which ends the
	// screencast; restart says to ask for it again.
	restart := make(chan struct{}, 1)
	unsub := t.b.c.subscribe(t.session, func(e Event) {
		if e.Method == "Page.frameNavigated" {
			var p struct {
				Frame struct {
					ParentID string `json:"parentId"`
				} `json:"frame"`
			}
			if json.Unmarshal(e.Params, &p) == nil && p.Frame.ParentID == "" {
				select {
				case restart <- struct{}{}:
				default:
				}
			}
			return
		}
		if e.Method != "Page.screencastFrame" {
			return
		}
		var f rawFrame
		if json.Unmarshal(e.Params, &f) != nil {
			return
		}
		for {
			select {
			case latest <- f:
				return
			default:
			}
			select {
			case <-latest:
			default:
			}
		}
	})
	defer unsub()

	// Starting can fail transiently, e.g. "Not attached to an active page"
	// while a navigation commits. Keyframes keep the picture coming and the
	// start is retried on every keyframe tick until it takes.
	started := false
	start := func() {
		err := t.call(ctx, "Page.startScreencast", map[string]any{
			"format":    "jpeg",
			"quality":   opts.Quality,
			"maxWidth":  opts.MaxWidth,
			"maxHeight": opts.MaxHeight,
		}, nil)
		started = err == nil
	}
	start()
	if !started {
		if _, err := t.Info(ctx); errors.Is(err, ErrNoSuchTab) {
			return ErrNoSuchTab
		}
	}
	defer func() {
		sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		_ = t.call(sctx, "Page.stopScreencast", nil, nil)
		scancel()
	}()

	// Every call to fn happens on this goroutine, so keyframes and
	// screencast frames never interleave.
	emit := fn
	lastFrame := time.Time{}
	activeUntil := time.Time{}
	keyframe := time.NewTicker(activeKeyframe / 2)
	defer keyframe.Stop()
	// An immediate keyframe gives a new viewer a picture straight away,
	// even of a page that will never repaint.
	if f, err := t.captureFrame(ctx, opts); err == nil {
		emit(f)
		lastFrame = time.Now()
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.b.c.done:
			return ErrDisconnected
		case f := <-latest:
			emit(Frame{Data: f.Data, Meta: f.Metadata})
			lastFrame = time.Now()
			// Ack only after the frame is handed off: that is the
			// backpressure that keeps Chrome from outrunning the viewer.
			_ = t.call(ctx, "Page.screencastFrameAck", map[string]any{"sessionId": f.SessionID}, nil)
		case <-opts.Refresh:
			activeUntil = time.Now().Add(activeFor)
		case <-restart:
			start()
		case <-keyframe.C:
			if !started {
				start()
			}
			every := opts.Keyframe
			if time.Now().Before(activeUntil) {
				every = activeKeyframe
			}
			if time.Since(lastFrame) < every {
				continue
			}
			f, err := t.captureFrame(ctx, opts)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				if _, ierr := t.Info(ctx); errors.Is(ierr, ErrNoSuchTab) {
					return ErrNoSuchTab
				}
				continue
			}
			emit(f)
			lastFrame = time.Now()
		}
	}
}

// captureFrame takes one JPEG screenshot of the viewport along with the
// layout metrics a screencast frame would carry.
func (t *chromeTab) captureFrame(ctx context.Context, opts ScreencastOptions) (Frame, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var lm struct {
		Viewport struct {
			PageX  float64 `json:"pageX"`
			PageY  float64 `json:"pageY"`
			Width  float64 `json:"clientWidth"`
			Height float64 `json:"clientHeight"`
			Scale  float64 `json:"scale"`
		} `json:"cssVisualViewport"`
	}
	if err := t.call(ctx, "Page.getLayoutMetrics", nil, &lm); err != nil {
		return Frame{}, err
	}
	params := map[string]any{"format": "jpeg", "quality": opts.Quality, "optimizeForSpeed": true}
	if w, h := lm.Viewport.Width, lm.Viewport.Height; w > 0 && h > 0 {
		// Clip to the visible viewport, scaled down to fit the size bounds.
		scale := math.Min(1, math.Min(float64(opts.MaxWidth)/w, float64(opts.MaxHeight)/h))
		params["clip"] = map[string]any{
			"x": lm.Viewport.PageX, "y": lm.Viewport.PageY, "width": w, "height": h, "scale": scale,
		}
	}
	var res struct {
		Data []byte `json:"data"`
	}
	if err := t.call(ctx, "Page.captureScreenshot", params, &res); err != nil {
		return Frame{}, err
	}
	scale := lm.Viewport.Scale
	if scale == 0 {
		scale = 1
	}
	return Frame{Data: res.Data, Meta: FrameMeta{
		DeviceWidth:     lm.Viewport.Width,
		DeviceHeight:    lm.Viewport.Height,
		PageScaleFactor: scale,
		ScrollOffsetX:   lm.Viewport.PageX,
		ScrollOffsetY:   lm.Viewport.PageY,
		Timestamp:       float64(time.Now().UnixMilli()) / 1000,
	}}, nil
}

// InsertText implements Screen.
func (t *chromeTab) InsertText(ctx context.Context, text string) error {
	return t.call(ctx, "Input.insertText", map[string]any{"text": text}, nil)
}

// BringToFront implements Screen.
func (t *chromeTab) BringToFront(ctx context.Context) error {
	return t.call(ctx, "Page.bringToFront", nil, nil)
}

// WatchDialogs implements Screen.
func (t *chromeTab) WatchDialogs(fn func(Dialog)) (cancel func()) {
	return t.b.c.subscribe(t.session, func(e Event) {
		switch e.Method {
		case "Page.javascriptDialogOpening":
			var d Dialog
			if json.Unmarshal(e.Params, &d) == nil {
				fn(d)
			}
		case "Page.javascriptDialogClosed":
			fn(Dialog{Closed: true})
		}
	})
}
