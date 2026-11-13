package daemon

import (
	"errors"
	"io/fs"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/names"
)

// probeDirty reads, once a time an NTFS disk is missing, whether its volume is marked dirty, as an unclean unplugging
// leaves it and Linux then refuses to mount it (specs.md §22). It reads only once the volume's device is there, a few
// sectors, so that a disk left unplugged is never woken by it; a disk back, or failed, forgets what was read.
func (l *loop) probeDirty(d *config.Disk, st *diskState) {
	if st.state != disks.Missing || d.FS != names.NTFS || d.State == config.DiskFailed {
		st.probed, st.dirty = false, false
		return
	}
	if st.probed {
		return
	}
	dirty, err := disks.ProbeNTFSDirty(l.opt.Machine, d)
	switch {
	case errors.Is(err, disks.ErrNoDevice), errors.Is(err, fs.ErrNotExist):
		// Not plugged in, or no machine to read it: the next check looks again.
		return
	case err != nil:
		l.log.Warn("the NTFS volume of a missing disk cannot be read", "disk", d.ID, "error", err)
	case dirty:
		l.log.Warn("the NTFS volume of a missing disk is marked dirty: repair it with chkdsk on Windows", "disk", d.ID)
		// A pass works the alert out, as after a reading of SMART that tells of trouble: nothing else brings one for
		// a disk that stays away, and a dry run sends none.
		if l.opt.Apply && l.set != nil && len(l.set.channels) > 0 {
			if now := l.opt.Machine.Now(); l.passDue == nil || *l.passDue > now {
				l.passDue = &now
			}
		}
	}
	st.probed, st.dirty = true, dirty
}

// dirtyDisks gives the disks whose NTFS volume was found dirty while missing, in the configuration's order.
func (l *loop) dirtyDisks() []string {
	var out []string
	for i := range l.opt.Config.Disks {
		if st := l.disks[l.opt.Config.Disks[i].ID]; st != nil && st.dirty {
			out = append(out, l.opt.Config.Disks[i].ID)
		}
	}
	return out
}
