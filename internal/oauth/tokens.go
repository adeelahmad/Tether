package oauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

// Signed values are "<prefix><base64url(json)>.<base64url(hmac)>". The
// prefix is part of the signed input, so one kind cannot pass as another.
const (
	dynPrefix     = "tcl_" // dynamically registered client ID
	accessPrefix  = "tat_"
	refreshPrefix = "trt_"
)

type kind string

const (
	kindAccess  kind = accessPrefix
	kindRefresh kind = refreshPrefix
)

var errInvalid = errors.New("invalid or expired token")

// claims are the contents of an access or refresh token.
type claims struct {
	Sub string `json:"sub"` // principal name
	FP  string `json:"fp"`  // fingerprint of the approving token
	CID string `json:"cid"` // client ID
	Exp int64  `json:"exp"`
	Iat int64  `json:"iat"`
	JTI string `json:"jti"`
}

// dynClient is a registered client, carried in its own ID.
type dynClient struct {
	Name     string   `json:"n,omitempty"`
	Redirect []string `json:"r"`
	Secret   bool     `json:"s,omitempty"` // confidential: secret is derived from the ID
	Iat      int64    `json:"t"`
}

func (s *Server) sign(prefix string, v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	body := prefix + base64.RawURLEncoding.EncodeToString(b)
	return body + "." + base64.RawURLEncoding.EncodeToString(s.mac(body)), nil
}

func (s *Server) verify(prefix, value string, v any) error {
	if !strings.HasPrefix(value, prefix) {
		return errInvalid
	}
	body, sig, ok := strings.Cut(value, ".")
	if !ok {
		return errInvalid
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, s.mac(body)) {
		return errInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(body, prefix))
	if err != nil {
		return errInvalid
	}
	if json.Unmarshal(raw, v) != nil {
		return errInvalid
	}
	return nil
}

func (s *Server) mac(body string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(body))
	return m.Sum(nil)
}

func (s *Server) issue(k kind, sub, fp, clientID string, ttl int64) (string, error) {
	now := s.now().Unix()
	return s.sign(string(k), claims{Sub: sub, FP: fp, CID: clientID, Iat: now, Exp: now + ttl, JTI: randomString(9)})
}

func (s *Server) open(token string, k kind) (*claims, error) {
	var c claims
	if err := s.verify(string(k), token, &c); err != nil {
		return nil, err
	}
	if s.now().Unix() >= c.Exp {
		return nil, errInvalid
	}
	return &c, nil
}

// clientSecret derives a confidential dynamic client's secret from its ID.
func (s *Server) clientSecret(clientID string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte("secret\x00" + clientID))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// lookupClient resolves a client ID: configured, or dynamically registered.
func (s *Server) lookupClient(id string) (Client, bool) {
	if c, ok := s.clients[id]; ok {
		return c, true
	}
	if !s.dynamic {
		return Client{}, false
	}
	var d dynClient
	if s.verify(dynPrefix, id, &d) != nil {
		return Client{}, false
	}
	c := Client{ID: id, Name: d.Name, RedirectURIs: d.Redirect}
	if d.Secret {
		c.Secret = s.clientSecret(id)
	}
	return c, true
}
