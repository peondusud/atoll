package disks

import (
	"encoding/hex"
	"slices"
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

type powerModeVector struct {
	Sense string  `json:"sense"`
	Mode  *string `json:"mode"`
	Note  string  `json:"note"`
}

// TestPowerModeVectors: what a disk's answer to CHECK POWER MODE says (specs.md §10).
func TestPowerModeVectors(t *testing.T) {
	for i, v := range vectors.Load[powerModeVector](t, "power_mode.json") {
		sense, err := hex.DecodeString(v.Sense)
		if err != nil {
			t.Fatalf("vector %d: %v", i, err)
		}
		want := PowerUnknown
		if v.Mode != nil {
			want = Power(*v.Mode)
		}
		if got := PowerModeOf(sense); got != want {
			t.Errorf("vector %d (%s): %q, want %q", i, v.Note, got, want)
		}
	}
}

type blockStatVector struct {
	Stat      string `json:"stat"`
	Completed int64  `json:"completed"`
	InFlight  int64  `json:"in_flight"`
	Error     bool   `json:"error"`
	Note      string `json:"note"`
}

// TestBlockStatVectors: a whole disk's I/O counters (specs.md §10).
func TestBlockStatVectors(t *testing.T) {
	for i, v := range vectors.Load[blockStatVector](t, "block_stat.json") {
		got, err := ParseBlockStat(v.Stat)
		switch {
		case v.Error && err == nil:
			t.Errorf("vector %d (%s): %+v, want an error", i, v.Note, got)
		case !v.Error && err != nil:
			t.Errorf("vector %d (%s): %v", i, v.Note, err)
		case !v.Error && got != (BlockStat{Completed: v.Completed, InFlight: v.InFlight}):
			t.Errorf("vector %d (%s): %+v", i, v.Note, got)
		}
	}
}

type spinDiskVector struct {
	Name       string  `json:"name"`
	Rotational *bool   `json:"rotational"`
	Before     *int64  `json:"before"`
	Now        *int64  `json:"now"`
	Busy       bool    `json:"busy"`
	Refused    bool    `json:"refused"`
	Answer     *string `json:"answer"`
}

type spinVector struct {
	Disks   []spinDiskVector `json:"disks"`
	Asked   []string         `json:"asked"`
	Standby bool             `json:"standby"`
	Note    string           `json:"note"`
}

// TestSpinVectors: whether a disk is in standby, from its whole disks, waking none (specs.md §10).
func TestSpinVectors(t *testing.T) {
	for i, v := range vectors.Load[spinVector](t, "spin.json") {
		list := make([]WholeDiskLook, 0, len(v.Disks))
		answers := map[string]Power{}
		for _, d := range v.Disks {
			list = append(list, WholeDiskLook{Name: d.Name, Rotational: d.Rotational, Before: d.Before, Now: d.Now, Busy: d.Busy, Refused: d.Refused})
			answers[d.Name] = PowerUnknown
			if d.Answer != nil {
				answers[d.Name] = Power(*d.Answer)
			}
		}
		asked := []string{}
		got := Standby(list, func(name string) Power {
			asked = append(asked, name)
			return answers[name]
		})
		if got != v.Standby || !slices.Equal(asked, v.Asked) {
			t.Errorf("vector %d (%s): standby %v, asked %q; want %v, %q", i, v.Note, got, asked, v.Standby, v.Asked)
		}
	}
}
