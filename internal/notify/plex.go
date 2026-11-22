package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// sectionsLimit bounds the answer of /library/sections, which lists libraries and their folders only.
const sectionsLimit = 4 << 20

// What Plex answers sets no number of scans Atoll asks for (scrutineer F050, 2026-11-22). A list of more library
// folders than maxLibraryFolders, every library's together, or a folder that changed in more libraries than
// maxLibrariesOfAFolder asks for none: no server of a home holds as many, and one that says so would have the daemon
// ask for a scan in each, one after the other, its passes waiting meanwhile. A notification then asks for at most
// maxLibrariesOfAFolder scans a folder, and one a library folder below one of them.
const (
	maxLibraryFolders     = 1024
	maxLibrariesOfAFolder = 8
)

// notifyBase, then refreshEach for each of Plex's scans or postEach for each request to Jellyfin or Emby, is the time
// one notification of a server may take: notifyBase, the time of one request, then what each asked takes, which the
// server answers at once as it scans later. Past it, what is left is not asked, and the notification is tried again
// later, so that a server that answers each slowly holds the daemon's turn for that long at most. Variables, so that a
// test can shorten them.
var (
	notifyBase  = requestTimeout
	refreshEach = 100 * time.Millisecond
	postEach    = time.Second
)

// Plex asks Plex for a partial scan of each folder that changed, in each library that holds it:
// GET /library/sections/<id>/refresh?path=<folder> (specs.md §21.5).
type Plex struct {
	URL string
	// TokenFile holds the X-Plex-Token: empty when Plex takes requests from this host without one.
	TokenFile string
	ViewMount string
	PathMap   map[string]string
	Client    *http.Client // one that follows no redirect when nil
}

// Name is the table of the configuration that sets Plex.
func (p *Plex) Name() string { return "notify.plex" }

// section is a library of Plex, with the folders it reads.
type section struct {
	Key      string
	Location []struct {
		Path string `json:"path"`
	}
}

// Scan reads Plex's libraries, then asks each library that holds a folder for a scan of it; a folder above a library
// asks for a scan of that library's folder. A folder in no library asks for nothing. What it is to ask is known
// before it asks anything, and bounded (maxLibrariesOfAFolder), and all of it is asked within a budget (notifyBase,
// refreshEach).
func (p *Plex) Scan(ctx context.Context, folders []string) error {
	sections, err := p.sections(ctx)
	if err != nil {
		return err
	}
	type refresh struct{ key, path string }
	asked := map[refresh]bool{}
	var plan []refresh
	for _, folder := range folders {
		seen := Seen(p.ViewMount, folder, p.PathMap)
		within := 0
		for _, s := range sections {
			for _, l := range s.Location {
				r := refresh{key: s.Key}
				switch {
				case Within(seen, l.Path):
					r.path = seen
					within++
				case Within(l.Path, seen):
					r.path = l.Path
				default:
					continue
				}
				if !asked[r] {
					asked[r] = true
					plan = append(plan, r)
				}
			}
		}
		if within > maxLibrariesOfAFolder {
			return fmt.Errorf("plex: %s is in %d library folders, more than the %d of a server: it asked for no scan", seen, within, maxLibrariesOfAFolder)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, notifyBase+time.Duration(len(plan))*refreshEach)
	defer cancel()
	for _, r := range plan {
		if err := p.get(ctx, "/library/sections/"+url.PathEscape(r.key)+"/refresh?path="+url.QueryEscape(r.path), nil); err != nil {
			return err
		}
	}
	return nil
}

// Check reads Plex's libraries.
func (p *Plex) Check(ctx context.Context) error {
	_, err := p.sections(ctx)
	return err
}

// sections reads Plex's libraries.
func (p *Plex) sections(ctx context.Context) ([]section, error) {
	var answer struct {
		MediaContainer struct {
			Directory []section
		}
	}
	if err := p.get(ctx, "/library/sections", &answer); err != nil {
		return nil, err
	}
	folders := 0
	for _, s := range answer.MediaContainer.Directory {
		folders += len(s.Location)
	}
	if folders > maxLibraryFolders {
		return nil, fmt.Errorf("plex: it lists %d library folders, more than the %d of a server: it is asked for no scan", folders, maxLibraryFolders)
	}
	return answer.MediaContainer.Directory, nil
}

// get sends a GET with the token, and decodes the JSON answer into v when v is not nil.
func (p *Plex) get(ctx context.Context, path string, v any) error {
	ctx, cancel := requestContext(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint(p.URL, path), nil)
	if err != nil {
		return fmt.Errorf("plex: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if p.TokenFile != "" {
		token, err := readSecret(p.TokenFile)
		if err != nil {
			return fmt.Errorf("plex: %w", err)
		}
		req.Header.Set("X-Plex-Token", token)
	}
	body, err := do(p.Client, req, sectionsLimit)
	if err != nil {
		return fmt.Errorf("plex: %w", err)
	}
	if v != nil {
		if err := json.Unmarshal(body, v); err != nil {
			return fmt.Errorf("plex: the libraries: %w", err)
		}
	}
	return nil
}
