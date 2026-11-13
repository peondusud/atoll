package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/fsx"
)

// toldWhen is a standard error that tells when a text is written to it.
type toldWhen struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	text string
	told chan struct{}
	once sync.Once
}

func (w *toldWhen) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	if strings.Contains(w.buf.String(), w.text) {
		w.once.Do(func() { close(w.told) })
	}
	return len(p), nil
}

func (w *toldWhen) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// A disk command edits the configuration as it is once the command holds the catalog's lock, which the command that
// held it before may have written (specs.md §10, §25). It read the file before it waited: the second of two disk
// commands run at once then edited the text of before the first, and failed with "the file changed since it was
// read", having changed nothing.
func TestADiskCommandEditsTheConfigurationItFindsUnderTheLock(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "catalog.db")
	configPath := filepath.Join(dir, "atoll.toml")
	text := "pool_id = \"p\"\ncatalog = " + strconv.Quote(file) + "\n[view]\nmount = " + strconv.Quote(filepath.Join(dir, "pool")) + "\n" +
		"[[disk]]\nid = \"t1\"\nuuid = \"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3b\"\nmount = " + strconv.Quote(filepath.Join(dir, "t1")) + "\nfs = \"ext4\"\n" +
		"[[disk]]\nid = \"t2\"\nuuid = \"7a2b4c6d-8e0f-4a1b-9c2d-3e4f5a6b7c8d\"\nmount = " + strconv.Quote(filepath.Join(dir, "t2")) + "\nfs = \"ext4\"\n"
	if err := os.WriteFile(configPath, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	// Another command holds the catalog.
	release, err := fsx.LockFile(file)
	if err != nil {
		t.Fatal(err)
	}
	stderr := &toldWhen{text: "waiting for the catalog", told: make(chan struct{})}
	var stdout bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run([]string{"--config", configPath, "disk", "fail", "t1", "--yes"}, &stdout, stderr)
	}()
	select {
	case <-stderr.told:
	case code := <-done:
		release()
		t.Fatalf("the command did not wait for the catalog: exit %d, %s", code, stderr.String())
	case <-time.After(20 * time.Second):
		release()
		t.Fatal("the command never said that it waits")
	}
	// That other command writes the configuration, as disk fail t2 would, then lets go of the catalog.
	edited, err := config.SetDiskState([]byte(text), "t2", config.DiskFailed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case code := <-done:
		if code != exitOK && code != exitPartial {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the command never ended")
	}
	now, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Parse(now)
	if err != nil {
		t.Fatal(err)
	}
	for i := range c.Disks {
		if c.Disks[i].State != config.DiskFailed {
			t.Errorf("disk %s is %q: one of the two commands' changes is lost", c.Disks[i].ID, c.Disks[i].State)
		}
	}
}

// A configuration that is in place, though its folder could not be synced after, is no configuration "unchanged": the
// command goes on with a warning and exits with 4. Any other failure left the file as it was (specs.md §10, §25).
func TestConfigInPlace(t *testing.T) {
	var kinds []string
	warn := func(kind, _ string) { kinds = append(kinds, kind) }
	if partial, err := configInPlace(nil, warn); partial || err != nil || len(kinds) != 0 {
		t.Errorf("written and synced: %v, %v, %v", partial, err, kinds)
	}
	failed := fmt.Errorf("%w: %w", fsx.ErrNotSynced, os.ErrPermission)
	if partial, err := configInPlace(failed, warn); !partial || err != nil || len(kinds) != 1 || kinds[0] != warnConfigNotSynced {
		t.Errorf("written, not synced: %v, %v, %v", partial, err, kinds)
	}
	kinds = nil
	if partial, err := configInPlace(fsx.ErrChangedMeanwhile, warn); partial || !errors.Is(err, fsx.ErrChangedMeanwhile) || len(kinds) != 0 {
		t.Errorf("not written: %v, %v, %v", partial, err, kinds)
	}
}
