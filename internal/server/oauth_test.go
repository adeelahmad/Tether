package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/adeelahmad/tether/internal/core"
	"github.com/adeelahmad/tether/internal/oauth"
	"github.com/adeelahmad/tether/internal/policy"
)

type principalFace struct{}

func (principalFace) Name() string { return "mcp" }
func (principalFace) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Principal", policy.FromContext(r.Context()).OwnerName())
}

var _ core.Face = principalFace{}

// TestOAuthWiring: OAuth endpoints are public, OAuth access tokens open the
// MCP face and nothing else, and MCP 401s point at the metadata.
func TestOAuthWiring(t *testing.T) {
	const token = "admin-token-0123456789"
	creds := []policy.Credential{{Token: token, Principal: &policy.Principal{Name: "default", Admin: true}}}
	o, err := oauth.New(oauth.Options{Key: []byte("0123456789abcdef0123456789abcdef"), Credentials: creds, DynamicRegistration: true})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Options{
		Token: token, Credentials: creds, OAuth: o, MCPPrefix: "/mcp",
		API: okHandler("api"), WebDriver: okHandler("webdriver"),
		Faces: map[string]core.Face{"/mcp": principalFace{}},
	}).Handler()
	serve := func(r *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	w := serve(newReq(http.MethodPost, "/mcp", nil))
	if w.Code != http.StatusUnauthorized ||
		!strings.Contains(w.Header().Get("WWW-Authenticate"), `resource_metadata="http://tether.test/.well-known/oauth-protected-resource/mcp"`) {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	if w := serve(newReq(http.MethodGet, "/.well-known/oauth-authorization-server", nil)); w.Code != http.StatusOK {
		t.Fatalf("metadata behind auth: %d", w.Code)
	}

	// Register, approve, exchange.
	w = serve(newReq(http.MethodPost, "/oauth/register", strings.NewReader(`{"redirect_uris":["http://127.0.0.1/cb"]}`)))
	var reg struct {
		ClientID string `json:"client_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &reg)
	verifier := strings.Repeat("v", 50)
	sum := sha256.Sum256([]byte(verifier))
	f := url.Values{"response_type": {"code"}, "client_id": {reg.ClientID}, "redirect_uri": {"http://127.0.0.1/cb"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}, "token": {token}}
	r := newReq(http.MethodPost, "/oauth/authorize", strings.NewReader(f.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = serve(r)
	loc, _ := url.Parse(w.Header().Get("Location"))
	tf := url.Values{"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")}, "client_id": {reg.ClientID}, "code_verifier": {verifier}}
	r = newReq(http.MethodPost, "/oauth/token", strings.NewReader(tf.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = serve(r)
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &tok); err != nil || tok.AccessToken == "" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}

	r = newReq(http.MethodPost, "/mcp", nil)
	r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	if w := serve(r); w.Code != http.StatusOK || w.Header().Get("X-Principal") != "default" {
		t.Fatalf("mcp with oauth token: %d %v", w.Code, w.Header())
	}
	for _, path := range []string{"/tether/v1/tabs", "/session"} {
		r = newReq(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		if w := serve(r); w.Code != http.StatusUnauthorized {
			t.Fatalf("%s accepted an oauth token: %d", path, w.Code)
		}
	}
	// The plain token still works on /mcp.
	r = newReq(http.MethodPost, "/mcp", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	if w := serve(r); w.Code != http.StatusOK {
		t.Fatalf("%d", w.Code)
	}
}

// newReq builds an origin-form request (not a proxy request) for tether.test.
func newReq(method, path string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, path, body)
	r.Host = "tether.test"
	return r
}
