package server

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/adeelahmad/tether/internal/policy"
)

func TestCheckBind(t *testing.T) {
	tests := []struct {
		addr     string
		token    bool
		insecure bool
		wantErr  error
	}{
		{"127.0.0.1:8787", false, false, nil},
		{"[::1]:8787", false, false, nil},
		{"localhost:8787", false, false, nil},
		{"0.0.0.0:8787", false, false, ErrPublicWithoutToken},
		{":8787", false, false, ErrPublicWithoutToken},
		{"100.101.102.103:8787", false, false, ErrPublicWithoutToken},
		{"0.0.0.0:8787", true, false, nil},
		{"0.0.0.0:8787", false, true, nil},
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			err := CheckBind(context.Background(), tt.addr, tt.token, tt.insecure)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
	if err := CheckBind(context.Background(), "no-port", false, false); err == nil {
		t.Fatal("expected error for malformed addr")
	}
}

func okHandler(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Face", name)
		w.WriteHeader(http.StatusOK)
	})
}

func newTestServer(token string, guard bool) http.Handler {
	return New(Options{
		Token:     token,
		Guard:     guard,
		Proxy:     okHandler("proxy"),
		API:       okHandler("api"),
		WebDriver: okHandler("webdriver"),
	}).Handler()
}

func basic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func TestRoutingAndAuth(t *testing.T) {
	const token = "s3cret-token-0123456789"
	h := newTestServer(token, false)
	tests := []struct {
		name       string
		method     string
		target     string
		header     map[string]string
		wantStatus int
		wantFace   string
	}{
		{"api needs auth", "GET", "/tether/v1/tabs", nil, 401, ""},
		{"api bearer", "GET", "/tether/v1/tabs", map[string]string{"Authorization": "Bearer " + token}, 200, "api"},
		{"api wrong token", "GET", "/tether/v1/tabs", map[string]string{"Authorization": "Bearer nope"}, 401, ""},
		{"webdriver basic", "POST", "/session", map[string]string{"Authorization": basic("any", token)}, 200, "webdriver"},
		{"proxy needs proxy auth", "GET", "http://example.com/", map[string]string{"Authorization": "Bearer " + token}, 407, ""},
		{"proxy basic", "GET", "http://example.com/", map[string]string{"Proxy-Authorization": basic("tether", token)}, 200, "proxy"},
		{"proxy bearer", "GET", "http://example.com/", map[string]string{"Proxy-Authorization": "Bearer " + token}, 200, "proxy"},
		{"connect", "CONNECT", "example.com:443", map[string]string{"Proxy-Authorization": "Bearer " + token}, 200, "proxy"},
		{"connect unauth", "CONNECT", "example.com:443", nil, 407, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, tt.target, nil)
			if tt.method == "CONNECT" {
				r.URL.Host = tt.target
			}
			for k, v := range tt.header {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.wantStatus || w.Header().Get("X-Face") != tt.wantFace {
				t.Fatalf("got %d face=%q, want %d face=%q", w.Code, w.Header().Get("X-Face"), tt.wantStatus, tt.wantFace)
			}
		})
	}
}

func TestProxyAuthorizationIsStripped(t *testing.T) {
	const token = "s3cret-token-0123456789"
	var seen string
	h := New(Options{Token: token, Proxy: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Proxy-Authorization")
	})}).Handler()
	r := httptest.NewRequest("GET", "http://example.com/", nil)
	r.Header.Set("Proxy-Authorization", "Bearer "+token)
	h.ServeHTTP(httptest.NewRecorder(), r)
	if seen != "" {
		t.Fatal("Proxy-Authorization reached the proxy face")
	}
}

func TestLocalGuard(t *testing.T) {
	h := newTestServer("", true)
	tests := []struct {
		name       string
		target     string
		host       string
		header     map[string]string
		wantStatus int
	}{
		{"loopback ip", "/tether/v1/tabs", "127.0.0.1:8787", nil, 200},
		{"localhost", "/tether/v1/tabs", "localhost:8787", nil, 200},
		{"dns rebinding", "/tether/v1/tabs", "attacker.example:8787", nil, 403},
		{"cross-origin page", "/session", "127.0.0.1:8787", map[string]string{"Origin": "https://evil.example"}, 403},
		{"cross-site fetch", "/session", "127.0.0.1:8787", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"local origin ok", "/session", "127.0.0.1:8787", map[string]string{"Origin": "http://localhost:3000"}, 200},
		{"proxy unaffected", "http://example.com/", "example.com", nil, 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", tt.target, nil)
			r.Host = tt.host
			for k, v := range tt.header {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tt.wantStatus)
			}
		})
	}
}

func TestRecoverer(t *testing.T) {
	h := New(Options{API: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })}).Handler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/tether/v1/x", nil))
	if w.Code != 500 {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestServeGracefulShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	s := New(Options{API: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(204)
	})})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln, 5*time.Second) }()

	respc := make(chan int, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/tether/v1/slow")
		if err != nil {
			respc <- -1
			return
		}
		resp.Body.Close()
		respc <- resp.StatusCode
	}()
	<-started
	cancel()
	time.Sleep(50 * time.Millisecond)
	close(release)
	if code := <-respc; code != 204 {
		t.Fatalf("in-flight request got %d", code)
	}
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
}

func TestPerTokenPrincipals(t *testing.T) {
	agent := &policy.Principal{Name: "agent"}
	var seen *policy.Principal
	capture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = policy.FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	h := New(Options{
		Token:       "admin-token-0123456789",
		Credentials: []policy.Credential{{Token: "agent-token-0123456789", Principal: agent}},
		Proxy:       capture,
		API:         capture,
	}).Handler()

	tests := []struct {
		name, target, header, value string
		wantStatus                  int
		wantName                    string
	}{
		{"agent api", "/tether/v1/tabs", "Authorization", "Bearer agent-token-0123456789", 200, "agent"},
		{"admin api", "/tether/v1/tabs", "Authorization", "Bearer admin-token-0123456789", 200, policy.DefaultTokenName},
		{"agent proxy", "http://example.com/", "Proxy-Authorization", basic("tether", "agent-token-0123456789"), 200, "agent"},
		{"unknown", "/tether/v1/tabs", "Authorization", "Bearer someone-else-0123456789", 401, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seen = nil
			r := httptest.NewRequest("GET", tt.target, nil)
			r.Header.Set(tt.header, tt.value)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.wantStatus {
				t.Fatalf("status %d, want %d", w.Code, tt.wantStatus)
			}
			got := ""
			if seen != nil {
				got = seen.Name
			}
			if got != tt.wantName {
				t.Fatalf("principal %q, want %q", got, tt.wantName)
			}
		})
	}
	if seen := (*authenticator)(nil); newAuthenticator(nil) != seen {
		t.Fatal("no credentials means no authenticator")
	}
}
