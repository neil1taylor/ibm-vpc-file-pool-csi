package cossetup

import (
	"bytes"
	"fmt"
	"net"
	"strings"
)

// zoneAddr is one address of a CBR network zone.
type zoneAddr struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// parseAddress turns an address given by the user into a zone address: an IP
// address, a range written a.b.c.d-e.f.g.h, or a subnet written in CIDR form.
func parseAddress(s string) (zoneAddr, error) {
	s = strings.TrimSpace(s)
	switch {
	case strings.Contains(s, "/"):
		if _, _, err := net.ParseCIDR(s); err != nil {
			return zoneAddr{}, fmt.Errorf("admin address %q is not a valid subnet: %w", s, err)
		}
		return zoneAddr{Type: "subnet", Value: s}, nil
	case strings.Contains(s, "-"):
		lo, hi, _ := strings.Cut(s, "-")
		if net.ParseIP(strings.TrimSpace(lo)) == nil || net.ParseIP(strings.TrimSpace(hi)) == nil {
			return zoneAddr{}, fmt.Errorf("admin address %q is not a valid range (use first-last)", s)
		}
		return zoneAddr{Type: "ipRange", Value: s}, nil
	default:
		if net.ParseIP(s) == nil {
			return zoneAddr{}, fmt.Errorf("admin address %q is not a valid IP address", s)
		}
		return zoneAddr{Type: "ipAddress", Value: s}, nil
	}
}

// zoneHolds says if the caller's address is in one of the zone's addresses. A
// VPC counts only for traffic from inside it, so it never covers a laptop.
func zoneHolds(addrs []zoneAddr, address string) bool {
	ip := net.ParseIP(strings.TrimSpace(address))
	if ip == nil {
		return false
	}
	for _, a := range addrs {
		switch a.Type {
		case "ipAddress":
			if other := net.ParseIP(a.Value); other != nil && other.Equal(ip) {
				return true
			}
		case "subnet":
			if _, n, err := net.ParseCIDR(a.Value); err == nil && n.Contains(ip) {
				return true
			}
		case "ipRange":
			lo, hi, _ := strings.Cut(a.Value, "-")
			l, h := net.ParseIP(strings.TrimSpace(lo)), net.ParseIP(strings.TrimSpace(hi))
			if l == nil || h == nil {
				continue
			}
			if l4, h4, ip4 := l.To4(), h.To4(), ip.To4(); l4 != nil && h4 != nil && ip4 != nil {
				l, h, ip = l4, h4, ip4
			}
			if bytes.Compare(ip, l) >= 0 && bytes.Compare(ip, h) <= 0 {
				return true
			}
		}
	}
	return false
}
