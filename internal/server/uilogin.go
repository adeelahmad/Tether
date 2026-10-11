package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"
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

func (a *authenticator) uiCookieValue() string {
	m := hmac.New(sha256.New, a.sum[:])
	m.Write([]byte("tether-remote-ui-v1"))
	return hex.EncodeToString(m.Sum(nil))
}

func (a *authenticator) uiCookieOK(r *http.Request) bool {
	c, err := r.Cookie(UICookieName)
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(c.Value), []byte(a.uiCookieValue()))
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
			HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteStrictMode,
		})
		http.Redirect(w, r, prefix+"/login", http.StatusSeeOther)
		return
	}
	if a == nil || a.check(r.Header.Get("Authorization")) || a.uiCookieOK(r) {
		next.ServeHTTP(w, r)
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
		if a.uiCookieOK(r) {
			http.Redirect(w, r, prefix+"/", http.StatusSeeOther)
			return
		}
		writeLoginPage(w, http.StatusOK, "")
	case http.MethodPost:
		if !sameOrigin(r) {
			writeLoginPage(w, http.StatusForbidden, "Cross-site login refused.")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := r.ParseForm(); err != nil {
			writeLoginPage(w, http.StatusBadRequest, "Invalid form.")
			return
		}
		if !a.match(r.PostForm.Get("token")) {
			time.Sleep(uiLoginDelay)
			writeLoginPage(w, http.StatusUnauthorized, "That token is not right.")
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     UICookieName,
			Value:    a.uiCookieValue(),
			Path:     prefix,
			MaxAge:   int((30 * 24 * time.Hour).Seconds()),
			HttpOnly: true,
			Secure:   isHTTPS(r),
			SameSite: http.SameSiteStrictMode,
		})
		http.Redirect(w, r, prefix+"/", http.StatusSeeOther)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// isHTTPS reports whether the browser reached us over HTTPS, directly or
// through a TLS-terminating proxy such as `tailscale serve`. Cookies are
// only marked Secure then: Tether usually serves plain HTTP on loopback or
// a tailnet, where browsers refuse to store Secure cookies at all.
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// sameOrigin reports whether a form post came from a page on this server
// (directly or via a reverse proxy that forwards the host). Modern browsers
// say so in Sec-Fetch-Site; otherwise the Origin must match. A request with
// neither (curl, very old browsers) cannot be a cross-site form post from a
// modern browser.
func sameOrigin(r *http.Request) bool {
	if sf := r.Header.Get("Sec-Fetch-Site"); sf != "" {
		return sf == "same-origin" || sf == "none"
	}
	o := r.Header.Get("Origin")
	if o == "" || o == "null" {
		return o == ""
	}
	u, err := url.Parse(o)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host) ||
		(r.Header.Get("X-Forwarded-Host") != "" && strings.EqualFold(u.Host, r.Header.Get("X-Forwarded-Host")))
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
<p>Enter the token this server was started with to open the remote browser.</p>
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
