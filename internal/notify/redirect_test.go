package notify

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/peondusud/atoll/internal/config"
)

// A redirect is not followed: the token or the key would go to whatever host the answer names (specs.md §21.5).
func TestMediaServersDoNotFollowARedirect(t *testing.T) {
	var reached atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		_, _ = io.WriteString(w, `{"MediaContainer":{"size":0,"Directory":[]}}`)
	}))
	defer other.Close()
	first := httptest.NewServer(http.RedirectHandler(other.URL+"/elsewhere", http.StatusTemporaryRedirect))
	defer first.Close()
	for _, s := range []Server{
		&Plex{URL: first.URL, TokenFile: secret(t, "tok\n"), ViewMount: "/srv/pool"},
		&MediaBrowser{Kind: config.Jellyfin, URL: first.URL, KeyFile: secret(t, "key\n"), ViewMount: "/srv/pool"},
		&MediaBrowser{Kind: config.Emby, URL: first.URL, KeyFile: secret(t, "key\n"), ViewMount: "/srv/pool"},
	} {
		if err := s.Check(t.Context()); err == nil {
			t.Errorf("%s: a check answered through a redirect", s.Name())
		}
	}
	if n := reached.Load(); n != 0 {
		t.Errorf("%d requests followed a redirect to another host", n)
	}
}
