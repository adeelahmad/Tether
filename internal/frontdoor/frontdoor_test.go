package frontdoor

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func classify(t *testing.T, input string) (*Conn, error) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	go func() { _, _ = io.WriteString(client, input) }()
	return Classify(server)
}

func TestClassify(t *testing.T) {
	bigCookie := "Cookie: c=" + strings.Repeat("x", 10000) + "\r\n"
	tests := []struct {
		name string
		in   string
		kind Kind
		host string
	}{
		{"origin form", "GET /tether/v1/health HTTP/1.1\r\nHost: 127.0.0.1:8787\r\n\r\n", KindLocal, ""},
		{"absolute form", "GET http://example.com/a?token=x HTTP/1.1\r\nHost: example.com\r\n\r\n", KindProxy, "example.com:80"},
		{"absolute with port", "POST http://example.com:8080/ HTTP/1.1\r\nHost: example.com:8080\r\n\r\n", KindProxy, "example.com:8080"},
		{"connect", "CONNECT github.com:443 HTTP/1.1\r\nHost: github.com:443\r\n\r\n", KindConnect, "github.com:443"},
		{"connect no port", "CONNECT github.com HTTP/1.1\r\n\r\n", KindConnect, "github.com:443"},
		{"head over 4KB", "GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\n" + bigCookie + "\r\n", KindProxy, "example.com:80"},
		{"http/1.0 without host", "GET / HTTP/1.0\r\n\r\n", KindLocal, ""},
		{"asterisk", "OPTIONS * HTTP/1.1\r\nHost: x\r\n\r\n", KindLocal, ""},
		{"leading blank line", "\r\nGET / HTTP/1.1\r\nHost: x\r\n\r\n", KindLocal, ""},
		{"bare LF", "GET / HTTP/1.1\nHost: x\n\n", KindLocal, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := classify(t, tt.in)
			if err != nil {
				t.Fatalf("Classify: %v", err)
			}
			if c.Kind != tt.kind || c.Host != tt.host {
				t.Errorf("got %v %q, want %v %q", c.Kind, c.Host, tt.kind, tt.host)
			}
			want := strings.TrimPrefix(tt.in, "\r\n")
			got := make([]byte, len(want))
			if _, err := io.ReadFull(c, got); err != nil {
				t.Fatalf("replay: %v", err)
			}
			if string(got) != want {
				t.Errorf("replayed head differs (%d vs %d bytes)", len(got), len(want))
			}
		})
	}
}

func TestClassifyRejects(t *testing.T) {
	tests := map[string]string{
		"not http":   "hello\r\n\r\n",
		"truncated":  "GET http://example.com/ HTTP/1.1\r\nHost: exa",
		"empty":      "",
		"too large":  "GET / HTTP/1.1\r\nX: " + strings.Repeat("y", MaxHeadBytes) + "\r\n\r\n",
		"no target":  "GET\r\n\r\n",
		"proxy, bad": "GET http://[::1 HTTP/1.1\r\n\r\n",
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			client, server := net.Pipe()
			defer server.Close()
			go func() { _, _ = io.WriteString(client, in); _ = client.Close() }()
			if c, err := Classify(server); err == nil {
				t.Errorf("accepted as %v", c.Kind)
			}
		})
	}
}

func TestClassifyBodyFollowsHead(t *testing.T) {
	in := "POST http://example.com/form HTTP/1.1\r\nHost: example.com\r\nContent-Length: 5\r\n\r\nhello"
	c, err := classify(t, in)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.ReadRequest(bufio.NewReader(c))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(req.Body)
	if req.URL.String() != "http://example.com/form" || string(body) != "hello" {
		t.Errorf("got %s %q", req.URL, body)
	}
}

func TestListenerServesHTTP(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", "[::1]:0"} {
		t.Run(addr, func(t *testing.T) {
			ln, err := Listen(context.Background(), addr, nil)
			if err != nil {
				if strings.Contains(addr, "::1") {
					t.Skipf("no IPv6 loopback: %v", err)
				}
				t.Fatal(err)
			}
			kinds := make(chan string, 4)
			srv := &http.Server{
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					kinds <- r.URL.String()
					_, _ = io.WriteString(w, "ok")
				}),
				ReadHeaderTimeout: time.Second,
			}
			go func() { _ = srv.Serve(ln) }()
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = srv.Shutdown(ctx)
			}()

			base := "http://" + ln.Addr().String()
			resp, err := http.Get(base + "/x")
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if string(b) != "ok" || <-kinds != "/x" {
				t.Fatalf("direct request: %q", b)
			}

			// Absolute-form, as a proxy client sends it.
			conn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_, _ = io.WriteString(conn, "GET http://example.com/y HTTP/1.1\r\nHost: example.com\r\n\r\n")
			resp, err = http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if got := <-kinds; got != "http://example.com/y" {
				t.Errorf("proxy request URL = %q", got)
			}
		})
	}
}

func TestListenerClose(t *testing.T) {
	ln, err := Listen(context.Background(), "127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	_ = ln.Close() // idempotent
	if _, err := ln.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Accept after Close = %v", err)
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = c.Close()
		t.Error("still accepting after Close")
	}
}

func TestListenBadAddr(t *testing.T) {
	for _, a := range []string{"nope", "127.0.0.1:x", "127.0.0.1:70000"} {
		if _, err := Listen(context.Background(), a, nil); err == nil {
			t.Errorf("Listen(%q) succeeded", a)
		}
	}
}

func TestStdLogDiscarded(t *testing.T) {
	ln, err := Listen(context.Background(), "127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// goproxy's decoder writes request lines, query tokens included, to the
	// global logger.
	if log.Writer() != io.Discard {
		t.Error("global logger still writes after Listen")
	}
}

func TestKindString(t *testing.T) {
	if KindLocal.String() != "local" || KindProxy.String() != "proxy" || KindConnect.String() != "connect" {
		t.Error("Kind.String")
	}
}
