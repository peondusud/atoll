package main

import (
	"bytes"
	"strings"
	"testing"
)

// balance refuses --move-originals, which comes later, and says so, before it reads anything (specs.md §13.3).
func TestBalanceMoveOriginals(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--config", notifyConfig(t, ""), "balance", "--move-originals"}, &stdout, &stderr); code != exitUsage ||
		stdout.Len() != 0 || !strings.Contains(stderr.String(), "--move-originals comes later") {
		t.Errorf("exit %d, %q, %q", code, stdout.String(), stderr.String())
	}
}
