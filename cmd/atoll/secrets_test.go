package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Each file of a secret others may read or write, or of an account that is none of Atoll's, gets a warning at its
// key; the configuration loads all the same, and the daemon's log says each warning, those of its text too
// (specs.md §24.4).
func TestSecretFileWarnings(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, mode os.FileMode) string {
		file := filepath.Join(dir, name)
		if err := os.WriteFile(file, []byte("tk"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(file, mode); err != nil {
			t.Fatal(err)
		}
		return file
	}
	readable, writable, closed := write("readable", 0o644), write("writable", 0o602), write("closed", 0o640)
	theirs := write("theirs", 0o600)
	root := os.Geteuid() == 0
	if root {
		if err := os.Chown(theirs, 4321, 4321); err != nil {
			t.Fatal(err)
		}
	}
	_, c := apiPool(t, "[notify.plex]\nurl = \"http://nas.lan:32400\"\ntoken_file = "+strconv.Quote(readable)+"\n"+
		"[alerts]\ngotify = \"https://gotify.example\"\ngotify_token_file = "+strconv.Quote(writable)+"\n"+
		"ntfy = \"https://ntfy.example/t\"\nntfy_token_file = "+strconv.Quote(theirs)+"\n"+
		"smtp = \"smtps://u@mail.example?from=a@example.org&to=b@example.org\"\nsmtp_password_file = "+strconv.Quote(closed)+"\n"+
		"[api]\ntoken_file = "+strconv.Quote(filepath.Join(dir, "none"))+"\n")
	var keys []string
	for _, w := range configWarnings(c) {
		keys = append(keys, w.Key+": "+w.Message)
	}
	want := []string{"alerts.gotify_token_file: this file holds a secret, and other accounts may write it"}
	if root {
		want = append(want, "alerts.ntfy_token_file: this file holds a secret, and it belongs to the account 4321")
	}
	want = append(want, "notify.plex.token_file: this file holds a secret, and other accounts may read it", "notify.plex.url: the secret of notify.plex.token_file go")
	if len(keys) != len(want) {
		t.Fatalf("%q, want %q", keys, want)
	}
	for i := range keys {
		if !strings.HasPrefix(keys[i], want[i]) {
			t.Errorf("%q, want %q", keys[i], want[i])
		}
	}
	var log bytes.Buffer
	logWarnings(slog.New(slog.NewTextHandler(&log, nil)), c)
	if n := strings.Count(log.String(), "the configuration has a warning"); n != len(want) || !strings.Contains(log.String(), "key=notify.plex.token_file") {
		t.Errorf("%d lines:\n%s", n, log.String())
	}
}
