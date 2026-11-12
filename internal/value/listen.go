package value

import (
	"fmt"
	"strconv"
	"strings"
)

// Listen is the address and port the local API listens on over TCP (specs.md §25.1). Exactly one of IPv4 and IPv6 is
// the address: IsIPv6 says which.
type Listen struct {
	IPv4   [4]byte
	IPv6   [16]byte
	IsIPv6 bool
	Port   uint16
}

// Address writes the address in its canonical form: dotted decimal, or RFC 5952 for IPv6, lowercase, with the longest
// run of two or more zero groups, the first of equal ones, written "::".
func (l Listen) Address() string {
	if !l.IsIPv6 {
		return fmt.Sprintf("%d.%d.%d.%d", l.IPv4[0], l.IPv4[1], l.IPv4[2], l.IPv4[3])
	}
	var groups [8]uint16
	for i := range groups {
		groups[i] = uint16(l.IPv6[2*i])<<8 | uint16(l.IPv6[2*i+1])
	}
	start, length := -1, 0
	for i := 0; i < 8; {
		if groups[i] != 0 {
			i++
			continue
		}
		j := i
		for j < 8 && groups[j] == 0 {
			j++
		}
		if j-i > length && j-i >= 2 {
			start, length = i, j-i
		}
		i = j
	}
	var b strings.Builder
	for i := 0; i < 8; i++ {
		if i == start {
			b.WriteString("::")
			i += length - 1
			continue
		}
		if i > 0 && i != start+length {
			b.WriteByte(':')
		}
		b.WriteString(strconv.FormatUint(uint64(groups[i]), 16))
	}
	return b.String()
}

// String writes it as the configuration would, the address in its canonical form.
func (l Listen) String() string {
	if l.IsIPv6 {
		return "[" + l.Address() + "]:" + strconv.Itoa(int(l.Port))
	}
	return l.Address() + ":" + strconv.Itoa(int(l.Port))
}

// ParseListen parses api.listen (specs.md §29.5):
//
//	listen = ( ipv4 | "[" ipv6 "]" ) ":" port
//	ipv4   = four decimal parts from 0 to 255, each with no leading zero, separated by "."
//	ipv6   = at most eight groups of 1 to 4 hex digits separated by ":", with at most one "::"
//	port   = nonzero digit, at most 5 digits         ; 1 to 65535
//
// An IPv6 address has no zone and no part written in decimal. The address is a loopback one or one of the Tailscale
// network: a name would be resolved, and the unspecified address would listen everywhere.
func ParseListen(s string) (Listen, error) {
	var l Listen
	var port string
	if rest, ok := strings.CutPrefix(s, "["); ok {
		host, after, found := strings.Cut(rest, "]:")
		if !found || !parseIPv6(host, &l.IPv6) {
			return Listen{}, fmt.Errorf("listen %q: want [an IPv6 address in hex groups, with no zone]:a port", s)
		}
		l.IsIPv6, port = true, after
	} else {
		at := strings.LastIndexByte(s, ':')
		if at < 0 || !parseIPv4(s[:at], &l.IPv4) {
			return Listen{}, fmt.Errorf("listen %q: want an IPv4 address in four decimal parts and a port, as 127.0.0.1:7341, or [an IPv6 address]:a port", s)
		}
		port = s[at+1:]
	}
	number, ok := parsePort(port)
	if !ok {
		return Listen{}, fmt.Errorf("listen %q: want a port from 1 to 65535, with no leading zero", s)
	}
	l.Port = number
	if !l.allowed() {
		return Listen{}, fmt.Errorf("listen %q: want a loopback address, or one of the Tailscale network (100.64.0.0/10, fd7a:115c:a1e0::/48)", s)
	}
	return l, nil
}

// parseIPv4 reads four decimal parts from 0 to 255, none with a leading zero.
func parseIPv4(host string, out *[4]byte) bool {
	parts := strings.Split(host, ".")
	if len(parts) != 4 {
		return false
	}
	for i, p := range parts {
		if p == "" || len(p) > 3 || (len(p) > 1 && p[0] == '0') || countDigits(p) != len(p) {
			return false
		}
		n, _ := strconv.Atoi(p)
		if n > 255 {
			return false
		}
		out[i] = byte(n)
	}
	return true
}

// parseIPv6 reads at most eight groups of 1 to 4 hex digits separated by ":", with at most one "::" standing for at
// least one zero group.
func parseIPv6(host string, out *[16]byte) bool {
	head, tail, compressed := strings.Cut(host, "::")
	if compressed && strings.Contains(tail, "::") {
		return false
	}
	groupsOf := func(text string) ([]uint16, bool) {
		if text == "" {
			return nil, true
		}
		var list []uint16
		for _, g := range strings.Split(text, ":") {
			if g == "" || len(g) > 4 {
				return nil, false
			}
			v, err := strconv.ParseUint(g, 16, 16)
			if err != nil || strings.ContainsAny(g, "+-_xX") {
				return nil, false
			}
			list = append(list, uint16(v))
		}
		return list, true
	}
	first, ok1 := groupsOf(head)
	last, ok2 := groupsOf(tail)
	if !ok1 || !ok2 {
		return false
	}
	switch {
	case compressed && len(first)+len(last) > 7:
		return false
	case !compressed && len(first) != 8:
		return false
	}
	var groups [8]uint16
	copy(groups[:], first)
	copy(groups[8-len(last):], last)
	for i, g := range groups {
		out[2*i], out[2*i+1] = byte(g>>8), byte(g)
	}
	return true
}

func parsePort(s string) (uint16, bool) {
	if s == "" || len(s) > 5 || s[0] == '0' || countDigits(s) != len(s) {
		return 0, false
	}
	n, _ := strconv.Atoi(s)
	if n > 65535 {
		return 0, false
	}
	return uint16(n), true
}

// allowed says whether the address is a loopback one (127.0.0.0/8, ::1) or one of the Tailscale network
// (100.64.0.0/10, fd7a:115c:a1e0::/48).
func (l Listen) allowed() bool {
	if !l.IsIPv6 {
		return l.IPv4[0] == 127 || (l.IPv4[0] == 100 && l.IPv4[1]&0xc0 == 64)
	}
	loopback := [16]byte{15: 1}
	tailscale := [6]byte{0xfd, 0x7a, 0x11, 0x5c, 0xa1, 0xe0}
	return l.IPv6 == loopback || [6]byte(l.IPv6[:6]) == tailscale
}
