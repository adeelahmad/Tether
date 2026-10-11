package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// MinTokenLen is the shortest token Tether accepts.
const MinTokenLen = 16

// DefaultTokenName names the principal behind --token.
const DefaultTokenName = "default"

// Credential binds a secret token to the principal it authenticates as.
type Credential struct {
	Token     string
	Principal *Principal
}

// File is the on-disk policy file (--policy-file), JSON:
//
//	{
//	  "allow": ["example.com"],           // global rules, merged with
//	  "deny":  ["admin.example.com"],     // --allow-domains/--deny-domains
//	  "tokens": [
//	    {"name": "research-agent", "token_env": "RESEARCH_TOKEN",
//	     "allow": ["wikipedia.org", "arxiv.org"], "scripts": false},
//	    {"name": "ops", "token_file": "ops.token", "admin": true}
//	  ]
//	}
type File struct {
	Allow  []string    `json:"allow"`
	Deny   []string    `json:"deny"`
	Tokens []TokenSpec `json:"tokens"`
}

// TokenSpec defines one token. Exactly one of Token, TokenEnv and
// TokenFile supplies the secret; TokenFile is relative to the policy file.
type TokenSpec struct {
	Name      string   `json:"name"`
	Token     string   `json:"token"`
	TokenEnv  string   `json:"token_env"`
	TokenFile string   `json:"token_file"`
	Admin     bool     `json:"admin"`
	Scripts   *bool    `json:"scripts"` // default true
	Allow     []string `json:"allow"`
	Deny      []string `json:"deny"`
}

// Loaded is a resolved policy file.
type Loaded struct {
	Global      Rules
	Credentials []Credential
}

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// LoadFile reads and resolves a policy file. env looks up environment
// variables for token_env (nil means os.LookupEnv).
func LoadFile(path string, env func(string) (string, bool)) (*Loaded, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading policy file: %w", err)
	}
	var f File
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parsing policy file %s: %w", path, err)
	}
	l, err := f.Resolve(filepath.Dir(path), env)
	if err != nil {
		return nil, fmt.Errorf("policy file %s: %w", path, err)
	}
	return l, nil
}

// Resolve validates f and reads its token secrets. Relative token_file
// paths are resolved against dir.
func (f *File) Resolve(dir string, env func(string) (string, bool)) (*Loaded, error) {
	if env == nil {
		env = os.LookupEnv
	}
	global, err := Rules{Allow: f.Allow, Deny: f.Deny}.Normalize()
	if err != nil {
		return nil, err
	}
	out := &Loaded{Global: global}
	names := map[string]bool{}
	for i, t := range f.Tokens {
		if !validName.MatchString(t.Name) {
			return nil, fmt.Errorf("tokens[%d]: name %q must be 1-64 letters, digits, '.', '_' or '-'", i, t.Name)
		}
		if names[t.Name] {
			return nil, fmt.Errorf("tokens[%d]: duplicate name %q", i, t.Name)
		}
		names[t.Name] = true
		secret, err := t.secret(dir, env)
		if err != nil {
			return nil, fmt.Errorf("token %q: %w", t.Name, err)
		}
		rules, err := Rules{Allow: t.Allow, Deny: t.Deny}.Normalize()
		if err != nil {
			return nil, fmt.Errorf("token %q: %w", t.Name, err)
		}
		scripts := t.Scripts == nil || *t.Scripts
		out.Credentials = append(out.Credentials, Credential{
			Token:     secret,
			Principal: &Principal{Name: t.Name, Admin: t.Admin, Scripts: scripts, Rules: rules},
		})
	}
	return out, nil
}

func (t TokenSpec) secret(dir string, env func(string) (string, bool)) (string, error) {
	n := 0
	for _, s := range []string{t.Token, t.TokenEnv, t.TokenFile} {
		if s != "" {
			n++
		}
	}
	if n != 1 {
		return "", errors.New("set exactly one of token, token_env or token_file")
	}
	var secret string
	switch {
	case t.Token != "":
		secret = t.Token
	case t.TokenEnv != "":
		v, ok := env(t.TokenEnv)
		if !ok {
			return "", fmt.Errorf("environment variable %s is not set", t.TokenEnv)
		}
		secret = v
	default:
		p := t.TokenFile
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("reading token_file: %w", err)
		}
		secret = string(b)
	}
	secret = strings.TrimSpace(secret)
	if len(secret) < MinTokenLen {
		return "", fmt.Errorf("token must be at least %d characters", MinTokenLen)
	}
	return secret, nil
}

// CheckCredentials rejects duplicate names or duplicate secrets, which
// would make authentication ambiguous.
func CheckCredentials(creds []Credential) error {
	names := map[string]bool{}
	secrets := map[string]string{}
	for _, c := range creds {
		if names[c.Principal.Name] {
			return fmt.Errorf("duplicate token name %q", c.Principal.Name)
		}
		names[c.Principal.Name] = true
		if other, ok := secrets[c.Token]; ok {
			return fmt.Errorf("tokens %q and %q share the same secret", other, c.Principal.Name)
		}
		secrets[c.Token] = c.Principal.Name
	}
	return nil
}
