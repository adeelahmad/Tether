package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adeelahmad/tether/internal/policy"
)

const (
	adminToken = "admin-token-0123456789"
	agentToken = "agent-token-0123456789"
	verifier   = "a-pkce-code-verifier-that-is-long-enough-0123456789"
)

var (
	admin = &policy.Principal{Name: "default", Admin: true, Scripts: true}
	agent = &policy.Principal{Name: "agent"}
)

func challenge(v string) string {
	s := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(s[:])
}

func newServer(t *testing.T, mutate func(*Options)) *Server {
	t.Helper()
	opts := Options{
		Key:                 []byte("0123456789abcdef0123456789abcdef"),
		Credentials:         []policy.Credential{{Token: adminToken, Principal: admin}, {Token: agentToken, Principal: agent}},
		DynamicRegistration: true,
		Clients:             []Client{{ID: "static", Secret: "static-secret-0123456789", RedirectURIs: []string{"https://app.example/cb"}}},
	}
	if mutate != nil {
		mutate(&opts)
	}
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	s.loginDelay = 0
	return s
}

func do(t *testing.T, s *Server, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func postForm(path string, f url.Values) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "http://tether.test"+path, strings.NewReader(f.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("%d %s: %v", w.Code, w.Body, err)
	}
	return m
}

func register(t *testing.T, s *Server, body string) map[string]any {
	t.Helper()
	w := do(t, s, httptest.NewRequest(http.MethodPost, "http://tether.test"+PathRegister, strings.NewReader(body)))
	if w.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", w.Code, w.Body)
	}
	return decode(t, w)
}

func authParams(clientID, redirect string) url.Values {
	return url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect},
		"code_challenge": {challenge(verifier)}, "code_challenge_method": {"S256"},
		"state": {"xyz"}, "scope": {"mcp"},
	}
}

// approve runs the consent POST and returns the redirect's query.
func approve(t *testing.T, s *Server, params url.Values, token string) url.Values {
	t.Helper()
	f := url.Values{}
	for k, v := range params {
		f[k] = v
	}
	f.Set("token", token)
	f.Set("action", "allow")
	r := postForm(PathAuthorize, f)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	w := do(t, s, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("authorize: %d %s", w.Code, w.Body)
	}
	u, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

func TestMetadata(t *testing.T) {
	s := newServer(t, nil)
	w := do(t, s, httptest.NewRequest(http.MethodGet, "http://tether.test"+PathResourceMetadata+"/mcp", nil))
	m := decode(t, w)
	if m["resource"] != "http://tether.test/mcp" || m["authorization_servers"].([]any)[0] != "http://tether.test" {
		t.Fatalf("%v", m)
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("metadata needs CORS for browser-based clients")
	}
	r := httptest.NewRequest(http.MethodGet, "http://tether.test"+PathServerMetadata, nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	m = decode(t, do(t, s, r))
	if m["issuer"] != "https://tether.test" || m["token_endpoint"] != "https://tether.test/oauth/token" ||
		m["registration_endpoint"] != "https://tether.test/oauth/register" {
		t.Fatalf("%v", m)
	}
	if !strings.Contains(s.Challenge(r), `resource_metadata="https://tether.test/.well-known/oauth-protected-resource/mcp"`) {
		t.Fatal(s.Challenge(r))
	}

	s = newServer(t, func(o *Options) { o.PublicURL = "https://pub.example/"; o.DynamicRegistration = false })
	m = decode(t, do(t, s, httptest.NewRequest(http.MethodGet, "http://tether.test"+PathServerMetadata, nil)))
	if m["issuer"] != "https://pub.example" || m["registration_endpoint"] != nil {
		t.Fatalf("%v", m)
	}
}

func TestDynamicClientFlow(t *testing.T) {
	s := newServer(t, nil)
	reg := register(t, s, `{"redirect_uris":["http://127.0.0.1:3000/cb"],"client_name":"Test <Agent>"}`)
	id := reg["client_id"].(string)
	if reg["token_endpoint_auth_method"] != "none" || reg["client_secret"] != nil {
		t.Fatalf("%v", reg)
	}

	// The consent page names the client, escaped, and carries the request.
	params := authParams(id, "http://127.0.0.1:49152/cb") // loopback: any port
	w := do(t, s, httptest.NewRequest(http.MethodGet, "http://tether.test"+PathAuthorize+"?"+params.Encode(), nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Test &lt;Agent&gt;") ||
		!strings.Contains(w.Body.String(), `name="code_challenge"`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self' http://127.0.0.1:49152") {
		t.Fatal(csp)
	}

	q := approve(t, s, params, agentToken)
	if q.Get("state") != "xyz" || q.Get("code") == "" || q.Get("iss") != "http://tether.test" {
		t.Fatalf("%v", q)
	}
	tok := decode(t, do(t, s, postForm(PathToken, url.Values{
		"grant_type": {"authorization_code"}, "code": {q.Get("code")}, "client_id": {id},
		"redirect_uri": {params.Get("redirect_uri")}, "code_verifier": {verifier},
	})))
	access, _ := tok["access_token"].(string)
	if s.Verify(access) != agent || tok["token_type"] != "Bearer" {
		t.Fatalf("%v", tok)
	}

	// Codes are single use.
	w = do(t, s, postForm(PathToken, url.Values{
		"grant_type": {"authorization_code"}, "code": {q.Get("code")}, "client_id": {id}, "code_verifier": {verifier},
	}))
	if w.Code != http.StatusBadRequest || decode(t, w)["error"] != "invalid_grant" {
		t.Fatalf("reused code: %d %s", w.Code, w.Body)
	}

	// Refresh gives a new working access token.
	tok2 := decode(t, do(t, s, postForm(PathToken, url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {tok["refresh_token"].(string)}, "client_id": {id},
	})))
	if s.Verify(tok2["access_token"].(string)) != agent {
		t.Fatalf("%v", tok2)
	}
	// A refresh token is not an access token, nor the other way round.
	if s.Verify(tok["refresh_token"].(string)) != nil {
		t.Fatal("refresh token accepted as access token")
	}
	w = do(t, s, postForm(PathToken, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {access}, "client_id": {id}}))
	if w.Code != http.StatusBadRequest {
		t.Fatal("access token accepted as refresh token")
	}
}

func TestTokenExpiryAndRevocation(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := newServer(t, func(o *Options) { o.AccessTTL = time.Minute })
	s.now = func() time.Time { return now }
	id := register(t, s, `{"redirect_uris":["https://app.example/cb"]}`)["client_id"].(string)
	q := approve(t, s, authParams(id, "https://app.example/cb"), adminToken)
	tok := decode(t, do(t, s, postForm(PathToken, url.Values{
		"grant_type": {"authorization_code"}, "code": {q.Get("code")}, "client_id": {id}, "code_verifier": {verifier},
	})))
	access := tok["access_token"].(string)
	if s.Verify(access) != admin {
		t.Fatal("fresh token rejected")
	}
	now = now.Add(2 * time.Minute)
	if s.Verify(access) != nil {
		t.Fatal("expired token accepted")
	}

	// A server whose admin token changed no longer honours the grant.
	rotated := newServer(t, func(o *Options) {
		o.Credentials = []policy.Credential{{Token: "a-brand-new-token-0123", Principal: admin}}
	})
	rotated.now = func() time.Time { return now.Add(-2 * time.Minute) }
	if rotated.Verify(access) != nil {
		t.Fatal("grant survived a token change")
	}
	w := do(t, rotated, postForm(PathToken, url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {tok["refresh_token"].(string)}, "client_id": {id},
	}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("refresh after token change: %d %s", w.Code, w.Body)
	}

	// A different signing key knows nothing of this server's tokens.
	other := newServer(t, func(o *Options) { o.Key = []byte("another-key-another-key-another-key") })
	other.now = rotated.now
	if other.Verify(access) != nil {
		t.Fatal("token verified under another key")
	}
}

func TestAuthorizeRejects(t *testing.T) {
	s := newServer(t, nil)
	id := register(t, s, `{"redirect_uris":["https://app.example/cb"]}`)["client_id"].(string)
	good := authParams(id, "https://app.example/cb")
	with := func(k, v string) url.Values {
		q := url.Values{}
		for kk, vv := range good {
			q[kk] = vv
		}
		if v == "" {
			q.Del(k)
		} else {
			q.Set(k, v)
		}
		return q
	}
	tests := []struct {
		name     string
		q        url.Values
		wantCode int
		wantErr  string // in the redirect, when there is one
	}{
		{"unknown client", with("client_id", "tcl_forged.sig"), http.StatusBadRequest, ""},
		{"unregistered redirect", with("redirect_uri", "https://evil.example/cb"), http.StatusBadRequest, ""},
		{"no pkce", with("code_challenge", ""), http.StatusSeeOther, "invalid_request"},
		{"plain pkce", with("code_challenge_method", "plain"), http.StatusSeeOther, "invalid_request"},
		{"wrong response type", with("response_type", "token"), http.StatusSeeOther, "unsupported_response_type"},
		{"wrong scope", with("scope", "admin"), http.StatusSeeOther, "invalid_scope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := do(t, s, httptest.NewRequest(http.MethodGet, "http://tether.test"+PathAuthorize+"?"+tt.q.Encode(), nil))
			if w.Code != tt.wantCode {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			if tt.wantErr != "" {
				u, _ := url.Parse(w.Header().Get("Location"))
				if u.Host != "app.example" || u.Query().Get("error") != tt.wantErr {
					t.Fatalf("%v", u)
				}
			}
		})
	}

	// Wrong token: the form comes back, no redirect.
	f := with("token", "not-the-token")
	r := postForm(PathAuthorize, f)
	if w := do(t, s, r); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "not right") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	// Deny redirects with access_denied.
	f = with("action", "deny")
	if w := do(t, s, postForm(PathAuthorize, f)); !strings.Contains(w.Header().Get("Location"), "error=access_denied") {
		t.Fatalf("%v", w.Header())
	}
	// Cross-site posts are refused.
	r = postForm(PathAuthorize, with("token", adminToken))
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if w := do(t, s, r); w.Code != http.StatusForbidden {
		t.Fatalf("%d", w.Code)
	}
}

func TestTokenEndpointRejects(t *testing.T) {
	s := newServer(t, nil)
	params := authParams("static", "https://app.example/cb")
	code := func() string { return approve(t, s, params, adminToken).Get("code") }

	tests := []struct {
		name string
		f    url.Values
		auth [2]string
		want string
		code int
	}{
		{"missing secret", url.Values{"grant_type": {"authorization_code"}, "code": {code()}, "client_id": {"static"}, "code_verifier": {verifier}},
			[2]string{}, "invalid_client", http.StatusUnauthorized},
		{"wrong secret", url.Values{"grant_type": {"authorization_code"}, "code": {code()}, "code_verifier": {verifier}},
			[2]string{"static", "nope"}, "invalid_client", http.StatusUnauthorized},
		{"wrong verifier", url.Values{"grant_type": {"authorization_code"}, "code": {code()}, "code_verifier": {"wrong-verifier-wrong-verifier-wrong-verifier-00"}},
			[2]string{"static", "static-secret-0123456789"}, "invalid_grant", http.StatusBadRequest},
		{"wrong redirect", url.Values{"grant_type": {"authorization_code"}, "code": {code()}, "code_verifier": {verifier}, "redirect_uri": {"https://app.example/other"}},
			[2]string{"static", "static-secret-0123456789"}, "invalid_grant", http.StatusBadRequest},
		{"unknown grant", url.Values{"grant_type": {"password"}},
			[2]string{"static", "static-secret-0123456789"}, "unsupported_grant_type", http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := postForm(PathToken, tt.f)
			if tt.auth[0] != "" {
				r.SetBasicAuth(tt.auth[0], tt.auth[1])
			}
			w := do(t, s, r)
			if w.Code != tt.code || decode(t, w)["error"] != tt.want {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}

	// The happy path with HTTP Basic client auth.
	r := postForm(PathToken, url.Values{"grant_type": {"authorization_code"}, "code": {code()}, "code_verifier": {verifier}})
	r.SetBasicAuth("static", "static-secret-0123456789")
	if w := do(t, s, r); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestRegisterRejects(t *testing.T) {
	s := newServer(t, nil)
	for _, body := range []string{
		`{}`,
		`not json`,
		`{"redirect_uris":["http://evil.example/cb"]}`,
		`{"redirect_uris":["javascript:alert(1)"]}`,
		`{"redirect_uris":["https://app.example/cb#frag"]}`,
		`{"redirect_uris":["https://app.example/cb"],"grant_types":["client_credentials"]}`,
		`{"redirect_uris":["https://app.example/cb"],"token_endpoint_auth_method":"private_key_jwt"}`,
	} {
		w := do(t, s, httptest.NewRequest(http.MethodPost, "http://tether.test"+PathRegister, strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, w.Code, w.Body)
		}
	}
	// Confidential dynamic clients get a secret that works.
	reg := register(t, s, `{"redirect_uris":["cursor://anysphere.cursor-retrieval/oauth/callback"],"token_endpoint_auth_method":"client_secret_post"}`)
	id, secret := reg["client_id"].(string), reg["client_secret"].(string)
	q := approve(t, s, authParams(id, "cursor://anysphere.cursor-retrieval/oauth/callback"), adminToken)
	w := do(t, s, postForm(PathToken, url.Values{
		"grant_type": {"authorization_code"}, "code": {q.Get("code")}, "client_id": {id}, "client_secret": {secret}, "code_verifier": {verifier},
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}

	off := newServer(t, func(o *Options) { o.DynamicRegistration = false })
	if w := do(t, off, httptest.NewRequest(http.MethodPost, "http://tether.test"+PathRegister, strings.NewReader(`{"redirect_uris":["https://a.example/cb"]}`))); w.Code != http.StatusNotFound {
		t.Fatalf("%d", w.Code)
	}
	if _, ok := off.lookupClient(id); ok {
		t.Fatal("dynamic client accepted with registration off")
	}
}

func TestNewValidates(t *testing.T) {
	for name, mutate := range map[string]func(*Options){
		"short key":      func(o *Options) { o.Key = []byte("short") },
		"no credentials": func(o *Options) { o.Credentials = nil },
		"bad redirect":   func(o *Options) { o.Clients = []Client{{ID: "x", RedirectURIs: []string{"http://evil.example/"}}} },
		"no redirect":    func(o *Options) { o.Clients = []Client{{ID: "x"}} },
		"reserved id":    func(o *Options) { o.Clients = []Client{{ID: "tcl_x", RedirectURIs: []string{"https://a.example/"}}} },
		"duplicate id": func(o *Options) {
			o.Clients = []Client{{ID: "x", RedirectURIs: []string{"https://a.example/"}}, {ID: "x", RedirectURIs: []string{"https://a.example/"}}}
		},
	} {
		opts := Options{Key: []byte("0123456789abcdef0123456789abcdef"), Credentials: []policy.Credential{{Token: adminToken, Principal: admin}}}
		mutate(&opts)
		if _, err := New(opts); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestLoadOrCreateKey(t *testing.T) {
	dir := t.TempDir()
	k1, err := LoadOrCreateKey(dir)
	if err != nil || len(k1) != 32 {
		t.Fatal(err)
	}
	k2, err := LoadOrCreateKey(dir)
	if err != nil || string(k1) != string(k2) {
		t.Fatal("key not persisted")
	}
	if st, _ := os.Stat(filepath.Join(dir, keyFile)); st.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("key file mode %v", st.Mode())
	}
	if err := os.WriteFile(filepath.Join(dir, keyFile), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(dir); err == nil {
		t.Fatal("garbage key accepted")
	}
}
