package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/adeelahmad/tether/internal/cdp"
	"github.com/adeelahmad/tether/internal/oauth"
	"github.com/adeelahmad/tether/internal/policy"
)

// DefaultConfigName is the config file Tether reads from its data
// directory when --config is not given.
const DefaultConfigName = "config.toml"

// DefaultConfigPath is where Tether looks for a config file by default:
// config.toml in the per-user Tether directory.
func DefaultConfigPath() string { return filepath.Join(DefaultDataDir(), DefaultConfigName) }

// fileConfig is the TOML config file. Every field is optional; pointers
// tell "absent" from a zero value. See tether.example.toml.
type fileConfig struct {
	Server struct {
		Addr                *string `toml:"addr"`
		InsecureAllowPublic *bool   `toml:"insecure_allow_public"`
		PublicURL           *string `toml:"public_url"`
		Timeout             *string `toml:"timeout"`
		IdleTimeout         *string `toml:"idle_timeout"`
		DataDir             *string `toml:"data_dir"`
		MITM                *bool   `toml:"mitm"`
	} `toml:"server"`
	Log struct {
		Level  *string `toml:"level"`
		Format *string `toml:"format"`
	} `toml:"log"`
	Auth struct {
		Token     *string            `toml:"token"`
		TokenEnv  *string            `toml:"token_env"`
		TokenFile *string            `toml:"token_file"`
		Tokens    []policy.TokenSpec `toml:"tokens"`
	} `toml:"auth"`
	Policy struct {
		Allow *[]string `toml:"allow"`
		Deny  *[]string `toml:"deny"`
		File  *string   `toml:"file"`
	} `toml:"policy"`
	Headers struct {
		Extra map[string]string `toml:"extra"`
		Strip []string          `toml:"strip"`
	} `toml:"headers"`
	Cookies []cookieSpec `toml:"cookies"`
	Chrome  struct {
		Endpoint    *string   `toml:"endpoint"`
		Launch      *bool     `toml:"launch"`
		Headless    *bool     `toml:"headless"`
		Bin         *string   `toml:"bin"`
		Args        *[]string `toml:"args"`
		UserDataDir *string   `toml:"user_data_dir"`
		Profile     *string   `toml:"profile"`
	} `toml:"chrome"`
	Console struct {
		Enabled *bool `toml:"enabled"`
		Buffer  *int  `toml:"buffer"`
	} `toml:"console"`
	MCP struct {
		Enabled *bool     `toml:"enabled"`
		OAuth   oauthSpec `toml:"oauth"`
	} `toml:"mcp"`
	Remote struct {
		Enabled *bool `toml:"enabled"`
		Quality *int  `toml:"quality"`
	} `toml:"remote"`
	Tailscale struct {
		Enabled   *bool   `toml:"enabled"`
		Publish   *string `toml:"publish"`
		HTTPSPort *int    `toml:"https_port"`
		Bin       *string `toml:"bin"`
	} `toml:"tailscale"`
}

type cookieSpec struct {
	Name     string    `toml:"name"`
	Value    string    `toml:"value"`
	ValueEnv string    `toml:"value_env"`
	URL      string    `toml:"url"`
	Domain   string    `toml:"domain"`
	Path     string    `toml:"path"`
	Secure   bool      `toml:"secure"`
	HTTPOnly bool      `toml:"http_only"`
	SameSite string    `toml:"same_site"`
	Expires  time.Time `toml:"expires"`
}

type oauthSpec struct {
	Enabled             *bool         `toml:"enabled"`
	AccessTokenTTL      *string       `toml:"access_token_ttl"`
	RefreshTokenTTL     *string       `toml:"refresh_token_ttl"`
	DynamicRegistration *bool         `toml:"dynamic_registration"`
	Clients             []oauthClient `toml:"clients"`
}

type oauthClient struct {
	ID           string   `toml:"id"`
	Name         string   `toml:"name"`
	Secret       string   `toml:"secret"`
	SecretEnv    string   `toml:"secret_env"`
	RedirectURIs []string `toml:"redirect_uris"`
}

// OAuthConfig configures the MCP face's OAuth authorization server.
type OAuthConfig struct {
	Enabled             bool
	AccessTTL           time.Duration
	RefreshTTL          time.Duration
	DynamicRegistration bool
	Clients             []oauth.Client
}

// loadedFile is a config file turned into flag defaults plus the settings
// only the file can express.
type loadedFile struct {
	path string
	// vals are flag values by flag name. They sit between the built-in
	// defaults and the environment.
	vals       map[string]string
	chromeArgs []string // exact list; the flag form splits on spaces
	tokens     []policy.Credential
	headers    map[string]string
	strip      []string
	cookies    []cdp.Cookie
	oauth      oauthSpec
}

// configPath finds the config file: --config, then $TETHER_CONFIG, then
// the default path if it exists. explicit reports whether one was named;
// an empty name (--config=) turns the file off.
func configPath(args []string, env Env) (path string, explicit bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !strings.HasPrefix(a, "-") || name != "config" {
			continue
		}
		if hasVal {
			path, explicit = val, true
		} else if i+1 < len(args) {
			path, explicit = args[i+1], true
			i++
		}
	}
	if explicit {
		return path, true
	}
	if v, ok := env("TETHER_CONFIG"); ok {
		return v, true
	}
	if p := DefaultConfigPath(); fileExists(p) {
		return p, false
	}
	return "", false
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// loadFile reads and validates a config file.
func loadFile(path string, env Env) (*loadedFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}
	var f fileConfig
	md, err := toml.Decode(string(raw), &f)
	if err != nil {
		return nil, fmt.Errorf("parsing config file %s: %w", path, err)
	}
	if extra := md.Undecoded(); len(extra) > 0 {
		return nil, fmt.Errorf("config file %s: unknown key %q", path, extra[0].String())
	}
	l, err := f.resolve(filepath.Dir(path), env)
	if err != nil {
		return nil, fmt.Errorf("config file %s: %w", path, err)
	}
	l.path = path
	return l, nil
}

func (f *fileConfig) resolve(dir string, env Env) (*loadedFile, error) {
	l := &loadedFile{vals: map[string]string{}, oauth: f.MCP.OAuth}
	setStr := func(name string, p *string) {
		if p != nil {
			l.vals[name] = *p
		}
	}
	setPath := func(name string, p *string) {
		if p != nil {
			l.vals[name] = resolvePath(dir, *p)
		}
	}
	setBool := func(name string, p *bool, invert bool) {
		if p != nil {
			l.vals[name] = strconv.FormatBool(*p != invert)
		}
	}
	setInt := func(name string, p *int) {
		if p != nil {
			l.vals[name] = strconv.Itoa(*p)
		}
	}
	setDur := func(name, key string, p *string) error {
		if p == nil {
			return nil
		}
		if _, err := time.ParseDuration(*p); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		l.vals[name] = *p
		return nil
	}

	s := f.Server
	setStr("addr", s.Addr)
	setBool("insecure-allow-public", s.InsecureAllowPublic, false)
	setStr("public-url", s.PublicURL)
	if err := setDur("timeout", "server.timeout", s.Timeout); err != nil {
		return nil, err
	}
	if err := setDur("idle-timeout", "server.idle_timeout", s.IdleTimeout); err != nil {
		return nil, err
	}
	setPath("data-dir", s.DataDir)
	setBool("no-mitm", s.MITM, true)
	setStr("log-level", f.Log.Level)
	setStr("log-format", f.Log.Format)

	a := f.Auth
	n := 0
	for _, p := range []*string{a.Token, a.TokenEnv, a.TokenFile} {
		if p != nil && *p != "" {
			n++
		}
	}
	if n > 1 {
		return nil, errors.New("auth: set only one of token, token_env and token_file")
	}
	switch {
	case a.Token != nil && *a.Token != "":
		l.vals["token"] = *a.Token
	case a.TokenEnv != nil && *a.TokenEnv != "":
		v, ok := env(*a.TokenEnv)
		if !ok {
			return nil, fmt.Errorf("auth.token_env: environment variable %s is not set", *a.TokenEnv)
		}
		l.vals["token"] = strings.TrimSpace(v)
	case a.TokenFile != nil && *a.TokenFile != "":
		l.vals["token-file"] = resolvePath(dir, *a.TokenFile)
	}
	if len(a.Tokens) > 0 {
		pf := policy.File{Tokens: a.Tokens}
		loaded, err := pf.Resolve(dir, env)
		if err != nil {
			return nil, fmt.Errorf("auth.tokens: %w", err)
		}
		l.tokens = loaded.Credentials
	}

	if f.Policy.Allow != nil {
		l.vals["allow-domains"] = strings.Join(*f.Policy.Allow, ",")
	}
	if f.Policy.Deny != nil {
		l.vals["deny-domains"] = strings.Join(*f.Policy.Deny, ",")
	}
	setPath("policy-file", f.Policy.File)

	for k, v := range f.Headers.Extra {
		if err := checkHeader(k, v); err != nil {
			return nil, fmt.Errorf("headers.extra: %w", err)
		}
	}
	l.headers = f.Headers.Extra
	for _, k := range f.Headers.Strip {
		if err := checkHeader(k, ""); err != nil {
			return nil, fmt.Errorf("headers.strip: %w", err)
		}
		l.strip = append(l.strip, k)
	}
	for i, c := range f.Cookies {
		ck, err := c.resolve(env)
		if err != nil {
			return nil, fmt.Errorf("cookies[%d]: %w", i, err)
		}
		l.cookies = append(l.cookies, ck)
	}

	c := f.Chrome
	setStr("chrome", c.Endpoint)
	setBool("launch", c.Launch, false)
	setBool("headless", c.Headless, false)
	if c.Bin != nil {
		if strings.ContainsAny(*c.Bin, `/\`) || strings.HasPrefix(*c.Bin, "~") {
			l.vals["chrome-bin"] = resolvePath(dir, *c.Bin)
		} else {
			l.vals["chrome-bin"] = *c.Bin // a name looked up on $PATH
		}
	}
	if c.Args != nil {
		l.chromeArgs = *c.Args
		l.vals["chrome-args"] = strings.Join(*c.Args, " ")
	}
	setPath("profile-dir", c.UserDataDir)
	setStr("chrome-profile", c.Profile)

	if f.Console.Enabled != nil && !*f.Console.Enabled {
		l.vals["console-buffer"] = "0"
	} else {
		setInt("console-buffer", f.Console.Buffer)
	}

	setBool("mcp", f.MCP.Enabled, false)
	setBool("mcp-oauth", f.MCP.OAuth.Enabled, false)
	setBool("no-remote", f.Remote.Enabled, true)
	setInt("remote-quality", f.Remote.Quality)

	ts := f.Tailscale
	setBool("tailscale", ts.Enabled, false)
	setStr("tailscale-publish", ts.Publish)
	setInt("tailscale-https-port", ts.HTTPSPort)
	setStr("tailscale-bin", ts.Bin)
	return l, nil
}

func (c cookieSpec) resolve(env Env) (cdp.Cookie, error) {
	if c.Name == "" {
		return cdp.Cookie{}, errors.New("name is required")
	}
	if c.URL == "" && c.Domain == "" {
		return cdp.Cookie{}, fmt.Errorf("cookie %q: set url or domain", c.Name)
	}
	value := c.Value
	if c.ValueEnv != "" {
		if c.Value != "" {
			return cdp.Cookie{}, fmt.Errorf("cookie %q: set value or value_env, not both", c.Name)
		}
		v, ok := env(c.ValueEnv)
		if !ok {
			return cdp.Cookie{}, fmt.Errorf("cookie %q: environment variable %s is not set", c.Name, c.ValueEnv)
		}
		value = v
	}
	out := cdp.Cookie{Name: c.Name, Value: value, URL: c.URL, Domain: c.Domain, Path: c.Path, Secure: c.Secure, HTTPOnly: c.HTTPOnly}
	switch strings.ToLower(c.SameSite) {
	case "":
	case "strict":
		out.SameSite = "Strict"
	case "lax":
		out.SameSite = "Lax"
	case "none":
		out.SameSite = "None"
	default:
		return cdp.Cookie{}, fmt.Errorf("cookie %q: same_site must be Strict, Lax or None", c.Name)
	}
	if !c.Expires.IsZero() {
		out.Expires = float64(c.Expires.Unix())
	}
	return out, nil
}

// resolveOAuth turns the file's [mcp.oauth] section into an OAuthConfig.
func resolveOAuth(s oauthSpec, enabled bool, env Env) (OAuthConfig, error) {
	o := OAuthConfig{Enabled: enabled, DynamicRegistration: true}
	if s.DynamicRegistration != nil {
		o.DynamicRegistration = *s.DynamicRegistration
	}
	for _, d := range []struct {
		key string
		p   *string
		out *time.Duration
	}{{"mcp.oauth.access_token_ttl", s.AccessTokenTTL, &o.AccessTTL}, {"mcp.oauth.refresh_token_ttl", s.RefreshTokenTTL, &o.RefreshTTL}} {
		if d.p == nil {
			continue
		}
		v, err := time.ParseDuration(*d.p)
		if err != nil || v <= 0 {
			return o, fmt.Errorf("%s must be a positive duration", d.key)
		}
		*d.out = v
	}
	for i, c := range s.Clients {
		secret := c.Secret
		if c.SecretEnv != "" {
			if secret != "" {
				return o, fmt.Errorf("mcp.oauth.clients[%d]: set secret or secret_env, not both", i)
			}
			v, ok := env(c.SecretEnv)
			if !ok {
				return o, fmt.Errorf("mcp.oauth.clients[%d]: environment variable %s is not set", i, c.SecretEnv)
			}
			secret = strings.TrimSpace(v)
		}
		if secret != "" && len(secret) < policy.MinTokenLen {
			return o, fmt.Errorf("mcp.oauth.clients[%d]: secret must be at least %d characters", i, policy.MinTokenLen)
		}
		o.Clients = append(o.Clients, oauth.Client{ID: c.ID, Name: c.Name, Secret: secret, RedirectURIs: c.RedirectURIs})
	}
	return o, nil
}

// checkHeader rejects header names and values that are not valid HTTP.
func checkHeader(name, value string) error {
	if name == "" {
		return errors.New("empty header name")
	}
	for _, r := range name {
		if r <= ' ' || r >= 0x7f || strings.ContainsRune(`()<>@,;:\"/[]?={}`, r) {
			return fmt.Errorf("invalid header name %q", name)
		}
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("invalid value for header %q", name)
	}
	return nil
}

// resolvePath expands a leading ~/ and makes p relative to dir.
func resolvePath(dir, p string) string {
	if p == "" {
		return p
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(dir, p)
}
