package tailnet

import (
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestIsTailnetIP(t *testing.T) {
	for ip, want := range map[string]bool{
		"100.64.0.1": true, "100.101.102.103": true, "100.127.255.254": true,
		"100.128.0.1": false, "10.0.0.1": false, "127.0.0.1": false,
		"fd7a:115c:a1e0::1": true, "fd7a:115c:a1e1::1": false, "::ffff:100.100.1.1": true,
	} {
		if got := IsTailnetIP(netip.MustParseAddr(ip)); got != want {
			t.Errorf("%s: %v", ip, got)
		}
	}
}

func TestParseStatus(t *testing.T) {
	s, err := parseStatus([]byte(`{"BackendState":"Running","Self":{"DNSName":"laptop.tail1234.ts.net.","TailscaleIPs":["fd7a:115c:a1e0::5","100.101.102.103"]}}`))
	if err != nil || s.DNSName != "laptop.tail1234.ts.net" || s.IPv4.String() != "100.101.102.103" {
		t.Fatalf("%+v %v", s, err)
	}
	for _, bad := range []string{
		`{"BackendState":"NeedsLogin","Self":{}}`,
		`{"BackendState":"Running"}`,
		`{"BackendState":"Running","Self":{"TailscaleIPs":["fd7a:115c:a1e0::5"]}}`,
		`nope`,
	} {
		if _, err := parseStatus([]byte(bad)); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
}

func TestPickIPv4(t *testing.T) {
	mk := func(s string) net.Addr {
		_, n, _ := net.ParseCIDR(s)
		n.IP = net.ParseIP(strings.Split(s, "/")[0])
		return n
	}
	ip, err := pickIPv4([]net.Addr{mk("127.0.0.1/8"), mk("192.168.1.5/24"), mk("fd7a:115c:a1e0::5/128"), mk("100.90.1.2/32")})
	if err != nil || ip.String() != "100.90.1.2" {
		t.Fatalf("%v %v", ip, err)
	}
	if _, err := pickIPv4([]net.Addr{mk("10.0.0.1/8")}); err == nil {
		t.Fatal("no tailnet address should be an error")
	}
}

// fakeCLI writes a tailscale stand-in that logs its arguments.
func fakeCLI(t *testing.T, script string) (CLI, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script stand-in")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "args.log")
	bin := filepath.Join(dir, "tailscale")
	body := "#!/bin/sh\necho \"$@\" >> " + log + "\n" + script
	if err := os.WriteFile(bin, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return CLI{Bin: bin}, log
}

func TestPublish(t *testing.T) {
	cli, log := fakeCLI(t, "")
	stop, err := cli.Publish(context.Background(), ModeServe, 443, "http://127.0.0.1:8787")
	if err != nil {
		t.Fatal(err)
	}
	if err := stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	if got := string(b); got != "serve --bg --yes --https=443 http://127.0.0.1:8787\nserve --https=443 off\n" {
		t.Fatalf("%q", got)
	}
	if _, err := cli.Publish(context.Background(), "tunnel", 443, "x"); err == nil {
		t.Fatal("bad mode accepted")
	}
}

func TestCLIErrors(t *testing.T) {
	cli, _ := fakeCLI(t, "echo 'not logged in' >&2\nexit 1\n")
	if _, err := cli.Status(context.Background()); err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("%v", err)
	}
	cli, _ = fakeCLI(t, `echo '{"BackendState":"Running","Self":{"DNSName":"a.b.ts.net.","TailscaleIPs":["100.64.1.1"]}}'`+"\n")
	if s, err := cli.Status(context.Background()); err != nil || s.DNSName != "a.b.ts.net" {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestPublicURL(t *testing.T) {
	if PublicURL("a.ts.net", 443) != "https://a.ts.net" || PublicURL("a.ts.net", 8443) != "https://a.ts.net:8443" {
		t.Fatal()
	}
}
