// Package core is the engine every Tether face is built on: it turns
// "this request, in this tab" into real browser work and back.
//
// Four faces share it — the HTTP proxy and WebDriver endpoints (shipping),
// and an MCP server and a remote-browser UI (stubs). Faces stay thin:
// they translate their protocol into core calls and core results back into
// their protocol. Sessions, policy and browser access live here.
package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/adeelahmad/tether/internal/cdp"
	"github.com/adeelahmad/tether/internal/session"
)

// Face is a protocol front-end over the core. The server mounts each face
// as an http.Handler.
type Face interface {
	Name() string
	http.Handler
}

// Core ties the session manager, the browser and the policy together.
type Core struct {
	Sessions *session.Manager
	Browser  cdp.Browser
	Policy   Policy
	Log      *slog.Logger
	// Timeout bounds a single request when the caller sets none.
	Timeout time.Duration
}

// Options configure New.
type Options struct {
	Policy  Policy
	Logger  *slog.Logger
	Timeout time.Duration
}

// New returns a Core.
func New(b cdp.Browser, m *session.Manager, opts Options) *Core {
	c := &Core{Sessions: m, Browser: b, Policy: opts.Policy, Log: opts.Logger, Timeout: opts.Timeout}
	if c.Policy == nil {
		c.Policy = AllowAll
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.Timeout <= 0 {
		c.Timeout = 60 * time.Second
	}
	return c
}

// Mode selects how a request is executed in the tab.
type Mode string

const (
	// ModeAuto navigates for GET/HEAD and fetches for everything else.
	ModeAuto Mode = ""
	// ModeRendered navigates and returns the live DOM for HTML documents.
	ModeRendered Mode = "rendered"
	// ModeRaw navigates and returns the original response bytes.
	ModeRaw Mode = "raw"
	// ModeFetch runs fetch() inside the tab, on the target's origin.
	ModeFetch Mode = "fetch"
)

// ParseMode validates a mode string.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.ToLower(s)); m {
	case ModeAuto, ModeRendered, ModeRaw, ModeFetch:
		return m, nil
	case "auto":
		return ModeAuto, nil
	}
	return "", fmt.Errorf("mode must be one of auto, rendered, raw, fetch")
}

// Request is a browser-executed HTTP request.
type Request struct {
	Method string
	URL    *url.URL
	Header http.Header
	Body   []byte
	Mode   Mode
	Wait   cdp.Wait
}

// Response is what the browser saw.
type Response struct {
	Status   int
	Header   http.Header
	Body     []byte
	FinalURL string
}

// Do executes req in session s. The session is locked for the duration, so
// requests to one tab are serialised while different tabs run in parallel.
func (c *Core) Do(ctx context.Context, s *session.Session, req *Request) (*Response, error) {
	if err := c.Policy.Allow(ctx, s, req.URL); err != nil {
		return nil, err
	}
	unlock, err := s.Lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()

	mode := req.Mode
	if mode == ModeAuto {
		if req.Method == http.MethodGet || req.Method == http.MethodHead {
			mode = ModeRendered
		} else {
			mode = ModeFetch
		}
	}
	if mode != ModeFetch && req.Method != http.MethodGet && req.Method != http.MethodHead {
		return nil, fmt.Errorf("%w: %s requires fetch mode", ErrBadRequest, req.Method)
	}

	var resp *Response
	if mode == ModeFetch {
		resp, err = c.fetch(ctx, s.Tab, req)
	} else {
		resp, err = c.navigate(ctx, s.Tab, req, mode)
	}
	if err != nil {
		return nil, err
	}
	if req.Method == http.MethodHead {
		resp.Body = nil
	}
	return resp, nil
}

// ErrBadRequest marks a request the core cannot execute as given.
var ErrBadRequest = errors.New("bad request")

func (c *Core) navigate(ctx context.Context, tab cdp.Tab, req *Request, mode Mode) (*Response, error) {
	wait := req.Wait
	if wait == "" {
		wait = cdp.WaitLoad
	}
	doc, err := tab.Navigate(ctx, req.URL.String(), wait)
	if err != nil {
		return nil, err
	}
	resp := &Response{Status: doc.Status, Header: CleanResponseHeader(doc.Header), FinalURL: doc.URL}
	if req.Method == http.MethodHead {
		return resp, nil
	}

	if mode == ModeRendered && isHTML(doc.MimeType) {
		html, err := tab.Evaluate(ctx, RenderedHTML, false)
		if err != nil {
			return nil, err
		}
		var s string
		if err := json.Unmarshal(html, &s); err != nil {
			return nil, err
		}
		resp.Body = []byte(s)
		resp.Header.Set("Content-Type", "text/html; charset=utf-8")
		return resp, nil
	}

	body, err := tab.ResponseBody(ctx, doc.RequestID)
	if err != nil {
		if !isHTML(doc.MimeType) && doc.RequestID != "" {
			return nil, fmt.Errorf("reading response body: %w", err)
		}
		// about:blank and friends: fall back to the DOM.
		html, herr := tab.Evaluate(ctx, RenderedHTML, false)
		if herr != nil {
			return nil, err
		}
		var s string
		_ = json.Unmarshal(html, &s)
		body = []byte(s)
	}
	resp.Body = body
	return resp, nil
}

// RenderedHTML is a JavaScript expression returning the live DOM,
// serialised with its doctype.
const RenderedHTML = `(() => {
  const dt = document.doctype;
  const doctype = dt ? '<!DOCTYPE ' + dt.name +
    (dt.publicId ? ' PUBLIC "' + dt.publicId + '"' : '') +
    (!dt.publicId && dt.systemId ? ' SYSTEM' : '') +
    (dt.systemId ? ' "' + dt.systemId + '"' : '') + '>\n' : '';
  return doctype + (document.documentElement ? document.documentElement.outerHTML : '');
})()`

func isHTML(mime string) bool {
	return mime == "text/html" || mime == "application/xhtml+xml"
}

// fetchJS runs inside the tab. The tab is on the target origin, so the
// request is same-origin and carries the browser's own cookies.
const fetchJS = `async function(url, method, headers, body) {
  const init = {method, headers, credentials: 'include', redirect: 'follow'};
  if (body) init.body = Uint8Array.from(atob(body), c => c.charCodeAt(0));
  const r = await fetch(url, init);
  const bytes = new Uint8Array(await r.arrayBuffer());
  let bin = '';
  for (let i = 0; i < bytes.length; i += 0x8000) {
    bin += String.fromCharCode.apply(null, bytes.subarray(i, i + 0x8000));
  }
  return {status: r.status, url: r.url, headers: [...r.headers], body: btoa(bin)};
}`

func (c *Core) fetch(ctx context.Context, tab cdp.Tab, req *Request) (*Response, error) {
	if err := ensureOrigin(ctx, tab, req.URL); err != nil {
		return nil, err
	}
	headers := map[string]string{}
	for k, vs := range CleanRequestHeader(req.Header) {
		headers[k] = strings.Join(vs, ", ")
	}
	var body any
	if len(req.Body) > 0 {
		body = base64.StdEncoding.EncodeToString(req.Body)
	}
	r, err := tab.CallFunction(ctx, fetchJS, cdp.CallOptions{
		Args: []cdp.Arg{
			{Value: req.URL.String()},
			{Value: req.Method},
			{Value: headers},
			{Value: body},
		},
		Await:   true,
		ByValue: true,
	})
	if err != nil {
		return nil, err
	}
	var out struct {
		Status  int         `json:"status"`
		URL     string      `json:"url"`
		Headers [][2]string `json:"headers"`
		Body    string      `json:"body"`
	}
	if err := json.Unmarshal(r.Value, &out); err != nil {
		return nil, fmt.Errorf("decoding fetch result: %w", err)
	}
	b, err := base64.StdEncoding.DecodeString(out.Body)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	for _, kv := range out.Headers {
		h.Add(kv[0], kv[1])
	}
	return &Response{Status: out.Status, Header: CleanResponseHeader(h), Body: b, FinalURL: out.URL}, nil
}

// ensureOrigin puts the tab on u's origin so fetch() is same-origin.
func ensureOrigin(ctx context.Context, tab cdp.Tab, u *url.URL) error {
	origin := u.Scheme + "://" + u.Host
	info, err := tab.Info(ctx)
	if err == nil {
		if cur, perr := url.Parse(info.URL); perr == nil && cur.Scheme+"://"+cur.Host == origin {
			return nil
		}
	}
	_, err = tab.Navigate(ctx, origin+"/", cdp.WaitDOMContentLoaded)
	return err
}

// Eval evaluates JavaScript in the session's tab via Runtime.evaluate.
func (c *Core) Eval(ctx context.Context, s *session.Session, expr string, awaitPromise bool) (json.RawMessage, error) {
	unlock, err := s.Lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.Tab.Evaluate(ctx, expr, awaitPromise)
}

// Navigate loads rawURL in the session's tab, subject to policy. The
// caller must hold the session lock.
func (c *Core) Navigate(ctx context.Context, s *session.Session, rawURL string, wait cdp.Wait) (*cdp.Document, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" {
		return nil, fmt.Errorf("%w: invalid URL %q", ErrBadRequest, rawURL)
	}
	if err := c.Policy.Allow(ctx, s, u); err != nil {
		return nil, err
	}
	return s.Tab.Navigate(ctx, u.String(), wait)
}

var hopByHop = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// CleanResponseHeader drops headers that must not be relayed to the
// client: hop-by-hop headers, encodings the browser already decoded, and
// Set-Cookie (the browser keeps its cookies server-side).
func CleanResponseHeader(in http.Header) http.Header {
	out := in.Clone()
	if out == nil {
		out = http.Header{}
	}
	for _, k := range hopByHop {
		out.Del(k)
	}
	for _, k := range []string{"Content-Encoding", "Content-Length", "Set-Cookie", "Set-Cookie2"} {
		out.Del(k)
	}
	return out
}

// CleanRequestHeader drops headers that must not reach the page's fetch():
// hop-by-hop and proxy headers, Tether control headers, and client cookies
// (the browser's own cookie jar is the point).
func CleanRequestHeader(in http.Header) http.Header {
	out := in.Clone()
	if out == nil {
		out = http.Header{}
	}
	for _, k := range hopByHop {
		out.Del(k)
	}
	for _, k := range []string{"Host", "Cookie", "Content-Length", "Accept-Encoding", "User-Agent", "Origin", "Referer"} {
		out.Del(k)
	}
	for k := range out {
		if strings.HasPrefix(strings.ToLower(k), "x-tether-") {
			out.Del(k)
		}
	}
	return out
}
