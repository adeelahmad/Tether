// Package frontdoor is Tether's connection front door, built on
// snail007/goproxy. goproxy's HTTP request decoder classifies each new
// connection (CONNECT tunnel,
// absolute-form proxy request, or a request for Tether's own endpoints)
// before the connection is handed to Tether's http.Server, which still
// applies auth, the loopback guard and routing to every request on it.
//
// Three goproxy behaviours are contained here:
//   - Its decoder reads the request head with a single fixed-size Read, so
//     large heads (big cookies) or slow clients would be cut off. The front
//     door reads the whole head first and gives the decoder exactly that.
//   - It prints every request line, query string included, to the
//     standard library's global logger. Tether logs through slog only, so
//     the global logger is discarded (see Listen).
//   - Its decoder panics on some short malformed heads; Classify turns
//     that into an error.
package frontdoor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/snail007/goproxy/utils"

	"github.com/adeelahmad/tether/internal/logx"
)

// MaxHeadBytes caps a request head, matching net/http's default.
const MaxHeadBytes = 1 << 20

// HeadTimeout bounds how long a new connection may take to send its first
// request head.
const HeadTimeout = 30 * time.Second

// Kind is what the first request on a connection asked for.
type Kind int

// Connection kinds.
const (
	KindLocal   Kind = iota // a request for Tether's own endpoints
	KindProxy               // an absolute-form proxy request (http://…)
	KindConnect             // a CONNECT tunnel request
)

func (k Kind) String() string {
	switch k {
	case KindProxy:
		return "proxy"
	case KindConnect:
		return "connect"
	}
	return "local"
}

// Conn is an accepted connection, classified by goproxy's decoder. Reads
// replay the already-decoded head before the rest of the stream.
type Conn struct {
	net.Conn
	r    io.Reader
	Kind Kind
	// Host is the proxy target as host:port (empty for KindLocal).
	Host string
}

func (c *Conn) Read(p []byte) (int, error) { return c.r.Read(p) }

// Listener accepts TCP connections, classifies each with goproxy's
// decoder, and yields them as a net.Listener for http.Server.
//
// It runs its own accept loop rather than goproxy's ServerChannel.ListenTCP,
// whose accept goroutine writes the function's named error result after it
// has returned (a data race under -race).
type Listener struct {
	ln     net.Listener
	log    *slog.Logger
	conns  chan net.Conn
	done   chan struct{}
	closer sync.Once
}

var discardStdLog sync.Once

// Listen binds addr (host:port; port 0 picks one) and starts accepting.
func Listen(ctx context.Context, addr string, lg *slog.Logger) (*Listener, error) {
	if lg == nil {
		lg = slog.Default()
	}
	// goproxy logs request lines, which carry query tokens, through the
	// standard library's global logger. Tether never uses it.
	discardStdLog.Do(func() { log.SetOutput(io.Discard) })

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	l := &Listener{
		ln:    ln,
		log:   lg.With("component", "frontdoor"),
		conns: make(chan net.Conn),
		done:  make(chan struct{}),
	}
	go l.acceptLoop()
	return l, nil
}

func (l *Listener) acceptLoop() {
	for {
		c, err := l.ln.Accept()
		if err != nil {
			if !l.closed() {
				l.log.Error("accept failed", "err", err)
				_ = l.Close()
			}
			return
		}
		go l.handle(c)
	}
}

// Accept implements net.Listener.
func (l *Listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close implements net.Listener.
func (l *Listener) Close() error {
	var err error
	l.closer.Do(func() {
		close(l.done)
		err = l.ln.Close()
	})
	return err
}

// Addr implements net.Listener.
func (l *Listener) Addr() net.Addr { return l.ln.Addr() }

func (l *Listener) closed() bool {
	select {
	case <-l.done:
		return true
	default:
		return false
	}
}

// handle classifies one connection and queues it for Accept.
func (l *Listener) handle(raw net.Conn) {
	defer func() {
		if v := recover(); v != nil {
			l.log.Error("connection handler panicked", "panic", v)
			_ = raw.Close()
		}
	}()
	c, err := Classify(raw)
	if err != nil {
		if !errors.Is(err, io.EOF) {
			l.log.Debug("dropped connection", "remote", raw.RemoteAddr().String(), "err", logx.Clean(err.Error()))
		}
		_ = raw.Close()
		return
	}
	l.log.Debug("accepted", "remote", raw.RemoteAddr().String(), "kind", c.Kind.String())
	select {
	case l.conns <- c:
	case <-l.done:
		_ = raw.Close()
	}
}

// Classify reads the first request head from raw and decodes it with
// goproxy's HTTP decoder. The returned Conn replays the head.
func Classify(raw net.Conn) (_ *Conn, err error) {
	defer func() {
		// goproxy's decoder slices its error message out of short,
		// malformed heads and panics; that is a rejected request.
		if v := recover(); v != nil {
			err = fmt.Errorf("decoding request: malformed head (%v)", v)
		}
	}()
	_ = raw.SetReadDeadline(time.Now().Add(HeadTimeout))
	br := bufio.NewReaderSize(raw, 4096)
	head, err := readHead(br)
	_ = raw.SetReadDeadline(time.Time{})
	if err != nil {
		return nil, err
	}

	// goproxy's decoder does one Read of at most bufSize bytes; hand it the
	// complete head in one piece. Its error paths close the conn they were
	// given, so give it a throwaway wrapper.
	var dec net.Conn = &headConn{Conn: raw, head: head}
	req, err := utils.NewHTTPRequest(&dec, len(head)+1, false, nil)
	originForm := isOriginForm(head)
	if err != nil && !originForm {
		// An origin-form request without a Host header (HTTP/1.0) fails
		// goproxy's URL rebuild; net/http judges those. Anything else is
		// not HTTP.
		return nil, fmt.Errorf("decoding request: %w", err)
	}
	c := &Conn{Conn: raw, r: io.MultiReader(bytes.NewReader(head), br)}
	switch {
	case err != nil || originForm:
	case req.IsHTTPS():
		c.Kind, c.Host = KindConnect, req.Host
	default:
		c.Kind, c.Host = KindProxy, req.Host
	}
	return c, nil
}

// isOriginForm reports whether the request target is a path ("/…" or "*")
// rather than an absolute URL or a CONNECT authority.
func isOriginForm(head []byte) bool {
	line, _, _ := bytes.Cut(head, []byte("\n"))
	f := bytes.Fields(line)
	if len(f) < 2 {
		return false
	}
	return f[1][0] == '/' || bytes.Equal(f[1], []byte("*"))
}

// readHead reads up to and including the blank line ending a request head.
func readHead(br *bufio.Reader) ([]byte, error) {
	var head []byte
	for {
		line, err := br.ReadSlice('\n')
		head = append(head, line...)
		if len(head) > MaxHeadBytes {
			return nil, errors.New("request head too large")
		}
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if errors.Is(err, io.EOF) && len(head) > 0 {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if bytes.Equal(line, []byte("\r\n")) || bytes.Equal(line, []byte("\n")) {
			if len(head) == len(line) {
				head = head[:0] // tolerate leading blank lines (RFC 9112 §2.2)
				continue
			}
			return head, nil
		}
	}
}

// headConn serves the decoder a single Read of the buffered head.
type headConn struct {
	net.Conn
	head []byte
	read bool
}

func (h *headConn) Read(p []byte) (int, error) {
	if h.read {
		return 0, io.EOF
	}
	h.read = true
	return copy(p, h.head), nil
}

// Close is a no-op: the decoder's error paths must not close the real
// connection; the front door decides that.
func (h *headConn) Close() error { return nil }
