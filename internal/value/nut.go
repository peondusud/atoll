package value

import (
	"errors"
	"fmt"
	"strings"
)

// NUTPort is the port upsd listens on, NUT's own, when ups.nut names none.
const NUTPort = 3493

// NUT is where the daemon reads a UPS through NUT (specs.md §20): the UPS's name, and the host and port of the upsd
// that knows it.
type NUT struct {
	UPS  string
	Host string
	Port int
}

// ParseNUT reads ups.nut as NUT writes it, <ups>@<host>[:<port>] (conformance/vectors/ups_address.json). A host in
// brackets is an IPv6 address, which a port may follow; one with more than one colon and no brackets is an IPv6 address
// with no port.
func ParseNUT(s string) (NUT, error) {
	if s == "" {
		return NUT{}, errors.New("no NUT address")
	}
	name, rest, found := strings.Cut(s, "@")
	switch {
	case !found:
		return NUT{}, fmt.Errorf("%q names no host: <ups>@<host>[:<port>]", s)
	case name == "":
		return NUT{}, fmt.Errorf("%q names no UPS", s)
	case strings.Contains(rest, "@"):
		return NUT{}, fmt.Errorf("%q holds more than one @", s)
	}
	host, port, hasPort := rest, "", false
	switch {
	case strings.HasPrefix(rest, "["):
		end := strings.IndexByte(rest, ']')
		if end < 0 {
			return NUT{}, fmt.Errorf("%q: a bracket is not closed", s)
		}
		host = rest[1:end]
		if after := rest[end+1:]; after != "" {
			p, ok := strings.CutPrefix(after, ":")
			if !ok {
				return NUT{}, fmt.Errorf("%q: what follows the bracket is no port", s)
			}
			port, hasPort = p, true
		}
	case strings.Count(rest, ":") == 1:
		host, port, _ = strings.Cut(rest, ":")
		hasPort = true
	}
	if host == "" {
		return NUT{}, fmt.Errorf("%q names no host", s)
	}
	n := NUTPort
	if hasPort {
		p, ok := parsePort(port)
		if !ok {
			return NUT{}, fmt.Errorf("%q: the port must be 1 to 65535, with no leading zero", s)
		}
		n = int(p)
	}
	return NUT{UPS: name, Host: host, Port: n}, nil
}
