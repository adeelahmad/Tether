package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// BridgeOptions configure Bridge.
type BridgeOptions struct {
	// URL is the MCP endpoint, e.g. http://127.0.0.1:8787/mcp.
	URL string
	// Token is Tether's bearer token, if it runs with one.
	Token  string
	Client *http.Client
	Logger *slog.Logger
}

// Bridge connects an MCP client that speaks stdio (newline-delimited
// JSON-RPC on stdin/stdout, as Claude Desktop and most IDE agents do) to
// Tether's Streamable HTTP endpoint, which may be local or across the
// tailnet. It returns when in reaches EOF or ctx ends, after ending the
// MCP session on the server.
func Bridge(ctx context.Context, opts BridgeOptions, in io.Reader, out io.Writer) error {
	if opts.URL == "" {
		return errors.New("bridge: no MCP URL")
	}
	b := &bridge{opts: opts, out: out}
	if b.opts.Client == nil {
		b.opts.Client = &http.Client{}
	}
	if b.opts.Logger == nil {
		b.opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(in)
		sc.Buffer(make([]byte, 64<<10), maxBody)
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) == 0 {
				continue
			}
			select {
			case lines <- append([]byte(nil), line...):
			case <-ctx.Done():
				return
			}
		}
		readErr <- sc.Err()
	}()

	// Messages are forwarded one at a time, in the order the client sent
	// them, so "navigate, then snapshot" never races. Once a session
	// exists, pings and cancellations skip the queue so they are never
	// stuck behind a slow tool call.
	ordered := make(chan []byte, 64)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for line := range ordered {
			b.forward(ctx, line)
		}
	}()
	defer func() {
		close(ordered)
		wg.Wait()
		b.terminate()
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-readErr:
			return err
		case line := <-lines:
			if b.sessionID() == "" || !skipsQueue(line) {
				select {
				case ordered <- line:
				case <-ctx.Done():
					return nil
				}
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				b.forward(ctx, line)
			}()
		}
	}
}

func skipsQueue(line []byte) bool {
	var m message
	if json.Unmarshal(line, &m) != nil {
		return false
	}
	return m.Method == "ping" || m.Method == "notifications/cancelled"
}

type bridge struct {
	opts BridgeOptions

	mu       sync.Mutex
	session  string
	protocol string

	outMu sync.Mutex
	out   io.Writer
}

func (b *bridge) sessionID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.session
}

// forward POSTs one message and writes whatever comes back to stdout.
func (b *bridge) forward(ctx context.Context, line []byte) {
	var m message
	if err := json.Unmarshal(line, &m); err != nil {
		b.write(errorResponse(nil, codeParseError, "invalid JSON from client"))
		return
	}
	resp, err := b.post(ctx, line)
	if err != nil {
		b.fail(&m, fmt.Sprintf("cannot reach Tether at %s: %v", b.opts.URL, err))
		return
	}
	defer resp.Body.Close()

	if m.Method == "initialize" && resp.StatusCode == http.StatusOK {
		b.mu.Lock()
		b.session = resp.Header.Get(HeaderSessionID)
		b.mu.Unlock()
	}
	switch {
	case resp.StatusCode == http.StatusAccepted:
		return
	case resp.StatusCode == http.StatusUnauthorized:
		b.fail(&m, "Tether rejected the token (HTTP 401); check --token / $TETHER_TOKEN")
		return
	case resp.StatusCode == http.StatusNotFound && b.sessionID() != "":
		b.mu.Lock()
		b.session = ""
		b.mu.Unlock()
		b.fail(&m, "the MCP session expired on the server (Tether restarted or it was idle too long); restart the MCP client to reconnect")
		return
	}

	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/event-stream") {
		b.relaySSE(resp.Body)
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		b.fail(&m, "reading Tether's response: "+err.Error())
		return
	}
	if !strings.HasPrefix(ct, "application/json") || !json.Valid(body) {
		b.fail(&m, fmt.Sprintf("Tether answered HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body))))
		return
	}
	if m.Method == "initialize" {
		var r struct {
			Result struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"result"`
		}
		if json.Unmarshal(body, &r) == nil {
			b.mu.Lock()
			b.protocol = r.Result.ProtocolVersion
			b.mu.Unlock()
		}
	}
	b.writeRaw(body)
}

func (b *bridge) post(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.opts.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	b.headers(req)
	return b.opts.Client.Do(req)
}

func (b *bridge) headers(req *http.Request) {
	if b.opts.Token != "" {
		req.Header.Set("Authorization", "Bearer "+b.opts.Token)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.session != "" {
		req.Header.Set(HeaderSessionID, b.session)
	}
	if b.protocol != "" {
		req.Header.Set(HeaderProtocolVersion, b.protocol)
	}
}

// relaySSE copies the data of each server-sent event to stdout. Tether
// answers with plain JSON, but this keeps the bridge usable with any
// Streamable HTTP server.
func (b *bridge) relaySSE(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	var data []string
	flush := func() {
		if len(data) > 0 {
			b.writeRaw([]byte(strings.Join(data, "\n")))
			data = nil
		}
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	flush()
}

// fail reports a transport failure to the client as a JSON-RPC error, for
// requests only: notifications have no one to answer.
func (b *bridge) fail(m *message, msg string) {
	b.opts.Logger.Warn("mcp bridge", "err", msg)
	if m.isRequest() {
		b.write(errorResponse(m.ID, codeInternalError, msg))
	}
}

func (b *bridge) write(v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	b.writeRaw(raw)
}

// writeRaw writes one message per line, as the stdio transport requires.
func (b *bridge) writeRaw(raw []byte) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return
	}
	buf.WriteByte('\n')
	b.outMu.Lock()
	defer b.outMu.Unlock()
	_, _ = b.out.Write(buf.Bytes())
}

// terminate ends the server-side session so the tabs it opened close now
// rather than at the idle timeout.
func (b *bridge) terminate() {
	id := b.sessionID()
	if id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, b.opts.URL, nil)
	if err != nil {
		return
	}
	b.headers(req)
	if resp, err := b.opts.Client.Do(req); err == nil {
		resp.Body.Close()
	}
}
