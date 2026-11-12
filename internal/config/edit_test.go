package config

import (
	"errors"
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

func TestEditVectors(t *testing.T) {
	type vector struct {
		Note string `json:"note"`
		TOML string `json:"toml"`
		Op   string `json:"op"`
		Disk *struct {
			ID      string   `json:"id"`
			UUID    string   `json:"uuid"`
			Mount   string   `json:"mount"`
			FS      string   `json:"fs"`
			Role    string   `json:"role"`
			Domain  string   `json:"domain"`
			Tags    []string `json:"tags"`
			Root    string   `json:"root"`
			Workdir string   `json:"workdir"`
			State   string   `json:"state"`
		} `json:"disk"`
		ID     string  `json:"id"`
		State  *string `json:"state"`
		Result *string `json:"result"`
		Error  bool    `json:"error"`
	}
	for i, v := range vectors.Load[vector](t, "config_edit.json") {
		var got []byte
		var err error
		switch v.Op {
		case "add":
			d := v.Disk
			got, err = AppendDisk([]byte(v.TOML), &NewDisk{ID: d.ID, UUID: d.UUID, Mount: d.Mount, FS: d.FS, Role: d.Role, Domain: d.Domain,
				Tags: d.Tags, Root: d.Root, Workdir: d.Workdir, State: d.State})
		case "remove":
			got, err = RemoveDisk([]byte(v.TOML), v.ID)
		case "state":
			got, err = SetDiskState([]byte(v.TOML), v.ID, *v.State)
		default:
			t.Fatalf("case %d: op %q", i, v.Op)
		}
		switch {
		case v.Error && err == nil:
			t.Errorf("case %d (%s): no error, gave %q", i, v.Note, got)
		case v.Error:
		case err != nil:
			t.Errorf("case %d (%s): %v", i, v.Note, err)
		case string(got) != *v.Result:
			t.Errorf("case %d (%s):\n got %q\nwant %q", i, v.Note, got, *v.Result)
		}
	}
}

// An edit that would change anything beyond the disk is refused, whatever the text looks like.
func TestEditRefusesMore(t *testing.T) {
	text := []byte("pool_id = \"p\"\n[view]\nmount = \"/srv/pool\"\n[[disk]]\nid = \"t1\"\nuuid = \"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3b\"\nmount = \"/mnt/t1\"\nfs = \"ext4\"\n")
	err := sameBut(text, append(text, "[defaults]\nreplicas = 2\n"...), func(map[string]any) error { return nil })
	if !errors.Is(err, ErrEditRefused) {
		t.Errorf("%v", err)
	}
	if _, err := tomlString("\xff"); err == nil {
		t.Error("a string that is not UTF-8")
	}
}
