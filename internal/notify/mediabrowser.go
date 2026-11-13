package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"unicode/utf8"

	"github.com/peondusud/atoll/internal/config"
)

// updatesPerRequest bounds the folders one request names.
const updatesPerRequest = 100

// MediaBrowser tells Jellyfin or Emby, which keep the API of their common ancestor, of the folders that changed:
// POST /Library/Media/Updated, each folder an update of type Modified (specs.md §21.5). Jellyfin takes its key in
// Authorization, as Jellyfin 12.0 no longer takes X-Emby-Token; Emby takes it in X-Emby-Token.
type MediaBrowser struct {
	// Kind is config.Jellyfin or config.Emby.
	Kind      string
	URL       string
	KeyFile   string
	ViewMount string
	PathMap   map[string]string
	Client    *http.Client // http.DefaultClient when nil
}

// Name is the table of the configuration that sets the server.
func (m *MediaBrowser) Name() string { return "notify." + m.Kind }

// update is one entry of /Library/Media/Updated.
type update struct {
	Path       string `json:"Path"`
	UpdateType string `json:"UpdateType"`
}

// Scan names the folders, a hundred a request. A folder whose path is not UTF-8 cannot go in JSON, and is left out.
func (m *MediaBrowser) Scan(ctx context.Context, folders []string) error {
	var updates []update
	for _, folder := range folders {
		if seen := Seen(m.ViewMount, folder, m.PathMap); utf8.ValidString(seen) {
			updates = append(updates, update{Path: seen, UpdateType: "Modified"})
		}
	}
	for len(updates) > 0 {
		n := min(len(updates), updatesPerRequest)
		body, err := json.Marshal(struct {
			Updates []update `json:"Updates"`
		}{updates[:n]})
		if err != nil {
			return fmt.Errorf("%s: %w", m.Kind, err)
		}
		if err := m.send(ctx, http.MethodPost, "/Library/Media/Updated", body); err != nil {
			return err
		}
		updates = updates[n:]
	}
	return nil
}

// Check reads the server's information, which takes the key.
func (m *MediaBrowser) Check(ctx context.Context) error {
	return m.send(ctx, http.MethodGet, "/System/Info", nil)
}

// send sends a request with the key. With no key file, the server takes nothing, and nothing is sent.
func (m *MediaBrowser) send(ctx context.Context, method, path string, body []byte) error {
	if m.KeyFile == "" {
		return fmt.Errorf("%s: no api_key_file: the server takes no request without a key", m.Kind)
	}
	key, err := readSecret(m.KeyFile)
	if err != nil {
		return fmt.Errorf("%s: %w", m.Kind, err)
	}
	ctx, cancel := requestContext(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, endpoint(m.URL, path), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%s: %w", m.Kind, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	switch m.Kind {
	case config.Jellyfin:
		req.Header.Set("Authorization", `MediaBrowser Token="`+key+`"`)
	case config.Emby:
		req.Header.Set("X-Emby-Token", key)
	default:
		return errors.New("notify: no media server of the kind " + m.Kind)
	}
	if _, err := do(m.Client, req, 512); err != nil {
		return fmt.Errorf("%s: %w", m.Kind, err)
	}
	return nil
}
