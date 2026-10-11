package session

import (
	"net"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// RegistrableDomain returns the eTLD+1 of host using the Public Suffix List,
// so "a.b.example.co.uk" and "www.example.co.uk" both map to
// "example.co.uk". IP addresses, single-label hosts (localhost) and bare
// public suffixes map to themselves. Ports are ignored and the result is
// lower-case.
func RegistrableDomain(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	if host == "" {
		return ""
	}
	if net.ParseIP(host) != nil || !strings.Contains(host, ".") {
		return host
	}
	d, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return host
	}
	return d
}
