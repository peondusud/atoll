package alerts

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// request is what a ntfy server got.
type request struct {
	method, path, title, priority, tags, auth, body string
}

// fakeNtfy answers each request with a status, and keeps what it got.
func fakeNtfy(t *testing.T, status int) (*httptest.Server, *[]request) {
	t.Helper()
	var got []request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, request{r.Method, r.URL.Path, r.Header.Get("Title"), r.Header.Get("Priority"), r.Header.Get("Tags"),
			r.Header.Get("Authorization"), string(body)})
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":"no"}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

// A POST of the text to the topic, with its title, its priority and the tag warning; the token, read from its file
// at each send, goes in Authorization (specs.md §23).
func TestNtfySends(t *testing.T) {
	srv, got := fakeNtfy(t, http.StatusOK)
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("tk_one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	n := &Ntfy{URL: srv.URL + "/atoll", TokenFile: token}
	m := &Message{Key: "k", Title: "Atoll: disk t1 is missing", Body: "line 1\nfilms/été.mkv", Priority: 4}
	if err := n.Send(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token, []byte("  tk_two  "), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&Ntfy{URL: srv.URL + "/atoll"}).Send(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	if err := n.Send(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	want := []request{
		{"POST", "/atoll", m.Title, "4", "warning", "Bearer tk_one", m.Body},
		{"POST", "/atoll", m.Title, "4", "warning", "", m.Body},
		{"POST", "/atoll", m.Title, "4", "warning", "Bearer tk_two", m.Body},
	}
	if len(*got) != len(want) {
		t.Fatalf("%+v", *got)
	}
	for i := range want {
		if (*got)[i] != want[i] {
			t.Errorf("request %d: %+v", i, (*got)[i])
		}
	}
}

// What does not go is an error: an answer other than 2xx, a server that cannot be reached, a token file missing or
// holding no token; with a bad token file, nothing is sent. No error names the topic.
func TestNtfyFails(t *testing.T) {
	m := &Message{Key: "k", Title: "t", Body: "b", Priority: 3}
	refused, got := fakeNtfy(t, http.StatusForbidden)
	var answered *AnswerError
	if err := (&Ntfy{URL: refused.URL + "/secret-topic"}).Send(t.Context(), m); !errors.As(err, &answered) || !strings.Contains(err.Error(), "403") {
		t.Errorf("403: %v", err)
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	err := (&Ntfy{URL: gone.URL + "/secret-topic"}).Send(t.Context(), m)
	if err == nil || errors.As(err, &answered) || strings.Contains(err.Error(), "secret-topic") {
		t.Errorf("unreachable: %v", err)
	}
	dir := t.TempDir()
	for name, text := range map[string]string{"empty": " \n", "two lines": "a\nb\n"} {
		file := filepath.Join(dir, name)
		if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := (&Ntfy{URL: refused.URL, TokenFile: file}).Send(t.Context(), m); err == nil || !strings.Contains(err.Error(), "holds no token on one line") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := (&Ntfy{URL: refused.URL, TokenFile: filepath.Join(dir, "none")}).Send(t.Context(), m); err == nil {
		t.Error("no token file: no error")
	}
	if len(*got) != 1 {
		t.Errorf("sent with a bad token file: %+v", *got)
	}
}
