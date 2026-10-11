// Package tailnet exposes Tether on a Tailscale tailnet using the
// tailscale CLI that is already installed and logged in: it finds this
// machine's tailnet address to listen on, and can publish the listener
// over HTTPS with `tailscale serve` (tailnet only) or `tailscale funnel`
// (public internet). Tether embeds no Tailscale code of its own.
package tailnet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Mode is how Tether is published beyond its own listener.
type Mode string

const (
	ModeNone   Mode = ""
	ModeServe  Mode = "serve"
	ModeFunnel Mode = "funnel"
)

var (
	cgnat = netip.MustParsePrefix("100.64.0.0/10")
	ula   = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

// IsTailnetIP reports whether ip is in Tailscale's address ranges.
func IsTailnetIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	return cgnat.Contains(ip) || ula.Contains(ip)
}

// Self describes this machine on the tailnet.
type Self struct {
	IPv4    netip.Addr
	DNSName string // e.g. my-laptop.tail1234.ts.net, without the trailing dot
}

// CLI runs the tailscale command line tool.
type CLI struct {
	Bin string // default "tailscale"
}

func (c CLI) bin() string {
	if c.Bin == "" {
		return "tailscale"
	}
	return c.Bin
}

func (c CLI) run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, c.bin(), args...) //nolint:gosec // the binary is operator-configured
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("tailscale %s: %s", args[0], msg)
	}
	return out, nil
}

// Status asks the local tailscaled who this machine is. It fails when
// Tailscale is not running or not logged in.
func (c CLI) Status(ctx context.Context) (Self, error) {
	out, err := c.run(ctx, "status", "--json")
	if err != nil {
		return Self{}, err
	}
	return parseStatus(out)
}

func parseStatus(raw []byte) (Self, error) {
	var st struct {
		BackendState string
		Self         *struct {
			DNSName      string
			TailscaleIPs []string
		}
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return Self{}, fmt.Errorf("tailscale status: %w", err)
	}
	if st.BackendState != "" && st.BackendState != "Running" {
		return Self{}, fmt.Errorf("tailscale is %s, not running; run `tailscale up`", st.BackendState)
	}
	if st.Self == nil {
		return Self{}, errors.New("tailscale status: no self node")
	}
	s := Self{DNSName: strings.TrimSuffix(st.Self.DNSName, ".")}
	for _, a := range st.Self.TailscaleIPs {
		if ip, err := netip.ParseAddr(a); err == nil && ip.Is4() {
			s.IPv4 = ip
			break
		}
	}
	if !s.IPv4.IsValid() {
		return Self{}, errors.New("tailscale status: this machine has no tailnet IPv4 address")
	}
	return s, nil
}

// InterfaceIPv4 finds a tailnet IPv4 address on this machine's network
// interfaces, for when the CLI is unavailable (e.g. in a container that
// shares the host's network).
func InterfaceIPv4() (netip.Addr, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return netip.Addr{}, err
	}
	return pickIPv4(addrs)
}

func pickIPv4(addrs []net.Addr) (netip.Addr, error) {
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if p, ok := netip.AddrFromSlice(ip); ok && p.Unmap().Is4() && IsTailnetIP(p) {
			return p.Unmap(), nil
		}
	}
	return netip.Addr{}, errors.New("no tailnet address found; is Tailscale up on this machine?")
}

// Publish runs `tailscale serve` or `tailscale funnel` in the background
// mode, forwarding https://<this machine>:<httpsPort>/ to target (Tether's
// own listener). The returned function removes it again.
func (c CLI) Publish(ctx context.Context, mode Mode, httpsPort int, target string) (stop func(context.Context) error, err error) {
	if mode != ModeServe && mode != ModeFunnel {
		return nil, fmt.Errorf("unknown tailscale mode %q", mode)
	}
	port := "--https=" + strconv.Itoa(httpsPort)
	if _, err := c.run(ctx, string(mode), "--bg", "--yes", port, target); err != nil {
		return nil, err
	}
	return func(ctx context.Context) error {
		_, err := c.run(ctx, string(mode), port, "off")
		return err
	}, nil
}

// PublicURL is the HTTPS origin that serve or funnel publishes on.
func PublicURL(dnsName string, httpsPort int) string {
	if httpsPort == 443 {
		return "https://" + dnsName
	}
	return "https://" + net.JoinHostPort(dnsName, strconv.Itoa(httpsPort))
}
