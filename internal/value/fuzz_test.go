package value

import (
	"math/big"
	"net/netip" //nolint:depguard // the reference: parsing only, no I/O, as net.Listen reads the address (cmd/atoll/api.go)
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/vectors"
)

// The value grammars of specs.md §29.5 hold what the configuration says, which anyone who writes it controls: each
// fuzz target checks a parser against a reference written another way, with math/big or the standard library, and
// what it gives against what the rest of Atoll reads from it. Each is seeded from its vectors.

// grammarNumber reads number = digits [ "." digits ] at the start of s, as a fraction, and gives the rest.
func grammarNumber(s string) (*big.Rat, string, bool) {
	whole := countDigits(s)
	if whole == 0 {
		return nil, s, false
	}
	end := whole
	if end < len(s) && s[end] == '.' {
		fraction := countDigits(s[end+1:])
		if fraction == 0 {
			return nil, s, false
		}
		end += 1 + fraction
	}
	r, ok := new(big.Rat).SetString(s[:end])
	return r, s[end:], ok
}

// floorInt64 rounds a non-negative fraction down, and says whether it fits in an int64.
func floorInt64(r *big.Rat) (int64, bool) {
	q := new(big.Int).Quo(r.Num(), r.Denom())
	return q.Int64(), q.IsInt64()
}

func FuzzDuration(f *testing.F) {
	for _, v := range vectors.Load[durationVector](f, "duration.json") {
		f.Add(v.Input)
	}
	units := map[byte]int64{'s': int64(time.Second), 'm': int64(time.Minute), 'h': int64(time.Hour), 'd': 24 * int64(time.Hour), 'w': 7 * 24 * int64(time.Hour)}
	f.Fuzz(func(t *testing.T, s string) {
		got, err := ParseDuration(s)
		var want *big.Int
		if len(s) >= 2 && countDigits(s) == len(s)-1 && units[s[len(s)-1]] > 0 {
			want, _ = new(big.Int).SetString(s[:len(s)-1], 10)
			want.Mul(want, big.NewInt(units[s[len(s)-1]]))
		}
		switch {
		case want == nil || !want.IsInt64():
			if err == nil {
				t.Fatalf("ParseDuration(%q) = %d, want an error", s, got)
			}
		case err != nil:
			t.Fatalf("ParseDuration(%q): %v, want %d", s, err, want)
		case int64(got) != want.Int64():
			t.Fatalf("ParseDuration(%q) = %d, want %d", s, got, want)
		default:
			// What the configuration writes back reads as the same duration.
			if again, err := ParseDuration(FormatDuration(got)); err != nil || again != got {
				t.Fatalf("ParseDuration(FormatDuration(%d) = %q) = %d, %v", got, FormatDuration(got), again, err)
			}
		}
	})
}

func FuzzSize(f *testing.F) {
	for _, v := range vectors.Load[sizeVector](f, "size.json") {
		f.Add(v.Input)
	}
	units := map[string]int64{"B": 1, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12, "KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40}
	f.Fuzz(func(t *testing.T, s string) {
		got, err := ParseSize(s)
		n, unit, ok := grammarNumber(s)
		var want int64
		if ok && units[unit] > 0 {
			want, ok = floorInt64(n.Mul(n, new(big.Rat).SetInt64(units[unit])))
		}
		switch {
		case !ok || units[unit] == 0:
			if err == nil {
				t.Fatalf("ParseSize(%q) = %d, want an error", s, got)
			}
		case err != nil:
			t.Fatalf("ParseSize(%q): %v, want %d", s, err, want)
		case got != want:
			t.Fatalf("ParseSize(%q) = %d, want %d", s, got, want)
		}
	})
}

func FuzzPercent(f *testing.F) {
	for _, v := range vectors.Load[percentVector](f, "percent.json") {
		var of, count int64
		if v.Of != nil {
			of = *v.Of
		}
		if v.Count != nil {
			count = *v.Count
		}
		f.Add(v.Input, of, count)
	}
	hundred := new(big.Rat).SetInt64(100)
	f.Fuzz(func(t *testing.T, s string, total, count int64) {
		p, err := ParsePercent(s)
		n, rest, ok := grammarNumber(s)
		if !ok || rest != "%" || n.Cmp(hundred) > 0 {
			if err == nil {
				t.Fatalf("ParsePercent(%q) = %v, want an error", s, p)
			}
			return
		}
		if err != nil {
			t.Fatalf("ParsePercent(%q): %v", s, err)
		}
		if p.String() != s {
			t.Fatalf("ParsePercent(%q).String() = %q", s, p.String())
		}
		// The share of a total, exactly: share = total × n / 100.
		total = max(total, 0)
		share := new(big.Rat).Mul(new(big.Rat).SetInt64(total), n)
		share.Quo(share, hundred)
		if want, _ := floorInt64(share); p.Of(total) != want {
			t.Fatalf("ParsePercent(%q).Of(%d) = %d, want %d", s, total, p.Of(total), want)
		}
		c := new(big.Rat).SetInt64(count).Cmp(share)
		if p.Below(count, total) != (c < 0) || p.Exceeded(count, total) != (c > 0) {
			t.Fatalf("ParsePercent(%q) of %d: Below(%d) = %v, Exceeded = %v, against %s", s, total, count, p.Below(count, total), p.Exceeded(count, total), share.FloatString(3))
		}
	})
}

func FuzzWindow(f *testing.F) {
	for _, v := range vectors.Load[windowVector](f, "window.json") {
		f.Add(v.Input, int64(1_800_000_000), 0)
	}
	f.Fuzz(func(t *testing.T, s string, unix int64, offset int) {
		w, err := ParseWindow(s)
		clock := func(text string) (int, bool) {
			if len(text) != 5 || text[2] != ':' || countDigits(text[:2]) != 2 || countDigits(text[3:]) != 2 {
				return 0, false
			}
			h, _ := strconv.Atoi(text[:2])
			m, _ := strconv.Atoi(text[3:])
			return h*60 + m, h < 24 && m < 60
		}
		var start, end int
		ok := len(s) == 11 && s[5] == '-'
		if ok {
			var ok1, ok2 bool
			start, ok1 = clock(s[:5])
			end, ok2 = clock(s[6:])
			ok = ok1 && ok2 && start != end
		}
		if !ok {
			if err == nil {
				t.Fatalf("ParseWindow(%q) = %v, want an error", s, w)
			}
			return
		}
		if err != nil {
			t.Fatalf("ParseWindow(%q): %v", s, err)
		}
		if w.String() != s {
			t.Fatalf("ParseWindow(%q).String() = %q", s, w.String())
		}
		// Any instant of four centuries, in a zone of a fixed offset: no clock jumps there.
		zone := time.FixedZone("fuzz", (offset%(14*60))*60)
		at := time.Unix(unix%(400*365*86400), 0).In(zone)
		minute := at.Hour()*60 + at.Minute()
		inside := start <= minute && minute < end || start > end && (minute >= start || minute < end)
		if w.Contains(at) != inside {
			t.Fatalf("ParseWindow(%q).Contains(%s) = %v, want %v", s, at, w.Contains(at), inside)
		}
		next := w.Next(at)
		if next.Before(at) || !w.Contains(next) || inside && !next.Equal(at) || next.Sub(at) > 24*time.Hour {
			t.Fatalf("ParseWindow(%q).Next(%s) = %s", s, at, next)
		}
		opened := w.Opened(at)
		if inside != !opened.IsZero() || inside && (opened.After(at) || at.Sub(opened) >= 24*time.Hour || opened.Hour()*60+opened.Minute() != start) {
			t.Fatalf("ParseWindow(%q).Opened(%s) = %s", s, at, opened)
		}
	})
}

func FuzzListen(f *testing.F) {
	for _, v := range vectors.Load[listenVector](f, "listen.json") {
		f.Add(v.Input)
	}
	f.Fuzz(func(t *testing.T, s string) {
		l, err := ParseListen(s)
		if err != nil {
			return
		}
		// What the daemon listens on is what the standard library reads from the same text, and from String.
		for _, text := range []string{s, l.String()} {
			ap, perr := netip.ParseAddrPort(text)
			if perr != nil {
				t.Fatalf("ParseListen(%q) = %v, and netip refuses %q: %v", s, l, text, perr)
			}
			want := netip.AddrFrom4(l.IPv4)
			if l.IsIPv6 {
				want = netip.AddrFrom16(l.IPv6)
			}
			if ap.Addr() != want || ap.Port() != l.Port || ap.Addr().Zone() != "" {
				t.Fatalf("ParseListen(%q) = %v, netip reads %q as %v", s, l, text, ap)
			}
			// Loopback, or the Tailscale network, and never everywhere.
			tailscale := netip.MustParsePrefix("100.64.0.0/10").Contains(want) || netip.MustParsePrefix("fd7a:115c:a1e0::/48").Contains(want)
			if !want.IsLoopback() && !tailscale || l.Port == 0 {
				t.Fatalf("ParseListen(%q) = %v, neither loopback nor Tailscale", s, l)
			}
		}
		if again, err := ParseListen(l.String()); err != nil || again != l {
			t.Fatalf("ParseListen(%q) = %v, %v, want %v", l.String(), again, err, l)
		}
	})
}

func FuzzNUT(f *testing.F) {
	for _, v := range vectors.Load[nutVector](f, "ups_address.json") {
		f.Add(v.NUT)
	}
	// A host of two colons or more and no brackets is taken whole, a bracket in it too, though no address or name has
	// one: upsd is then not reached, and the grammar is version 1's (specs.md §29.3). Such a host cannot be written
	// another way.
	f.Add("0@::]")
	f.Fuzz(func(t *testing.T, s string) {
		n, err := ParseNUT(s)
		if err != nil {
			return
		}
		if n.UPS == "" || strings.Contains(n.UPS, "@") || n.Host == "" || n.Port < 1 || n.Port > 65535 || !strings.HasPrefix(s, n.UPS+"@") {
			t.Fatalf("ParseNUT(%q) = %+v", s, n)
		}
		if strings.ContainsAny(n.Host, "[]") {
			return
		}
		// Written back as NUT writes it, it reads the same.
		host := n.Host
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		text := n.UPS + "@" + host + ":" + strconv.Itoa(n.Port)
		if again, err := ParseNUT(text); err != nil || again != n {
			t.Fatalf("ParseNUT(%q) = %+v, %v, want %+v from %q", text, again, err, n, s)
		}
	})
}

func FuzzMinFree(f *testing.F) {
	for _, v := range vectors.Load[minFreeVector](f, "min_free.json") {
		if v.MinFree == nil {
			continue
		}
		var capacity int64
		if v.Capacity != nil {
			capacity = *v.Capacity
		}
		f.Add(strings.Join(*v.MinFree, ","), capacity)
	}
	f.Fuzz(func(t *testing.T, list string, capacity int64) {
		values := strings.Split(list, ",")
		m, err := ParseMinFree(values)
		if err != nil {
			return
		}
		capacity = max(capacity, 0)
		reserved, err := m.Reserved(capacity)
		if err != nil {
			t.Fatalf("ParseMinFree(%q).Reserved(%d): %v", values, capacity, err)
		}
		// The smallest of the values, each a size or a share of the capacity.
		smallest := int64(-1)
		for _, v := range values {
			var each int64
			if strings.HasSuffix(v, "%") {
				p, err := ParsePercent(v)
				if err != nil {
					t.Fatalf("ParseMinFree(%q) took %q", values, v)
				}
				each = p.Of(capacity)
			} else if each, err = ParseSize(v); err != nil {
				t.Fatalf("ParseMinFree(%q) took %q", values, v)
			}
			if smallest < 0 || each < smallest {
				smallest = each
			}
		}
		usable, err := m.Usable(capacity)
		if err != nil || reserved != smallest || usable != max(capacity-reserved, 0) {
			t.Fatalf("ParseMinFree(%q) of %d: reserved %d, usable %d, %v; want reserved %d", values, capacity, reserved, usable, err, smallest)
		}
	})
}
