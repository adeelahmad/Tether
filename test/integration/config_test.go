//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/adeelahmad/tether/internal/cdp"
	"github.com/adeelahmad/tether/internal/config"
)

// TestConfiguredHeadersAndCookies: [headers] and [[cookies]] from the config
// file reach a real Chrome.
func TestConfiguredHeadersAndCookies(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seeded := ""
		if c, err := r.Cookie("seeded"); err == nil {
			seeded = c.Value
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"purpose": r.Header.Get("X-Purpose"), "debug": r.Header.Get("X-Debug"),
			"keep": r.Header.Get("X-Keep"), "seeded": seeded,
		})
	}))
	defer s.Close()
	_, c := startTether(t, func(cfg *config.Config) {
		cfg.ExtraHeaders = map[string]string{"X-Purpose": "agent"}
		cfg.StripHeaders = []string{"X-Debug"}
		cfg.Cookies = []cdp.Cookie{{URL: s.URL, Name: "seeded", Value: "yes"}}
	})

	// POST replays as fetch() in the tab, carrying client headers.
	_, body := send(t, c, "POST", s.URL+"/echo", strings.NewReader("x"), map[string]string{
		"Content-Type": "text/plain", "X-Debug": "secret", "X-Keep": "kept",
	})
	var got map[string]string
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	want := map[string]string{"purpose": "agent", "debug": "", "keep": "kept", "seeded": "yes"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q (%s)", k, got[k], v, body)
		}
	}
}
