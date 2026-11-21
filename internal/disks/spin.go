package disks

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Power is what Atoll knows of whether a whole disk spins (specs.md §10).
type Power string

// What a whole disk's power mode says.
const (
	// PowerUnknown is no answer: it counts as spinning.
	PowerUnknown  Power = ""
	PowerStandby  Power = "standby"
	PowerSpinning Power = "spinning"
)

// SpinReader tells whether the whole disks of a machine spin, waking none of them (specs.md §10). A machine need not
// have it: then every disk counts as online.
type SpinReader interface {
	// Rotational says whether a whole disk has a spindle: one with none, an SSD, never sleeps.
	Rotational(name string) (bool, error)
	// BlockStat gives a whole disk's I/O counters, as the kernel keeps them. ATA pass-through, as a power mode asked,
	// counts in none of them.
	BlockStat(name string) (BlockStat, error)
	// PowerMode asks a whole disk its power mode, by ATA CHECK POWER MODE through SCSI ATA pass-through, which wakes
	// no disk. It gives PowerUnknown and an error when the disk gives no answer that says it.
	PowerMode(name string) (Power, error)
}

// ATA's sense data for ATA pass-through (SAT): the response codes, the ATA Status Return descriptor, the additional
// sense code of "ATA pass-through information available", and ERR of the status.
const (
	senseFixed      = 0x70
	senseDescriptor = 0x72
	ataReturnCode   = 0x09
	ataReturnLength = 0x0c
	ascPassThrough  = 0x00
	ascqPassThrough = 0x1d
	statusErr       = 0x01
)

// PowerModeOf reads what a disk answered to CHECK POWER MODE, from the sense data of an ATA PASS-THROUGH (16) sent
// with CK_COND set (specs.md §10, conformance/vectors/power_mode.json): the count it returns, from the ATA Status Return
// descriptor of descriptor-format sense data, or from the information field of fixed-format sense data that says ATA
// pass-through information is available, its status free of ERR.
func PowerModeOf(sense []byte) Power {
	count, status, ok := ataReturn(sense)
	if !ok || status&statusErr != 0 {
		return PowerUnknown
	}
	switch count {
	case 0x00, 0x01, 0x40:
		return PowerStandby
	case 0x41, 0x80, 0x81, 0x82, 0x83, 0xff:
		return PowerSpinning
	}
	return PowerUnknown
}

// ataReturn finds the count and the status an ATA command returned in sense data.
func ataReturn(sense []byte) (count, status byte, ok bool) {
	switch {
	case len(sense) >= 8 && sense[0] == senseDescriptor:
		end := min(8+int(sense[7]), len(sense))
		for at := 8; at+2 <= end; {
			code, length := sense[at], int(sense[at+1])
			if at+2+length > end {
				return 0, 0, false
			}
			if code == ataReturnCode && length == ataReturnLength {
				return sense[at+5], sense[at+13], true
			}
			at += 2 + length
		}
	case len(sense) >= 14 && sense[0]&0x7f == senseFixed && sense[12] == ascPassThrough && sense[13] == ascqPassThrough:
		return sense[6], sense[4], true
	}
	return 0, 0, false
}

// BlockStat is what a whole disk's counters say of its I/O: the requests it completed, reads, writes, discards and
// flushes, and those under way.
type BlockStat struct {
	Completed, InFlight int64
}

// The fields of /sys/block/<name>/stat: 11 before Linux 4.18, 15 with discards, 17 with flushes.
const (
	statReads     = 0
	statWrites    = 4
	statInFlight  = 8
	statDiscards  = 11
	statFlushes   = 15
	statFieldsMin = 11
)

// ParseBlockStat reads the content of /sys/block/<name>/stat (specs.md §10, conformance/vectors/block_stat.json).
func ParseBlockStat(text string) (BlockStat, error) {
	fields := strings.Fields(text)
	if len(fields) < statFieldsMin {
		return BlockStat{}, fmt.Errorf("block stat: %d fields, want %d at least", len(fields), statFieldsMin)
	}
	values := make([]int64, len(fields))
	for i, f := range fields {
		v, err := strconv.ParseInt(f, 10, 64)
		if err != nil || v < 0 {
			return BlockStat{}, fmt.Errorf("block stat: field %d is %q", i+1, f)
		}
		values[i] = v
	}
	s := BlockStat{Completed: values[statReads] + values[statWrites], InFlight: values[statInFlight]}
	for _, at := range []int{statDiscards, statFlushes} {
		if at < len(values) {
			s.Completed += values[at]
		}
	}
	return s, nil
}

// WholeDiskLook is what a check knows of a whole disk below a mount before it asks it anything: whether it has a
// spindle (nil when that cannot be read), its completed I/O at the last check and now (nil for no earlier count, or
// counters that cannot be read), whether requests are under way, and whether it gave no answer to an earlier check.
type WholeDiskLook struct {
	Name        string
	Rotational  *bool
	Before, Now *int64
	Busy        bool
	Refused     bool
}

// Standby says whether a disk is in standby, from the whole disks below its mount, asking ask the power mode of those
// its counters say nothing of (specs.md §10, conformance/vectors/spin.json). A whole disk with no spindle spins; so
// does one that completed I/O since the last check or has requests under way, and nothing is sent to it. One that gave
// no answer before is unknown, as is one that gives none now, and unknown counts as spinning. The disk is in standby
// when one of its whole disks answers standby: a read may need it.
func Standby(list []WholeDiskLook, ask func(name string) Power) bool {
	standby := false
	for i := range list {
		w := &list[i]
		moved := w.Before != nil && w.Now != nil && *w.Before != *w.Now
		if (w.Rotational != nil && !*w.Rotational) || moved || w.Busy || w.Refused {
			continue
		}
		if ask(w.Name) == PowerStandby {
			standby = true
		}
	}
	return standby
}

// ErrNoPowerMode says that a whole disk gave no answer that says its power mode.
var ErrNoPowerMode = errors.New("no answer to CHECK POWER MODE")
