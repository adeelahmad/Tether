package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/adeelahmad/tether/internal/httpx"
	"github.com/adeelahmad/tether/internal/policy"
)

// UI login. Browsers cannot attach a bearer token to page loads, image
// requests or websocket handshakes, so for the remote UI the server trades
// the token, once, for a cookie:
//
//   - HttpOnly, so page scripts (including a compromised one) cannot read it;
//   - SameSite=Strict, so no other site can make the browser send it;
//   - Path-scoped to the UI prefix, and accepted only there: the cookie
//     never authenticates the proxy, the control API or WebDriver;
//   - an HMAC of the token, so it dies when the token changes and the
//     token itself is never stored in the browser.

// UICookieName is the remote UI's login cookie.
const UICookieName = "tether_ui"

// uiLoginDelay slows down token guessing on the login form.
var uiLoginDelay = 500 * time.Millisecond

// uiCookieValue is the cookie for one credential: an HMAC of its token,
// so each token's cookie is distinct and maps back to its principal.
func uiCookieValue(c credential) string {
	m := hmac.New(sha256.New, c.sum[:])
	m.Write([]byte("tether-remote-ui-v1"))
	return hex.EncodeToString(m.Sum(nil))
}

// uiCookie returns the principal whose login cookie r carries, or nil.
// Every credential is compared, in constant time.
func (a *authenticator) uiCookie(r *http.Request) *policy.Principal {
	ck, err := r.Cookie(UICookieName)
	if err != nil {
		return nil
	}
	var found *policy.Principal
	for _, c := range a.creds {
		if hmac.Equal([]byte(ck.Value), []byte(uiCookieValue(c))) {
			found = c.p
		}
	}
	return found
}

func (a *authenticator) credentialFor(p *policy.Principal) (credential, bool) {
	for _, c := range a.creds {
		if c.p == p {
			return c, true
		}
	}
	return credential{}, false
}

func underPrefix(path, prefix string) bool {
	return prefix != "" && (path == prefix || strings.HasPrefix(path, prefix+"/"))
}

// uiGate authenticates requests under the UI prefix: bearer token or UI
// cookie, with login/logout handled here. a may be nil (no token
// configured), in which case login is unnecessary and only redirects.
func uiGate(a *authenticator, prefix string, w http.ResponseWriter, r *http.Request, next http.Handler) {
	switch r.URL.Path {
	case prefix + "/login":
		if a == nil {
			http.Redirect(w, r, prefix+"/", http.StatusSeeOther)
			return
		}
		a.login(prefix, w, r)
		return
	case prefix + "/logout":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: UICookieName, Value: "", Path: prefix, MaxAge: -1,
			HttpOnly: true, Secure: httpx.IsHTTPS(r), SameSite: http.SameSiteStrictMode,
		})
		http.Redirect(w, r, prefix+"/login", http.StatusSeeOther)
		return
	}
	if a == nil {
		next.ServeHTTP(w, r)
		return
	}
	p := a.check(r.Header.Get("Authorization"))
	if p == nil {
		p = a.uiCookie(r)
	}
	if p != nil {
		next.ServeHTTP(w, r.WithContext(policy.WithPrincipal(r.Context(), p)))
		return
	}
	if r.Method == http.MethodGet && (r.URL.Path == prefix || r.URL.Path == prefix+"/") {
		http.Redirect(w, r, prefix+"/login", http.StatusSeeOther)
		return
	}
	a.deny(w)
}

func (a *authenticator) login(prefix string, w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if a.uiCookie(r) != nil {
			http.Redirect(w, r, prefix+"/", http.StatusSeeOther)
			return
		}
		writeLoginPage(w, http.StatusOK, "")
	case http.MethodPost:
		if !httpx.SameOrigin(r) {
			writeLoginPage(w, http.StatusForbidden, "Cross-site login refused.")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := r.ParseForm(); err != nil {
			writeLoginPage(w, http.StatusBadRequest, "Invalid form.")
			return
		}
		p := a.match(r.PostForm.Get("token"))
		c, ok := a.credentialFor(p)
		if p == nil || !ok {
			time.Sleep(uiLoginDelay)
			writeLoginPage(w, http.StatusUnauthorized, "That token is not right.")
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     UICookieName,
			Value:    uiCookieValue(c),
			Path:     prefix,
			MaxAge:   int((30 * 24 * time.Hour).Seconds()),
			HttpOnly: true,
			Secure:   httpx.IsHTTPS(r),
			SameSite: http.SameSiteStrictMode,
		})
		http.Redirect(w, r, prefix+"/", http.StatusSeeOther)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

var loginPage = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>Tether · sign in</title>
<style>
:root{--bg:#f6f7f9;--fg:#15171a;--muted:#5b6270;--card:#fff;--line:#d9dde3;--accent:#2f6fed;--err:#c4302b}
@media (prefers-color-scheme:dark){:root{--bg:#0f1115;--fg:#e8eaee;--muted:#9aa2b1;--card:#171a20;--line:#2a2f38;--accent:#6b9bff;--err:#ff6b63}}
*{box-sizing:border-box}body{margin:0;min-height:100vh;display:grid;place-items:center;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,-apple-system,Segoe UI,sans-serif;padding:16px}
form{width:100%;max-width:360px;background:var(--card);border:1px solid var(--line);border-radius:12px;padding:28px}
h1{font-size:20px;margin:0 0 4px}p{margin:0 0 20px;color:var(--muted)}
label{display:block;font-weight:600;margin-bottom:6px}
input{width:100%;padding:10px 12px;border:1px solid var(--line);border-radius:8px;background:var(--bg);color:var(--fg);font:inherit}
input:focus{outline:2px solid var(--accent);outline-offset:1px}
button{margin-top:16px;width:100%;padding:10px;border:0;border-radius:8px;background:var(--accent);color:#fff;font:inherit;font-weight:600;cursor:pointer}
.err{color:var(--err);margin:12px 0 0}
</style></head><body>
<form method="post" autocomplete="on">
<h1>Tether</h1>
<p>Enter your Tether token to open the remote browser.</p>
<input type="text" name="username" value="tether" autocomplete="username" hidden>
<label for="token">Token</label>
<input id="token" name="token" type="password" autocomplete="current-password" required autofocus>
<button type="submit">Open remote browser</button>
{{if .}}<p class="err" role="alert">{{.}}</p>{{end}}
</form></body></html>`))

func writeLoginPage(w http.ResponseWriter, status int, msg string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin") // no-referrer would turn the form's Origin into "null"
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = loginPage.Execute(w, msg)
}
