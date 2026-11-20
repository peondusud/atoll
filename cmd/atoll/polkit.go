package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/daemon"
)

// polkitAccount is the account the rule speaks for: the one deploy/atoll.service runs the daemon as, and that
// deploy/50-atoll.rules lets start any mount unit (specs.md §15.13, §26).
const polkitAccount = "atoll"

// Why a disk's mount unit is left out of the rule, a closed list (api/schemas/polkit-rule.schema.json).
const (
	// polkitFailed is a disk failed, which the daemon never mounts again.
	polkitFailed = "failed"
	// polkitNoUnitName is a mount point whose unit's name would pass 255 bytes, which systemd names by a hash Atoll does
	// not compute: the daemon mounts that disk again itself.
	polkitNoUnitName = "no-unit-name"
)

// polkitUnit is one unit of api/schemas/polkit-rule.schema.json.
type polkitUnit struct {
	Disk string `json:"disk"`
	Unit string `json:"unit"`
}

// polkitLeftOut is one disk of left_out.
type polkitLeftOut struct {
	Disk   string `json:"disk"`
	Reason string `json:"reason"`
}

// polkitOutput is the output of api/schemas/polkit-rule.schema.json.
type polkitOutput struct {
	Account string          `json:"account"`
	Units   []polkitUnit    `json:"units"`
	LeftOut []polkitLeftOut `json:"left_out"`
	Rule    string          `json:"rule"`
}

// runPolkitRule prints polkit's rule for the pool (specs.md §25; docs/threat-model.md, G22): the account atoll may
// start the mount units of the configuration's disks, which the remount after a USB reset starts (§15.13), and is
// refused the start of any other mount unit, which deploy/50-atoll.rules would let it start. Root installs it in
// /etc/polkit-1/rules.d/, where polkitd reads it before that rule (deploy/README.md). It reads the configuration
// alone: no catalog, no disk, no lock. It exits with 4 when a disk in service is left out, the rule printed all the same.
func runPolkitRule(configPath string, args []string, stdout, stderr io.Writer) int {
	asJSON := false
	for _, a := range args {
		if a != "--json" {
			return usage(stderr, fmt.Sprintf(T("polkit-rule: unknown argument %q"), a))
		}
		if err := setFlag(&asJSON, a); err != nil {
			return usage(stderr, "polkit-rule: "+err.Error())
		}
	}
	c, err := load(configPath)
	if err != nil {
		return failure(stderr, err)
	}
	out := polkitRule(c)
	if asJSON {
		err = writeJSON(stdout, out)
	} else {
		err = writeText(stdout, out.Rule)
	}
	if err != nil {
		return failure(stderr, err)
	}
	code := exitOK
	for _, l := range out.LeftOut {
		if l.Reason == polkitNoUnitName {
			report(stderr, fmt.Sprintf(T("disk %q: its mount unit would have a name past 255 bytes, which systemd replaces by a hash Atoll does not compute: the rule leaves it out, and the daemon mounts that disk again itself after a USB reset"), l.Disk))
			code = exitPartial
		}
	}
	return code
}

// polkitRule gives the rule for the configuration's disks, in their order: each disk's mount unit, as systemd names
// it (daemon.MountUnit), but for a disk failed, which the daemon never mounts again, and one whose unit Atoll cannot
// name, which the daemon mounts again itself (specs.md §15.13). Its text depends on nothing but the configuration.
func polkitRule(c *config.Config) *polkitOutput {
	out := &polkitOutput{Account: polkitAccount, Units: []polkitUnit{}, LeftOut: []polkitLeftOut{}}
	for _, d := range c.Disks {
		if d.State == config.DiskFailed {
			out.LeftOut = append(out.LeftOut, polkitLeftOut{Disk: d.ID, Reason: polkitFailed})
			continue
		}
		unit, err := daemon.MountUnit(d.Mount)
		if err != nil {
			out.LeftOut = append(out.LeftOut, polkitLeftOut{Disk: d.ID, Reason: polkitNoUnitName})
			continue
		}
		out.Units = append(out.Units, polkitUnit{Disk: d.ID, Unit: unit})
	}
	out.Rule = polkitRuleText(c.PoolID, out)
	return out
}

// polkitRuleText writes the rule, for polkitd's JavaScript (ES5, as Duktape reads it): a start of a unit of the list by
// the account is allowed; a start of any other mount unit by it is refused, which polkitd decides there, before
// deploy/50-atoll.rules allows it; anything else is left to the rules after it. In ASCII and in English, whatever the
// language: it is a file to install, as mergerfs's line is. The pool's and the disks' ids are identifiers, which a
// comment holds as they are; each unit's name is a JavaScript string (jsString).
func polkitRuleText(pool string, out *polkitOutput) string {
	var b strings.Builder
	b.WriteString("// /etc/polkit-1/rules.d/49-atoll-pool.rules: Atoll's polkit rule for the pool " + pool + ", which atoll polkit-rule prints\n" +
		"// (deploy/README.md). polkitd reads it before 50-atoll.rules, the .deb's, which lets the account " + polkitAccount + " start any mount\n" +
		"// unit: with it, " + polkitAccount + " may start the mount units of the pool's disks below, as the remount after a USB reset does, and\n" +
		"// the start of any other mount unit is refused. Print it again after atoll disk add, remove, replace or fail: a disk\n" +
		"// whose unit is not below, the daemon mounts itself, not through systemd.\n" +
		"polkit.addRule(function (action, subject) {\n")
	if len(out.Units) == 0 {
		b.WriteString("\tvar units = [];\n")
	} else {
		b.WriteString("\tvar units = [\n")
		for i, u := range out.Units {
			comma := ","
			if i == len(out.Units)-1 {
				comma = ""
			}
			b.WriteString("\t\t" + jsString(u.Unit) + comma + " // " + u.Disk + "\n")
		}
		b.WriteString("\t];\n")
	}
	if len(out.LeftOut) > 0 {
		left := make([]string, len(out.LeftOut))
		for i, l := range out.LeftOut {
			left[i] = l.Disk + " (" + l.Reason + ")"
		}
		b.WriteString("\t// Left out: " + strings.Join(left, ", ") + ".\n")
	}
	b.WriteString("\tif (action.id !== \"org.freedesktop.systemd1.manage-units\" || subject.user !== " + jsString(polkitAccount) + " ||\n" +
		"\t\taction.lookup(\"verb\") !== \"start\") {\n" +
		"\t\treturn polkit.Result.NOT_HANDLED;\n" +
		"\t}\n" +
		"\tvar unit = action.lookup(\"unit\") || \"\";\n" +
		"\tif (units.indexOf(unit) >= 0) {\n" +
		"\t\treturn polkit.Result.YES;\n" +
		"\t}\n" +
		"\tif (/\\.mount$/.test(unit)) {\n" +
		"\t\treturn polkit.Result.NO;\n" +
		"\t}\n" +
		"\treturn polkit.Result.NOT_HANDLED;\n" +
		"});\n")
	return b.String()
}

// jsString writes text as a JavaScript string literal between double quotes, for a name of systemd's, which is ASCII:
// a backslash and a double quote take a backslash, as systemd's escapes would otherwise be read as JavaScript's, "\x2d"
// as "-", another unit's name; a byte outside printable ASCII, which no name of systemd's holds, is written \u00 and
// its two hexadecimal digits, so that nothing can end a line of the rule. JSON reads such a literal as JavaScript does.
func jsString(text string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := range len(text) {
		switch c := text[i]; {
		case c == '\\' || c == '"':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c >= 0x20 && c < 0x7f:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, `\u%04x`, c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
