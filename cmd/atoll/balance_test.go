package main

import (
	"bytes"
	"strings"
	"testing"
)

// balance takes --move-originals, in any place among its flags, and refuses an argument it does not know, before it
// reads anything (specs.md §13.3).
func TestBalanceMoveOriginals(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--config", notifyConfig(t, ""), "balance", "--move-originals", "--originals"}, &stdout, &stderr); code != exitUsage ||
		stdout.Len() != 0 || !strings.Contains(stderr.String(), `"--originals"`) {
		t.Errorf("exit %d, %q, %q", code, stdout.String(), stderr.String())
	}
}
