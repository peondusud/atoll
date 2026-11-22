package alerts

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A token or a password of a channel is read with a limit, and what is no regular file, a FIFO among them, is refused
// without waiting for a writer, which would hold the alerts for good (specs.md §23).
func TestATokenIsReadWithALimit(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, []byte(strings.Repeat("a", maxToken+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readToken(big); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("a file past the limit: %v", err)
	}
	fits := filepath.Join(dir, "fits")
	if err := os.WriteFile(fits, []byte(strings.Repeat("a", maxToken-1)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readToken(fits); err != nil || len(got) != maxToken-1 {
		t.Errorf("a file at the limit: %d bytes, %v", len(got), err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := readToken(fifo)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a FIFO was read")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a FIFO held the reader")
	}
}
