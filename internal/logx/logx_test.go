package logx

import (
	"bytes"
	"net/url"
	"strings"
	"testing"
)

func TestRedaction(t *testing.T) {
	var buf bytes.Buffer
	log, err := New(&buf, "debug", "json")
	if err != nil {
		t.Fatal(err)
	}
	log.Info("req", "token", "s3cret", "Cookie", "sid=abc", "proxy_authorization", "Basic xyz", "tab", "T1")
	out := buf.String()
	for _, secret := range []string{"s3cret", "sid=abc", "Basic xyz"} {
		if strings.Contains(out, secret) {
			t.Errorf("secret %q leaked: %s", secret, out)
		}
	}
	if !strings.Contains(out, "T1") || !strings.Contains(out, Redacted) {
		t.Errorf("unexpected output: %s", out)
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := New(&bytes.Buffer{}, "loud", "text"); err == nil {
		t.Error("bad level accepted")
	}
	if _, err := New(&bytes.Buffer{}, "info", "xml"); err == nil {
		t.Error("bad format accepted")
	}
}

func TestSafeURL(t *testing.T) {
	tests := map[string]string{
		"https://user:pw@example.com/a?token=x#frag": "https://example.com/a?…",
		"http://example.com/path":                    "http://example.com/path",
	}
	for in, want := range tests {
		u, _ := url.Parse(in)
		if got := SafeURL(u); got != want {
			t.Errorf("SafeURL(%q) = %q, want %q", in, got, want)
		}
	}
	if SafeURL(nil) != "" {
		t.Error("nil URL")
	}
}

func TestClean(t *testing.T) {
	tests := map[string]string{
		"tab-1":                  "tab-1",
		"x\r\nlevel=ERROR msg=y": "xlevel=ERROR msg=y",
		"a\nb\rc":                "abc",
	}
	for in, want := range tests {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}
