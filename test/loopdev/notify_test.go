//go:build loopdev

package loopdev

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/peondusud/atoll/internal/daemon"
	"github.com/peondusud/atoll/internal/names"
)

// TestNotifyMediaServer: the daemon asks a Plex for a scan of the folders that changed beside the view, as Plex sees
// them: one where a file was written straight to a disk, and one where a file deleted on one disk took its copy on the
// other with it. A file written through the view, which Plex's own watch sees, asks for nothing, nor does its copy
// (specs.md §21.5).
func TestNotifyMediaServer(t *testing.T) {
	needRoot(t)
	needMergerfs(t)
	var mu sync.Mutex
	var asked []string
	pool := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/library/sections" {
			_, _ = io.WriteString(w, fmt.Sprintf(`{"MediaContainer":{"Directory":[{"key":"1","Location":[{"path":%q}]}]}}`, pool))
			return
		}
		asked = append(asked, r.URL.Path+" "+r.URL.Query().Get("path"))
	}))
	defer srv.Close()
	wasAsked := func(folder string) bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(asked, "/library/sections/1/refresh "+filepath.Join(pool, folder))
	}
	v := newVolume(t, names.Ext4)
	at := mountPoint(t)
	mountVolume(t, v, "", at)
	b := newPassBenchWith(t, v, at, "[defaults]\nmode = \"immediate\"\n"+fmt.Sprintf("[notify.plex]\nurl = %q\n", srv.URL))
	view := viewOf(t, b)
	mu.Lock()
	pool = view
	mu.Unlock()
	passes, stop := runDaemon(t, b, filepath.Join(t.TempDir(), "fstab"))
	waitPass(t, passes, daemon.BecauseStart)

	// Straight to a disk, by another process: the watcher takes this one's for Atoll's, as the daemon runs in it.
	direct := filepath.Join(at, "media", "direct")
	must(t, "sh", "-c", "mkdir -p '"+direct+"' && printf 'a film' > '"+filepath.Join(direct, "a.mkv")+"'")
	waitFor(t, "Plex asked for the folder written straight to a disk", func() bool { return wasAsked("direct") })

	// Through the view, then copied to the other disk: nothing to ask for. Then deleted on one disk, which takes the
	// other copy: the folder is asked for.
	for _, folder := range []string{"viewonly", "both"} {
		mustDo(t, os.MkdirAll(filepath.Join(view, folder), 0o755))
		mustDo(t, os.WriteFile(filepath.Join(view, folder, "b.mkv"), []byte("a film: "+folder), 0o644))
	}
	onBoth := func(name string) bool {
		_, a := os.Stat(filepath.Join(b.keep, "media", name))
		_, c := os.Stat(filepath.Join(at, "media", name))
		return a == nil && c == nil
	}
	waitFor(t, "the copies of the files written through the view", func() bool { return onBoth("viewonly/b.mkv") && onBoth("both/b.mkv") })
	must(t, "rm", filepath.Join(at, "media", "both", "b.mkv"))
	waitFor(t, "the deletion taken to the other disk", func() bool {
		_, err := os.Stat(filepath.Join(b.keep, "media", "both", "b.mkv"))
		return os.IsNotExist(err)
	})
	waitFor(t, "Plex asked for the folder of the deletion", func() bool { return wasAsked("both") })
	if wasAsked("viewonly") {
		t.Error("Plex was asked for a folder written through the view")
	}
	stop()
}
