//go:build !conformance

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestDeliveredBinaryIgnoresTheControlFile: the delivered binary must have no way to take a
// folder for a disk (specs.md §29.4, invariant 3). So it is built as make build does, and given
// everything a conformance build would obey.
func TestDeliveredBinaryIgnoresTheControlFile(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "atoll")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("ATOLL_CONFORMANCE")) {
		t.Error("the delivered binary holds the name of the control variable")
	}

	disk := filepath.Join(dir, "disk")
	if err := os.MkdirAll(filepath.Join(disk, "media"), 0o755); err != nil {
		t.Fatal(err)
	}
	const uuid = "3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3b"
	control := filepath.Join(dir, "conformance.json")
	body := fmt.Sprintf(`{"now": "2026-01-01T00:00:00Z", "disks": [{"id": "t1", "uuid": %q, "fs": "ext4", "path": %q, "capacity": 1000000}]}`, uuid, disk)
	if err := os.WriteFile(control, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "atoll.toml")
	configText := fmt.Sprintf("pool_id = \"p\"\ncatalog = %q\n[view]\nmount = %q\n[[disk]]\nid = \"t1\"\nuuid = %q\nmount = %q\nfs = \"ext4\"\n",
		filepath.Join(dir, "catalog.db"), filepath.Join(dir, "pool"), uuid, disk)
	if err := os.WriteFile(configPath, []byte(configText), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), bin, "--config", configPath, "disk", "list", "--json")
	cmd.Env = append(os.Environ(), "ATOLL_CONFORMANCE="+control)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("disk list: %v", err)
	}
	var got diskListOutput
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if len(got.Disks) != 1 || got.Disks[0].State != "missing" || got.Disks[0].Reason == nil || *got.Disks[0].Reason != "not-mounted" {
		t.Errorf("a folder taken for a disk: %s", out)
	}
}
