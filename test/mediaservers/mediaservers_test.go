//go:build mediaservers

package mediaservers

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/notify"
)

// The test writes the library at viewMount, as the merged view holds it, and the servers read the same volume at
// serverRoot, as a media server in a container does; path_map takes one to the other (specs.md §21.5).
const (
	viewMount  = "/pool"
	serverRoot = "/data"
)

var pathMap = map[string]string{viewMount: serverRoot}

const (
	// quiet is how long the test waits to see that a server finds nothing on its own.
	quiet = 20 * time.Second
	// settle is how long a server has to show what a notification told it of.
	settle = 3 * time.Minute
)

// TestPlex tries the notifications on Plex, with the token of the server, which run.sh reads once PLEX_CLAIM has
// claimed it, as notify.plex.token_file gives it (specs.md §21.5). Plex 1.43.5 unclaimed answers 401 to another host
// of an allowed network: the test then has nothing to try.
func TestPlex(t *testing.T) {
	t.Parallel()
	base := need(t, "ATOLL_PLEX_URL")
	c := &client{base: base, header: http.Header{}}
	tokenFile := os.Getenv("ATOLL_PLEX_TOKEN_FILE")
	if tokenFile != "" {
		token, err := os.ReadFile(tokenFile)
		must(t, err)
		c.header.Set("X-Plex-Token", strings.TrimSpace(string(token)))
	}
	type identityAnswer struct {
		MediaContainer struct {
			Version    string
			Claimed    bool
			StartState string
		}
	}
	var identity identityAnswer
	waitFor(t, "Plex answers", 3*time.Minute, func(ctx context.Context) (bool, error) {
		identity = identityAnswer{}
		err := c.do(ctx, http.MethodGet, "/identity", nil, &identity)
		return identity.MediaContainer.StartState == "", err
	})
	t.Logf("Plex %s, claimed: %v", identity.MediaContainer.Version, identity.MediaContainer.Claimed)
	ctx := context.Background()
	if err := c.do(ctx, http.MethodGet, "/library/sections", nil, nil); err != nil {
		if tokenFile == "" && strings.Contains(err.Error(), "401") {
			t.Skipf("Plex takes no request from this host without a token: run with PLEX_CLAIM set (%v)", err)
		}
		t.Fatal(err)
	}
	// Plex must not scan on its own, or the test could not tell what a notification does.
	must(t, c.do(ctx, http.MethodPut, "/:/prefs?FSEventLibraryUpdatesEnabled=0&FSEventLibraryPartialScanEnabled=0&ScheduledLibraryUpdatesEnabled=0", nil, nil))
	plant(t, "plex")
	q := url.Values{"name": {"Atoll Movies"}, "type": {"movie"}, "agent": {"tv.plex.agents.none"}, "scanner": {"Plex Movie"},
		"language": {"xn"}, "location": {serverRoot + "/plex/Movies"}}
	must(t, c.do(ctx, http.MethodPost, "/library/sections?"+q.Encode(), nil, nil))
	var sections struct {
		MediaContainer struct {
			Directory []struct {
				Key   string `json:"key"`
				Title string `json:"title"`
			}
		}
	}
	must(t, c.do(ctx, http.MethodGet, "/library/sections", nil, &sections))
	key := ""
	for _, d := range sections.MediaContainer.Directory {
		if d.Title == "Atoll Movies" {
			key = d.Key
		}
	}
	if key == "" {
		t.Fatalf("Plex made no library: %+v", sections)
	}
	list := func(ctx context.Context) ([]string, error) {
		var all struct {
			MediaContainer struct {
				Metadata []struct {
					Media []struct {
						Part []struct {
							File string `json:"file"`
						}
					}
				}
			}
		}
		if err := c.do(ctx, http.MethodGet, "/library/sections/"+key+"/all", nil, &all); err != nil {
			return nil, err
		}
		var files []string
		for _, m := range all.MediaContainer.Metadata {
			for _, media := range m.Media {
				for _, p := range media.Part {
					files = append(files, p.File)
				}
			}
		}
		return files, nil
	}
	scenario(t, "plex", &notify.Plex{URL: base, TokenFile: tokenFile, ViewMount: viewMount, PathMap: pathMap}, list)
	if tokenFile != "" {
		// Without token_file, Plex must take this host's requests without a token, as its allowedNetworks says,
		// which run.sh sets to the private ranges.
		t.Run("allowed-network", func(t *testing.T) {
			if err := (&notify.Plex{URL: base}).Check(ctx); err != nil {
				t.Errorf("atoll notify test without a token, from an allowed network: %v", err)
			}
		})
	}
}

// TestJellyfin tries the notifications on Jellyfin, set up through its first-run wizard with an API key of its own.
func TestJellyfin(t *testing.T) {
	t.Parallel()
	base := need(t, "ATOLL_JELLYFIN_URL")
	key, list := mediaBrowser(t, "jellyfin", base, "Authorization")
	scenario(t, "jellyfin", &notify.MediaBrowser{Kind: config.Jellyfin, URL: base, KeyFile: key, ViewMount: viewMount, PathMap: pathMap}, list)
}

// TestEmby tries the notifications on Emby, set up as Jellyfin is.
func TestEmby(t *testing.T) {
	t.Parallel()
	base := need(t, "ATOLL_EMBY_URL")
	key, list := mediaBrowser(t, "emby", base, "X-Emby-Authorization")
	scenario(t, "emby", &notify.MediaBrowser{Kind: config.Emby, URL: base, KeyFile: key, ViewMount: viewMount, PathMap: pathMap}, list)
}

// mediaBrowser sets up Jellyfin or Emby, which keep the API of their common ancestor: the first-run wizard with an
// account, an API key, a short delay before the server acts on what it is told of, and a library of movies that does
// not watch its folders, kind/Movies. It gives the file that holds the key, and what lists the library.
func mediaBrowser(t *testing.T, kind, base, authHeader string) (string, func(context.Context) ([]string, error)) {
	t.Helper()
	const device = `MediaBrowser Client="atoll-test", Device="atoll-test", DeviceId="atoll-test", Version="1"`
	c := &client{base: base, header: http.Header{authHeader: {device}}}
	waitFor(t, "the server answers", 3*time.Minute, func(ctx context.Context) (bool, error) {
		return true, c.do(ctx, http.MethodGet, "/System/Info/Public", nil, nil)
	})
	ctx := context.Background()
	var info struct{ ProductName, Version string }
	must(t, c.do(ctx, http.MethodGet, "/System/Info/Public", nil, &info))
	t.Logf("%s %s %s", kind, info.ProductName, info.Version)
	password := secret(t)
	must(t, c.do(ctx, http.MethodPost, "/Startup/Configuration", map[string]any{"UICulture": "en-US", "MetadataCountryCode": "US", "PreferredMetadataLanguage": "en"}, nil))
	must(t, c.do(ctx, http.MethodGet, "/Startup/User", nil, nil))
	must(t, c.do(ctx, http.MethodPost, "/Startup/User", map[string]any{"Name": "atoll", "Password": password}, nil))
	must(t, c.do(ctx, http.MethodPost, "/Startup/Complete", nil, nil))
	var auth struct{ AccessToken string }
	must(t, c.do(ctx, http.MethodPost, "/Users/AuthenticateByName", map[string]any{"Username": "atoll", "Pw": password}, &auth))
	if auth.AccessToken == "" {
		t.Fatal("the server gave no access token")
	}
	c.header.Set(authHeader, device+`, Token="`+auth.AccessToken+`"`)
	must(t, c.do(ctx, http.MethodPost, "/Auth/Keys?app=atoll", nil, nil))
	var keys struct {
		Items []struct{ AccessToken, AppName string }
	}
	must(t, c.do(ctx, http.MethodGet, "/Auth/Keys", nil, &keys))
	apiKey := ""
	for _, k := range keys.Items {
		if k.AppName == "atoll" {
			apiKey = k.AccessToken
		}
	}
	if apiKey == "" {
		t.Fatalf("the server made no API key: %d keys", len(keys.Items))
	}
	// The server acts on what it is told of after LibraryMonitorDelay, 60 s by default.
	var conf map[string]any
	must(t, c.do(ctx, http.MethodGet, "/System/Configuration", nil, &conf))
	if _, ok := conf["LibraryMonitorDelay"]; !ok {
		t.Log("the server's configuration has no LibraryMonitorDelay")
	}
	conf["LibraryMonitorDelay"] = 5
	must(t, c.do(ctx, http.MethodPost, "/System/Configuration", conf, nil))
	plant(t, kind)
	q := url.Values{"name": {"Atoll Movies"}, "collectionType": {"movies"}, "refreshLibrary": {"true"}, "paths": {serverRoot + "/" + kind + "/Movies"}}
	options := map[string]any{"LibraryOptions": map[string]any{
		"EnableRealtimeMonitor": false,
		"PathInfos":             []map[string]string{{"Path": serverRoot + "/" + kind + "/Movies"}},
	}}
	must(t, c.do(ctx, http.MethodPost, "/Library/VirtualFolders?"+q.Encode(), options, nil))
	file := filepath.Join(t.TempDir(), kind+".key")
	must(t, os.WriteFile(file, []byte(apiKey+"\n"), 0o600))
	list := func(ctx context.Context) ([]string, error) {
		var items struct {
			Items []struct{ Path string }
		}
		if err := c.do(ctx, http.MethodGet, "/Items?Recursive=true&IncludeItemTypes=Movie&Fields=Path", nil, &items); err != nil {
			return nil, err
		}
		var files []string
		for _, i := range items.Items {
			files = append(files, i.Path)
		}
		return files, nil
	}
	return file, list
}

// scenario writes, renames and deletes a movie in the folder kind/Movies of the view, and tells the server of each
// change as the daemon would (internal/daemon/mediaservers.go): the server must list what the view holds after each.
func scenario(t *testing.T, kind string, s notify.Server, list func(context.Context) ([]string, error)) {
	t.Helper()
	ctx := context.Background()
	if err := s.Check(ctx); err != nil {
		t.Fatalf("atoll notify test: %v", err)
	}
	movies := kind + "/Movies"
	listed := func(file string) (bool, error) {
		files, err := list(ctx)
		return slices.Contains(files, notify.Seen(viewMount, file, pathMap)), err
	}
	waitFor(t, "the library lists the movie it was made with, alone", settle, func(ctx context.Context) (bool, error) {
		files, err := list(ctx)
		if err == nil && (len(files) != 1 || files[0] != notify.Seen(viewMount, seed(kind), pathMap)) {
			return false, fmt.Errorf("the library lists %q", files)
		}
		return err == nil, err
	})

	// A file written straight to a disk: the pass that finds it tells the folder that holds it.
	first := movies + "/Atoll Test (2020)"
	firstFile := first + "/Atoll Test (2020).mkv"
	if !t.Run("new", func(t *testing.T) {
		mkdir(t, first)
		copySample(t, firstFile)
		time.Sleep(quiet)
		if found, err := listed(firstFile); err != nil || found {
			t.Fatalf("the server found the file on its own (%v): the test cannot tell what a notification does", err)
		}
		tell(t, s, first)
		waitFor(t, "the server lists the new file", settle, func(context.Context) (bool, error) { return listed(firstFile) })
	}) {
		return
	}

	// Radarr renames the folder, then the file in it: the pass tells the folders of both paths of each rename.
	second := movies + "/Atoll Renamed (2021)"
	secondFile := second + "/Atoll Renamed (2021).mkv"
	if !t.Run("rename", func(t *testing.T) {
		must(t, os.Rename(view(first), view(second)))
		must(t, os.Rename(view(second+"/Atoll Test (2020).mkv"), view(secondFile)))
		tell(t, s, movies, second)
		waitFor(t, "the server lists the file at its new name alone", settle, func(context.Context) (bool, error) {
			now, err := listed(secondFile)
			if err != nil || !now {
				return false, err
			}
			before, err := listed(firstFile)
			return !before, err
		})
	}) {
		return
	}

	// The file and its folder deleted on one disk while the daemon was stopped: the pass sends the other copies to
	// the trash and tells the folder of the file, which is no longer there.
	t.Run("delete", func(t *testing.T) {
		must(t, os.RemoveAll(view(second)))
		tell(t, s, second)
		waitFor(t, "the server lists the file no more", settle, func(context.Context) (bool, error) {
			found, err := listed(secondFile)
			return !found, err
		})
	})
}

// tell asks the server to scan folders of the view, as the daemon does after a pass.
func tell(t *testing.T, s notify.Server, folders ...string) {
	t.Helper()
	if err := s.Scan(context.Background(), folders); err != nil {
		t.Fatalf("%s: %v", s.Name(), err)
	}
}

// client sends requests to a server's API and decodes its JSON answers.
type client struct {
	base   string
	header http.Header
}

func (c *client) do(ctx context.Context, method, path string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return err
	}
	for k, v := range c.header {
		req.Header[k] = v
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s: %.300s", method, path, resp.Status, data)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s %s: %w: %.300s", method, path, err, data)
		}
	}
	return nil
}

// waitFor calls cond every 2 s until it holds, and fails the test past limit.
func waitFor(t *testing.T, what string, limit time.Duration, cond func(context.Context) (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		ok, err := cond(context.Background())
		if err == nil && ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s (last error: %v)", what, limit, err)
		}
		time.Sleep(2 * time.Second)
	}
}

func need(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s is not set: run test/mediaservers/run.sh", name)
	}
	return strings.TrimSuffix(v, "/")
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// view gives the path of a folder or file of the view, as the test writes it.
func view(p string) string { return viewMount + "/" + p }

func mkdir(t *testing.T, p string) {
	t.Helper()
	must(t, os.MkdirAll(view(p), 0o755))
}

// seed is the movie a library holds when it is made, as a library in use does. Plex 1.43.5 and Jellyfin 12.4 ignore
// what they are told of in a library none of whose scans found a file yet, until a scan of the whole library finds
// one (measured on 2026-11-19).
func seed(kind string) string { return kind + "/Movies/Atoll Seed (2000)/Atoll Seed (2000).mkv" }

// plant makes the folder of kind's library, with its seed.
func plant(t *testing.T, kind string) {
	t.Helper()
	mkdir(t, path.Dir(seed(kind)))
	copySample(t, seed(kind))
}

func copySample(t *testing.T, p string) {
	t.Helper()
	data, err := os.ReadFile("testdata/sample.mkv")
	must(t, err)
	must(t, os.WriteFile(view(p), data, 0o644))
}

// secret gives a password made for this run, never printed.
func secret(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
