// Package metrics writes the state of the daemon as Prometheus metrics, in the text format 0.0.4 (specs.md §23.1).
// What Render writes comes from a Snapshot and the contract api/metrics.json alone, so that the Rust version writes the
// same bytes for the same state (conformance/vectors/metrics.json). A Recorder holds the daemon's Snapshot between the
// steps that change it and the requests that read it.
package metrics

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/peondusud/atoll/api"
)

// ContentType is what /metrics answers with: the text format 0.0.4.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// DiskStates are the states a disk of the configuration can be in, each a sample of atoll_disk_state (specs.md §4).
var DiskStates = []string{"online", "standby", "readonly", "draining", "missing", "failed"}

// FileStates are the states of a file, each a sample of atoll_files (specs.md §4).
var FileStates = []string{"ok", "pending", "degraded", "unprotected", "unavailable", "corrupt", "conflict", "name-incompatible", "name-collision"}

// UPSPowers are what the UPS can run on, each a sample of atoll_ups_power (specs.md §20).
var UPSPowers = []string{"online", "battery", "low"}

// Family is a metric of the contract: its name, its type, its labels in their order, and its help.
type Family struct {
	Name   string   `json:"name"`
	Type   string   `json:"type"`
	Labels []string `json:"labels"`
	Help   string   `json:"help"`
}

var (
	metricName = regexp.MustCompile(`^atoll_[a-z0-9_]+$`)
	labelName  = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// Families reads the contract, api/metrics.json, in its order. An error says that it breaks the rules of the text
// format: a name or a label it cannot have, a type other than gauge or counter, a counter whose name does not end in
// _total, or a name twice.
func Families() ([]Family, error) {
	var doc struct {
		FormatVersion int      `json:"format_version"`
		Spec          string   `json:"spec"`
		Description   string   `json:"description"`
		Metrics       []Family `json:"metrics"`
	}
	decoder := json.NewDecoder(bytes.NewReader(api.Metrics()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("api/metrics.json: %w", err)
	}
	if doc.FormatVersion != 1 {
		return nil, fmt.Errorf("api/metrics.json: format version %d, want 1", doc.FormatVersion)
	}
	seen := map[string]bool{}
	for _, f := range doc.Metrics {
		switch {
		case !metricName.MatchString(f.Name):
			return nil, fmt.Errorf("api/metrics.json: %q is no name of a metric of Atoll", f.Name)
		case seen[f.Name]:
			return nil, fmt.Errorf("api/metrics.json: %q twice", f.Name)
		case f.Type != "gauge" && f.Type != "counter":
			return nil, fmt.Errorf("api/metrics.json: %q is a %q, want gauge or counter", f.Name, f.Type)
		case (f.Type == "counter") != strings.HasSuffix(f.Name, "_total"):
			return nil, fmt.Errorf("api/metrics.json: %q: a counter's name ends in _total, and only a counter's", f.Name)
		case f.Help == "" || strings.ContainsAny(f.Help, "\n\\"):
			return nil, fmt.Errorf("api/metrics.json: %q: its help is one line, with no backslash", f.Name)
		}
		seen[f.Name] = true
		labels := map[string]bool{}
		for _, l := range f.Labels {
			if !labelName.MatchString(l) || labels[l] {
				return nil, fmt.Errorf("api/metrics.json: %q: label %q", f.Name, l)
			}
			labels[l] = true
		}
	}
	return doc.Metrics, nil
}

// Snapshot is the state the metrics are written from. Its JSON is what conformance/vectors/metrics.json gives as the
// input of each case.
type Snapshot struct {
	// Version is the version Atoll was built as.
	Version string `json:"version"`
	// Disks are the disks of the configuration.
	Disks []Disk `json:"disks"`
	// Files holds the files of each state and their bytes, as the last pass left them: nil before one ended. A state it
	// does not name has none.
	Files map[string]Count `json:"files,omitempty"`
	// Passes counts the passes that ended, and PassFailures those that failed, since the daemon started.
	Passes       int64 `json:"passes"`
	PassFailures int64 `json:"pass_failures"`
	// LastPass is the last pass that ended, nil before one did.
	LastPass *LastPass `json:"last_pass,omitempty"`
	// Operations counts the operations of the passes by kind and outcome, since the daemon started, in any order.
	Operations []Operation `json:"operations,omitempty"`
	// CopiedBytes counts the bytes the passes copied since the daemon started.
	CopiedBytes int64 `json:"copied_bytes"`
	// Waiting is what the last pass left waiting, nil before one ended.
	Waiting *int64 `json:"waiting,omitempty"`
	// Alerts holds each condition of an alert the last pass found holding, with its count, in any order.
	Alerts []Alert `json:"alerts,omitempty"`
	// UPS is what the daemon makes of the UPS, nil without ups.enabled.
	UPS *UPS `json:"ups,omitempty"`
}

// Disk is a disk of the configuration.
type Disk struct {
	ID    string `json:"id"`
	FS    string `json:"fs"`
	Role  string `json:"role"`
	Mount string `json:"mount"`
	// State is one of DiskStates, and Standby says that the last mount check found the disk asleep.
	State   string `json:"state"`
	Standby bool   `json:"standby,omitempty"`
	// Space is what a mount check last measured, nil before one did.
	Space *Space `json:"space,omitempty"`
	// Catalog is what the catalog held of the disk after the last pass, nil before one ended.
	Catalog *DiskCatalog `json:"catalog,omitempty"`
	// Smart is the disk's last reading of SMART, nil when it has none.
	Smart *Smart `json:"smart,omitempty"`
	// What the scrub read, and found corrupt, since the daemon started.
	ScrubReadCopies int64 `json:"scrub_read_copies"`
	ScrubReadBytes  int64 `json:"scrub_read_bytes"`
	ScrubCorrupt    int64 `json:"scrub_corrupt"`
}

// Space is a disk's size and its free bytes.
type Space struct {
	Capacity int64 `json:"capacity"`
	Free     int64 `json:"free"`
}

// DiskCatalog is what the catalog holds of a disk: its copies, its trash, and the copies the scrub has to read there.
type DiskCatalog struct {
	Copies         int64 `json:"copies"`
	Bytes          int64 `json:"bytes"`
	TrashEntries   int64 `json:"trash_entries"`
	TrashBytes     int64 `json:"trash_bytes"`
	ScrubDueCopies int64 `json:"scrub_due_copies"`
	ScrubDueBytes  int64 `json:"scrub_due_bytes"`
}

// Smart is a reading of SMART, at ReadNS. Temperature is 0 when the disk gives none.
type Smart struct {
	ReadNS        int64 `json:"read_ns"`
	Failing       bool  `json:"failing,omitempty"`
	Reallocated   int64 `json:"reallocated"`
	Pending       int64 `json:"pending"`
	Uncorrectable int64 `json:"uncorrectable"`
	Temperature   int64 `json:"temperature"`
}

// Count is a number of files and their bytes.
type Count struct {
	Files int64 `json:"files"`
	Bytes int64 `json:"bytes"`
}

// LastPass is when a pass ended, how long it took, and whether it was partial or held something.
type LastPass struct {
	EndNS      int64 `json:"end_ns"`
	DurationNS int64 `json:"duration_ns"`
	Partial    bool  `json:"partial,omitempty"`
	Held       bool  `json:"held,omitempty"`
}

// Operation counts the operations of one kind that ended with one outcome.
type Operation struct {
	Kind    string `json:"kind"`
	Outcome string `json:"outcome"`
	Count   int64  `json:"count"`
}

// Alert is a condition of an alert that holds, by its key (specs.md §23), and its count.
type Alert struct {
	Key   string `json:"key"`
	Count int64  `json:"count"`
}

// UPS is what the UPS runs on, one of UPSPowers, and whether it went unheard.
type UPS struct {
	Power       string `json:"power"`
	Unreachable bool   `json:"unreachable,omitempty"`
}

// sample is a line of a metric: its label values, in the contract's order, and its value as written.
type sample struct {
	labels []string
	value  string
}

// errNoMetric says that a sample names no metric of the contract, or has other labels: a mistake of this package.
var errNoMetric = errors.New("no such metric in api/metrics.json")

// Render writes the metrics of a snapshot: each metric of the contract that has a sample, in the contract's order,
// with its HELP and TYPE lines, then its samples by their label values in byte order. It changes nothing in s.
func Render(s *Snapshot) ([]byte, error) {
	families, err := Families()
	if err != nil {
		return nil, err
	}
	return write(families, samplesOf(s))
}

// write writes the samples of each family, and fails on a sample of no family or with other labels.
func write(families []Family, samples map[string][]sample) ([]byte, error) {
	var b bytes.Buffer
	for _, f := range families {
		list := samples[f.Name]
		delete(samples, f.Name)
		if len(list) == 0 {
			continue
		}
		for _, one := range list {
			if len(one.labels) != len(f.Labels) {
				return nil, fmt.Errorf("%s: %d label values for %d labels: %w", f.Name, len(one.labels), len(f.Labels), errNoMetric)
			}
		}
		slices.SortFunc(list, func(a, b sample) int { return slices.Compare(a.labels, b.labels) })
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", f.Name, f.Help, f.Name, f.Type)
		for _, one := range list {
			b.WriteString(f.Name)
			if len(f.Labels) > 0 {
				b.WriteByte('{')
				for i, l := range f.Labels {
					if i > 0 {
						b.WriteByte(',')
					}
					b.WriteString(l)
					b.WriteString(`="`)
					b.WriteString(escapeLabel(one.labels[i]))
					b.WriteByte('"')
				}
				b.WriteByte('}')
			}
			b.WriteByte(' ')
			b.WriteString(one.value)
			b.WriteByte('\n')
		}
	}
	for name := range samples {
		return nil, fmt.Errorf("%s: %w", name, errNoMetric)
	}
	return b.Bytes(), nil
}

// samplesOf gives the samples of a snapshot, by metric.
func samplesOf(s *Snapshot) map[string][]sample {
	out := map[string][]sample{}
	add := func(name, value string, labels ...string) {
		out[name] = append(out[name], sample{labels: labels, value: value})
	}
	integer := func(n int64) string { return strconv.FormatInt(n, 10) }
	flag := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	add("atoll_build_info", "1", s.Version)
	for i := range s.Disks {
		d := &s.Disks[i]
		add("atoll_disk_info", "1", d.ID, d.FS, d.Role, d.Mount)
		for _, state := range DiskStates {
			add("atoll_disk_state", flag(d.State == state), d.ID, state)
		}
		if d.Space != nil {
			add("atoll_disk_capacity_bytes", integer(d.Space.Capacity), d.ID)
			add("atoll_disk_free_bytes", integer(d.Space.Free), d.ID)
		}
		if c := d.Catalog; c != nil {
			add("atoll_disk_copies", integer(c.Copies), d.ID)
			add("atoll_disk_copy_bytes", integer(c.Bytes), d.ID)
			add("atoll_trash_entries", integer(c.TrashEntries), d.ID)
			add("atoll_trash_bytes", integer(c.TrashBytes), d.ID)
			add("atoll_scrub_due_copies", integer(c.ScrubDueCopies), d.ID)
			add("atoll_scrub_due_bytes", integer(c.ScrubDueBytes), d.ID)
		}
		if r := d.Smart; r != nil {
			add("atoll_disk_smart_failing", flag(r.Failing), d.ID)
			add("atoll_disk_smart_sectors", integer(r.Reallocated), d.ID, "reallocated")
			add("atoll_disk_smart_sectors", integer(r.Pending), d.ID, "pending")
			add("atoll_disk_smart_sectors", integer(r.Uncorrectable), d.ID, "uncorrectable")
			if r.Temperature != 0 {
				add("atoll_disk_temperature_celsius", integer(r.Temperature), d.ID)
			}
			add("atoll_disk_smart_read_timestamp_seconds", Seconds(r.ReadNS), d.ID)
		}
		add("atoll_disk_standby", flag(d.Standby), d.ID)
		add("atoll_scrub_read_copies_total", integer(d.ScrubReadCopies), d.ID)
		add("atoll_scrub_read_bytes_total", integer(d.ScrubReadBytes), d.ID)
		add("atoll_scrub_corrupt_total", integer(d.ScrubCorrupt), d.ID)
	}
	if s.Files != nil {
		for _, state := range FileStates {
			add("atoll_files", integer(s.Files[state].Files), state)
			add("atoll_file_bytes", integer(s.Files[state].Bytes), state)
		}
	}
	add("atoll_passes_total", integer(s.Passes))
	add("atoll_pass_failures_total", integer(s.PassFailures))
	if p := s.LastPass; p != nil {
		add("atoll_last_pass_end_timestamp_seconds", Seconds(p.EndNS))
		add("atoll_last_pass_duration_seconds", Seconds(p.DurationNS))
		add("atoll_last_pass_partial", flag(p.Partial))
		add("atoll_last_pass_held", flag(p.Held))
	}
	for _, op := range s.Operations {
		add("atoll_operations_total", integer(op.Count), op.Kind, op.Outcome)
	}
	add("atoll_copied_bytes_total", integer(s.CopiedBytes))
	if s.Waiting != nil {
		add("atoll_files_waiting", integer(*s.Waiting))
	}
	for _, a := range s.Alerts {
		// A key names a disk after its first colon: "disk-missing:t2" is the alert disk-missing of t2 (specs.md §23).
		alert, disk, _ := strings.Cut(a.Key, ":")
		add("atoll_alert", integer(a.Count), alert, disk)
	}
	if u := s.UPS; u != nil {
		for _, power := range UPSPowers {
			add("atoll_ups_power", flag(u.Power == power), power)
		}
		add("atoll_ups_unreachable", flag(u.Unreachable))
	}
	return out
}

// Seconds writes a time or a duration in integer nanoseconds as seconds, with nine digits after the point: the
// same bytes in every language, where a float would be written as each language writes floats.
func Seconds(ns int64) string {
	sign, u := "", uint64(ns)
	if ns < 0 {
		// The two's complement of MinInt64 is its absolute value, which only an unsigned integer holds.
		sign, u = "-", -uint64(ns)
	}
	return fmt.Sprintf("%s%d.%09d", sign, u/1e9, u%1e9)
}

// escapeLabel escapes a label value as the text format asks: a backslash, a double quote and a line feed.
func escapeLabel(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}
