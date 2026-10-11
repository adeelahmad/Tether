package policy

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestNormalize(t *testing.T) {
	tests := []struct {
		name    string
		in      Rules
		want    Rules
		wantErr bool
	}{
		{"empty", Rules{}, Rules{}, false},
		{"cleans", Rules{Allow: []string{" Example.COM. ", "", ".github.com", "example.com"}, Deny: []string{"*.Ads.example.com"}},
			Rules{Allow: []string{"example.com", "github.com"}, Deny: []string{"*.ads.example.com"}}, false},
		{"star", Rules{Allow: []string{"*"}}, Rules{Allow: []string{"*"}}, false},
		{"url is not a pattern", Rules{Allow: []string{"https://example.com"}}, Rules{}, true},
		{"path is not a pattern", Rules{Deny: []string{"example.com/admin"}}, Rules{}, true},
		{"inner wildcard", Rules{Allow: []string{"foo.*.com"}}, Rules{}, true},
		{"bare wildcard prefix", Rules{Allow: []string{"*."}}, Rules{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.in.Normalize()
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestCheck(t *testing.T) {
	r, err := Rules{
		Allow: []string{"example.com", "*.github.io"},
		Deny:  []string{"admin.example.com"},
	}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		url  string
		want bool
	}{
		{"https://example.com/", true},
		{"https://www.example.com/x", true},
		{"https://EXAMPLE.com./", true},
		{"https://admin.example.com/", false},
		{"https://x.admin.example.com/", false},
		{"https://notexample.com/", false},
		{"https://foo.github.io/", true},
		{"https://github.io/", false},
		{"about:blank", true},
		{"data:text/html,hi", true},
		{"file:///etc/passwd", false},
		{"chrome://settings", false},
		{"javascript:alert(1)", false},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			err := r.Check(mustURL(t, tt.url))
			if (err == nil) != tt.want {
				t.Fatalf("Check(%s) = %v, want allowed=%v", tt.url, err, tt.want)
			}
			if err != nil && !errors.Is(err, ErrForbidden) {
				t.Fatalf("error does not wrap ErrForbidden: %v", err)
			}
		})
	}

	if (Rules{}).Check(mustURL(t, "file:///x")) != nil {
		t.Fatal("empty rules must allow everything")
	}
	denyOnly := Rules{Deny: []string{"evil.test"}}
	if denyOnly.Check(mustURL(t, "https://fine.test/")) != nil || denyOnly.Check(mustURL(t, "https://a.evil.test/")) == nil {
		t.Fatal("deny-only rules")
	}
	if denyOnly.Check(mustURL(t, "file:///x")) == nil {
		t.Fatal("any rule restricts to web schemes")
	}
}

func TestPrincipal(t *testing.T) {
	var none *Principal
	if !none.CanUse("anyone") || !none.CanAdopt() || !none.CanScript() || none.OwnerName() != "" || !none.RulesOrEmpty().Empty() {
		t.Fatal("nil principal is unrestricted")
	}
	agent := &Principal{Name: "agent", Rules: Rules{Allow: []string{"example.com"}}}
	if !agent.CanUse("agent") || agent.CanUse("other") || agent.CanUse("") || agent.CanAdopt() || agent.CanScript() {
		t.Fatal("agent permissions")
	}
	admin := &Principal{Name: "ops", Admin: true, Scripts: true}
	if !admin.CanUse("agent") || !admin.CanUse("") || !admin.CanAdopt() || !admin.CanScript() {
		t.Fatal("admin permissions")
	}
	ctx := WithPrincipal(context.Background(), agent)
	if FromContext(ctx) != agent || FromContext(context.Background()) != nil {
		t.Fatal("context round trip")
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ops.token"), []byte("ops-token-from-file-0123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	write := func(body string) string {
		p := filepath.Join(dir, "policy.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	env := func(k string) (string, bool) {
		if k == "AGENT_TOKEN" {
			return "agent-token-from-env-0123", true
		}
		return "", false
	}

	l, err := LoadFile(write(`{
		"allow": ["Example.com"], "deny": ["admin.example.com"],
		"tokens": [
			{"name": "agent", "token_env": "AGENT_TOKEN", "allow": ["wikipedia.org"], "scripts": false},
			{"name": "ops", "token_file": "ops.token", "admin": true},
			{"name": "inline", "token": "inline-token-0123456789"}
		]}`), env)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(l.Global, Rules{Allow: []string{"example.com"}, Deny: []string{"admin.example.com"}}) {
		t.Fatalf("global %+v", l.Global)
	}
	if len(l.Credentials) != 3 {
		t.Fatalf("creds %+v", l.Credentials)
	}
	agent, ops, inline := l.Credentials[0], l.Credentials[1], l.Credentials[2]
	if agent.Token != "agent-token-from-env-0123" || agent.Principal.Scripts || agent.Principal.Admin ||
		!reflect.DeepEqual(agent.Principal.Rules.Allow, []string{"wikipedia.org"}) {
		t.Fatalf("agent %+v", agent.Principal)
	}
	if ops.Token != "ops-token-from-file-0123" || !ops.Principal.Admin || !ops.Principal.Scripts {
		t.Fatalf("ops %+v", ops.Principal)
	}
	if inline.Token != "inline-token-0123456789" || !inline.Principal.Scripts {
		t.Fatalf("inline %+v", inline.Principal)
	}

	bad := []struct{ name, body, want string }{
		{"unknown field", `{"tokens": [], "alow": []}`, "unknown field"},
		{"no secret", `{"tokens": [{"name": "a"}]}`, "exactly one"},
		{"two secrets", `{"tokens": [{"name": "a", "token": "aaaaaaaaaaaaaaaaaaaa", "token_env": "AGENT_TOKEN"}]}`, "exactly one"},
		{"short", `{"tokens": [{"name": "a", "token": "short"}]}`, "at least"},
		{"missing env", `{"tokens": [{"name": "a", "token_env": "NOPE"}]}`, "not set"},
		{"missing file", `{"tokens": [{"name": "a", "token_file": "nope"}]}`, "token_file"},
		{"bad name", `{"tokens": [{"name": "a b", "token": "aaaaaaaaaaaaaaaaaaaa"}]}`, "name"},
		{"dup name", `{"tokens": [{"name": "a", "token": "aaaaaaaaaaaaaaaaaaaa"}, {"name": "a", "token": "bbbbbbbbbbbbbbbbbbbb"}]}`, "duplicate"},
		{"bad rule", `{"tokens": [{"name": "a", "token": "aaaaaaaaaaaaaaaaaaaa", "allow": ["http://x"]}]}`, "invalid domain"},
		{"bad global", `{"deny": ["a/b"]}`, "invalid domain"},
		{"not json", `{`, "parsing"},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadFile(write(tt.body), env)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
	if _, err := LoadFile(filepath.Join(dir, "missing.json"), env); err == nil {
		t.Fatal("missing file")
	}
}

func TestCheckCredentials(t *testing.T) {
	p := func(n string) *Principal { return &Principal{Name: n} }
	if err := CheckCredentials([]Credential{{"t1-0123456789abcdef", p("a")}, {"t2-0123456789abcdef", p("b")}}); err != nil {
		t.Fatal(err)
	}
	if err := CheckCredentials([]Credential{{"t1-0123456789abcdef", p("a")}, {"t2-0123456789abcdef", p("a")}}); err == nil {
		t.Fatal("duplicate name accepted")
	}
	if err := CheckCredentials([]Credential{{"same-0123456789abcdef", p("a")}, {"same-0123456789abcdef", p("b")}}); err == nil {
		t.Fatal("shared secret accepted")
	}
}

// The shipped example must stay a valid policy file.
func TestExamplePolicyFile(t *testing.T) {
	raw, err := os.ReadFile("../../examples/policy.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "policy.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mail-triage.token"), []byte("mail-triage-token-0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(string) (string, bool) { return "an-env-token-0123456789", true }
	l, err := LoadFile(filepath.Join(dir, "policy.json"), env)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Credentials) != 3 || l.Global.Empty() {
		t.Fatalf("%+v", l)
	}
}
