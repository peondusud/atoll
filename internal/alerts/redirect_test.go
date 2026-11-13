package alerts

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// A redirect is not followed: the token, and the alert with it, would go to whatever host the answer names, and a POST
// redirected with 301 or 302 becomes a GET, which sends nothing and is answered 200 (specs.md §23). What it leads to
// stays out of the error, as the topic of a public ntfy server is its secret.
func TestAlertsDoNotFollowARedirect(t *testing.T) {
	var reached atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Add(1) }))
	defer other.Close()
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		first := httptest.NewServer(http.RedirectHandler(other.URL+"/elsewhere", status))
		for _, ch := range []Channel{&Ntfy{URL: first.URL + "/topic", TokenFile: token}, &Gotify{URL: first.URL, TokenFile: token}} {
			err := ch.Send(t.Context(), &Message{Key: "k", Title: "a title", Body: "a body", Priority: 3})
			if err == nil {
				t.Errorf("%s, %d: an alert that went nowhere counts as sent", ch.Name(), status)
			} else if strings.Contains(err.Error(), "elsewhere") {
				t.Errorf("%s, %d: the error says where the redirect leads: %v", ch.Name(), status, err)
			}
		}
		first.Close()
	}
	if n := reached.Load(); n != 0 {
		t.Errorf("%d requests followed a redirect to another host", n)
	}
}
