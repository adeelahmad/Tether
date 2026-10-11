// Package config parses Tether's configuration from flags, the
// environment and a TOML config file. Every flag has a TETHER_*
// environment variable and most have a config-file key. Precedence, from
// strongest: an explicitly passed flag, the environment, the config file,
// the built-in default.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/adeelahmad/tether/internal/cdp"
	"github.com/adeelahmad/tether/internal/policy"
)

// DefaultAddr is loopback-only by design.
const DefaultAddr = "127.0.0.1:8787"

// Config is the `tether serve` configuration.
type Config struct {
	Addr                string
	Token               string
	InsecureAllowPublic bool

	Chrome     string // DevTools endpoint spec (see cdp.ResolveEndpoint)
	Launch     bool
	Headless   bool
	ChromeBin  string
	ChromeArgs []string
	ProfileDir string
	// ChromeProfile is the profile directory name inside ProfileDir, e.g.
	// "Profile 1" (--profile-directory). Only used with Launch.
	ChromeProfile string
	IdleTimeout   time.Duration
	Timeout       time.Duration
	// ConsoleBuffer is how many console/log entries each tab keeps; 0
	// turns capture off.
	ConsoleBuffer int

	AllowDomains []string
	DenyDomains  []string
	PolicyFile   string
	DataDir      string
	NoMITM       bool
	MCP          bool
	// PublicURL is the externally visible base URL (OAuth issuer).
	PublicURL string
	OAuth     OAuthConfig

	// ExtraHeaders are sent by the browser with every request from the
	// tabs Tether attaches to; StripHeaders are client request headers
	// the proxy drops before replaying a request.
	ExtraHeaders map[string]string
	StripHeaders []string
	// Cookies are stored in the browser's cookie jar at startup.
	Cookies []cdp.Cookie

	// ConfigFile is the config file that was read, if any.
	ConfigFile string

	NoRemote      bool
	RemoteQuality int

	LogLevel  string
	LogFormat string

	// Derived by Parse: the merged global rules and every accepted token
	// (--token as the admin "default" principal, plus the config and
	// policy files').
	Rules       policy.Rules
	Credentials []policy.Credential

	fileTokens []policy.Credential
}

// Env looks up an environment variable.
type Env func(string) (string, bool)

// Parse parses args (without the program/subcommand name) for `serve`.
func Parse(args []string, env Env, out io.Writer) (*Config, error) {
	if env == nil {
		env = os.LookupEnv
	}
	c := &Config{}
	file := &loadedFile{vals: map[string]string{}}
	if path, _ := configPath(args, env); path != "" {
		var err error
		if file, err = loadFile(path, env); err != nil {
			return nil, err
		}
		c.ConfigFile = path
	}
	fs := flag.NewFlagSet("tether serve", flag.ContinueOnError)
	fs.SetOutput(out)

	// layered returns the value below the flag: the environment, else
	// the config file, else nothing.
	layered := func(name, envName string) (string, bool) {
		if v, ok := env(envName); ok {
			return v, true
		}
		v, ok := file.vals[name]
		return v, ok
	}
	str := func(p *string, name, envName, def, usage string) {
		if v, ok := layered(name, envName); ok {
			def = v
		}
		fs.StringVar(p, name, def, usage+" [$"+envName+"]")
	}
	boolean := func(p *bool, name, envName string, def bool, usage string) {
		if v, ok := layered(name, envName); ok {
			if b, err := strconv.ParseBool(v); err == nil {
				def = b
			}
		}
		fs.BoolVar(p, name, def, usage+" [$"+envName+"]")
	}
	integer := func(p *int, name, envName string, def int, usage string) {
		if v, ok := layered(name, envName); ok {
			if n, err := strconv.Atoi(v); err == nil {
				def = n
			}
		}
		fs.IntVar(p, name, def, usage+" [$"+envName+"]")
	}
	dur := func(p *time.Duration, name, envName string, def time.Duration, usage string) {
		if v, ok := layered(name, envName); ok {
			if d, err := time.ParseDuration(v); err == nil {
				def = d
			}
		}
		fs.DurationVar(p, name, def, usage+" [$"+envName+"]")
	}

	var configFlag string
	fs.StringVar(&configFlag, "config", "", "TOML config file (default: "+DefaultConfigPath()+" if it exists; --config= for none) [$TETHER_CONFIG]")

	var allow, deny, tokenFile, chromeArgs string
	str(&c.Addr, "addr", "TETHER_ADDR", DefaultAddr, "listen address (loopback by default)")
	str(&c.Token, "token", "TETHER_TOKEN", "", "bearer token required on every request (required for non-loopback --addr)")
	str(&tokenFile, "token-file", "TETHER_TOKEN_FILE", "", "read the bearer token from this file")
	boolean(&c.InsecureAllowPublic, "insecure-allow-public", "TETHER_INSECURE_ALLOW_PUBLIC", false, "allow a non-loopback --addr with no token (dangerous)")
	str(&c.Chrome, "chrome", "TETHER_CHROME", "auto", "Chrome DevTools endpoint: auto, a port, host:port, http(s)://…, ws://…, or a DevToolsActivePort path")
	boolean(&c.Launch, "launch", "TETHER_LAUNCH", false, "launch a dedicated Chrome instead of connecting to one")
	boolean(&c.Headless, "headless", "TETHER_HEADLESS", false, "with --launch: run Chrome headless")
	str(&c.ChromeBin, "chrome-bin", "TETHER_CHROME_BIN", "", "with --launch: Chrome executable (default: auto-detect)")
	str(&chromeArgs, "chrome-args", "TETHER_CHROME_ARGS", "", "with --launch: extra space-separated Chrome flags, e.g. --no-sandbox")
	str(&c.ProfileDir, "profile-dir", "TETHER_PROFILE_DIR", "", "with --launch: Chrome user data directory (default: <data-dir>/chrome-profile)")
	str(&c.ChromeProfile, "chrome-profile", "TETHER_CHROME_PROFILE", "", "with --launch: profile inside --profile-dir, e.g. \"Profile 1\" (default: the last one used)")
	dur(&c.IdleTimeout, "idle-timeout", "TETHER_IDLE_TIMEOUT", 15*time.Minute, "close tabs Tether opened after this much inactivity (0 disables)")
	dur(&c.Timeout, "timeout", "TETHER_TIMEOUT", 60*time.Second, "default per-request timeout")
	integer(&c.ConsoleBuffer, "console-buffer", "TETHER_CONSOLE_BUFFER", 1000, "console and browser log entries kept per tab (0 disables capture and leaves the Runtime/Log domains off)")
	str(&allow, "allow-domains", "TETHER_ALLOW_DOMAINS", "", "comma-separated domain allowlist (default: all)")
	str(&deny, "deny-domains", "TETHER_DENY_DOMAINS", "", "comma-separated domain denylist; deny beats allow")
	str(&c.PolicyFile, "policy-file", "TETHER_POLICY_FILE", "", "JSON or TOML policy file: global rules plus per-token rules (see docs/POLICIES.md)")
	str(&c.DataDir, "data-dir", "TETHER_DATA_DIR", DefaultDataDir(), "where the local CA and launched profile live")
	boolean(&c.NoMITM, "no-mitm", "TETHER_NO_MITM", false, "refuse HTTPS CONNECT instead of intercepting it with the local CA")
	boolean(&c.NoRemote, "no-remote", "TETHER_NO_REMOTE", false, "disable the remote-browser web UI at /remote/")
	integer(&c.RemoteQuality, "remote-quality", "TETHER_REMOTE_QUALITY", 70, "JPEG quality of the remote UI video, 1-100")
	boolean(&c.MCP, "mcp", "TETHER_MCP", true, "serve the MCP face for AI agents at /mcp")
	boolean(&c.OAuth.Enabled, "mcp-oauth", "TETHER_MCP_OAUTH", false, "let MCP clients sign in with OAuth (needs a token; see docs/CONFIG.md)")
	str(&c.PublicURL, "public-url", "TETHER_PUBLIC_URL", "", "externally visible base URL, e.g. https://tether.example.ts.net (default: from each request)")
	str(&c.LogLevel, "log-level", "TETHER_LOG_LEVEL", "info", "debug, info, warn or error")
	str(&c.LogFormat, "log-format", "TETHER_LOG_FORMAT", "text", "text or json")

	err := fs.Parse(args)
	if err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if tokenFile != "" && c.Token == "" {
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			return nil, fmt.Errorf("reading token file: %w", err)
		}
		c.Token = strings.TrimSpace(string(b))
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if _, inEnv := env("TETHER_CHROME_ARGS"); file.chromeArgs != nil && !set["chrome-args"] && !inEnv {
		c.ChromeArgs = file.chromeArgs // the file's list keeps arguments with spaces intact
	} else {
		c.ChromeArgs = strings.Fields(chromeArgs)
	}
	oauthOn := c.OAuth.Enabled
	if c.OAuth, err = resolveOAuth(file.oauth, oauthOn, env); err != nil {
		return nil, fmt.Errorf("config file %s: %w", c.ConfigFile, err)
	}
	c.ExtraHeaders, c.StripHeaders, c.Cookies = file.headers, file.strip, file.cookies
	c.fileTokens = file.tokens
	c.AllowDomains = splitList(allow)
	c.DenyDomains = splitList(deny)
	if c.ProfileDir == "" {
		c.ProfileDir = filepath.Join(c.DataDir, "chrome-profile")
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if err := c.resolvePolicy(env); err != nil {
		return nil, err
	}
	if c.OAuth.Enabled && len(c.Credentials) == 0 {
		return nil, errors.New("MCP OAuth needs at least one token to approve sign-ins with: set --token or [auth] in the config file")
	}
	return c, nil
}

func splitList(s string) []string {
	var out []string
	for _, d := range strings.Split(s, ",") {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, d)
		}
	}
	return out
}

// resolvePolicy merges the global rules and collects every token.
func (c *Config) resolvePolicy(env Env) error {
	rules := policy.Rules{Allow: c.AllowDomains, Deny: c.DenyDomains}
	c.Credentials = nil
	if c.Token != "" {
		c.Credentials = append(c.Credentials, policy.Credential{
			Token:     c.Token,
			Principal: &policy.Principal{Name: policy.DefaultTokenName, Admin: true, Scripts: true},
		})
	}
	c.Credentials = append(c.Credentials, c.fileTokens...)
	if c.PolicyFile != "" {
		l, err := policy.LoadFile(c.PolicyFile, env)
		if err != nil {
			return err
		}
		rules.Allow = append(rules.Allow, l.Global.Allow...)
		rules.Deny = append(rules.Deny, l.Global.Deny...)
		c.Credentials = append(c.Credentials, l.Credentials...)
	}
	if err := policy.CheckCredentials(c.Credentials); err != nil {
		return err
	}
	var err error
	c.Rules, err = rules.Normalize()
	return err
}

// HasAuth reports whether any token is configured.
func (c *Config) HasAuth() bool { return len(c.Credentials) > 0 || c.Token != "" }

// Validate checks values that flags alone cannot.
func (c *Config) Validate() error {
	if c.Addr == "" {
		return errors.New("--addr is required")
	}
	if c.Timeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	if c.IdleTimeout < 0 {
		return errors.New("--idle-timeout must not be negative")
	}
	if c.RemoteQuality < 1 || c.RemoteQuality > 100 {
		return errors.New("--remote-quality must be between 1 and 100")
	}
	if c.ConsoleBuffer < 0 {
		return errors.New("--console-buffer must not be negative")
	}
	if c.Token != "" && len(c.Token) < 16 {
		return errors.New("--token must be at least 16 characters")
	}
	if c.PublicURL != "" {
		u, err := url.Parse(c.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") {
			return errors.New("--public-url must be an http(s) origin like https://tether.example.ts.net")
		}
	}
	return nil
}

// DefaultDataDir is the per-user Tether directory.
func DefaultDataDir() string {
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "tether")
	}
	return ".tether"
}
