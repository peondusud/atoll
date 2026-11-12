package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// notifyConfig writes a configuration whose ntfy channel is url, and gives its path.
func notifyConfig(t *testing.T, url string) string {
	t.Helper()
	dir := t.TempDir()
	config := filepath.Join(dir, "atoll.toml")
	text := "pool_id = \"p\"\ncatalog = " + strconv.Quote(filepath.Join(dir, "catalog.db")) + "\n[view]\nmount = " + strconv.Quote(filepath.Join(dir, "pool")) + "\n" +
		"[alerts]\nntfy = " + strconv.Quote(url) + "\n"
	if err := os.WriteFile(config, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return config
}

// notify test sends a test alert through the channel set and says whether it went, in text or in JSON; a server
// that refuses it gives exit 4, and none set exit 1 with nothing on standard output (specs.md §23, §25). The
// scenarios judge the exit codes; the output of a server that answers needs one, which they do not have.
func TestNotifyTest(t *testing.T) {
	status := http.StatusOK
	var titles []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		titles = append(titles, r.Header.Get("Title"))
		w.WriteHeader(status)
	}))
	defer srv.Close()
	config := notifyConfig(t, srv.URL+"/atoll")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--config", config, "notify", "test"}, &stdout, &stderr); code != exitOK || stdout.String() != "alerts.ntfy: sent\n" {
		t.Errorf("exit %d, %q, %q", code, stdout.String(), stderr.String())
	}
	if len(titles) != 1 || titles[0] != "Atoll: a test alert" {
		t.Errorf("the server got %q", titles)
	}
	stdout.Reset()
	if code := run([]string{"--config", config, "notify", "test", "--json"}, &stdout, &stderr); code != exitOK {
		t.Errorf("--json: exit %d", code)
	}
	var sent notifyOutput
	if err := json.Unmarshal(stdout.Bytes(), &sent); err != nil || len(sent.Channels) != 1 || sent.Channels[0] != (notifyChannel{Channel: "alerts.ntfy", Sent: true}) {
		t.Errorf("--json: %q, %v", stdout.String(), err)
	}
	status = http.StatusUnauthorized
	stdout.Reset()
	if code := run([]string{"--config", config, "notify", "test", "--json"}, &stdout, &stderr); code != exitPartial {
		t.Errorf("refused: exit %d", code)
	}
	var out notifyOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || len(out.Channels) != 1 || out.Channels[0].Sent || !strings.Contains(out.Channels[0].Error, "401") {
		t.Errorf("refused: %q, %v", stdout.String(), err)
	}
	stdout.Reset()
	if code := run([]string{"--config", config, "notify", "test"}, &stdout, &stderr); code != exitPartial || !strings.HasPrefix(stdout.String(), "alerts.ntfy: not sent: ") {
		t.Errorf("refused, in text: exit %d, %q", code, stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--config", notifyConfig(t, ""), "notify", "test", "--json"}, &stdout, &stderr); code != exitError || stdout.Len() != 0 || !strings.Contains(stderr.String(), "alerts.ntfy") {
		t.Errorf("none set: exit %d, %q, %q", code, stdout.String(), stderr.String())
	}
	if code := run([]string{"--config", config, "notify", "test"}, brokenWriter{}, &stderr); code != exitError {
		t.Errorf("on a broken output: exit %d", code)
	}
}
