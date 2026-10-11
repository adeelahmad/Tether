package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/adeelahmad/tether/internal/cdp/cdptest"
	"github.com/adeelahmad/tether/internal/certs"
	"github.com/adeelahmad/tether/internal/core"
	"github.com/adeelahmad/tether/internal/session"
)

type fixture struct {
	browser *cdptest.Browser
	core    *core.Core
	srv     *httptest.Server
	ca      *certs.Authority
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	b := cdptest.NewBrowser()
	m := session.NewManager(b, session.Options{})
	c := core.New(b, m, core.Options{Timeout: 5 * time.Second})
	ca, err := certs.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(c, ca, nil))
	t.Cleanup(srv.Close)
	return &fixture{browser: b, core: c, srv: srv, ca: ca}
}

func (f *fixture) client(t *testing.T) *http.Client {
	t.Helper()
	pu, _ := url.Parse(f.srv.URL)
	pool := x509.NewCertPool()
	pool.AddCert(f.ca.Cert)
	return &http.Client{
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(pu),
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       10 * time.Second,
	}
}

func get(t *testing.T, c *http.Client, rawURL string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, rawURL, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestHTTPProxyFirstContactAndReuse(t *testing.T) {
	f := newFixture(t)
	f.browser.Pages["http://shop.example.co.uk/account"] = cdptest.Page{Status: 200, MimeType: "text/html", HTML: "<html>hi alice</html>"}
	c := f.client(t)

	resp, body := get(t, c, "http://shop.example.co.uk/account", nil)
	if resp.StatusCode != 200 || body != "<html>hi alice</html>" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	tab := resp.Header.Get(HeaderTabID)
	if tab == "" || resp.Header.Get(HeaderSession) != "new" {
		t.Fatalf("headers: %v", resp.Header)
	}
	ck := resp.Cookies()
	if len(ck) != 1 || ck[0].Name != CookieName || ck[0].Value != tab || ck[0].Domain != "example.co.uk" || ck[0].Path != "/" || !ck[0].HttpOnly {
		t.Fatalf("session cookie = %+v", ck)
	}

	// A sibling subdomain without the cookie lands in the same tab.
	resp, _ = get(t, c, "http://www.example.co.uk/", nil)
	if resp.Header.Get(HeaderTabID) != tab || resp.Header.Get(HeaderSession) != "domain" {
		t.Fatalf("expected domain reuse, got %v", resp.Header)
	}
	// With the cookie, no new cookie is set.
	resp, _ = get(t, c, "http://www.example.co.uk/", map[string]string{"Cookie": CookieName + "=" + tab})
	if resp.Header.Get(HeaderSession) != "cookie" || len(resp.Cookies()) != 0 {
		t.Fatalf("cookie session: %v", resp.Header)
	}
}

func TestHTTPProxyDeadTab(t *testing.T) {
	f := newFixture(t)
	c := f.client(t)

	resp, body := get(t, c, "http://example.com/", map[string]string{HeaderTabID: "DEAD"})
	if resp.StatusCode != http.StatusGone || resp.Header.Get("X-Tether-Error") != core.CodeTabGone || !strings.Contains(body, "tab_gone") {
		t.Fatalf("header: %d %v %s", resp.StatusCode, resp.Header, body)
	}
	resp, _ = get(t, c, "http://example.com/", map[string]string{"Cookie": CookieName + "=DEAD"})
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("cookie: %d", resp.StatusCode)
	}
	if ck := resp.Cookies(); len(ck) != 1 || ck[0].MaxAge >= 0 {
		t.Fatalf("stale cookie not cleared: %+v", ck)
	}
}

func TestHTTPProxyBadControlHeaders(t *testing.T) {
	f := newFixture(t)
	c := f.client(t)
	for _, h := range []map[string]string{
		{HeaderMode: "bogus"},
		{HeaderWait: "forever"},
		{HeaderTimeout: "-1s"},
	} {
		if resp, _ := get(t, c, "http://example.com/", h); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%v: status %d", h, resp.StatusCode)
		}
	}
}

func TestHTTPSInterception(t *testing.T) {
	f := newFixture(t)
	f.browser.Pages["https://secure.example.com/inbox"] = cdptest.Page{Status: 200, MimeType: "text/html", HTML: "<html>inbox</html>"}
	c := f.client(t)

	resp, body := get(t, c, "https://secure.example.com/inbox", nil)
	if resp.StatusCode != 200 || body != "<html>inbox</html>" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	if resp.TLS == nil || resp.TLS.PeerCertificates[0].Subject.CommonName != "secure.example.com" {
		t.Fatal("leaf certificate not minted for the CONNECT host")
	}
	ck := resp.Cookies()
	if len(ck) != 1 || !ck[0].Secure || ck[0].Domain != "example.com" {
		t.Fatalf("https session cookie = %+v", ck)
	}
	// Keep-alive inside the tunnel: a second request reuses the tab.
	resp2, _ := get(t, c, "https://secure.example.com/inbox", nil)
	if resp2.Header.Get(HeaderTabID) != resp.Header.Get(HeaderTabID) {
		t.Fatal("second https request used a different tab")
	}
}

func TestConnectRefusedWithoutCA(t *testing.T) {
	b := cdptest.NewBrowser()
	c := core.New(b, session.NewManager(b, session.Options{}), core.Options{})
	srv := httptest.NewServer(New(c, nil, nil))
	defer srv.Close()
	pu, _ := url.Parse(srv.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
	_, err := client.Get("https://example.com/")
	if err == nil || !strings.Contains(err.Error(), "Not Implemented") {
		t.Fatalf("expected CONNECT to be refused, got %v", err)
	}
}

func TestNonProxyRequest(t *testing.T) {
	f := newFixture(t)
	resp, err := http.Get(f.srv.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestStripSessionCookie(t *testing.T) {
	tests := []struct {
		in   []string
		want []string
	}{
		{[]string{"tether_tab=X"}, nil},
		{[]string{"a=1; tether_tab=X; b=2"}, []string{"a=1; b=2"}},
		{[]string{"a=1", "tether_tab=X"}, []string{"a=1"}},
		{[]string{"tether_tabby=1"}, []string{"tether_tabby=1"}},
		{nil, nil},
	}
	for _, tt := range tests {
		h := http.Header{}
		for _, v := range tt.in {
			h.Add("Cookie", v)
		}
		StripSessionCookie(h)
		got := h.Values("Cookie")
		if strings.Join(got, "|") != strings.Join(tt.want, "|") {
			t.Errorf("Strip(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSessionCookieScope(t *testing.T) {
	tests := []struct {
		url, domain string
		secure      bool
	}{
		{"https://mail.google.com/", "google.com", true},
		{"http://a.b.example.co.uk/", "example.co.uk", false},
		{"http://localhost:3000/", "", false},
		{"http://127.0.0.1:8080/", "", false},
	}
	for _, tt := range tests {
		u, _ := url.Parse(tt.url)
		c := SessionCookie(u, "T")
		if c.Domain != tt.domain || c.Secure != tt.secure || c.Path != "/" {
			t.Errorf("%s: %+v", tt.url, c)
		}
	}
}

func TestIsProxyRequest(t *testing.T) {
	abs := httptest.NewRequest("GET", "http://example.com/", nil)
	rel := httptest.NewRequest("GET", "/status", nil)
	con := httptest.NewRequest("CONNECT", "example.com:443", nil)
	con.URL = &url.URL{Host: "example.com:443"}
	if !IsProxyRequest(abs) || IsProxyRequest(rel) || !IsProxyRequest(con) {
		t.Fatal("IsProxyRequest misclassified")
	}
}
