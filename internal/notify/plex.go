package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// sectionsLimit bounds the answer of /library/sections, which lists libraries and their folders only.
const sectionsLimit = 4 << 20

// Plex asks Plex for a partial scan of each folder that changed, in each library that holds it:
// GET /library/sections/<id>/refresh?path=<folder> (specs.md §21.5).
type Plex struct {
	URL string
	// TokenFile holds the X-Plex-Token: empty when Plex takes requests from this host without one.
	TokenFile string
	ViewMount string
	PathMap   map[string]string
	Client    *http.Client // http.DefaultClient when nil
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
// asks for a scan of that library's folder. A folder in no library asks for nothing.
func (p *Plex) Scan(ctx context.Context, folders []string) error {
	sections, err := p.sections(ctx)
	if err != nil {
		return err
	}
	type refresh struct{ key, path string }
	asked := map[refresh]bool{}
	for _, folder := range folders {
		seen := Seen(p.ViewMount, folder, p.PathMap)
		for _, s := range sections {
			for _, l := range s.Location {
				r := refresh{key: s.Key}
				switch {
				case Within(seen, l.Path):
					r.path = seen
				case Within(l.Path, seen):
					r.path = l.Path
				default:
					continue
				}
				if asked[r] {
					continue
				}
				asked[r] = true
				if err := p.get(ctx, "/library/sections/"+url.PathEscape(r.key)+"/refresh?path="+url.QueryEscape(r.path), nil); err != nil {
					return err
				}
			}
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
