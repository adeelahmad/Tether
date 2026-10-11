package remote

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/adeelahmad/tether/internal/cdp"
	"github.com/adeelahmad/tether/internal/core"
	"github.com/adeelahmad/tether/internal/logx"
	"github.com/adeelahmad/tether/internal/session"
)

// Wire protocol, one websocket per watched tab (GET /remote/ws/{id}).
//
// Server → client:
//   - binary: one video frame, laid out as a 4-byte big-endian length N,
//     N bytes of JSON cdp.FrameMeta, then the JPEG.
//   - text:   a JSON serverMsg (tab info, errors, clipboard, end of tab).
//
// Client → server: text JSON clientMsg, one input or command per message.

// clientMsg is everything the browser UI can send. T selects the kind.
type clientMsg struct {
	T string `json:"t"`

	// mouse / wheel
	Type       string  `json:"type,omitempty"`
	X          float64 `json:"x,omitempty"`
	Y          float64 `json:"y,omitempty"`
	Button     string  `json:"button,omitempty"`
	Buttons    int     `json:"buttons,omitempty"`
	ClickCount int     `json:"clickCount,omitempty"`
	DeltaX     float64 `json:"deltaX,omitempty"`
	DeltaY     float64 `json:"deltaY,omitempty"`
	Modifiers  int     `json:"modifiers,omitempty"`

	// key
	Key      string   `json:"key,omitempty"`
	Code     string   `json:"code,omitempty"`
	KeyCode  int      `json:"keyCode,omitempty"`
	Commands []string `json:"commands,omitempty"`

	// text (paste) / key text
	Text string `json:"text,omitempty"`

	// nav
	URL string `json:"url,omitempty"`

	// dialog
	Accept bool `json:"accept,omitempty"`
}

// serverMsg is every text message the server sends.
type serverMsg struct {
	T       string `json:"t"`
	URL     string `json:"url,omitempty"`
	Title   string `json:"title,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Text    string `json:"text,omitempty"`
	Busy    *bool  `json:"busy,omitempty"`

	Dialog *cdp.Dialog `json:"dialog,omitempty"`
}

const (
	maxMessage   = 1 << 20 // a large paste
	maxKeyText   = 16
	infoEvery    = time.Second
	touchEvery   = 15 * time.Second
	writeTimeout = 10 * time.Second
)

// editing commands a key event may carry (see cdp.KeyEvent.Commands).
var allowedCommands = map[string]bool{
	"selectAll": true, "copy": true, "cut": true, "paste": true, "undo": true, "redo": true,
}

func (u *UI) serveWS(w http.ResponseWriter, r *http.Request) {
	s, err := u.core.Sessions.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		u.fail(w, err)
		return
	}
	scr, ok := s.Tab.(cdp.Screen)
	if !ok {
		u.fail(w, fmt.Errorf("%w: this browser backend cannot stream tabs", errNotImplemented))
		return
	}
	// Accept checks that Origin matches Host, so other sites cannot open a
	// socket to a tab even if they could reach the listener.
	opts := &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled}
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
		// Behind a reverse proxy such as `tailscale serve`, the page's
		// origin is the forwarded host. Browsers cannot forge this header,
		// so allowing it does not let another site in.
		opts.OriginPatterns = []string{fh}
	}
	c, err := websocket.Accept(w, r, opts)
	if err != nil {
		return // Accept has written the response
	}
	c.SetReadLimit(maxMessage)
	u.log.Info("remote viewer connected", "tab", logx.Clean(s.ID))
	defer u.log.Info("remote viewer disconnected", "tab", logx.Clean(s.ID))

	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()
	go func() { // end the viewer on server shutdown or client disconnect
		select {
		case <-r.Context().Done():
		case <-u.done:
		case <-ctx.Done():
		}
		cancel()
	}()

	v := u.hub.join(s.ID, scr)
	defer u.hub.leave(v)

	vc := &viewerConn{u: u, c: c, s: s, scr: scr, v: v, out: make(chan serverMsg, 16)}
	unwatch := scr.WatchDialogs(func(d cdp.Dialog) { vc.send(serverMsg{T: "dialog", Dialog: &d}) })
	defer unwatch()
	go vc.readLoop(ctx, cancel)
	err = vc.writeLoop(ctx, v)
	switch {
	case errors.Is(err, errEnded), errors.Is(err, cdp.ErrNoSuchTab):
		_ = c.Close(websocket.StatusNormalClosure, "tab closed")
	case errors.Is(err, cdp.ErrDisconnected):
		_ = c.Close(websocket.StatusGoingAway, "browser disconnected")
	default:
		_ = c.Close(websocket.StatusNormalClosure, "")
	}
}

type viewerConn struct {
	u   *UI
	c   *websocket.Conn
	s   *session.Session
	scr cdp.Screen
	v   *viewer
	out chan serverMsg
}

// send queues a text message without ever blocking the caller.
func (vc *viewerConn) send(m serverMsg) {
	select {
	case vc.out <- m:
	default:
	}
}

// writeLoop is the only writer on the socket: frames, info and replies.
func (vc *viewerConn) writeLoop(ctx context.Context, v *viewer) error {
	info := time.NewTicker(infoEvery)
	defer info.Stop()
	touch := time.NewTicker(touchEvery)
	defer touch.Stop()
	var lastInfo cdp.TabInfo
	sendInfo := func() error {
		ictx, cancel := context.WithTimeout(ctx, 2*time.Second)
		ti, err := vc.s.Tab.Info(ictx)
		cancel()
		if err == nil && ti != lastInfo { // a failed lookup just waits for the next tick
			lastInfo = ti
			return vc.writeJSON(ctx, serverMsg{T: "info", URL: ti.URL, Title: ti.Title})
		}
		return nil
	}
	if err := sendInfo(); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-v.ended:
			err := v.reason()
			if err == nil {
				err = errEnded
			}
			code := "tab_gone"
			if errors.Is(err, cdp.ErrDisconnected) {
				code = core.CodeBrowserGone
			}
			_ = vc.writeJSON(ctx, serverMsg{T: "ended", Code: code, Message: err.Error()})
			return err
		case f := <-v.frames:
			if err := vc.writeFrame(ctx, f); err != nil {
				return err
			}
		case m := <-vc.out:
			if err := vc.writeJSON(ctx, m); err != nil {
				return err
			}
		case <-info.C:
			if err := sendInfo(); err != nil {
				return err
			}
		case <-touch.C:
			vc.s.Touch() // a tab someone is watching is not idle
		}
	}
}

func (vc *viewerConn) writeJSON(ctx context.Context, m serverMsg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return vc.c.Write(ctx, websocket.MessageText, b)
}

func (vc *viewerConn) writeFrame(ctx context.Context, f cdp.Frame) error {
	b, err := encodeFrame(f)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return vc.c.Write(ctx, websocket.MessageBinary, b)
}

// encodeFrame lays a frame out for the wire (see the protocol comment).
func encodeFrame(f cdp.Frame) ([]byte, error) {
	meta, err := json.Marshal(f.Meta)
	if err != nil {
		return nil, err
	}
	b := make([]byte, 4, 4+len(meta)+len(f.Data))
	binary.BigEndian.PutUint32(b, uint32(len(meta))) //nolint:gosec // metadata is a few hundred bytes
	b = append(b, meta...)
	return append(b, f.Data...), nil
}

func (vc *viewerConn) readLoop(ctx context.Context, cancel context.CancelFunc) {
	defer cancel()
	for {
		typ, b, err := vc.c.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var m clientMsg
		if err := json.Unmarshal(b, &m); err != nil {
			vc.send(serverMsg{T: "error", Code: core.CodeBadRequest, Message: "invalid message"})
			continue
		}
		if err := vc.handle(ctx, m); err != nil && ctx.Err() == nil {
			vc.send(errorMsg(err))
		}
	}
}

// handle runs one client message. Input goes straight to the tab, without
// the session lock: a person must be able to click and type while a slow
// navigation or a script holds the tab. Navigation commands take the lock
// in the background so input keeps flowing meanwhile.
func (vc *viewerConn) handle(ctx context.Context, m clientMsg) error {
	vc.s.Touch()
	vc.v.poke()
	ictx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	switch m.T {
	case "mouse", "wheel":
		e := cdp.MouseEvent{
			Type: m.Type, X: m.X, Y: m.Y, Button: m.Button, Buttons: m.Buttons,
			ClickCount: m.ClickCount, DeltaX: m.DeltaX, DeltaY: m.DeltaY, Modifiers: m.Modifiers,
		}
		if m.T == "wheel" {
			e.Type = "mouseWheel"
		}
		if err := e.Validate(); err != nil {
			return errors.Join(core.ErrBadRequest, err)
		}
		return vc.scr.Mouse(ictx, e)
	case "key":
		k, err := keyEvent(m)
		if err != nil {
			return err
		}
		return vc.s.Tab.Key(ictx, k)
	case "text":
		if !utf8.ValidString(m.Text) {
			return fmt.Errorf("%w: text must be UTF-8", core.ErrBadRequest)
		}
		return vc.scr.InsertText(ictx, m.Text)
	case "copy":
		v, err := vc.s.Tab.Evaluate(ictx, selectionJS, false)
		if err != nil {
			return err
		}
		var text string
		_ = json.Unmarshal(v, &text)
		vc.send(serverMsg{T: "clipboard", Text: text})
		return nil
	case "front":
		return vc.scr.BringToFront(ictx)
	case "dialog":
		return vc.scr.HandleDialog(ictx, m.Accept, m.Text)
	case "nav", "back", "forward", "reload":
		go vc.navigate(ctx, m)
		return nil
	}
	return fmt.Errorf("%w: unknown message type %q", core.ErrBadRequest, m.T)
}

// selectionJS returns the selected text, including inside inputs.
const selectionJS = `(() => {
  const a = document.activeElement;
  if (a && typeof a.selectionStart === 'number' && typeof a.value === 'string') {
    return a.value.slice(a.selectionStart, a.selectionEnd);
  }
  return String(document.getSelection() || '');
})()`

func (vc *viewerConn) navigate(ctx context.Context, m clientMsg) {
	busy, idle := true, false
	vc.send(serverMsg{T: "busy", Busy: &busy})
	defer vc.send(serverMsg{T: "busy", Busy: &idle})

	nctx, cancel := context.WithTimeout(ctx, vc.u.core.Timeout)
	defer cancel()
	unlock, err := vc.s.Lock(nctx)
	if err == nil {
		switch m.T {
		case "nav":
			_, err = vc.u.core.Navigate(nctx, vc.s, normalizeURL(m.URL), cdp.WaitDOMContentLoaded)
		case "back":
			err = vc.s.Tab.History(nctx, -1)
		case "forward":
			err = vc.s.Tab.History(nctx, 1)
		case "reload":
			err = vc.s.Tab.Reload(nctx, cdp.WaitDOMContentLoaded)
		}
		unlock()
	}
	if err != nil && ctx.Err() == nil { // nobody to tell once the viewer left
		vc.send(errorMsg(err))
	}
}

func errorMsg(err error) serverMsg {
	_, code := core.Classify(err)
	if errors.Is(err, errNotImplemented) {
		code = core.CodeNotImplemented
	}
	return serverMsg{T: "error", Code: code, Message: err.Error()}
}

func keyEvent(m clientMsg) (cdp.KeyEvent, error) {
	k := cdp.KeyEvent{Type: m.Type, Key: m.Key, Code: m.Code, KeyCode: m.KeyCode, Modifiers: m.Modifiers}
	switch k.Type {
	case "keyDown":
		if m.Text == "" {
			k.Type = "rawKeyDown" // no character: don't let Chrome insert one
		}
	case "keyUp", "rawKeyDown", "char":
	default:
		return k, fmt.Errorf("%w: key type must be keyDown, keyUp, rawKeyDown or char", core.ErrBadRequest)
	}
	if k.Type != "keyUp" {
		if utf8.RuneCountInString(m.Text) > maxKeyText || !utf8.ValidString(m.Text) {
			return k, fmt.Errorf("%w: key text too long", core.ErrBadRequest)
		}
		k.Text = m.Text
	}
	for _, c := range m.Commands {
		if !allowedCommands[c] {
			return k, fmt.Errorf("%w: command %q not allowed", core.ErrBadRequest, c)
		}
		k.Commands = append(k.Commands, c)
	}
	return k, nil
}
