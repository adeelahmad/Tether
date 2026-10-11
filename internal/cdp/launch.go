package cdp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// LaunchOptions configure Launch.
type LaunchOptions struct {
	// Bin is the Chrome executable; empty means FindChrome.
	Bin string
	// UserDataDir is the profile directory. Use a persistent directory so
	// logins survive restarts; empty means a throwaway temp directory.
	UserDataDir string
	// Profile picks a profile inside UserDataDir by its directory name,
	// e.g. "Default" or "Profile 1" (--profile-directory). Empty means
	// Chrome's choice, normally the last one used.
	Profile   string
	Headless  bool
	ExtraArgs []string
}

// Process is a Chrome launched by Tether.
type Process struct {
	WSURL string

	cmd     *exec.Cmd
	tempDir string
	exited  chan struct{}
}

// Launch starts Chrome with remote debugging on a random loopback port and
// returns once its DevTools endpoint is up.
func Launch(ctx context.Context, opts LaunchOptions) (*Process, error) {
	bin := opts.Bin
	if bin == "" {
		var err error
		if bin, err = FindChrome(); err != nil {
			return nil, err
		}
	}
	p := &Process{exited: make(chan struct{})}
	dir := opts.UserDataDir
	if dir == "" {
		d, err := os.MkdirTemp("", "tether-chrome-")
		if err != nil {
			return nil, err
		}
		dir, p.tempDir = d, d
	} else if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	portFile := filepath.Join(dir, "DevToolsActivePort")
	_ = os.Remove(portFile)

	args := []string{
		"--remote-debugging-port=0",
		"--remote-debugging-address=127.0.0.1",
		"--user-data-dir=" + dir,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-background-networking",
		"--disable-features=Translate",
	}
	if opts.Profile != "" {
		args = append(args, "--profile-directory="+opts.Profile)
	}
	if opts.Headless {
		args = append(args, "--headless=new", "--hide-scrollbars", "--mute-audio")
	}
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		// Chrome refuses to sandbox as root (containers, CI).
		args = append(args, "--no-sandbox")
	}
	args = append(args, opts.ExtraArgs...)
	args = append(args, "about:blank")

	// Not CommandContext: Chrome must outlive the launch context; Close
	// stops it.
	p.cmd = exec.Command(bin, args...) //nolint:gosec,noctx // bin is operator-supplied; lifetime is managed by Close
	if err := p.cmd.Start(); err != nil {
		p.cleanup()
		return nil, fmt.Errorf("launch %s: %w", bin, err)
	}
	go func() { _ = p.cmd.Wait(); close(p.exited) }()

	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for {
		if b, err := os.ReadFile(portFile); err == nil {
			if ws, err := ParseActivePort(string(b)); err == nil {
				p.WSURL = ws
				return p, nil
			}
		}
		select {
		case <-ctx.Done():
			p.kill()
			return nil, ctx.Err()
		case <-p.exited:
			p.cleanup()
			return nil, errors.New("chrome exited during startup")
		case <-deadline.C:
			p.kill()
			return nil, errors.New("timed out waiting for chrome DevTools endpoint")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Close waits briefly for a launched Chrome that was asked to quit, kills
// it otherwise, and removes a temporary profile.
func (p *Process) Close() error {
	if p.cmd != nil && p.cmd.Process != nil {
		select {
		case <-p.exited:
		case <-time.After(3 * time.Second):
			_ = p.cmd.Process.Kill()
			select {
			case <-p.exited:
			case <-time.After(5 * time.Second):
			}
		}
	}
	p.cleanup()
	return nil
}

func (p *Process) kill() {
	_ = p.cmd.Process.Kill()
	<-p.exited
	p.cleanup()
}

func (p *Process) cleanup() {
	if p.tempDir != "" {
		_ = os.RemoveAll(p.tempDir)
	}
}

// FindChrome locates a Chrome or Chromium executable. $TETHER_CHROME_BIN
// wins when set.
func FindChrome() (string, error) {
	if b := os.Getenv("TETHER_CHROME_BIN"); b != "" {
		return b, nil
	}
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		candidates = []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
		}
	case "windows":
		for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData"} {
			if base := os.Getenv(env); base != "" {
				candidates = append(candidates, filepath.Join(base, "Google", "Chrome", "Application", "chrome.exe"))
			}
		}
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", errors.New("chrome not found; set TETHER_CHROME_BIN or --chrome-bin")
}
