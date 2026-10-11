package cdp

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseActivePort(t *testing.T) {
	tests := []struct {
		in, want string
		wantErr  bool
	}{
		{"9222\n/devtools/browser/abc-123\n", "ws://127.0.0.1:9222/devtools/browser/abc-123", false},
		{"  41234 \n /devtools/browser/x ", "ws://127.0.0.1:41234/devtools/browser/x", false},
		{"9222", "", true},
		{"nope\n/devtools/browser/x", "", true},
		{"70000\n/devtools/browser/x", "", true},
		{"9222\n/json", "", true},
	}
	for _, tt := range tests {
		got, err := ParseActivePort(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("ParseActivePort(%q) = %q, %v", tt.in, got, err)
		}
	}
}

func devtoolsServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/version" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"Browser":"Chrome/1","webSocketDebuggerUrl":"ws://127.0.0.1:1/devtools/browser/guid"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestResolveEndpoint(t *testing.T) {
	srv := devtoolsServer(t)
	host := strings.TrimPrefix(srv.URL, "http://")
	_, port, _ := net.SplitHostPort(host)

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "DevToolsActivePort"), []byte(port+"\n/devtools/browser/file-guid\n"), 0o600)

	tests := []struct {
		spec, want string
	}{
		{"ws://10.0.0.1:9222/devtools/browser/x", "ws://10.0.0.1:9222/devtools/browser/x"},
		{srv.URL, "ws://" + host + "/devtools/browser/guid"},
		{host, "ws://" + host + "/devtools/browser/guid"},
		{dir, "ws://127.0.0.1:" + port + "/devtools/browser/file-guid"},
		{filepath.Join(dir, "DevToolsActivePort"), "ws://127.0.0.1:" + port + "/devtools/browser/file-guid"},
	}
	for _, tt := range tests {
		got, err := ResolveEndpoint(context.Background(), tt.spec)
		if err != nil || got != tt.want {
			t.Errorf("ResolveEndpoint(%q) = %q, %v; want %q", tt.spec, got, err, tt.want)
		}
	}
}

func TestResolveEndpointErrors(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close() // nothing listens here now
	stale := filepath.Join(t.TempDir(), "DevToolsActivePort")
	_, port, _ := net.SplitHostPort(addr)
	os.WriteFile(stale, []byte(port+"\n/devtools/browser/x\n"), 0o600)
	for _, spec := range []string{"http://" + addr, stale, filepath.Join(t.TempDir(), "missing")} {
		if _, err := ResolveEndpoint(context.Background(), spec); err == nil {
			t.Errorf("ResolveEndpoint(%q) succeeded", spec)
		}
	}
}

func TestParseWait(t *testing.T) {
	for in, want := range map[string]Wait{"": WaitLoad, "none": WaitNone, "networkidle": WaitNetworkIdle, "domcontentloaded": WaitDOMContentLoaded} {
		if got, err := ParseWait(in); err != nil || got != want {
			t.Errorf("ParseWait(%q) = %q %v", in, got, err)
		}
	}
	if _, err := ParseWait("forever"); err == nil {
		t.Error("bad wait accepted")
	}
}

func TestIsStaleObject(t *testing.T) {
	if !IsStaleObject(&ProtocolError{Message: "Could not find object with given id"}) {
		t.Error("stale object not detected")
	}
	if IsStaleObject(&ProtocolError{Message: "something else"}) || IsStaleObject(context.Canceled) {
		t.Error("false positive")
	}
}

func TestRedactWS(t *testing.T) {
	if got := redactWS("ws://127.0.0.1:9222/devtools/browser/secret-guid"); strings.Contains(got, "secret") {
		t.Errorf("guid leaked: %s", got)
	}
}
