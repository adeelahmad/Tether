//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/adeelahmad/tether/internal/config"
	"github.com/adeelahmad/tether/internal/policy"
	"github.com/adeelahmad/tether/internal/proxy"
)

const (
	agentToken = "agent-token-0123456789"
	bobToken   = "bob-token-0123456789ab"
	adminToken = "admin-token-0123456789"
)

// TestPolicies drives a real Chrome as a restricted agent token: the
// browser-side guard must stop navigations the core never sees (script
// and redirects), and other tokens must not see the agent's tabs.
func TestPolicies(t *testing.T) {
	// "other" is a site the agent may not reach. Both listen on 127.0.0.1;
	// the agent is allowed 127.0.0.1 and reaches "other" as localhost.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>other</title><h1>SECRET-OTHER-SITE</h1>`)
	}))
	defer other.Close()
	otherURL := strings.Replace(other.URL, "127.0.0.1", "localhost", 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/home", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>home</title><h1>HOME</h1><a id="out" href="`+otherURL+`/">out</a>`)
	})
	mux.HandleFunc("/bounce", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, otherURL+"/", http.StatusFound)
	})
	home := httptest.NewServer(mux)
	defer home.Close()

	base, _ := startTether(t, func(c *config.Config) {
		c.Token = adminToken
		c.Credentials = []policy.Credential{
			{Token: adminToken, Principal: &policy.Principal{Name: "default", Admin: true, Scripts: true}},
			{Token: agentToken, Principal: &policy.Principal{Name: "agent", Scripts: true, Rules: policy.Rules{Allow: []string{"127.0.0.1"}}}},
			{Token: bobToken, Principal: &policy.Principal{Name: "bob"}},
		}
	})

	api := func(token, method, path, body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		b, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(b, &out)
		return resp.StatusCode, out
	}
	eval := func(token, tab, expr string) string {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"expression": expr})
		code, out := api(token, "POST", "/tether/v1/tabs/"+tab+"/eval", string(b))
		if code != 200 {
			t.Fatalf("eval %q: %d %v", expr, code, out)
		}
		s, _ := out["value"].(string)
		return s
	}

	code, out := api(agentToken, "GET", "/tether/v1/whoami", "")
	if code != 200 || out["name"] != "agent" {
		t.Fatalf("whoami: %d %v", code, out)
	}

	// Direct navigation outside the token's rules is refused up front.
	if code, out := api(agentToken, "POST", "/tether/v1/tabs", `{"url":"`+otherURL+`/"}`); code != 403 {
		t.Fatalf("create on forbidden URL: %d %v", code, out)
	}

	code, out = api(agentToken, "POST", "/tether/v1/tabs", `{"url":"`+home.URL+`/home"}`)
	if code != 201 || out["owner"] != "agent" {
		t.Fatalf("create: %d %v", code, out)
	}
	tab := out["id"].(string)

	// Page script and link clicks are held to the same rules in the
	// browser: the forbidden page never loads.
	eval(agentToken, tab, `location.href = "`+otherURL+`/"; "ok"`)
	time.Sleep(1500 * time.Millisecond)
	if html := eval(agentToken, tab, `document.documentElement.outerHTML`); strings.Contains(html, "SECRET-OTHER-SITE") {
		t.Fatal("script navigation escaped the policy")
	}
	code, out = api(agentToken, "POST", "/tether/v1/tabs", `{"url":"`+home.URL+`/home"}`)
	if code != 201 {
		t.Fatalf("create second tab: %d %v", code, out)
	}
	tab2 := out["id"].(string)
	eval(agentToken, tab2, `document.getElementById("out").click(); "ok"`)
	time.Sleep(1500 * time.Millisecond)
	if html := eval(agentToken, tab2, `document.documentElement.outerHTML`); strings.Contains(html, "SECRET-OTHER-SITE") {
		t.Fatal("link click escaped the policy")
	}

	// A redirect off the allowlist is stopped and reported as forbidden.
	proxyAs := func(token string) *http.Client {
		pu, _ := url.Parse(base)
		pu.User = url.UserPassword("tether", token)
		return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}, Timeout: 60 * time.Second}
	}
	agentProxy := proxyAs(agentToken)
	resp, body := send(t, agentProxy, "GET", home.URL+"/bounce", nil, map[string]string{proxy.HeaderTabID: tab})
	if resp.StatusCode != 403 || strings.Contains(body, "SECRET-OTHER-SITE") {
		t.Fatalf("redirect: %d %s", resp.StatusCode, body)
	}

	// Another token cannot see or drive the agent's tab, through any face.
	if code, _ := api(bobToken, "GET", "/tether/v1/tabs/"+tab, ""); code != 410 {
		t.Fatalf("bob get: %d", code)
	}
	bobProxy := proxyAs(bobToken)
	if resp, _ := send(t, bobProxy, "GET", home.URL+"/home", nil, map[string]string{proxy.HeaderTabID: tab}); resp.StatusCode != 410 {
		t.Fatalf("bob proxy on agent tab: %d", resp.StatusCode)
	}
	if code, out := api(bobToken, "GET", "/tether/v1/tabs", ""); code != 200 || len(out["tabs"].([]any)) != 0 {
		t.Fatalf("bob list: %d %v", code, out)
	}
	// bob has no script permission, even in his own tab.
	code, out = api(bobToken, "POST", "/tether/v1/tabs", "")
	if code != 201 {
		t.Fatalf("bob create: %d", code)
	}
	if code, _ := api(bobToken, "POST", "/tether/v1/tabs/"+out["id"].(string)+"/eval", `{"expression":"1"}`); code != 403 {
		t.Fatalf("bob eval: %d", code)
	}

	// Session rules narrow further, in the browser too.
	if code, out := api(agentToken, "PUT", "/tether/v1/tabs/"+tab+"/policy", `{"deny":["127.0.0.1"]}`); code != 200 {
		t.Fatalf("put policy: %d %v", code, out)
	}
	resp, _ = send(t, agentProxy, "GET", home.URL+"/home", nil, map[string]string{proxy.HeaderTabID: tab})
	if resp.StatusCode != 403 {
		t.Fatalf("session deny: %d", resp.StatusCode)
	}

	// The admin token, with no rules of its own, browses freely in its
	// own tab.
	code, out = api(adminToken, "POST", "/tether/v1/tabs", `{"url":"`+otherURL+`/"}`)
	if code != 201 {
		t.Fatalf("admin create: %d %v", code, out)
	}
	if html := eval(adminToken, out["id"].(string), `document.documentElement.outerHTML`); !strings.Contains(html, "SECRET-OTHER-SITE") {
		t.Fatalf("admin page did not load: %s", html)
	}
}
