package config

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/adeelahmad/tether/internal/policy"
)

func envOf(m map[string]string) Env {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestParse(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("from-a-file-0123456789\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policyFile := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policyFile, []byte(`{"allow":["example.com"],"deny":["evil.test"],
		"tokens":[{"name":"agent","token_env":"AGENT","allow":["example.com"]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		args    []string
		env     map[string]string
		check   func(t *testing.T, c *Config)
		wantErr bool
	}{
		{"defaults", nil, nil, func(t *testing.T, c *Config) {
			if c.Addr != DefaultAddr || c.Chrome != "auto" || c.IdleTimeout != 15*time.Minute || c.Token != "" || c.LogLevel != "info" {
				t.Fatalf("%+v", c)
			}
		}, false},
		{"env applies", nil, map[string]string{"TETHER_ADDR": "127.0.0.1:9000", "TETHER_IDLE_TIMEOUT": "1m", "TETHER_LAUNCH": "true"}, func(t *testing.T, c *Config) {
			if c.Addr != "127.0.0.1:9000" || c.IdleTimeout != time.Minute || !c.Launch {
				t.Fatalf("%+v", c)
			}
		}, false},
		{"flags win over env", []string{"--addr", "127.0.0.1:1", "--launch=false"}, map[string]string{"TETHER_ADDR": "127.0.0.1:9000", "TETHER_LAUNCH": "1"}, func(t *testing.T, c *Config) {
			if c.Addr != "127.0.0.1:1" || c.Launch {
				t.Fatalf("%+v", c)
			}
		}, false},
		{"allowlist", []string{"--allow-domains", " github.com, ,example.com "}, nil, func(t *testing.T, c *Config) {
			if !reflect.DeepEqual(c.AllowDomains, []string{"github.com", "example.com"}) {
				t.Fatalf("%v", c.AllowDomains)
			}
		}, false},
		{"token file", []string{"--token-file", tokenFile}, nil, func(t *testing.T, c *Config) {
			if c.Token != "from-a-file-0123456789" {
				t.Fatalf("token %q", c.Token)
			}
		}, false},
		{"profile under data dir", []string{"--data-dir", "/x"}, nil, func(t *testing.T, c *Config) {
			if c.ProfileDir != filepath.Join("/x", "chrome-profile") {
				t.Fatal(c.ProfileDir)
			}
		}, false},
		{"deny list", []string{"--deny-domains", "evil.test", "--allow-domains", "Example.com"}, nil, func(t *testing.T, c *Config) {
			if !reflect.DeepEqual(c.Rules, policy.Rules{Allow: []string{"example.com"}, Deny: []string{"evil.test"}}) {
				t.Fatalf("%+v", c.Rules)
			}
		}, false},
		{"token is the admin default principal", []string{"--token", "a-long-enough-token"}, nil, func(t *testing.T, c *Config) {
			if len(c.Credentials) != 1 || c.Credentials[0].Principal.Name != "default" || !c.Credentials[0].Principal.Admin || !c.HasAuth() {
				t.Fatalf("%+v", c.Credentials)
			}
		}, false},
		{"policy file", []string{"--policy-file", policyFile, "--token", "a-long-enough-token", "--allow-domains", "github.com"},
			map[string]string{"AGENT": "agent-token-0123456789"}, func(t *testing.T, c *Config) {
				if len(c.Credentials) != 2 || c.Credentials[1].Principal.Name != "agent" || c.Credentials[1].Token != "agent-token-0123456789" {
					t.Fatalf("%+v", c.Credentials)
				}
				if !reflect.DeepEqual(c.Rules.Allow, []string{"github.com", "example.com"}) || c.Rules.Deny[0] != "evil.test" {
					t.Fatalf("%+v", c.Rules)
				}
			}, false},
		{"policy file alone enables auth", []string{"--policy-file", policyFile}, map[string]string{"AGENT": "agent-token-0123456789"}, func(t *testing.T, c *Config) {
			if !c.HasAuth() {
				t.Fatal("no auth")
			}
		}, false},
		{"policy file token clashes with --token", []string{"--policy-file", policyFile, "--token", "agent-token-0123456789"},
			map[string]string{"AGENT": "agent-token-0123456789"}, nil, true},
		{"policy file missing env", []string{"--policy-file", policyFile}, nil, nil, true},
		{"bad deny pattern", []string{"--deny-domains", "http://x"}, nil, nil, true},
		{"short token", []string{"--token", "short"}, nil, nil, true},
		{"bad timeout", []string{"--timeout", "0s"}, nil, nil, true},
		{"stray args", []string{"extra"}, nil, nil, true},
		{"unknown flag", []string{"--nope"}, nil, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Parse(tt.args, envOf(tt.env), io.Discard)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.check != nil {
				tt.check(t, c)
			}
		})
	}
}
