// Package config parses Tether's configuration from flags and the
// environment. Every flag has a TETHER_* environment variable; an
// explicitly passed flag always wins over the environment.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/adeelahmad/tether/internal/policy"
)

// DefaultAddr is loopback-only by design.
const DefaultAddr = "127.0.0.1:8787"

// Config is the `tether serve` configuration.
type Config struct {
	Addr                string
	Token               string
	InsecureAllowPublic bool

	Chrome      string // DevTools endpoint spec (see cdp.ResolveEndpoint)
	Launch      bool
	Headless    bool
	ChromeBin   string
	ChromeArgs  []string
	ProfileDir  string
	IdleTimeout time.Duration
	Timeout     time.Duration
	// ConsoleBuffer is how many console/log entries each tab keeps; 0
	// turns capture off.
	ConsoleBuffer int

	AllowDomains []string
	DenyDomains  []string
	PolicyFile   string
	DataDir      string
	NoMITM       bool
	MCP          bool

	LogLevel  string
	LogFormat string

	// Derived by Parse: the merged global rules and every accepted token
	// (--token as the admin "default" principal, plus the policy file's).
	Rules       policy.Rules
	Credentials []policy.Credential
}

// Env looks up an environment variable.
type Env func(string) (string, bool)

// Parse parses args (without the program/subcommand name) for `serve`.
func Parse(args []string, env Env, out io.Writer) (*Config, error) {
	if env == nil {
		env = os.LookupEnv
	}
	c := &Config{}
	fs := flag.NewFlagSet("tether serve", flag.ContinueOnError)
	fs.SetOutput(out)

	str := func(p *string, name, envName, def, usage string) {
		if v, ok := env(envName); ok {
			def = v
		}
		fs.StringVar(p, name, def, usage+" [$"+envName+"]")
	}
	boolean := func(p *bool, name, envName string, def bool, usage string) {
		if v, ok := env(envName); ok {
			if b, err := strconv.ParseBool(v); err == nil {
				def = b
			}
		}
		fs.BoolVar(p, name, def, usage+" [$"+envName+"]")
	}
	integer := func(p *int, name, envName string, def int, usage string) {
		if v, ok := env(envName); ok {
			if n, err := strconv.Atoi(v); err == nil {
				def = n
			}
		}
		fs.IntVar(p, name, def, usage+" [$"+envName+"]")
	}
	dur := func(p *time.Duration, name, envName string, def time.Duration, usage string) {
		if v, ok := env(envName); ok {
			if d, err := time.ParseDuration(v); err == nil {
				def = d
			}
		}
		fs.DurationVar(p, name, def, usage+" [$"+envName+"]")
	}

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
	str(&c.ProfileDir, "profile-dir", "TETHER_PROFILE_DIR", "", "with --launch: Chrome profile directory (default: <data-dir>/chrome-profile)")
	dur(&c.IdleTimeout, "idle-timeout", "TETHER_IDLE_TIMEOUT", 15*time.Minute, "close tabs Tether opened after this much inactivity (0 disables)")
	dur(&c.Timeout, "timeout", "TETHER_TIMEOUT", 60*time.Second, "default per-request timeout")
	integer(&c.ConsoleBuffer, "console-buffer", "TETHER_CONSOLE_BUFFER", 1000, "console and browser log entries kept per tab (0 disables capture and leaves the Runtime/Log domains off)")
	str(&allow, "allow-domains", "TETHER_ALLOW_DOMAINS", "", "comma-separated domain allowlist (default: all)")
	str(&deny, "deny-domains", "TETHER_DENY_DOMAINS", "", "comma-separated domain denylist; deny beats allow")
	str(&c.PolicyFile, "policy-file", "TETHER_POLICY_FILE", "", "JSON policy file: global rules plus per-token rules (see SECURITY.md)")
	str(&c.DataDir, "data-dir", "TETHER_DATA_DIR", DefaultDataDir(), "where the local CA and launched profile live")
	boolean(&c.NoMITM, "no-mitm", "TETHER_NO_MITM", false, "refuse HTTPS CONNECT instead of intercepting it with the local CA")
	boolean(&c.MCP, "mcp", "TETHER_MCP", true, "serve the MCP face for AI agents at /mcp")
	str(&c.LogLevel, "log-level", "TETHER_LOG_LEVEL", "info", "debug, info, warn or error")
	str(&c.LogFormat, "log-format", "TETHER_LOG_FORMAT", "text", "text or json")

	if err := fs.Parse(args); err != nil {
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
	c.ChromeArgs = strings.Fields(chromeArgs)
	c.AllowDomains = splitList(allow)
	c.DenyDomains = splitList(deny)
	if c.ProfileDir == "" {
		c.ProfileDir = filepath.Join(c.DataDir, "chrome-profile")
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, c.resolvePolicy(env)
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
	if c.ConsoleBuffer < 0 {
		return errors.New("--console-buffer must not be negative")
	}
	if c.Token != "" && len(c.Token) < 16 {
		return errors.New("--token must be at least 16 characters")
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
