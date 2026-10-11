// Package oauth is a small, self-contained OAuth 2.1 authorization server
// for Tether's MCP face, so that MCP clients which only speak the MCP
// authorization spec (claude.ai connectors, Claude Desktop, IDEs) can
// connect without anyone pasting a bearer token into them.
//
// The flow is the standard one: the client discovers the server through
// protected-resource and authorization-server metadata (RFC 9728, RFC
// 8414), registers itself (RFC 7591) or uses a client from the config,
// and runs the authorization-code flow with PKCE (S256 only). The consent
// page asks the person for a Tether token; the grant then acts as that
// token's principal, with its admin, script and domain rules. OAuth
// tokens authenticate the MCP endpoint only, never the proxy, WebDriver
// or the control API.
//
// The server keeps almost no state. Registered clients, access tokens and
// refresh tokens are HMAC-signed with a key kept in the data directory,
// so they survive restarts; authorization codes live in memory for a
// minute. Grants die when the token they were approved with is removed or
// changed. Deleting the key file revokes everything.
package oauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/adeelahmad/tether/internal/policy"
)

// Endpoint paths. The well-known paths are fixed by the RFCs.
const (
	PathResourceMetadata = "/.well-known/oauth-protected-resource"
	PathServerMetadata   = "/.well-known/oauth-authorization-server"
	PathOpenIDMetadata   = "/.well-known/openid-configuration"
	PathAuthorize        = "/oauth/authorize"
	PathToken            = "/oauth/token" //nolint:gosec // a URL path, not a credential
	PathRegister         = "/oauth/register"
)

// Scope is the only scope Tether grants.
const Scope = "mcp"

const (
	defaultAccessTTL  = time.Hour
	defaultRefreshTTL = 30 * 24 * time.Hour
	codeTTL           = time.Minute
	maxCodes          = 1024
	keyFile           = "oauth.key"
)

// Client is a statically configured OAuth client.
type Client struct {
	ID           string
	Secret       string // empty: a public client, PKCE only
	Name         string
	RedirectURIs []string
}

// Options configure a Server.
type Options struct {
	// Key signs clients and tokens; at least 32 bytes. See LoadOrCreateKey.
	Key []byte
	// Credentials are the Tether tokens a person may approve a grant with.
	Credentials []policy.Credential
	// PublicURL is the externally visible base URL, e.g.
	// https://tether.tail1234.ts.net. Empty derives it from each request.
	PublicURL string
	// Resource is the path of the protected resource, e.g. "/mcp".
	Resource   string
	AccessTTL  time.Duration
	RefreshTTL time.Duration
	// DynamicRegistration lets clients register themselves (RFC 7591).
	DynamicRegistration bool
	Clients             []Client
	Logger              *slog.Logger
}

// Server is the authorization server.
type Server struct {
	key        []byte
	creds      []cred
	publicURL  string
	resource   string
	accessTTL  time.Duration
	refreshTTL time.Duration
	dynamic    bool
	clients    map[string]Client
	log        *slog.Logger
	now        func() time.Time
	// loginDelay slows down token guessing on the consent page.
	loginDelay time.Duration

	mu    sync.Mutex
	codes map[string]*authCode
}

type cred struct {
	sum [32]byte
	p   *policy.Principal
}

type authCode struct {
	clientID    string
	redirectURI string
	challenge   string
	principal   string
	fingerprint string
	expires     time.Time
}

// New returns a Server.
func New(opts Options) (*Server, error) {
	if len(opts.Key) < 32 {
		return nil, errors.New("oauth: signing key must be at least 32 bytes")
	}
	if len(opts.Credentials) == 0 {
		return nil, errors.New("oauth: needs at least one token to approve grants with")
	}
	s := &Server{
		key:        opts.Key,
		publicURL:  strings.TrimRight(opts.PublicURL, "/"),
		resource:   opts.Resource,
		accessTTL:  opts.AccessTTL,
		refreshTTL: opts.RefreshTTL,
		dynamic:    opts.DynamicRegistration,
		clients:    map[string]Client{},
		log:        opts.Logger,
		now:        time.Now,
		loginDelay: 500 * time.Millisecond,
		codes:      map[string]*authCode{},
	}
	if s.resource == "" {
		s.resource = "/mcp"
	}
	if s.accessTTL <= 0 {
		s.accessTTL = defaultAccessTTL
	}
	if s.refreshTTL <= 0 {
		s.refreshTTL = defaultRefreshTTL
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	s.log = s.log.With("component", "oauth")
	for _, c := range opts.Credentials {
		s.creds = append(s.creds, cred{sum: sha256.Sum256([]byte(c.Token)), p: c.Principal})
	}
	for _, c := range opts.Clients {
		if c.ID == "" || strings.HasPrefix(c.ID, dynPrefix) {
			return nil, fmt.Errorf("oauth: invalid client id %q", c.ID)
		}
		if _, dup := s.clients[c.ID]; dup {
			return nil, fmt.Errorf("oauth: duplicate client id %q", c.ID)
		}
		if len(c.RedirectURIs) == 0 {
			return nil, fmt.Errorf("oauth: client %q needs at least one redirect URI", c.ID)
		}
		for _, u := range c.RedirectURIs {
			if err := checkRedirectURI(u); err != nil {
				return nil, fmt.Errorf("oauth: client %q: %w", c.ID, err)
			}
		}
		s.clients[c.ID] = c
	}
	return s, nil
}

// LoadOrCreateKey reads the signing key from dir, creating a random one
// (mode 0600) the first time.
func LoadOrCreateKey(dir string) ([]byte, error) {
	p := filepath.Join(dir, keyFile)
	if b, err := os.ReadFile(p); err == nil {
		k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(k) < 32 {
			return nil, fmt.Errorf("oauth: %s is not a valid key; delete it to make a new one", p)
		}
		return k, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("oauth: reading key: %w", err)
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(p, []byte(base64.StdEncoding.EncodeToString(k)+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("oauth: writing key: %w", err)
	}
	return k, nil
}

// Owns reports whether path is one of the server's public endpoints,
// which are served without authentication.
func (s *Server) Owns(path string) bool {
	switch path {
	case PathServerMetadata, PathOpenIDMetadata, PathAuthorize, PathToken, PathRegister:
		return true
	}
	return path == PathResourceMetadata || strings.HasPrefix(path, PathResourceMetadata+"/")
}

// ServeHTTP serves the endpoints Owns reports.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == PathAuthorize {
		s.authorize(w, r)
		return
	}
	// The rest are machine endpoints that browser-based MCP clients call
	// cross-origin. None of them uses cookies, so open CORS is safe.
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Mcp-Protocol-Version")
	h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch r.URL.Path {
	case PathServerMetadata, PathOpenIDMetadata:
		s.serverMetadata(w, r)
	case PathToken:
		s.token(w, r)
	case PathRegister:
		s.register(w, r)
	default:
		s.resourceMetadata(w, r)
	}
}

// Challenge is the WWW-Authenticate value for an unauthenticated request
// to the protected resource: it points the client at the metadata.
func (s *Server) Challenge(r *http.Request) string {
	return fmt.Sprintf(`Bearer realm="tether", resource_metadata=%q`, s.base(r)+PathResourceMetadata+s.resource)
}

// Verify returns the principal an access token acts as, or nil.
func (s *Server) Verify(token string) *policy.Principal {
	c, err := s.open(token, kindAccess)
	if err != nil {
		return nil
	}
	return s.principal(c.Sub, c.FP)
}

// base is the issuer: the configured public URL, or this request's origin.
func (s *Server) base(r *http.Request) string {
	if s.publicURL != "" {
		return s.publicURL
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) resourceMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET")
		return
	}
	b := s.base(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 b + s.resource,
		"authorization_servers":    []string{b},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         []string{Scope},
		"resource_name":            "Tether",
	})
}

func (s *Server) serverMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET")
		return
	}
	b := s.base(r)
	m := map[string]any{
		"issuer":                                         b,
		"authorization_endpoint":                         b + PathAuthorize,
		"token_endpoint":                                 b + PathToken,
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none", "client_secret_post", "client_secret_basic"},
		"scopes_supported":                               []string{Scope},
		"authorization_response_iss_parameter_supported": true,
	}
	if s.dynamic {
		m["registration_endpoint"] = b + PathRegister
	}
	writeJSON(w, http.StatusOK, m)
}

// principal resolves a grant's subject, provided the token it was approved
// with is still configured unchanged.
func (s *Server) principal(name, fp string) *policy.Principal {
	for _, c := range s.creds {
		if c.p.Name == name && hmac.Equal([]byte(s.fingerprint(c)), []byte(fp)) {
			return c.p
		}
	}
	return nil
}

// fingerprint ties a grant to the exact token it was approved with,
// without putting anything derived from the token alone into the grant.
func (s *Server) fingerprint(c cred) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte("fp\x00"))
	m.Write(c.sum[:])
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:12])
}

// match finds the credential for a token typed on the consent page,
// comparing against every token in constant time.
func (s *Server) match(token string) (cred, bool) {
	sum := sha256.Sum256([]byte(token))
	var found cred
	ok := false
	for _, c := range s.creds {
		if subtle.ConstantTimeCompare(sum[:], c.sum[:]) == 1 {
			found, ok = c, true
		}
	}
	return found, ok
}

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes an RFC 6749 error response.
func writeError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, "invalid_request", "method not allowed")
}

// checkRedirectURI accepts https URIs, http only on loopback (RFC 8252),
// and private-use schemes for native apps (cursor://, vscode://).
func checkRedirectURI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Fragment != "" {
		return fmt.Errorf("invalid redirect URI %q", raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		if u.Host == "" {
			return fmt.Errorf("invalid redirect URI %q", raw)
		}
	case "http":
		if !isLoopback(u.Hostname()) {
			return fmt.Errorf("redirect URI %q: plain http is only allowed on loopback", raw)
		}
	case "javascript", "data", "file", "vbscript", "blob", "about":
		return fmt.Errorf("redirect URI %q: scheme not allowed", raw)
	}
	return nil
}

func isLoopback(host string) bool {
	switch strings.ToLower(strings.Trim(host, "[]")) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// redirectMatches compares a requested redirect URI with a registered one.
// Loopback URIs match on any port, as RFC 8252 requires for native apps.
func redirectMatches(registered, requested string) bool {
	if registered == requested {
		return true
	}
	a, err1 := url.Parse(registered)
	b, err2 := url.Parse(requested)
	if err1 != nil || err2 != nil || a.Scheme != "http" || b.Scheme != "http" {
		return false
	}
	return isLoopback(a.Hostname()) && a.Hostname() == b.Hostname() &&
		a.Path == b.Path && a.RawQuery == b.RawQuery
}
