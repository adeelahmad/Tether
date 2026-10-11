package oauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/adeelahmad/tether/internal/httpx"
	"github.com/adeelahmad/tether/internal/logx"
)

// register implements RFC 7591 dynamic client registration.
func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	if !s.dynamic {
		writeError(w, http.StatusNotFound, "invalid_request", "dynamic client registration is disabled")
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	var req struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
		AuthMethod   string   `json:"token_endpoint_auth_method"`
		GrantTypes   []string `json:"grant_types"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil || json.Unmarshal(body, &req) != nil {
		writeError(w, http.StatusBadRequest, "invalid_client_metadata", "request body must be a JSON client metadata object")
		return
	}
	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > 10 {
		writeError(w, http.StatusBadRequest, "invalid_redirect_uri", "give between 1 and 10 redirect_uris")
		return
	}
	for _, u := range req.RedirectURIs {
		if err := checkRedirectURI(u); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_redirect_uri", err.Error())
			return
		}
	}
	for _, g := range req.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			writeError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported grant type "+g)
			return
		}
	}
	method := req.AuthMethod
	switch method {
	case "":
		method = "none"
	case "none", "client_secret_post", "client_secret_basic":
	default:
		writeError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported token_endpoint_auth_method "+method)
		return
	}
	name := req.ClientName
	if len(name) > 100 {
		name = name[:100]
	}
	now := s.now().Unix()
	id, err := s.sign(dynPrefix, dynClient{Name: name, Redirect: req.RedirectURIs, Secret: method != "none", Iat: now})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "could not register client")
		return
	}
	resp := map[string]any{
		"client_id":                  id,
		"client_id_issued_at":        now,
		"client_name":                name,
		"redirect_uris":              req.RedirectURIs,
		"token_endpoint_auth_method": method,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"scope":                      Scope,
	}
	if method != "none" {
		resp["client_secret"] = s.clientSecret(id)
		resp["client_secret_expires_at"] = 0
	}
	s.log.Info("registered oauth client", "name", logx.Clean(name))
	writeJSON(w, http.StatusCreated, resp)
}

// authRequest is a validated authorization request.
type authRequest struct {
	client      Client
	redirectURI string
	state       string
	challenge   string
}

// parseAuthRequest validates the parameters of an authorization request.
// When the client or redirect URI is bad, redirect is false: the error is
// shown to the person instead of being sent to an unverified URI.
func (s *Server) parseAuthRequest(q url.Values) (req authRequest, errCode, errDesc string, redirect bool) {
	c, ok := s.lookupClient(q.Get("client_id"))
	if !ok {
		return req, "invalid_client", "Unknown client. Ask the app to register again.", false
	}
	req.client = c
	ru := q.Get("redirect_uri")
	if ru == "" && len(c.RedirectURIs) == 1 {
		ru = c.RedirectURIs[0]
	}
	if !slices.ContainsFunc(c.RedirectURIs, func(reg string) bool { return redirectMatches(reg, ru) }) {
		return req, "invalid_request", "The redirect URI is not registered for this client.", false
	}
	req.redirectURI = ru
	req.state = q.Get("state")
	if q.Get("response_type") != "code" {
		return req, "unsupported_response_type", "only response_type=code is supported", true
	}
	req.challenge = q.Get("code_challenge")
	if req.challenge == "" || q.Get("code_challenge_method") != "S256" {
		return req, "invalid_request", "PKCE with code_challenge_method=S256 is required", true
	}
	if len(req.challenge) < 43 || len(req.challenge) > 128 {
		return req, "invalid_request", "invalid code_challenge", true
	}
	if sc := q.Get("scope"); sc != "" && !slices.Contains(strings.Fields(sc), Scope) {
		return req, "invalid_scope", "the only scope is " + Scope, true
	}
	return req, "", "", true
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	var q url.Values
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		q = r.URL.Query()
	case http.MethodPost:
		if !httpx.SameOrigin(r) {
			writeConsent(w, http.StatusForbidden, consentPage{Error: "Cross-site request refused."})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		if err := r.ParseForm(); err != nil {
			writeConsent(w, http.StatusBadRequest, consentPage{Error: "Invalid form."})
			return
		}
		q = r.PostForm
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req, code, desc, redirect := s.parseAuthRequest(q)
	if code != "" {
		if !redirect {
			writeConsent(w, http.StatusBadRequest, consentPage{Error: desc})
			return
		}
		s.redirectError(w, r, req, code, desc)
		return
	}
	page := consentPage{
		Client:   req.client.Name,
		Redirect: redirectLabel(req.redirectURI),
		Fields:   hiddenFields(q),
	}
	if page.Client == "" {
		page.Client = "An MCP client"
	}
	if r.Method != http.MethodPost {
		writeConsentFor(w, http.StatusOK, page, req.redirectURI)
		return
	}
	if q.Get("action") == "deny" {
		s.redirectError(w, r, req, "access_denied", "the person declined")
		return
	}
	c, ok := s.match(q.Get("token"))
	if !ok {
		time.Sleep(s.loginDelay)
		page.Error = "That token is not right."
		writeConsentFor(w, http.StatusUnauthorized, page, req.redirectURI)
		return
	}
	codeValue := randomString(32)
	s.mu.Lock()
	now := s.now()
	for k, v := range s.codes {
		if now.After(v.expires) {
			delete(s.codes, k)
		}
	}
	if len(s.codes) >= maxCodes {
		s.mu.Unlock()
		s.redirectError(w, r, req, "temporarily_unavailable", "too many pending authorizations")
		return
	}
	s.codes[codeValue] = &authCode{
		clientID:    req.client.ID,
		redirectURI: req.redirectURI,
		challenge:   req.challenge,
		principal:   c.p.Name,
		fingerprint: s.fingerprint(c),
		expires:     now.Add(codeTTL),
	}
	s.mu.Unlock()
	s.log.Info("oauth grant approved", "client", logx.Clean(page.Client), "principal", c.p.Name)
	s.redirect(w, r, req, url.Values{"code": {codeValue}})
}

func (s *Server) redirectError(w http.ResponseWriter, r *http.Request, req authRequest, code, desc string) {
	s.redirect(w, r, req, url.Values{"error": {code}, "error_description": {desc}})
}

func (s *Server) redirect(w http.ResponseWriter, r *http.Request, req authRequest, params url.Values) {
	u, _ := url.Parse(req.redirectURI) // validated
	q := u.Query()
	for k, vs := range params {
		q[k] = vs
	}
	if req.state != "" {
		q.Set("state", req.state)
	}
	q.Set("iss", s.base(r))
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

// token implements the token endpoint: authorization_code and
// refresh_token grants.
func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid form body")
		return
	}
	f := r.PostForm
	client, ok := s.authenticateClient(r, f)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="tether"`)
		writeError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
		return
	}
	var sub, fp string
	switch f.Get("grant_type") {
	case "authorization_code":
		s.mu.Lock()
		ac := s.codes[f.Get("code")]
		delete(s.codes, f.Get("code")) // single use, even when the exchange fails
		s.mu.Unlock()
		if ac == nil || s.now().After(ac.expires) || ac.clientID != client.ID {
			writeError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired authorization code")
			return
		}
		if ru := f.Get("redirect_uri"); ru != "" && ru != ac.redirectURI {
			writeError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match")
			return
		}
		sum := sha256.Sum256([]byte(f.Get("code_verifier")))
		if subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(ac.challenge)) != 1 {
			writeError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
			return
		}
		sub, fp = ac.principal, ac.fingerprint
	case "refresh_token":
		c, err := s.open(f.Get("refresh_token"), kindRefresh)
		if err != nil || c.CID != client.ID {
			writeError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired refresh token")
			return
		}
		sub, fp = c.Sub, c.FP
	default:
		writeError(w, http.StatusBadRequest, "unsupported_grant_type", "use authorization_code or refresh_token")
		return
	}
	if s.principal(sub, fp) == nil {
		writeError(w, http.StatusBadRequest, "invalid_grant", "the token this grant was approved with is no longer configured")
		return
	}
	access, err1 := s.issue(kindAccess, sub, fp, client.ID, int64(s.accessTTL/time.Second))
	refresh, err2 := s.issue(kindRefresh, sub, fp, client.ID, int64(s.refreshTTL/time.Second))
	if err1 != nil || err2 != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "could not issue tokens")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int64(s.accessTTL / time.Second),
		"refresh_token": refresh,
		"scope":         Scope,
	})
}

// authenticateClient identifies the client at the token endpoint: public
// clients by client_id alone (PKCE protects them), confidential ones by
// secret, via HTTP Basic or the form.
func (s *Server) authenticateClient(r *http.Request, f url.Values) (Client, bool) {
	id, secret, basic := r.BasicAuth()
	if basic {
		if u, err := url.QueryUnescape(id); err == nil {
			id = u
		}
		if u, err := url.QueryUnescape(secret); err == nil {
			secret = u
		}
	} else {
		id, secret = f.Get("client_id"), f.Get("client_secret")
	}
	if fid := f.Get("client_id"); basic && fid != "" && fid != id {
		return Client{}, false
	}
	c, ok := s.lookupClient(id)
	if !ok {
		return Client{}, false
	}
	if c.Secret == "" {
		return c, true
	}
	a := sha256.Sum256([]byte(secret))
	b := sha256.Sum256([]byte(c.Secret))
	return c, subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

type consentPage struct {
	Client   string
	Redirect string
	Fields   map[string]string
	Error    string
}

// hiddenFields carries the authorization request through the form.
func hiddenFields(q url.Values) map[string]string {
	out := map[string]string{}
	for _, k := range []string{"response_type", "client_id", "redirect_uri", "state", "scope",
		"code_challenge", "code_challenge_method", "resource"} {
		if v := q.Get(k); v != "" {
			out[k] = v
		}
	}
	return out
}

func redirectLabel(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if u.Host != "" {
		return u.Host
	}
	return u.Scheme + ":"
}

// formAction is the CSP source the consent form may submit, and so
// redirect, to: this server plus the client's redirect origin.
func formAction(redirectURI string) string {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return "'self'"
	}
	if u.Scheme == "http" || u.Scheme == "https" {
		return "'self' " + u.Scheme + "://" + u.Host
	}
	return "'self' " + u.Scheme + ":"
}

func writeConsent(w http.ResponseWriter, status int, p consentPage) {
	writeConsentFor(w, status, p, "")
}

func writeConsentFor(w http.ResponseWriter, status int, p consentPage, redirectURI string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	fa := "'self'"
	if redirectURI != "" {
		fa = formAction(redirectURI)
	}
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action "+fa+"; frame-ancestors 'none'; base-uri 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = consentTmpl.Execute(w, p)
}

var consentTmpl = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>Tether · authorize</title>
<style>
:root{--bg:#f6f7f9;--fg:#15171a;--muted:#5b6270;--card:#fff;--line:#d9dde3;--accent:#2f6fed;--err:#c4302b}
@media (prefers-color-scheme:dark){:root{--bg:#0f1115;--fg:#e8eaee;--muted:#9aa2b1;--card:#171a20;--line:#2a2f38;--accent:#6b9bff;--err:#ff6b63}}
*{box-sizing:border-box}body{margin:0;min-height:100vh;display:grid;place-items:center;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,-apple-system,Segoe UI,sans-serif;padding:16px}
form,.box{width:100%;max-width:400px;background:var(--card);border:1px solid var(--line);border-radius:12px;padding:28px}
h1{font-size:20px;margin:0 0 4px}p{margin:0 0 16px;color:var(--muted)}strong{color:var(--fg)}
label{display:block;font-weight:600;margin-bottom:6px}
input{width:100%;padding:10px 12px;border:1px solid var(--line);border-radius:8px;background:var(--bg);color:var(--fg);font:inherit}
input:focus{outline:2px solid var(--accent);outline-offset:1px}
.row{display:flex;gap:8px;margin-top:16px}
button{flex:1;padding:10px;border:1px solid var(--line);border-radius:8px;background:transparent;color:var(--fg);font:inherit;font-weight:600;cursor:pointer}
button.primary{background:var(--accent);border-color:var(--accent);color:#fff}
.err{color:var(--err);margin:12px 0 0}
</style></head><body>
{{if .Fields}}<form method="post" autocomplete="on">
<h1>Allow access to your browser?</h1>
<p><strong>{{.Client}}</strong> wants to use your logged-in browser through Tether's MCP server. It will act as the token you enter, with that token's permissions. After you approve, you go back to <strong>{{.Redirect}}</strong>.</p>
{{range $k, $v := .Fields}}<input type="hidden" name="{{$k}}" value="{{$v}}">
{{end}}<input type="text" name="username" value="tether" autocomplete="username" hidden>
<label for="token">Tether token</label>
<input id="token" name="token" type="password" autocomplete="current-password" autofocus>
<div class="row"><button type="submit" name="action" value="deny" formnovalidate>Deny</button>
<button class="primary" type="submit" name="action" value="allow">Allow</button></div>
{{if .Error}}<p class="err" role="alert">{{.Error}}</p>{{end}}
</form>{{else}}<div class="box"><h1>Can't authorize</h1><p class="err" role="alert">{{.Error}}</p></div>{{end}}
</body></html>`))
