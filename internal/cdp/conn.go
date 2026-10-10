package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	rodcdp "github.com/go-rod/rod/lib/cdp"
)

// Event is a CDP event delivered to subscribers.
type Event struct {
	SessionID string
	Method    string
	Params    json.RawMessage
}

// conn is a single DevTools websocket connection with a fan-out event bus.
// Calls are multiplexed over flattened target sessions.
type conn struct {
	ws     *rodcdp.WebSocket
	client *rodcdp.Client

	mu     sync.Mutex
	subs   map[int]subscriber
	nextID int

	done chan struct{}
}

type subscriber struct {
	sessionID string // "" = every session
	fn        func(Event)
}

func dial(ctx context.Context, wsURL string) (*conn, error) {
	ws := &rodcdp.WebSocket{}
	if err := ws.Connect(ctx, wsURL, nil); err != nil {
		return nil, fmt.Errorf("cdp: connect %s: %w", redactWS(wsURL), err)
	}
	c := &conn{
		ws:     ws,
		client: rodcdp.New().Start(ws),
		subs:   map[int]subscriber{},
		done:   make(chan struct{}),
	}
	go c.dispatch()
	return c, nil
}

// dispatch fans events out to subscribers. Callbacks run on this goroutine
// and must not block.
func (c *conn) dispatch() {
	defer close(c.done)
	for e := range c.client.Event() {
		c.mu.Lock()
		subs := make([]subscriber, 0, len(c.subs))
		for _, s := range c.subs {
			subs = append(subs, s)
		}
		c.mu.Unlock()
		ev := Event{SessionID: e.SessionID, Method: e.Method, Params: e.Params}
		for _, s := range subs {
			if s.sessionID == "" || s.sessionID == e.SessionID {
				s.fn(ev)
			}
		}
	}
}

func (c *conn) subscribe(sessionID string, fn func(Event)) (cancel func()) {
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	c.subs[id] = subscriber{sessionID: sessionID, fn: fn}
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		delete(c.subs, id)
		c.mu.Unlock()
	}
}

// call invokes method on sessionID ("" = browser) and decodes the result
// into out (which may be nil).
func (c *conn) call(ctx context.Context, sessionID, method string, params, out any) error {
	select {
	case <-c.done:
		return ErrDisconnected
	default:
	}
	if params == nil {
		params = struct{}{}
	}
	raw, err := c.client.Call(ctx, sessionID, method, params)
	if err != nil {
		return wrapErr(method, err)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("cdp: decode %s: %w", method, err)
	}
	return nil
}

func (c *conn) close() error { return c.ws.Close() }

// ErrDisconnected is returned once the DevTools connection is gone.
var ErrDisconnected = errors.New("cdp: browser connection closed")

// ProtocolError is an error reported by Chrome for a CDP call.
type ProtocolError struct {
	Method  string
	Code    int
	Message string
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("cdp: %s: %s (%d)", e.Method, e.Message, e.Code)
}

func wrapErr(method string, err error) error {
	var pe *rodcdp.Error
	if errors.As(err, &pe) {
		msg := pe.Message
		if pe.Data != "" {
			msg += ": " + pe.Data
		}
		return &ProtocolError{Method: method, Code: pe.Code, Message: msg}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("cdp: %s: %w", method, err)
}

// IsStaleObject reports whether err means a remote object handle no longer
// exists (typically because the page navigated).
func IsStaleObject(err error) bool {
	var pe *ProtocolError
	if !errors.As(err, &pe) {
		return false
	}
	m := strings.ToLower(pe.Message)
	return strings.Contains(m, "could not find object") ||
		strings.Contains(m, "cannot find context") ||
		strings.Contains(m, "no node with given id") ||
		strings.Contains(m, "inspected target navigated or closed")
}

// redactWS strips the per-browser GUID from a ws URL before it is logged.
func redactWS(u string) string {
	if i := strings.Index(u, "/devtools/"); i >= 0 {
		return u[:i] + "/devtools/…"
	}
	return u
}
