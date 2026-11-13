package alerts

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gotifyRequest is what a Gotify server got.
type gotifyRequest struct {
	method, path, key, contentType string
	message                        gotifyMessage
}

// fakeGotify answers each request with a status, and keeps what it got.
func fakeGotify(t *testing.T, status int) (*httptest.Server, *[]gotifyRequest) {
	t.Helper()
	var got []gotifyRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req := gotifyRequest{method: r.Method, path: r.URL.Path, key: r.Header.Get("X-Gotify-Key"), contentType: r.Header.Get("Content-Type")}
		if err := json.Unmarshal(body, &req.message); err != nil {
			t.Errorf("not JSON: %q", body)
		}
		got = append(got, req)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":"Unauthorized"}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

// A POST to <alerts.gotify>/message of the title, the text and the priority, 8 for what ntfy sends at 4 and 5
// otherwise, with the application token, read from its file at each send, in X-Gotify-Key (specs.md §23).
func TestGotifySends(t *testing.T) {
	srv, got := fakeGotify(t, http.StatusOK)
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("AppTok.one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := &Gotify{URL: srv.URL + "/", TokenFile: token}
	urgent := &Message{Key: "k", Title: "Atoll: disk t1 is missing", Body: "line 1\nfilms/été.mkv", Priority: 4}
	if err := g.Send(t.Context(), urgent); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token, []byte("  AppTok.two  "), 0o600); err != nil {
		t.Fatal(err)
	}
	other := &Message{Key: "k2", Title: "Atoll: 3 files in conflict", Body: "x", Priority: 3}
	if err := g.Send(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	want := []gotifyRequest{
		{"POST", "/message", "AppTok.one", "application/json", gotifyMessage{Title: urgent.Title, Message: urgent.Body, Priority: 8}},
		{"POST", "/message", "AppTok.two", "application/json", gotifyMessage{Title: other.Title, Message: other.Body, Priority: 5}},
	}
	if len(*got) != len(want) {
		t.Fatalf("%d requests: %+v", len(*got), *got)
	}
	for i := range want {
		if (*got)[i] != want[i] {
			t.Errorf("request %d: %+v, want %+v", i, (*got)[i], want[i])
		}
	}
}

// A refusal is the server's answer, which says why; with no token file, or a token file that holds none, nothing is
// sent at all.
func TestGotifyFails(t *testing.T) {
	srv, got := fakeGotify(t, http.StatusUnauthorized)
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Message{Key: "k", Title: "t", Body: "b", Priority: 3}
	err := (&Gotify{URL: srv.URL, TokenFile: token}).Send(t.Context(), m)
	var answer *AnswerError
	if !errors.As(err, &answer) || !strings.Contains(answer.Text, "Unauthorized") {
		t.Errorf("a refusal: %v", err)
	}
	if err := (&Gotify{URL: srv.URL}).Send(t.Context(), m); err == nil || !strings.Contains(err.Error(), "gotify_token_file") {
		t.Errorf("no token file: %v", err)
	}
	if err := os.WriteFile(token, []byte("two\nlines"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&Gotify{URL: srv.URL, TokenFile: token}).Send(t.Context(), m); err == nil {
		t.Error("a token on two lines")
	}
	if len(*got) != 1 {
		t.Errorf("%d requests sent, want the one refused", len(*got))
	}
}
