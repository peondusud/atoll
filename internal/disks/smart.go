package disks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/peondusud/atoll/internal/config"
)

// Smart is what a reading of SMART says of a disk, as smartctl -j -n standby gives it (specs.md §23).
type Smart struct {
	// Asleep says that the disk was in standby or asleep, and smartctl left it so: nothing was read.
	Asleep bool `json:"asleep,omitempty"`
	// NeedsSAT says that the answer asks for a device type, as behind a USB bridge smartctl does not know: a
	// reading with -d sat follows.
	NeedsSAT bool `json:"needs_sat,omitempty"`
	// Failing says that the disk's own health check failed, or that an NVMe disk raised a critical warning.
	Failing bool `json:"failing"`
	// Reallocated, Pending and Uncorrectable count the sectors the disk moved, waits to move, and could not read:
	// ATA attributes 5, 197 and 198; an NVMe disk's media errors are Uncorrectable.
	Reallocated   int64 `json:"reallocated"`
	Pending       int64 `json:"pending"`
	Uncorrectable int64 `json:"uncorrectable"`
	// Temperature is in degrees Celsius, 0 when the disk gives none.
	Temperature int64 `json:"temperature"`
}

// Sectors counts the sectors the disk moved, waits to move, and could not read: an alert's count, which grows as the
// disk gets worse (specs.md §23).
func (s *Smart) Sectors() int64 {
	return s.Reallocated + s.Pending + s.Uncorrectable
}

// Worse says that a reading tells of more trouble than an earlier one: a health check failed that had passed, or
// more bad sectors. A nil earlier reading is one that told of none.
func (s *Smart) Worse(earlier *Smart) bool {
	if earlier == nil {
		earlier = &Smart{}
	}
	return s.Failing && !earlier.Failing || s.Sectors() > earlier.Sectors()
}

// smartctlOutput is the part of smartctl -j's output a reading needs.
type smartctlOutput struct {
	Smartctl struct {
		ExitStatus *int `json:"exit_status"`
		Messages   []struct {
			String string `json:"string"`
		} `json:"messages"`
	} `json:"smartctl"`
	SmartStatus *struct {
		Passed *bool `json:"passed"`
	} `json:"smart_status"`
	Temperature *struct {
		Current *int64 `json:"current"`
	} `json:"temperature"`
	ATA *struct {
		Table []struct {
			ID  int `json:"id"`
			Raw struct {
				Value int64 `json:"value"`
			} `json:"raw"`
		} `json:"table"`
	} `json:"ata_smart_attributes"`
	NVMe *struct {
		CriticalWarning int64 `json:"critical_warning"`
		MediaErrors     int64 `json:"media_errors"`
		Temperature     int64 `json:"temperature"`
	} `json:"nvme_smart_health_information_log"`
}

// The bits of smartctl's exit status a reading looks at: the device could not be opened, or was in a low-power mode
// with -n; and the disk failed its health check.
const (
	smartctlOpenFailed = 1 << 1
	smartctlFailing    = 1 << 3
)

// ParseSmart reads what smartctl -j -n standby printed for one device (specs.md §23). An answer that holds no
// reading, neither a disk asleep nor one that asks for a device type, is an error.
func ParseSmart(out []byte) (Smart, error) {
	var o smartctlOutput
	if err := json.Unmarshal(out, &o); err != nil {
		return Smart{}, fmt.Errorf("smartctl: %w", err)
	}
	if o.Smartctl.ExitStatus == nil {
		return Smart{}, errors.New("smartctl: no exit_status")
	}
	status := *o.Smartctl.ExitStatus
	said := func(words ...string) bool {
		for _, m := range o.Smartctl.Messages {
			for _, w := range words {
				if strings.Contains(m.String, w) {
					return true
				}
			}
		}
		return false
	}
	var s Smart
	switch {
	case status&smartctlOpenFailed != 0 && said("STANDBY", "SLEEP"):
		s.Asleep = true
		return s, nil
	case said("-d option", "Unknown USB bridge"):
		s.NeedsSAT = true
		return s, nil
	}
	read := false
	if o.SmartStatus != nil && o.SmartStatus.Passed != nil {
		read = true
		s.Failing = !*o.SmartStatus.Passed
	}
	s.Failing = s.Failing || status&smartctlFailing != 0
	if o.ATA != nil {
		read = true
		for _, a := range o.ATA.Table {
			switch a.ID {
			case 5:
				s.Reallocated = a.Raw.Value
			case 197:
				s.Pending = a.Raw.Value
			case 198:
				s.Uncorrectable = a.Raw.Value
			}
		}
	}
	if o.NVMe != nil {
		read = true
		s.Failing = s.Failing || o.NVMe.CriticalWarning != 0
		s.Uncorrectable = o.NVMe.MediaErrors
		s.Temperature = o.NVMe.Temperature
	}
	if o.Temperature != nil && o.Temperature.Current != nil {
		s.Temperature = *o.Temperature.Current
	}
	if !read {
		return Smart{}, fmt.Errorf("smartctl: no reading, exit status %d", status)
	}
	return s, nil
}

// WorstSmart gives the reading of a disk that lives on several devices: the worst of each, failing if one is, each
// count the largest, the temperature the highest. Asleep only when every device is.
func WorstSmart(readings []Smart) Smart {
	var w Smart
	w.Asleep = len(readings) > 0
	for _, r := range readings {
		w.Asleep = w.Asleep && r.Asleep
		w.Failing = w.Failing || r.Failing
		w.Reallocated = max(w.Reallocated, r.Reallocated)
		w.Pending = max(w.Pending, r.Pending)
		w.Uncorrectable = max(w.Uncorrectable, r.Uncorrectable)
		w.Temperature = max(w.Temperature, r.Temperature)
	}
	return w
}

// SmartRecord is a reading as the catalog keeps it, in disks.smart_json: when it was read, and what it said. Only
// a reading of a disk awake is kept (specs.md §23).
type SmartRecord struct {
	ReadNS int64 `json:"read_ns"`
	Smart
}

// SmartReader runs smartctl, for the daemon's reading of SMART (specs.md §23). A machine need not have it: then
// nothing reads SMART.
type SmartReader interface {
	// Smartctl gives what smartctl printed on its standard output with these arguments, whatever its exit status, as
	// smartctl -j says there what went wrong; an error when it could not run.
	Smartctl(ctx context.Context, args ...string) ([]byte, error)
}

// ErrNoSmart says that a machine cannot read SMART: it runs no smartctl, or cannot tell the whole disks below a
// mount.
var ErrNoSmart = errors.New("this machine reads no SMART")

// smartTimeout is how long smartctl has for one device: a USB bridge that hangs must not hold the daemon.
const smartTimeout = 30 * time.Second

// ReadSmart reads the SMART of the whole disks below a disk's mount (specs.md §23): smartctl -j -n standby on each,
// again with -d sat when the answer asks for a device type, and the worst of the readings. A disk asleep is left
// so, and its reading says only that.
func ReadSmart(ctx context.Context, m Machine, d *config.Disk) (Smart, error) {
	reader, runs := m.(SmartReader)
	topology, tells := m.(Topology)
	if !runs || !tells {
		return Smart{}, ErrNoSmart
	}
	seen := m.Mount(d.Mount)
	if seen.Entry == nil {
		return Smart{}, fmt.Errorf("%s: not mounted", d.Mount)
	}
	devices, err := topology.PhysicalDisks(d.FS, seen.Entry, d.UUID)
	if err != nil {
		return Smart{}, fmt.Errorf("its devices: %w", err)
	}
	if len(devices) == 0 {
		return Smart{}, fmt.Errorf("%s: no device", d.Mount)
	}
	readings := make([]Smart, 0, len(devices))
	for _, name := range devices {
		s, err := readDevice(ctx, reader, "/dev/"+name)
		if err != nil {
			return Smart{}, fmt.Errorf("/dev/%s: %w", name, err)
		}
		readings = append(readings, s)
	}
	return WorstSmart(readings), nil
}

// readDevice reads one device, and again with -d sat when the answer asks for a device type.
func readDevice(ctx context.Context, reader SmartReader, device string) (Smart, error) {
	run := func(args ...string) (Smart, error) {
		ctx, cancel := context.WithTimeout(ctx, smartTimeout)
		defer cancel()
		out, err := reader.Smartctl(ctx, args...)
		if err != nil {
			return Smart{}, err //nolint:wrapcheck // the reader says what it ran
		}
		return ParseSmart(out)
	}
	s, err := run("-j", "-n", "standby", device)
	if err != nil || !s.NeedsSAT {
		return s, err
	}
	if s, err = run("-j", "-n", "standby", "-d", "sat", device); err == nil && s.NeedsSAT {
		return Smart{}, errors.New("smartctl: it still asks for a device type with -d sat")
	}
	return s, err
}
