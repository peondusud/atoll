package notify

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A media server's token or key is read with a limit, and what is no regular file, a FIFO among them, is refused
// without waiting for a writer, which would hold a notification for good (specs.md §21.5).
func TestASecretIsReadWithALimit(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, []byte(strings.Repeat("a", maxSecret+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecret(big); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("a file past the limit: %v", err)
	}
	fits := filepath.Join(dir, "fits")
	if err := os.WriteFile(fits, []byte(strings.Repeat("a", maxSecret-1)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readSecret(fits); err != nil || len(got) != maxSecret-1 {
		t.Errorf("a file at the limit: %d bytes, %v", len(got), err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := readSecret(fifo)
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
