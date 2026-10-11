package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/adeelahmad/tether/internal/cdp"
	"github.com/adeelahmad/tether/internal/cdp/cdptest"
	"github.com/adeelahmad/tether/internal/session"
)

func setup(t *testing.T) (*Core, *cdptest.Browser, *session.Session) {
	t.Helper()
	b := cdptest.NewBrowser()
	m := session.NewManager(b, session.Options{})
	c := New(b, m, Options{})
	s, err := m.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c, b, s
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

func TestDoNavigate(t *testing.T) {
	c, b, s := setup(t)
	b.Pages["https://example.com/"] = cdptest.Page{
		Status: 200, MimeType: "text/html", HTML: "<html>live dom</html>", Body: []byte("<html>original</html>"),
		Header: http.Header{"Content-Type": {"text/html"}, "Set-Cookie": {"sid=1"}, "Content-Encoding": {"br"}, "X-Site": {"yes"}},
	}
	b.Pages["https://example.com/data.json"] = cdptest.Page{Status: 404, MimeType: "application/json", Body: []byte(`{"a":1}`)}

	tests := []struct {
		name, method, url string
		mode              Mode
		wantStatus        int
		wantBody          string
	}{
		{"html rendered", "GET", "https://example.com/", ModeAuto, 200, "<html>live dom</html>"},
		{"html raw", "GET", "https://example.com/", ModeRaw, 200, "<html>original</html>"},
		{"non-html is raw", "GET", "https://example.com/data.json", ModeAuto, 404, `{"a":1}`},
		{"head has no body", "HEAD", "https://example.com/", ModeAuto, 200, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := c.Do(context.Background(), s, &Request{Method: tt.method, URL: mustURL(tt.url), Mode: tt.mode})
			if err != nil {
				t.Fatal(err)
			}
			if resp.Status != tt.wantStatus || string(resp.Body) != tt.wantBody {
				t.Fatalf("got %d %q, want %d %q", resp.Status, resp.Body, tt.wantStatus, tt.wantBody)
			}
			for _, h := range []string{"Set-Cookie", "Content-Encoding"} {
				if resp.Header.Get(h) != "" {
					t.Errorf("%s leaked to client", h)
				}
			}
		})
	}
}

func TestDoFetch(t *testing.T) {
	c, b, s := setup(t)
	tab := s.Tab.(*cdptest.Tab)
	body := base64.StdEncoding.EncodeToString([]byte(`{"ok":true}`))
	tab.CallResult = json.RawMessage(fmt.Sprintf(`{"status":201,"url":"https://api.example.com/x","headers":[["content-type","application/json"],["set-cookie","a=b"]],"body":%q}`, body))
	_ = b

	req := &Request{
		Method: "POST",
		URL:    mustURL("https://api.example.com/x"),
		Header: http.Header{"Content-Type": {"application/json"}, "Cookie": {"tether_tab=1"}, "X-Tether-Mode": {"fetch"}, "Proxy-Authorization": {"secret"}},
		Body:   []byte(`{"q":1}`),
	}
	resp, err := c.Do(context.Background(), s, req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != 201 || string(resp.Body) != `{"ok":true}` || resp.Header.Get("Set-Cookie") != "" {
		t.Fatalf("unexpected response %+v", resp)
	}
	// fetch mode first moves the tab onto the target origin.
	if len(tab.Navigations) != 1 || tab.Navigations[0] != "https://api.example.com/" {
		t.Fatalf("navigations = %v", tab.Navigations)
	}
	call := tab.Calls[len(tab.Calls)-1]
	hdrs := call.Args[2].Value.(map[string]string)
	for _, h := range []string{"Cookie", "X-Tether-Mode", "Proxy-Authorization"} {
		if _, ok := hdrs[h]; ok {
			t.Errorf("%s forwarded to the page", h)
		}
	}
	if hdrs["Content-Type"] != "application/json" {
		t.Errorf("content-type not forwarded: %v", hdrs)
	}

	// Same origin again: no extra navigation.
	if _, err := c.Do(context.Background(), s, req); err != nil {
		t.Fatal(err)
	}
	if len(tab.Navigations) != 1 {
		t.Fatalf("re-navigated on same origin: %v", tab.Navigations)
	}
}

func TestDoRejectsBodyMethodsOutsideFetch(t *testing.T) {
	c, _, s := setup(t)
	_, err := c.Do(context.Background(), s, &Request{Method: "POST", URL: mustURL("https://a.com"), Mode: ModeRendered})
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("err = %v", err)
	}
}

func TestPolicyEnforcedInRequestPath(t *testing.T) {
	c, _, s := setup(t)
	c.Policy = DomainAllowlist([]string{"example.com"})
	_, err := c.Do(context.Background(), s, &Request{Method: "GET", URL: mustURL("https://evil.test/")})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.Navigate(context.Background(), s, "https://evil.test/", cdp.WaitLoad); !errors.Is(err, ErrForbidden) {
		t.Fatalf("navigate err = %v", err)
	}
	if _, err := c.Do(context.Background(), s, &Request{Method: "GET", URL: mustURL("https://www.example.com/")}); err != nil {
		t.Fatal(err)
	}
}

func TestDomainAllowlist(t *testing.T) {
	p := DomainAllowlist([]string{"Example.com", " .github.com ", ""})
	tests := []struct {
		url   string
		allow bool
	}{
		{"https://example.com/", true},
		{"https://www.example.com/x", true},
		{"https://api.github.com", true},
		{"https://notexample.com", false},
		{"https://example.com.evil.net", false},
		{"about:blank", true},
		{"file:///etc/passwd", false},
		{"javascript:alert(1)", false},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			err := p.Allow(context.Background(), nil, mustURL(tt.url))
			if (err == nil) != tt.allow {
				t.Fatalf("allow=%v err=%v", tt.allow, err)
			}
		})
	}
	if DomainAllowlist(nil).Allow(context.Background(), nil, mustURL("file:///x")) != nil {
		t.Fatal("empty allowlist must allow everything")
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": ModeAuto, "auto": ModeAuto, "RAW": ModeRaw, "fetch": ModeFetch, "rendered": ModeRendered} {
		got, err := ParseMode(in)
		if err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ParseMode("nope"); err == nil {
		t.Error("expected error")
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("x: %w", session.ErrTabGone), 410, CodeTabGone},
		{fmt.Errorf("x: %w", ErrForbidden), 403, CodeForbidden},
		{ErrBadRequest, 400, CodeBadRequest},
		{&cdp.NavigationError{URL: "u", Text: "net::ERR_NAME_NOT_RESOLVED"}, 502, CodeNavigation},
		{context.DeadlineExceeded, 504, CodeTimeout},
		{&cdp.JSError{Text: "boom"}, 422, CodeJavaScript},
		{cdp.ErrDisconnected, 503, CodeBrowserGone},
		{errors.New("other"), 500, CodeInternal},
	}
	for _, tt := range tests {
		status, code := Classify(tt.err)
		if status != tt.status || code != tt.code {
			t.Errorf("Classify(%v) = %d %s, want %d %s", tt.err, status, code, tt.status, tt.code)
		}
	}
}

func TestEval(t *testing.T) {
	c, _, s := setup(t)
	s.Tab.(*cdptest.Tab).EvalResult = json.RawMessage(`42`)
	v, err := c.Eval(context.Background(), s, "6*7", true)
	if err != nil || string(v) != "42" {
		t.Fatalf("eval = %s, %v", v, err)
	}
}
