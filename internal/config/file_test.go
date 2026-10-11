package config

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestMain points the per-user config directory at an empty temp dir, so
// a real ~/.config/tether/config.toml never leaks into the tests.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tether-config-test-")
	if err != nil {
		panic(err)
	}
	for _, k := range []string{"XDG_CONFIG_HOME", "HOME", "AppData"} {
		_ = os.Setenv(k, dir)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const fullConfig = `
[server]
addr = "127.0.0.1:9100"
public_url = "https://tether.example.ts.net"
timeout = "30s"
idle_timeout = "5m"
data_dir = "data"
mitm = false

[log]
level = "debug"
format = "json"

[auth]
token_file = "admin.token"

[[auth.tokens]]
name = "agent"
token_env = "AGENT_TOKEN"
allow = ["wikipedia.org"]
scripts = false

[policy]
allow = ["wikipedia.org", "example.com"]
deny = ["admin.example.com"]

[headers]
extra = { "X-Purpose" = "agent" }
strip = ["X-Debug"]

[[cookies]]
url = "https://example.com"
name = "consent"
value = "yes"
same_site = "lax"
expires = 2030-01-01T00:00:00Z

[[cookies]]
domain = ".example.org"
name = "sid"
value_env = "SID"
secure = true
http_only = true

[chrome]
launch = true
headless = true
bin = "google-chrome"
args = ["--window-size=1280,800", "--lang=en US"]
user_data_dir = "profiles"
profile = "Profile 1"

[console]
buffer = 50

[mcp]
enabled = true

[mcp.oauth]
enabled = true
access_token_ttl = "10m"
dynamic_registration = false

[[mcp.oauth.clients]]
id = "my-agent"
name = "My agent"
redirect_uris = ["http://127.0.0.1/callback"]

[remote]
enabled = false
quality = 40

[tailscale]
enabled = true
publish = "serve"
https_port = 8443
bin = "/usr/local/bin/tailscale"
`

func TestConfigFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "admin.token", "admin-token-0123456789\n")
	path := writeFile(t, dir, "tether.toml", fullConfig)
	env := envOf(map[string]string{"AGENT_TOKEN": "agent-token-0123456789", "SID": "s3cret"})

	c, err := Parse([]string{"--config", path}, env, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.ConfigFile != path || c.Addr != "127.0.0.1:9100" || c.PublicURL != "https://tether.example.ts.net" ||
		c.Timeout != 30*time.Second || c.IdleTimeout != 5*time.Minute || !c.NoMITM ||
		c.DataDir != filepath.Join(dir, "data") || c.LogLevel != "debug" || c.LogFormat != "json" {
		t.Fatalf("server settings: %+v", c)
	}
	if c.Token != "admin-token-0123456789" || len(c.Credentials) != 2 || c.Credentials[1].Principal.Name != "agent" ||
		c.Credentials[1].Principal.Scripts || c.Credentials[1].Principal.Rules.Allow[0] != "wikipedia.org" {
		t.Fatalf("auth: %+v", c.Credentials)
	}
	if !reflect.DeepEqual(c.Rules.Allow, []string{"wikipedia.org", "example.com"}) || c.Rules.Deny[0] != "admin.example.com" {
		t.Fatalf("rules: %+v", c.Rules)
	}
	if c.ExtraHeaders["X-Purpose"] != "agent" || !reflect.DeepEqual(c.StripHeaders, []string{"X-Debug"}) {
		t.Fatalf("headers: %v %v", c.ExtraHeaders, c.StripHeaders)
	}
	if len(c.Cookies) != 2 || c.Cookies[0].SameSite != "Lax" || c.Cookies[0].Expires != 1893456000 ||
		c.Cookies[1].Value != "s3cret" || !c.Cookies[1].Secure || !c.Cookies[1].HTTPOnly {
		t.Fatalf("cookies: %+v", c.Cookies)
	}
	if !c.Launch || !c.Headless || c.ChromeBin != "google-chrome" || c.ChromeProfile != "Profile 1" ||
		c.ProfileDir != filepath.Join(dir, "profiles") ||
		!reflect.DeepEqual(c.ChromeArgs, []string{"--window-size=1280,800", "--lang=en US"}) {
		t.Fatalf("chrome: %+v", c)
	}
	if c.ConsoleBuffer != 50 || !c.MCP || !c.NoRemote || c.RemoteQuality != 40 {
		t.Fatalf("faces: %+v", c)
	}
	if !c.Tailscale.Enabled || c.Tailscale.Publish != "serve" || c.Tailscale.HTTPSPort != 8443 || c.Tailscale.Bin != "/usr/local/bin/tailscale" {
		t.Fatalf("tailscale: %+v", c.Tailscale)
	}
	if !c.OAuth.Enabled || c.OAuth.DynamicRegistration || c.OAuth.AccessTTL != 10*time.Minute ||
		len(c.OAuth.Clients) != 1 || c.OAuth.Clients[0].ID != "my-agent" {
		t.Fatalf("oauth: %+v", c.OAuth)
	}
}

func TestConfigFilePrecedence(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "tether.toml", `
[server]
addr = "127.0.0.1:9100"
timeout = "30s"
[chrome]
args = ["--a b"]
[console]
enabled = false
[policy]
allow = ["file.example"]
`)
	tests := []struct {
		name  string
		args  []string
		env   map[string]string
		check func(t *testing.T, c *Config)
	}{
		{"file beats defaults", nil, nil, func(t *testing.T, c *Config) {
			if c.Addr != "127.0.0.1:9100" || c.Timeout != 30*time.Second || c.ConsoleBuffer != 0 ||
				!reflect.DeepEqual(c.ChromeArgs, []string{"--a b"}) || c.Rules.Allow[0] != "file.example" {
				t.Fatalf("%+v", c)
			}
		}},
		{"env beats file", nil, map[string]string{"TETHER_ADDR": "127.0.0.1:9200", "TETHER_CHROME_ARGS": "--x --y", "TETHER_CONSOLE_BUFFER": "7"}, func(t *testing.T, c *Config) {
			if c.Addr != "127.0.0.1:9200" || c.Timeout != 30*time.Second || c.ConsoleBuffer != 7 ||
				!reflect.DeepEqual(c.ChromeArgs, []string{"--x", "--y"}) {
				t.Fatalf("%+v", c)
			}
		}},
		{"flag beats env and file", []string{"--addr", "127.0.0.1:9300", "--allow-domains", "flag.example"}, map[string]string{"TETHER_ADDR": "127.0.0.1:9200"}, func(t *testing.T, c *Config) {
			if c.Addr != "127.0.0.1:9300" || !reflect.DeepEqual(c.Rules.Allow, []string{"flag.example"}) {
				t.Fatalf("%+v", c)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"--config=" + path}, tt.args...)
			c, err := Parse(args, envOf(tt.env), io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, c)
		})
	}
}

func TestConfigFileDiscovery(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "tether.toml", "[server]\naddr = \"127.0.0.1:9400\"\n")

	c, err := Parse(nil, envOf(map[string]string{"TETHER_CONFIG": path}), io.Discard)
	if err != nil || c.Addr != "127.0.0.1:9400" {
		t.Fatalf("TETHER_CONFIG: %v %+v", err, c)
	}

	def := DefaultConfigPath()
	if err := os.MkdirAll(filepath.Dir(def), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Dir(def), DefaultConfigName, "[server]\naddr = \"127.0.0.1:9500\"\n")
	t.Cleanup(func() { _ = os.Remove(def) })
	if c, err = Parse(nil, envOf(nil), io.Discard); err != nil || c.Addr != "127.0.0.1:9500" || c.ConfigFile != def {
		t.Fatalf("default path: %v %+v", err, c)
	}
	if c, err = Parse([]string{"--config="}, envOf(nil), io.Discard); err != nil || c.Addr != DefaultAddr || c.ConfigFile != "" {
		t.Fatalf("--config= should skip the default file: %v %+v", err, c)
	}
	if _, err = Parse([]string{"--config", filepath.Join(dir, "missing.toml")}, envOf(nil), io.Discard); err == nil {
		t.Fatal("a named config file that is missing must be an error")
	}
}

func TestConfigFileErrors(t *testing.T) {
	tests := []struct {
		name, body, want string
		env              map[string]string
	}{
		{"unknown key", "[server]\nlisten_on = \"x\"\n", `unknown key "server.listen_on"`, nil},
		{"syntax", "[server\n", "parsing config file", nil},
		{"bad duration", "[server]\ntimeout = \"soon\"\n", "server.timeout", nil},
		{"two token sources", "[auth]\ntoken = \"a-long-enough-token\"\ntoken_env = \"X\"\n", "only one of", nil},
		{"missing token env", "[auth]\ntoken_env = \"NOPE\"\n", "NOPE is not set", nil},
		{"bad token entry", "[[auth.tokens]]\nname = \"a\"\ntoken = \"short\"\n", "at least 16", nil},
		{"bad header", "[headers]\nextra = { \"Bad Header\" = \"x\" }\n", "invalid header name", nil},
		{"cookie without target", "[[cookies]]\nname = \"a\"\nvalue = \"b\"\n", "set url or domain", nil},
		{"cookie same_site", "[[cookies]]\nname = \"a\"\nurl = \"https://x.test\"\nsame_site = \"sometimes\"\n", "same_site", nil},
		{"oauth without token", "[mcp.oauth]\nenabled = true\n", "needs at least one token", nil},
		{"oauth bad ttl", "[auth]\ntoken = \"a-long-enough-token\"\n[mcp.oauth]\nenabled = true\naccess_token_ttl = \"-1m\"\n", "positive duration", nil},
		{"short client secret", "[auth]\ntoken = \"a-long-enough-token\"\n[[mcp.oauth.clients]]\nid = \"x\"\nsecret = \"short\"\nredirect_uris = [\"https://x.test/cb\"]\n", "at least 16", nil},
		{"bad public url", "[server]\npublic_url = \"https://x.test/path\"\n", "--public-url", nil},
		{"publish without token", "[tailscale]\npublish = \"funnel\"\n", "needs a token", nil},
		{"bad publish mode", "[tailscale]\npublish = \"tunnel\"\n", "serve or funnel", nil},
		{"funnel port", "[auth]\ntoken = \"a-long-enough-token\"\n[tailscale]\npublish = \"funnel\"\nhttps_port = 444\n", "443, 8443 or 10000", nil},
		{"bad deny pattern", "[policy]\ndeny = [\"http://x\"]\n", "invalid domain pattern", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeFile(t, t.TempDir(), "tether.toml", tt.body)
			_, err := Parse([]string{"--config", path}, envOf(tt.env), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

// TestExampleConfig keeps tether.example.toml loadable.
func TestExampleConfig(t *testing.T) {
	path := filepath.Join("..", "..", "tether.example.toml")
	c, err := Parse([]string{"--config", path}, envOf(map[string]string{"TETHER_TOKEN": "an-example-token-0123456789"}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != DefaultAddr || c.ConsoleBuffer != 1000 || !c.MCP || c.Tailscale.Enabled || c.Tailscale.Publish != "" {
		t.Fatalf("%+v", c)
	}
}
