package remote

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/adeelahmad/tether/internal/cdp"
	"github.com/adeelahmad/tether/internal/logx"
)

// hub runs at most one screencast per tab and fans its frames out to every
// viewer of that tab. Chrome allows one screencast per DevTools session, and
// one stream is cheaper than many anyway.
type hub struct {
	opts cdp.ScreencastOptions
	log  *slog.Logger

	mu    sync.Mutex
	casts map[string]*cast
	// newest cast ever started per tab, running or stopping: a new cast
	// waits for its predecessor, or the old one's Page.stopScreencast
	// would end the new stream.
	latest map[string]*cast
}

func newHub(opts cdp.ScreencastOptions, log *slog.Logger) *hub {
	return &hub{opts: opts, log: log, casts: map[string]*cast{}, latest: map[string]*cast{}}
}

// cast is one running screencast.
type cast struct {
	tabID   string
	cancel  context.CancelFunc
	refresh chan struct{} // see cdp.ScreencastOptions.Refresh
	done    chan struct{} // closed when the screencast has stopped
	prev    *cast         // the tab's previous cast, which must stop first
	err     error         // why it stopped; read after done
	viewers map[*viewer]struct{}
	last    *cdp.Frame // replayed to viewers who join mid-stream
}

// viewer receives the newest frame of a cast. Slow viewers skip frames
// rather than slow the cast down for everyone.
type viewer struct {
	frames chan cdp.Frame
	ended  <-chan struct{}
	c      *cast
}

// errEnded is reported when a cast stops because its tab session ended.
var errEnded = errors.New("tab session ended")

// join attaches a viewer to tabID's cast, starting it if needed.
func (h *hub) join(tabID string, scr cdp.Screen) *viewer {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.casts[tabID]
	if c == nil {
		ctx, cancel := context.WithCancel(context.Background())
		c = &cast{
			tabID: tabID, cancel: cancel, refresh: make(chan struct{}, 1),
			done: make(chan struct{}), viewers: map[*viewer]struct{}{}, prev: h.latest[tabID],
		}
		h.casts[tabID] = c
		h.latest[tabID] = c
		go h.run(ctx, c, scr)
		h.log.Debug("screencast started", "tab", logx.Clean(tabID))
	}
	v := &viewer{frames: make(chan cdp.Frame, 1), ended: c.done, c: c}
	c.viewers[v] = struct{}{}
	if c.last != nil {
		v.frames <- *c.last
	}
	return v
}

func (h *hub) run(ctx context.Context, c *cast, scr cdp.Screen) {
	var err error
	if c.prev != nil {
		select {
		case <-c.prev.done:
		case <-ctx.Done():
		}
	}
	if ctx.Err() == nil {
		opts := h.opts
		opts.Refresh = c.refresh
		err = scr.Screencast(ctx, opts, func(f cdp.Frame) { h.broadcast(c, f) })
	}
	h.mu.Lock()
	if h.casts[c.tabID] == c {
		delete(h.casts, c.tabID)
	}
	if h.latest[c.tabID] == c {
		delete(h.latest, c.tabID)
	}
	c.prev = nil // let the chain be collected
	if c.err == nil {
		c.err = err
	}
	h.mu.Unlock()
	close(c.done)
	h.log.Debug("screencast stopped", "tab", logx.Clean(c.tabID), "err", err)
}

func (h *hub) broadcast(c *cast, f cdp.Frame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c.last = &f
	for v := range c.viewers {
		select {
		case v.frames <- f:
			continue
		default:
		}
		select { // drop the stale frame, keep the new one
		case <-v.frames:
		default:
		}
		select {
		case v.frames <- f:
		default:
		}
	}
}

// poke tells v's cast that someone is interacting with the tab.
func (v *viewer) poke() {
	select {
	case v.c.refresh <- struct{}{}:
	default:
	}
}

// leave detaches v, stopping the cast when nobody is watching.
func (h *hub) leave(v *viewer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := v.c
	delete(c.viewers, v)
	if len(c.viewers) == 0 {
		if h.casts[c.tabID] == c {
			delete(h.casts, c.tabID)
		}
		c.cancel()
	}
}

// end stops tabID's cast because the tab is gone; viewers see errEnded.
func (h *hub) end(tabID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c := h.casts[tabID]; c != nil {
		delete(h.casts, tabID)
		c.err = errEnded
		c.cancel()
	}
}

// stopAll stops every cast (shutdown).
func (h *hub) stopAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, c := range h.casts {
		delete(h.casts, id)
		c.err = errEnded
		c.cancel()
	}
}

// active reports how many casts are running.
func (h *hub) active() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.casts)
}

// reason says why v's cast stopped. Only valid once v.ended is closed.
func (v *viewer) reason() error { return v.c.err }
