// Package proxy is Tether's HTTP/HTTPS forward-proxy face. Point any
// client at it as a plain proxy and each request is replayed inside a real
// Chrome tab, so the response comes from the logged-in browser: its
// cookies, its storage, its JavaScript.
//
// HTTPS works by terminating the CONNECT tunnel with a leaf certificate
// from Tether's local CA (see package certs); clients trust that CA once.
package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/adeelahmad/tether/internal/cdp"
	"github.com/adeelahmad/tether/internal/certs"
	"github.com/adeelahmad/tether/internal/core"
	"github.com/adeelahmad/tether/internal/httpx"
	"github.com/adeelahmad/tether/internal/logx"
	"github.com/adeelahmad/tether/internal/session"
)

// Control headers. Clients may send the request headers; Tether strips
// them before the request reaches the page.
const (
	HeaderTabID    = "X-Tether-Tab-ID"    // request + response
	HeaderMode     = "X-Tether-Mode"      // auto | rendered | raw | fetch
	HeaderWait     = "X-Tether-Wait"      // none | domcontentloaded | load | networkidle
	HeaderTimeout  = "X-Tether-Timeout"   // Go duration, e.g. 30s
	HeaderFinalURL = "X-Tether-Final-URL" // response: URL after redirects
	HeaderSession  = "X-Tether-Session"   // response: how the tab was chosen
)

// CookieName is the session cookie. It is scoped to the registrable
// domain, so one client can hold one per site without collisions, and it
// is stripped from every request before the browser sees it.
const CookieName = "tether_tab"

// MaxBody caps request bodies replayed through fetch().
const MaxBody = 32 << 20

// Proxy is the forward-proxy face.
type Proxy struct {
	core *core.Core
	ca   *certs.Authority
	log  *slog.Logger
}

var _ core.Face = (*Proxy)(nil)

// New returns a Proxy. ca may be nil, in which case CONNECT is refused.
func New(c *core.Core, ca *certs.Authority, log *slog.Logger) *Proxy {
	if log == nil {
		log = slog.Default()
	}
	return &Proxy{core: c, ca: ca, log: log.With("face", "proxy")}
}

// Name implements core.Face.
func (p *Proxy) Name() string { return "proxy" }

// IsProxyRequest reports whether r is addressed to the proxy (CONNECT or
// absolute-form) rather than to Tether's own endpoints.
func IsProxyRequest(r *http.Request) bool {
	return r.Method == http.MethodConnect || r.URL.IsAbs()
}

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodConnect:
		p.serveConnect(w, r)
	case r.URL.IsAbs():
		p.serveTarget(w, r, r.URL)
	default:
		httpx.WriteError(w, http.StatusBadRequest, core.CodeBadRequest, "not a proxy request")
	}
}

func (p *Proxy) serveTarget(w http.ResponseWriter, r *http.Request, target *url.URL) {
	start := time.Now()
	if target.Scheme != "http" && target.Scheme != "https" {
		httpx.WriteError(w, http.StatusBadRequest, core.CodeBadRequest, "unsupported scheme "+target.Scheme)
		return
	}
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		httpx.WriteError(w, http.StatusNotImplemented, core.CodeNotImplemented, "websocket proxying is not supported")
		return
	}
	mode, err := core.ParseMode(r.Header.Get(HeaderMode))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, core.CodeBadRequest, err.Error())
		return
	}
	wait, err := cdp.ParseWait(r.Header.Get(HeaderWait))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, core.CodeBadRequest, err.Error())
		return
	}
	timeout := p.core.Timeout
	if v := r.Header.Get(HeaderTimeout); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			httpx.WriteError(w, http.StatusBadRequest, core.CodeBadRequest, "invalid "+HeaderTimeout)
			return
		}
		timeout = d
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	hints := session.Hints{TabID: strings.TrimSpace(r.Header.Get(HeaderTabID)), Host: target.Hostname()}
	if c, err := r.Cookie(CookieName); err == nil {
		hints.CookieTabID = c.Value
	}
	StripSessionCookie(r.Header)

	s, src, err := p.core.Sessions.Resolve(ctx, hints)
	if err != nil {
		if src == session.SourceCookie {
			// Clear the stale cookie so the client's next request recovers.
			http.SetCookie(w, expiredCookie(target))
		}
		p.fail(w, r, target, err, start)
		return
	}

	var body []byte
	if r.Body != nil {
		body, err = io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, core.CodeBadRequest, "reading request body: "+err.Error())
			return
		}
		if len(body) > MaxBody {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge, core.CodeBadRequest, "request body too large")
			return
		}
	}

	resp, err := p.core.Do(ctx, s, &core.Request{
		Method: r.Method,
		URL:    target,
		Header: r.Header,
		Body:   body,
		Mode:   mode,
		Wait:   wait,
	})
	w.Header().Set(HeaderTabID, s.ID)
	w.Header().Set(HeaderSession, string(src))
	if src == session.SourceNew || (src == session.SourceDomain && hints.CookieTabID != s.ID) {
		http.SetCookie(w, SessionCookie(target, s.ID))
	}
	if err != nil {
		p.fail(w, r, target, err, start)
		return
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if resp.FinalURL != "" {
		w.Header().Set(HeaderFinalURL, resp.FinalURL)
	}
	status := resp.Status
	if status < 100 || status > 999 {
		status = http.StatusOK
	}
	if r.Method != http.MethodHead {
		w.Header().Set("Content-Length", strconv.Itoa(len(resp.Body)))
	}
	w.WriteHeader(status)
	_, _ = w.Write(resp.Body)
	p.log.Info("proxied", "method", r.Method, "url", logx.SafeURL(target), "tab", s.ID,
		"session", string(src), "status", status, "bytes", len(resp.Body), "dur", time.Since(start).Round(time.Millisecond))
}

func (p *Proxy) fail(w http.ResponseWriter, r *http.Request, target *url.URL, err error, start time.Time) {
	status, code := core.Classify(err)
	p.log.Warn("proxy request failed", "method", r.Method, "url", logx.SafeURL(target),
		"status", status, "code", code, "err", err, "dur", time.Since(start).Round(time.Millisecond))
	httpx.WriteError(w, status, code, err.Error())
}

// SessionCookie builds the Set-Cookie for a tab: Domain is the registrable
// domain (host-only for IPs and single-label hosts), Path is /.
func SessionCookie(target *url.URL, tabID string) *http.Cookie {
	c := &http.Cookie{
		Name:     CookieName,
		Value:    tabID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   target.Scheme == "https",
	}
	if d := session.RegistrableDomain(target.Hostname()); strings.Contains(d, ".") && net.ParseIP(d) == nil {
		c.Domain = d
	}
	return c
}

func expiredCookie(target *url.URL) *http.Cookie {
	c := SessionCookie(target, "")
	c.MaxAge = -1
	return c
}

// StripSessionCookie removes Tether's cookie from a request's Cookie
// headers, leaving any others intact.
func StripSessionCookie(h http.Header) {
	vals := h.Values("Cookie")
	if len(vals) == 0 {
		return
	}
	h.Del("Cookie")
	for _, line := range vals {
		var keep []string
		for _, part := range strings.Split(line, ";") {
			name, _, _ := strings.Cut(strings.TrimSpace(part), "=")
			if name == CookieName || strings.TrimSpace(part) == "" {
				continue
			}
			keep = append(keep, strings.TrimSpace(part))
		}
		if len(keep) > 0 {
			h.Add("Cookie", strings.Join(keep, "; "))
		}
	}
}

// serveConnect terminates a CONNECT tunnel with a CA-signed leaf and serves
// the HTTP/1.1 requests inside it.
func (p *Proxy) serveConnect(w http.ResponseWriter, r *http.Request) {
	if p.ca == nil {
		httpx.WriteError(w, http.StatusNotImplemented, core.CodeNotImplemented, "HTTPS interception is disabled")
		return
	}
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		host, port = r.Host, "443"
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		httpx.WriteError(w, http.StatusInternalServerError, core.CodeInternal, "connection cannot be hijacked")
		return
	}
	raw, buf, err := hj.Hijack()
	if err != nil {
		p.log.Warn("hijack failed", "err", err)
		return
	}
	defer raw.Close()
	if _, err := raw.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	// Bytes the client pipelined after CONNECT are the TLS ClientHello.
	c := raw
	if buf != nil && buf.Reader.Buffered() > 0 {
		c = &bufferedConn{Conn: raw, r: buf.Reader}
	}
	tlsConn := tls.Server(c, p.ca.TLSConfig(host))
	_ = tlsConn.SetDeadline(time.Now().Add(15 * time.Second))
	if err := tlsConn.HandshakeContext(r.Context()); err != nil {
		p.log.Debug("tls handshake failed", "host", host, "err", err)
		return
	}
	_ = tlsConn.SetDeadline(time.Time{})

	authority := host
	if port != "443" {
		authority = net.JoinHostPort(host, port)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, ir *http.Request) {
			u := *ir.URL
			u.Scheme = "https"
			u.Host = authority
			if ir.Host != "" && ir.Host != authority && hostOnly(ir.Host) == host {
				u.Host = ir.Host
			}
			p.serveTarget(w, ir, &u)
		}),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          log.New(io.Discard, "", 0),
		BaseContext:       func(net.Listener) context.Context { return r.Context() },
	}
	_ = srv.Serve(newOneConnListener(tlsConn))
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// oneConnListener hands out a single connection, then blocks until that
// connection closes, so http.Server.Serve lives exactly as long as the
// tunnel.
type oneConnListener struct {
	conn      net.Conn
	once      sync.Once
	closeOnce sync.Once
	closed    chan struct{}
}

func newOneConnListener(c net.Conn) *oneConnListener {
	l := &oneConnListener{closed: make(chan struct{})}
	l.conn = &notifyConn{Conn: c, onClose: func() { l.Close() }}
	return l
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() { c = l.conn })
	if c != nil {
		return c, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

type notifyConn struct {
	net.Conn
	once    sync.Once
	onClose func()
}

func (c *notifyConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.onClose)
	return err
}
