// Package ups reads a UPS through NUT, as the daemon does to pause on battery and to stop its work before the battery
// runs out (specs.md §20). What a reading says is a pure decision, Next, whose cases are in
// conformance/vectors/ups_power.json; Read asks upsd, over NUT's own protocol, with no process and no login.
package ups

import (
	"slices"
	"strings"
)

// Power is what the daemon makes of the UPS.
type Power string

// What the daemon makes of the UPS: on line power, on battery, or low, its battery running out or a shutdown forced.
const (
	Online  Power = "online"
	Battery Power = "battery"
	Low     Power = "low"
)

// Reading is what the daemon makes of one reading of ups.status.
type Reading struct {
	Power Power
	// Answered says that the reading said something, so that the time of the last answer becomes now; Unreachable that
	// upsd has said nothing for dead time or longer.
	Answered    bool
	Unreachable bool
}

// Next works out what a reading of ups.status says, given the power before it, when the last answer that said
// something came, and how long the UPS may go unheard, upsmon's DEADTIME (specs.md §20). status is nil when upsd gave
// none. A reading says something when its flags hold OL, OB or FSD: low with FSD, or OB and LB together, as upsmon
// counts a UPS critical; on battery with OB; on line power with OL. From low, only OL goes back. A reading that says
// nothing keeps the power for dead time; past it, a UPS on battery is counted low, as upsmon counts one it no longer
// hears critical, and one on line power stays so.
func Next(last Power, answeredNS, nowNS, deadNS int64, status *string) Reading {
	if status != nil {
		flags := strings.Split(*status, " ")
		has := func(flag string) bool { return slices.Contains(flags, flag) }
		var power Power
		switch {
		case has("FSD"), has("OB") && has("LB"):
			power = Low
		case has("OB"):
			power = Battery
		case has("OL"):
			power = Online
		}
		if power != "" {
			if last == Low && power == Battery {
				power = Low
			}
			return Reading{Power: power, Answered: true}
		}
	}
	if nowNS-answeredNS < deadNS {
		return Reading{Power: last}
	}
	if last == Online {
		return Reading{Power: Online, Unreachable: true}
	}
	return Reading{Power: Low, Unreachable: true}
}

// Answer reads one line of upsd's answer to GET VAR <ups> ups.status, its line feed taken off: the value, or nil, and
// the code of an ERR line (conformance/vectors/ups_answer.json).
func Answer(name, line string) (status *string, code string) {
	line = strings.TrimSuffix(line, "\r")
	if rest, ok := strings.CutPrefix(line, "ERR "); ok {
		code, _, _ = strings.Cut(rest, " ")
		return nil, code
	}
	value, ok := strings.CutPrefix(line, "VAR "+name+" ups.status \"")
	if !ok {
		return nil, ""
	}
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		switch c := value[i]; c {
		case '\\':
			if i+1 == len(value) || (value[i+1] != '"' && value[i+1] != '\\') {
				return nil, ""
			}
			i++
			b.WriteByte(value[i])
		case '"':
			if i != len(value)-1 {
				return nil, ""
			}
			s := b.String()
			return &s, ""
		default:
			b.WriteByte(c)
		}
	}
	// No closing quote.
	return nil, ""
}
