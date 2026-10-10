package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DefaultEndpoint is the conventional --remote-debugging-port address.
const DefaultEndpoint = "http://127.0.0.1:9222"

// ResolveEndpoint turns a user-supplied Chrome location into a browser-level
// DevTools websocket URL. Accepted forms:
//
//	auto                      127.0.0.1:9222, then DevToolsActivePort files
//	                          of the standard Chrome profiles (this covers
//	                          Chrome's chrome://inspect/#remote-debugging
//	                          toggle, which needs no relaunch)
//	ws://host:port/devtools/… used verbatim
//	http://host:port          discovered via /json/version
//	host:port, or a port      shorthand for http://host:port
//	/path/DevToolsActivePort  read from a file (or its directory)
func ResolveEndpoint(ctx context.Context, spec string) (string, error) {
	spec = strings.TrimSpace(spec)
	switch {
	case spec == "" || spec == "auto":
		return resolveAuto(ctx)
	case strings.HasPrefix(spec, "ws://") || strings.HasPrefix(spec, "wss://"):
		return spec, nil
	case strings.HasPrefix(spec, "http://") || strings.HasPrefix(spec, "https://"):
		return discover(ctx, spec)
	case isFileSpec(spec):
		return fromActivePortFile(ctx, spec)
	}
	if _, err := strconv.Atoi(spec); err == nil {
		spec = "127.0.0.1:" + spec
	}
	return discover(ctx, "http://"+spec)
}

func isFileSpec(s string) bool {
	if strings.ContainsAny(s, `/\`) {
		return true
	}
	_, err := os.Stat(s)
	return err == nil
}

func resolveAuto(ctx context.Context) (string, error) {
	if ws, err := discover(ctx, DefaultEndpoint); err == nil {
		return ws, nil
	}
	var tried []string
	for _, dir := range profileDirs() {
		p := filepath.Join(dir, "DevToolsActivePort")
		tried = append(tried, p)
		if ws, err := fromActivePortFile(ctx, p); err == nil {
			return ws, nil
		}
	}
	return "", fmt.Errorf("no Chrome with remote debugging found at %s or in %d profile directories; "+
		"start Chrome with --remote-debugging-port=9222 (and a --user-data-dir), enable "+
		"chrome://inspect/#remote-debugging, or use `tether serve --launch`", DefaultEndpoint, len(tried))
}

// discover asks a DevTools HTTP endpoint for its browser websocket URL.
func discover(ctx context.Context, base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	u.Path = "/json/version"
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	// Never route the local DevTools endpoint through an HTTP proxy.
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("devtools endpoint %s: %w", base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("devtools endpoint %s: %s", base, resp.Status)
	}
	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", fmt.Errorf("devtools endpoint %s: %w", base, err)
	}
	if v.WebSocketDebuggerURL == "" {
		return "", errors.New("devtools endpoint returned no webSocketDebuggerUrl")
	}
	// Chrome reports the host it bound; keep the host the user asked for so
	// remote/forwarded endpoints work.
	ws, err := url.Parse(v.WebSocketDebuggerURL)
	if err != nil {
		return "", err
	}
	ws.Host = u.Host
	if u.Scheme == "https" {
		ws.Scheme = "wss"
	}
	return ws.String(), nil
}

// fromActivePortFile reads Chrome's DevToolsActivePort file: line one is the
// port, line two the browser websocket path.
func fromActivePortFile(ctx context.Context, p string) (string, error) {
	if st, err := os.Stat(p); err == nil && st.IsDir() {
		p = filepath.Join(p, "DevToolsActivePort")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	ws, err := ParseActivePort(string(b))
	if err != nil {
		return "", fmt.Errorf("%s: %w", p, err)
	}
	// The file outlives crashed browsers; make sure something listens.
	u, _ := url.Parse(ws)
	d := net.Dialer{Timeout: time.Second}
	c, err := d.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return "", fmt.Errorf("%s is stale: %w", p, err)
	}
	_ = c.Close()
	return ws, nil
}

// ParseActivePort parses the contents of a DevToolsActivePort file.
func ParseActivePort(s string) (string, error) {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) < 2 {
		return "", errors.New("malformed DevToolsActivePort")
	}
	port, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil || port <= 0 || port > 65535 {
		return "", errors.New("malformed DevToolsActivePort port")
	}
	path := strings.TrimSpace(lines[1])
	if !strings.HasPrefix(path, "/devtools/browser/") {
		return "", errors.New("malformed DevToolsActivePort path")
	}
	return fmt.Sprintf("ws://127.0.0.1:%d%s", port, path), nil
}

// profileDirs are the default user-data directories of Chrome-family
// browsers on this OS, most likely first.
func profileDirs() []string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		base := filepath.Join(home, "Library", "Application Support")
		return []string{
			filepath.Join(base, "Google", "Chrome"),
			filepath.Join(base, "Google", "Chrome Beta"),
			filepath.Join(base, "Google", "Chrome Canary"),
			filepath.Join(base, "Chromium"),
		}
	case "windows":
		base := os.Getenv("LOCALAPPDATA")
		return []string{
			filepath.Join(base, "Google", "Chrome", "User Data"),
			filepath.Join(base, "Google", "Chrome Beta", "User Data"),
			filepath.Join(base, "Google", "Chrome SxS", "User Data"),
			filepath.Join(base, "Chromium", "User Data"),
		}
	default:
		base := filepath.Join(home, ".config")
		if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
			base = x
		}
		return []string{
			filepath.Join(base, "google-chrome"),
			filepath.Join(base, "google-chrome-beta"),
			filepath.Join(base, "google-chrome-unstable"),
			filepath.Join(base, "chromium"),
		}
	}
}
