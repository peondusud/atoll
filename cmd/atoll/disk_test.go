package main

import (
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
)

// TestDiskListFrom covers what no scenario can reach: a folder taken for a disk has no mount
// options, so readonly comes only from a real mount, which test/loopdev checks in the verdict.
func TestDiskListFrom(t *testing.T) {
	list := []config.Disk{
		{ID: "a", UUID: "u1", Mount: "/mnt/a", FS: "ext4", Role: "data"},
		{ID: "b", UUID: "u2", Mount: "/mnt/b", FS: "ntfs", Role: "cache"},
		{ID: "c", UUID: "u3", Mount: "/mnt/c", FS: "xfs", Role: "data"},
	}
	out := diskListFrom(list, []disks.Verdict{
		{State: disks.Online},
		{State: disks.Readonly, Reason: disks.ADS, Message: "remount it"},
		{State: disks.Missing, Reason: disks.NoRoot, Message: "no folder"},
	})
	if got := out.Disks[0]; got.State != "online" || got.Reason != nil || got.Message != nil {
		t.Errorf("online: %+v", got)
	}
	if got := out.Disks[1]; got.State != "readonly" || got.Reason == nil || *got.Reason != "ads" || got.Message == nil || *got.Message != "remount it" || got.Role != "cache" {
		t.Errorf("readonly: %+v", got)
	}
	if got := out.Disks[2]; got.State != "missing" || got.Reason == nil || *got.Reason != "no-root" || got.Message == nil || *got.Message != "no folder" {
		t.Errorf("missing: %+v", got)
	}
	var text strings.Builder
	if err := printDiskList(&text, out); err != nil {
		t.Fatal(err)
	}
	const want = "a: online (/mnt/a, ext4)\nb: readonly (/mnt/b, ntfs): ads: remount it\nc: missing (/mnt/c, xfs): no-root: no folder\n"
	if text.String() != want {
		t.Errorf("got %q, want %q", text.String(), want)
	}
}
