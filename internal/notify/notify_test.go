package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/vectors"
)

type pathMapVector struct {
	Note      string `json:"note"`
	ViewMount string `json:"view_mount"`
	Folder    string `json:"folder"`
	PathMap   []struct {
		Host   string `json:"host"`
		Server string `json:"server"`
	} `json:"path_map"`
	Seen string `json:"seen"`
}

func TestPathMapVectors(t *testing.T) {
	for i, v := range vectors.Load[pathMapVector](t, "path_map.json") {
		pathMap := map[string]string{}
		for _, e := range v.PathMap {
			pathMap[e.Host] = e.Server
		}
		if got := Seen(v.ViewMount, v.Folder, pathMap); got != v.Seen {
			t.Errorf("vector %d (%s): %q, want %q", i, v.Note, got, v.Seen)
		}
	}
}

const second = int64(time.Second)

// A folder is told at once, then at most once a window; a server that could not be told waits; a folder set aside
// while a disk is missing waits for its release (specs.md §21.4, §21.5).
func TestQueue(t *testing.T) {
	q := NewQueue()
	if _, ok := q.Next(); ok || q.Ready(0) != nil {
		t.Fatal("an empty queue has something")
	}
	q.Add("b", "a", "b")
	if got := q.Ready(100 * second); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("ready %q", got)
	}
	q.Told([]string{"a", "b"}, 100*second)
	if _, ok := q.Next(); ok {
		t.Error("something left once told")
	}
	// Changed again 10 s later: told 30 s after the last time.
	q.Add("a")
	if got := q.Ready(110 * second); got != nil {
		t.Errorf("told again within the window: %q", got)
	}
	if next, ok := q.Next(); !ok || next != 130*second {
		t.Errorf("next %d, %v", next, ok)
	}
	if got := q.Ready(130 * second); !slices.Equal(got, []string{"a"}) {
		t.Errorf("ready %q", got)
	}
	// A server that could not be told waits RetryAfter, whatever the window says.
	q.Failed(130 * second)
	if got := q.Ready(131 * second); got != nil {
		t.Errorf("ready while waiting: %q", got)
	}
	if next, _ := q.Next(); next != 130*second+RetryAfter.Nanoseconds() {
		t.Errorf("next %d", next)
	}
	// Held: out of the queue until released, and a change meanwhile does not bring it back.
	q.Add("c")
	at := 130*second + RetryAfter.Nanoseconds()
	q.Hold([]string{"c", "x"})
	q.Add("c")
	if got := q.Ready(at); !slices.Equal(got, []string{"a"}) || q.Held() != 1 {
		t.Errorf("ready %q, %d held", got, q.Held())
	}
	q.Told([]string{"a"}, at)
	if _, ok := q.Next(); ok {
		t.Error("a held folder is due")
	}
	q.Release()
	if got := q.Ready(at); !slices.Equal(got, []string{"c"}) || q.Held() != 0 {
		t.Errorf("released: %q, %d held", got, q.Held())
	}
}

// secret writes a token file.
func secret(t *testing.T, text string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// recorder keeps what a fake server was asked.
type recorder struct {
	mu   sync.Mutex
	got  []string
	auth []string
	body []string
}

func (r *recorder) add(req *http.Request, auth string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	data, _ := io.ReadAll(req.Body)
	r.got = append(r.got, req.Method+" "+req.URL.RequestURI())
	r.auth = append(r.auth, auth)
	r.body = append(r.body, string(data))
}

// Plex is asked for a scan of each folder in each library that holds it, by the path it sees; a folder above a
// library asks for that library's folder, and a folder in none asks for nothing (specs.md §21.5).
func TestPlexScan(t *testing.T) {
	var r recorder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.add(req, req.Header.Get("X-Plex-Token"))
		if req.URL.Path == "/library/sections" {
			if req.Header.Get("Accept") != "application/json" {
				t.Errorf("accept %q", req.Header.Get("Accept"))
			}
			_, _ = io.WriteString(w, `{"MediaContainer":{"size":2,"Directory":[`+
				`{"key":"1","type":"show","title":"Series","Location":[{"id":1,"path":"/data/tv"},{"id":3,"path":"/other/tv"}]},`+
				`{"key":"2","type":"movie","title":"Films","Location":[{"id":2,"path":"/data/films"}]}]}}`)
		}
	}))
	defer srv.Close()
	p := &Plex{URL: srv.URL + "/", TokenFile: secret(t, "tok\n"), ViewMount: "/srv/pool", PathMap: map[string]string{"/srv/pool": "/data"}}
	if err := p.Scan(t.Context(), []string{"tv/Show/Season 1", "tv/Show/Season 1", "", "music/x"}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GET /library/sections",
		"GET /library/sections/1/refresh?path=%2Fdata%2Ftv%2FShow%2FSeason+1",
		"GET /library/sections/1/refresh?path=%2Fdata%2Ftv",
		"GET /library/sections/2/refresh?path=%2Fdata%2Ffilms",
	}
	if !slices.Equal(r.got, want) {
		t.Errorf("asked\n%q, want\n%q", r.got, want)
	}
	for _, a := range r.auth {
		if a != "tok" {
			t.Errorf("token %q", a)
		}
	}
	if err := p.Check(t.Context()); err != nil {
		t.Error(err)
	}
}

// Plex without a token file sends none; one that refuses is an error, and so is a token file with no token.
func TestPlexRefuses(t *testing.T) {
	var r recorder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.add(req, req.Header.Get("X-Plex-Token"))
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()
	p := &Plex{URL: srv.URL, ViewMount: "/srv/pool"}
	if err := p.Scan(t.Context(), []string{"tv"}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("%v", err)
	}
	if len(r.auth) != 1 || r.auth[0] != "" {
		t.Errorf("token %q", r.auth)
	}
	p.TokenFile = secret(t, "  \n")
	if err := p.Check(t.Context()); err == nil || len(r.got) != 1 {
		t.Errorf("an empty token file: %v, asked %q", err, r.got)
	}
}

// Jellyfin and Emby get the folders as updates of type Modified, a hundred a request, with the key where each takes
// it; a folder that is not UTF-8 is left out, and a server with no key file is asked nothing (specs.md §21.5).
func TestMediaBrowserScan(t *testing.T) {
	for _, kind := range []string{config.Jellyfin, config.Emby} {
		var r recorder
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			auth := req.Header.Get("Authorization")
			if kind == config.Emby {
				auth = req.Header.Get("X-Emby-Token")
			}
			r.add(req, auth)
			if req.Method == http.MethodPost && req.Header.Get("Content-Type") != "application/json" {
				t.Errorf("%s: content type %q", kind, req.Header.Get("Content-Type"))
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		m := &MediaBrowser{Kind: kind, URL: srv.URL, KeyFile: secret(t, "k3y"), ViewMount: "/srv/pool", PathMap: map[string]string{"/srv/pool": "/media"}}
		folders := []string{"bad\xff"}
		for i := range 101 {
			folders = append(folders, "tv/"+string(rune('a'+i%26))+strings.Repeat("x", i/26))
		}
		if err := m.Scan(t.Context(), folders); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if err := m.Check(t.Context()); err != nil {
			t.Errorf("%s: %v", kind, err)
		}
		srv.Close()
		if !slices.Equal(r.got, []string{"POST /Library/Media/Updated", "POST /Library/Media/Updated", "GET /System/Info"}) {
			t.Errorf("%s: asked %q", kind, r.got)
		}
		wantAuth := `MediaBrowser Token="k3y"`
		if kind == config.Emby {
			wantAuth = "k3y"
		}
		for _, a := range r.auth {
			if a != wantAuth {
				t.Errorf("%s: key %q", kind, a)
			}
		}
		var first, second struct{ Updates []update }
		if err := json.Unmarshal([]byte(r.body[0]), &first); err != nil || len(first.Updates) != 100 ||
			first.Updates[0] != (update{Path: "/media/tv/a", UpdateType: "Modified"}) {
			t.Errorf("%s: %v %q", kind, err, r.body[0][:min(200, len(r.body[0]))])
		}
		if err := json.Unmarshal([]byte(r.body[1]), &second); err != nil || len(second.Updates) != 1 {
			t.Errorf("%s: %v %q", kind, err, r.body[1])
		}
	}
	m := &MediaBrowser{Kind: config.Jellyfin, URL: "http://127.0.0.1:9", ViewMount: "/srv/pool"}
	if err := m.Scan(context.Background(), []string{"tv"}); err == nil || !strings.Contains(err.Error(), "api_key_file") {
		t.Errorf("no key file: %v", err)
	}
}

// The servers the configuration sets, in its order, with the view's mount.
func TestServers(t *testing.T) {
	c, err := config.Parse([]byte("pool_id = \"p\"\ncatalog = \"/srv/catalog.db\"\n[view]\nmount = \"/srv/pool\"\n" +
		"[notify.emby]\nurl = \"http://e:8096\"\n[notify.plex]\nurl = \"http://p:32400\"\ntoken_file = \"/etc/atoll/plex.token\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := Servers(c)
	if len(got) != 2 || got[0].Name() != "notify.plex" || got[1].Name() != "notify.emby" {
		t.Fatalf("%+v", got)
	}
	if p, _ := got[0].(*Plex); p == nil || p.ViewMount != "/srv/pool" || p.TokenFile != "/etc/atoll/plex.token" {
		t.Errorf("%+v", got[0])
	}
}
