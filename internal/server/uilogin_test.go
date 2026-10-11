package server

import (
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/adeelahmad/tether/internal/core"
	"github.com/adeelahmad/tether/internal/policy"
)

func newUIServer(token string, guard bool) http.Handler {
	return New(Options{
		Token:     token,
		Guard:     guard,
		Proxy:     okHandler("proxy"),
		API:       okHandler("api"),
		WebDriver: okHandler("webdriver"),
		Faces:     map[string]core.Face{"/remote": face{okHandler("remote")}},
		UIPrefix:  "/remote",
	}).Handler()
}

type face struct{ http.Handler }

func (face) Name() string { return "remote" }

func login(t *testing.T, h http.Handler, token string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/remote/login", strings.NewReader(url.Values{"token": {token}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestUILogin(t *testing.T) {
	uiLoginDelay = 0
	const token = "s3cret-token-0123456789"
	h := newUIServer(token, false)

	// Not signed in: the page redirects to the form, the API says 401.
	tests := []struct {
		method, target string
		wantStatus     int
		wantLocation   string
	}{
		{"GET", "/remote/", 303, "/remote/login"},
		{"GET", "/remote", 303, "/remote/login"},
		{"GET", "/remote/api/tabs", 401, ""},
		{"GET", "/remote/login", 200, ""},
		{"POST", "/remote/logout", 303, "/remote/login"},
		{"GET", "/remote/logout", 405, ""},
	}
	for _, tt := range tests {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tt.method, tt.target, nil))
		if w.Code != tt.wantStatus || w.Header().Get("Location") != tt.wantLocation {
			t.Errorf("%s %s = %d %q", tt.method, tt.target, w.Code, w.Header().Get("Location"))
		}
	}

	// A wrong token is refused; a cross-site post is refused even with
	// the right one.
	if w := login(t, h, "wrong-token-0123456789", nil); w.Code != 401 || len(w.Result().Cookies()) != 0 {
		t.Fatalf("wrong token: %d", w.Code)
	}
	for _, hdr := range []map[string]string{
		{"Origin": "https://evil.example"},
		{"Origin": "null"},
		{"Sec-Fetch-Site": "cross-site", "Origin": "http://example.com"},
		{"Sec-Fetch-Site": "same-site"},
	} {
		if w := login(t, h, token, hdr); w.Code != 403 {
			t.Fatalf("cross-site login %v: %d", hdr, w.Code)
		}
	}
	if w := login(t, h, token, map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "null"}); w.Code != 303 {
		t.Fatalf("same-origin login with opaque origin: %d", w.Code)
	}
	if w := login(t, h, token, map[string]string{"Origin": "https://box.ts.net", "X-Forwarded-Host": "box.ts.net"}); w.Code != 303 {
		t.Fatalf("login behind a reverse proxy: %d", w.Code)
	}

	w := login(t, h, token, map[string]string{"Origin": "http://example.com"})
	if w.Code != 303 || w.Header().Get("Location") != "/remote/" {
		t.Fatalf("login: %d %q", w.Code, w.Header().Get("Location"))
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %v", cookies)
	}
	c := cookies[0]
	if c.Name != UICookieName || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/remote" {
		t.Fatalf("cookie = %+v", c)
	}
	if strings.Contains(c.Value, token) {
		t.Fatal("cookie carries the token itself")
	}

	// The cookie opens the UI, and only the UI.
	withCookie := func(target string) int {
		r := httptest.NewRequest("GET", target, nil)
		r.AddCookie(c)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if code := withCookie("/remote/api/tabs"); code != 200 {
		t.Errorf("ui with cookie: %d", code)
	}
	if code := withCookie("/remote/login"); code != 303 {
		t.Errorf("login page when signed in: %d", code)
	}
	for _, target := range []string{"/tether/v1/tabs", "/session", "/remoteX"} {
		if code := withCookie(target); code != 401 {
			t.Errorf("%s with ui cookie: %d, want 401", target, code)
		}
	}

	// A cookie minted for another token is worthless.
	other := uiCookieValue(credential{sum: sha256.Sum256([]byte("another-token-0123456789"))})
	r := httptest.NewRequest("GET", "/remote/api/tabs", nil)
	r.AddCookie(&http.Cookie{Name: UICookieName, Value: other})
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("foreign cookie: %d", w.Code)
	}

	// The bearer token still works for scripts.
	r = httptest.NewRequest("GET", "/remote/api/tabs", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("bearer: %d", w.Code)
	}
}

func TestUIWithoutToken(t *testing.T) {
	h := newUIServer("", true)
	tests := []struct {
		target, host, accept string
		wantStatus           int
		wantLocation         string
	}{
		{"/remote/", "127.0.0.1:8787", "", 200, ""},
		{"/remote/login", "127.0.0.1:8787", "", 303, "/remote/"},
		{"/remote/", "attacker.example", "", 403, ""}, // the loopback guard still applies
		{"/", "127.0.0.1:8787", "text/html,application/xhtml+xml", 302, "/remote/"},
		{"/", "127.0.0.1:8787", "application/json", 200, ""},
	}
	for _, tt := range tests {
		r := httptest.NewRequest("GET", tt.target, nil)
		r.Host = tt.host
		if tt.accept != "" {
			r.Header.Set("Accept", tt.accept)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tt.wantStatus || w.Header().Get("Location") != tt.wantLocation {
			t.Errorf("GET %s (%s) = %d %q", tt.target, tt.host, w.Code, w.Header().Get("Location"))
		}
	}
}

func TestUICookieSecureOverHTTPS(t *testing.T) {
	uiLoginDelay = 0
	const token = "s3cret-token-0123456789"
	h := newUIServer(token, false)
	w := login(t, h, token, map[string]string{"X-Forwarded-Proto": "https"})
	if c := w.Result().Cookies(); len(c) != 1 || !c[0].Secure {
		t.Fatalf("login over https: %+v", c)
	}
	r := httptest.NewRequest("POST", "/remote/logout", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if c := w.Result().Cookies(); len(c) != 1 || !c[0].Secure || c[0].MaxAge >= 0 {
		t.Fatalf("logout over https: %+v", c)
	}
	if c := login(t, h, token, nil).Result().Cookies(); len(c) != 1 || c[0].Secure {
		t.Fatalf("login over plain http must not be Secure: %+v", c)
	}
}

// Each token gets its own UI cookie, and requests made with it carry that
// token's principal, so the UI is held to the token's rules and tabs.
func TestUILoginCarriesPrincipal(t *testing.T) {
	uiLoginDelay = 0
	agent := &policy.Principal{Name: "agent"}
	whoami := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(policy.FromContext(r.Context()).Name))
	})
	h := New(Options{
		Token:       "admin-token-0123456789",
		Credentials: []policy.Credential{{Token: "agent-token-0123456789", Principal: agent}},
		Faces:       map[string]core.Face{"/remote": face{whoami}},
		UIPrefix:    "/remote",
	}).Handler()

	for token, want := range map[string]string{"agent-token-0123456789": "agent", "admin-token-0123456789": policy.DefaultTokenName} {
		w := login(t, h, token, nil)
		if w.Code != 303 {
			t.Fatalf("login %s: %d", want, w.Code)
		}
		r := httptest.NewRequest("GET", "/remote/api/tabs", nil)
		for _, c := range w.Result().Cookies() {
			r.AddCookie(c)
		}
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, r)
		if rw.Code != 200 || rw.Body.String() != want {
			t.Fatalf("cookie for %s: %d %q", want, rw.Code, rw.Body.String())
		}
	}
}
