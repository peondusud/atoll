package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/fsx"
)

// requestTimeout is how long one request to a media server has.
const requestTimeout = 10 * time.Second

// Server tells one media server of the folders of the view that changed (specs.md §21.5).
type Server interface {
	// Name is the configuration table that sets the server.
	Name() string
	// Scan asks the server to scan folders, each a path relative to the top of the view, as the server sees them.
	Scan(ctx context.Context, folders []string) error
	// Check asks the server something that changes nothing, which its token or key must open: atoll notify test.
	Check(ctx context.Context) error
}

// Servers gives a server for each media server the configuration sets, in its order.
func Servers(c *config.Config) []Server {
	view, _ := c.Effective()["view.mount"].(string)
	out := make([]Server, 0, len(c.MediaServers))
	for _, m := range c.MediaServers {
		if m.Kind == config.Plex {
			out = append(out, &Plex{URL: m.URL, TokenFile: m.SecretFile, ViewMount: view, PathMap: m.PathMap})
		} else {
			out = append(out, &MediaBrowser{Kind: m.Kind, URL: m.URL, KeyFile: m.SecretFile, ViewMount: view, PathMap: m.PathMap})
		}
	}
	return out
}

// Seen gives the path a server sees for a folder of the view: the view's mount, then the folder, through the longest
// key of pathMap that is a prefix of it by whole components (specs.md §21.5). With no such key, the path is the
// host's.
func Seen(viewMount, folder string, pathMap map[string]string) string {
	host := viewMount
	if folder != "" {
		host = viewMount + "/" + folder
	}
	best := ""
	for key := range pathMap {
		if Within(host, key) && len(key) > len(best) {
			best = key
		}
	}
	if best == "" {
		return host
	}
	return pathMap[best] + host[len(best):]
}

// Within says whether path is folder or below it, by whole components.
func Within(path, folder string) bool {
	return path == folder || strings.HasPrefix(path, folder+"/") || folder == "/"
}

// readSecret reads the token or key a file holds, on one line, at each request, so that a new one needs no reload.
func readSecret(file string) (string, error) {
	data, err := fsx.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("the secret file: %w", err)
	}
	secret := strings.TrimSpace(string(data))
	if secret == "" || strings.ContainsAny(secret, "\r\n\"") {
		return "", fmt.Errorf("the secret file %s holds no token on one line", file)
	}
	return secret, nil
}

// direct is the client of a request that carries a token or a key: it follows no redirect, as the secret would go to
// whatever host the answer names. The address in the configuration is the server's own.
var direct = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// do sends a request and reads the answer, which must be 2xx; the body read is at most limit bytes.
func do(client *http.Client, req *http.Request, limit int64) ([]byte, error) {
	if client == nil {
		client = direct
	}
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 == 3 {
		return nil, fmt.Errorf("%s: a redirect, which is not followed: write the address it leads to in the configuration", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(body[:min(len(body), 512)]))
	}
	if err != nil {
		return nil, fmt.Errorf("the answer: %w", err)
	}
	return body, nil
}

// endpoint joins a server's URL and a path.
func endpoint(base, path string) string {
	return strings.TrimRight(base, "/") + path
}

// requestContext gives a request its time.
func requestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, requestTimeout)
}
