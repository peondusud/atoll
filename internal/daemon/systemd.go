package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/fsx"
)

// The remount after a USB reset (specs.md §15.13) asks systemd to start the mount unit its fstab generator made of the
// disk's /etc/fstab entry, rather than running mount itself: what the mount starts, as ntfs-3g's FUSE process, then
// belongs to that unit, and a stop or a restart of the daemon's service, which ends every process of the service,
// leaves the disk mounted. The account atoll may start a mount unit through deploy/50-atoll.rules, polkit's rule the
// .deb installs. Where systemd is not PID 1, has no unit for the mount point, or refuses, the daemon runs mount itself,
// as it always did, and says why once.

// systemdWay is how the remount reaches systemd.
type systemdWay struct {
	// fstab is the table systemd-fstab-generator makes mount units of: a remount that reads another runs mount itself.
	fstab string
	// booted is a folder that is there only when systemd is PID 1, as sd_booted(3) asks.
	booted string
	// systemctl is the program, by its absolute path, never one found on PATH (docs/threat-model.md, G19).
	systemctl string
}

// machineSystemd is the machine's own systemd.
var machineSystemd = systemdWay{fstab: "/etc/fstab", booted: "/run/systemd/system", systemctl: "/usr/bin/systemctl"}

const (
	// unitNameMax is the longest name systemd gives a unit as it is: past it, systemd-fstab-generator names the unit of
	// an entry by a hash, which Atoll does not compute.
	unitNameMax = 255
	// unmountSeen is how long the remount waits, once the dead mount is detached, for systemd to read from the mount
	// table that it is gone: until then the unit is active, and a start does nothing.
	unmountSeen = 10 * time.Second
	// unitPoll is how often the remount asks systemd about the unit while it waits.
	unitPoll = 100 * time.Millisecond
)

// Why a remount mounts the disk itself: each is said once a run, with what systemctl said (specs.md §15.13).
const (
	whyNoSystemd   = "systemd is not the service manager"
	whyOtherTable  = "the remount reads a table that is not the one systemd reads"
	whyNoUnitName  = "the mount point has no mount unit name Atoll can tell"
	whyNoSystemctl = "systemctl cannot say what systemd holds of the mount point"
	whyNoUnit      = "systemd has no unit for the mount point: run systemctl daemon-reload once /etc/fstab changed"
	whyRefused     = "systemd refused to start the mount unit: install Atoll's polkit rule (deploy/README.md)"
)

// mountUnit gives the name of the mount unit systemd gives a mount point: what systemd-escape --path --suffix=mount
// writes, and systemd-fstab-generator names the unit of an /etc/fstab entry (conformance/vectors/mount_unit.json).
// The path loses its empty and "." components; "/" between two becomes "-"; every byte but an ASCII letter, a digit,
// ":", "_" and a "." that is not first becomes \x and two lowercase hexadecimal digits.
func mountUnit(where string) (string, error) {
	if !strings.HasPrefix(where, "/") {
		return "", fmt.Errorf("mount unit of %q: not an absolute path", where)
	}
	var parts []string
	for part := range strings.SplitSeq(where, "/") {
		switch part {
		case "", ".":
		case "..":
			return "", fmt.Errorf("mount unit of %q: a '..' component", where)
		default:
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return "-.mount", nil
	}
	path := strings.Join(parts, "/")
	var b strings.Builder
	for i := range len(path) {
		switch c := path[i]; {
		case c == '/':
			b.WriteByte('-')
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == ':', c == '_', c == '.' && i > 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, `\x%02x`, c)
		}
	}
	b.WriteString(".mount")
	if b.Len() > unitNameMax {
		return "", fmt.Errorf("mount unit of %q: %d bytes, which systemd names by a hash", where, b.Len())
	}
	return b.String(), nil
}

// unitShown is what systemctl show says of a unit: its LoadState, ActiveState, Result, FragmentPath, What and Job.
type unitShown struct {
	load, active, result, fragment, what, job string
}

// parseShown reads what systemctl show writes, a property a line; one it left out is empty.
func parseShown(out []byte) unitShown {
	var u unitShown
	for line := range strings.SplitSeq(string(out), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "LoadState":
			u.load = value
		case "ActiveState":
			u.active = value
		case "Result":
			u.result = value
		case "FragmentPath":
			u.fragment = value
		case "What":
			u.what = value
		case "Job":
			u.job = value
		}
	}
	return u
}

// mounted says whether a mount unit's ActiveState holds a mount, or one systemd is taking away.
func mounted(active string) bool {
	return active == "active" || active == "reloading" || active == "deactivating"
}

// systemd gives the way to systemd the loop takes: the machine's, unless a test gave another.
func (l *loop) systemd() systemdWay {
	if l.set.systemd == (systemdWay{}) {
		return machineSystemd
	}
	return l.set.systemd
}

// mountUnitOf gives the mount unit systemd would start for a disk's mount point, or why the remount cannot go through
// systemd, and what systemctl said of it.
func (l *loop) mountUnitOf(ctx context.Context, d *config.Disk) (unit, why, detail string) {
	way := l.systemd()
	if l.set.fstab != way.fstab {
		return "", whyOtherTable, l.set.fstab
	}
	if booted, err := fsx.Exists(way.booted); err != nil {
		return "", whyNoSystemd, err.Error()
	} else if !booted {
		return "", whyNoSystemd, way.booted + " is not there"
	}
	unit, err := mountUnit(d.Mount)
	if err != nil {
		return "", whyNoUnitName, err.Error()
	}
	shown, err := l.showUnit(ctx, unit)
	switch {
	case err != nil:
		return "", whyNoSystemctl, err.Error()
	case shown.load != "loaded" || shown.fragment == "":
		// A unit read from the mount table alone has no fragment: systemd has nothing to mount it with.
		return "", whyNoUnit, fmt.Sprintf("%s: LoadState=%s FragmentPath=%s", unit, shown.load, shown.fragment)
	}
	return unit, "", ""
}

// startUnit has systemd start a disk's mount unit, once the dead mount is detached, and waits for its job to end: the
// unit is then active, the disk mounted, or not, which the next check finds (specs.md §15.13). It gives why the
// remount must mount the disk itself when systemd refused the start, and "" otherwise: a unit that systemd started and
// that failed is not mounted by hand after it, which would make its FUSE process the service's again.
func (l *loop) startUnit(ctx context.Context, d *config.Disk, unit string) (why, detail string) {
	way := l.systemd()
	shown, seen, err := l.awaitUnit(ctx, unit, unmountSeen, func(u unitShown) bool { return !mounted(u.active) })
	switch {
	case err != nil:
		return whyNoSystemctl, err.Error()
	case !seen:
		l.log.Warn("the remount failed: systemd still holds the mount unit active once the dead mount is detached",
			"disk", d.ID, "unit", unit, "state", shown.active)
		return "", ""
	}
	// A device that is not back yet is not waited for here: systemd would hold the start until it gives up on the
	// device, 90 s later, and the loop with it. The next check tries again.
	if strings.HasPrefix(shown.what, "/") && !strings.HasPrefix(shown.what, "//") {
		if there, err := fsx.Reachable(shown.what); err == nil && !there {
			l.log.Warn("the remount waits: its device is not there", "disk", d.ID, "unit", unit, "device", shown.what)
			return "", ""
		}
	}
	if out, err := l.command(ctx, way.systemctl, "--no-block", "--no-ask-password", "start", "--", unit); err != nil {
		return whyRefused, fmt.Sprintf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	shown, done, err := l.awaitUnit(ctx, unit, l.remountWait(), func(u unitShown) bool { return u.job == "" })
	switch {
	case err != nil:
		l.log.Warn("the remount failed", "disk", d.ID, "unit", unit, "error", err)
	case !done:
		l.log.Warn("the remount failed", "disk", d.ID, "unit", unit, "error", fmt.Sprintf("no answer within %v", l.remountWait()))
	case shown.active != "active":
		l.log.Warn("the remount failed", "disk", d.ID, "unit", unit, "state", shown.active, "result", shown.result)
	default:
		l.log.Info("remounted after a reset", "disk", d.ID, "mount", d.Mount, "unit", unit)
	}
	return "", ""
}

// awaitUnit asks systemd about a unit until done holds, for so long at most, and gives what it said last and whether
// done held then.
func (l *loop) awaitUnit(ctx context.Context, unit string, within time.Duration, done func(unitShown) bool) (unitShown, bool, error) {
	deadline := time.Now().Add(within)
	tick := time.NewTicker(unitPoll)
	defer tick.Stop()
	for {
		shown, err := l.showUnit(ctx, unit)
		switch {
		case err != nil:
			return shown, false, err
		case done(shown):
			return shown, true, nil
		case !time.Now().Before(deadline):
			return shown, false, nil
		}
		select {
		case <-ctx.Done():
			return shown, false, fmt.Errorf("waiting for %s: %w", unit, ctx.Err())
		case <-tick.C:
		}
	}
}

// showUnit asks systemd what it holds of a unit. Reading a unit takes no authorization.
func (l *loop) showUnit(ctx context.Context, unit string) (unitShown, error) {
	out, err := l.output(ctx, l.systemd().systemctl, "show", "-p", "LoadState", "-p", "ActiveState", "-p", "Result",
		"-p", "FragmentPath", "-p", "What", "-p", "Job", "--", unit)
	if err != nil {
		return unitShown{}, fmt.Errorf("systemctl show %s: %w", unit, err)
	}
	return parseShown(out), nil
}

// mountsItself says, once a run for each reason, why a remount mounts the disk itself rather than through systemd: its
// FUSE process, as ntfs-3g's, is then the service's, and a stop or a restart of the daemon unmounts the disk.
func (l *loop) mountsItself(ctx context.Context, d *config.Disk, why, detail string) {
	if l.mountWhys == nil {
		l.mountWhys = map[string]bool{}
	}
	if l.mountWhys[why] {
		return
	}
	l.mountWhys[why] = true
	level := slog.LevelWarn
	if why == whyNoSystemd || why == whyOtherTable {
		level = slog.LevelInfo
	}
	l.log.Log(ctx, level, "the remount mounts the disk itself, not through systemd", "disk", d.ID, "why", why,
		"detail", detail)
}
