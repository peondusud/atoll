package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
)

// apiPool writes a configuration with one disk that is not mounted, and makes its catalog.
func apiPool(t *testing.T, more string) (string, *config.Config) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "atoll.toml")
	file := filepath.Join(dir, "catalog.db")
	text := "pool_id = \"p\"\ncatalog = " + strconv.Quote(file) + "\n[view]\nmount = " + strconv.Quote(filepath.Join(dir, "pool")) + "\n" +
		"[[disk]]\nid = \"t1\"\nuuid = \"3f1c9a2e-5b7d-4c1e-9a0b-2d4f6e8a1c3b\"\nmount = " + strconv.Quote(filepath.Join(dir, "nowhere")) + "\nfs = \"ext4\"\n" + more
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Create(t.Context(), file, "p"); err != nil {
		t.Fatal(err)
	}
	c, err := load(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, c
}

func newTestAPI(t *testing.T, c *config.Config) *apiServer {
	t.Helper()
	m, err := disks.NewMachine()
	if err != nil {
		t.Fatal(err)
	}
	return newAPIServer(c, m, slog.New(slog.DiscardHandler))
}

func ask(h http.Handler, method, path, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(context.Background(), method, path, nil)
	if token != "" {
		r.Header.Set("Authorization", token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// Each path answers what its command's --json gives, byte for byte, GET alone; another path, a disk the configuration
// does not list and a catalog that cannot be used are said so (specs.md §25.1).
func TestTheAPIAnswersAsTheCommands(t *testing.T) {
	configPath, c := apiPool(t, "")
	h := newTestAPI(t, c).routes()
	for path, args := range map[string][]string{
		"/v1/status": {"status", "--json"}, "/v1/disks": {"disk", "list", "--json"}, "/v1/disks/t1": {"disk", "show", "t1", "--json"},
		"/v1/trash": {"trash", "list", "--json"}, "/v1/guards": {"guard", "list", "--json"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(append([]string{"--config", configPath}, args...), &stdout, &stderr); code != exitOK {
			t.Fatalf("%v: exit %d: %s", args, code, stderr.String())
		}
		w := ask(h, http.MethodGet, path, "")
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" || w.Body.String() != stdout.String() {
			t.Errorf("%s: %d %q\n%s\nwant\n%s", path, w.Code, w.Header().Get("Content-Type"), w.Body.String(), stdout.String())
		}
	}
	var health struct {
		Version string `json:"version"`
	}
	if w := ask(h, http.MethodGet, "/v1/health", ""); w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &health) != nil || health.Version == "" {
		t.Errorf("health: %d %s", w.Code, w.Body.String())
	}
	for _, method := range []string{http.MethodPost, http.MethodHead, http.MethodDelete} {
		if w := ask(h, method, "/v1/status", ""); w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodGet {
			t.Errorf("%s: %d", method, w.Code)
		}
	}
	for _, path := range []string{"/v1/nothing", "/", "/v1/disks/zz", "/v2/status"} {
		if w := ask(h, http.MethodGet, path, ""); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"error"`) {
			t.Errorf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	file, _ := c.Effective()["catalog"].(string)
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(file, 0o755); err != nil {
		t.Fatal(err)
	}
	if w := ask(h, http.MethodGet, "/v1/status", ""); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"error"`) {
		t.Errorf("a catalog that cannot be used: %d %s", w.Code, w.Body.String())
	}
}

// Over TCP every request carries the token, read again each time; without it, or with another, the answer is 401 with
// nothing more (specs.md §25.1).
func TestTheAPIOverTCPAsksForItsToken(t *testing.T) {
	token := filepath.Join(t.TempDir(), "api.token")
	if err := os.WriteFile(token, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, c := apiPool(t, "[api]\nlisten = \"127.0.0.1:7341\"\ntoken_file = "+strconv.Quote(token)+"\n")
	a := newTestAPI(t, c)
	h := a.withToken(a.routes())
	for _, header := range []string{"", "Bearer", "Bearer ", "Bearer s3cre", "Bearer s3cret ", "bearer s3cret", "Basic czNjcmV0"} {
		if w := ask(h, http.MethodGet, "/v1/health", header); w.Code != http.StatusUnauthorized || w.Body.Len() != 0 || w.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%q: %d %q", header, w.Code, w.Body.String())
		}
	}
	if w := ask(h, http.MethodGet, "/v1/health", "Bearer s3cret"); w.Code != http.StatusOK {
		t.Errorf("the token: %d", w.Code)
	}
	if err := os.WriteFile(token, []byte("n3w\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if w := ask(h, http.MethodGet, "/v1/health", "Bearer n3w"); w.Code != http.StatusOK {
		t.Errorf("a new token: %d", w.Code)
	}
	if w := ask(h, http.MethodGet, "/v1/health", "Bearer s3cret"); w.Code != http.StatusUnauthorized {
		t.Errorf("the old token: %d", w.Code)
	}
	for _, text := range []string{"", "\n"} {
		if err := os.WriteFile(token, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if w := ask(h, http.MethodGet, "/v1/health", "Bearer "); w.Code != http.StatusUnauthorized {
			t.Errorf("an empty token %q: %d", text, w.Code)
		}
	}
	if err := os.Remove(token); err != nil {
		t.Fatal(err)
	}
	if w := ask(h, http.MethodGet, "/v1/health", "Bearer n3w"); w.Code != http.StatusUnauthorized {
		t.Errorf("no token file: %d", w.Code)
	}
}

func socketClient(path string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}}
}

func getHealth(t *testing.T, client *http.Client, url, token string) int {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(r)
	if err != nil {
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// The socket is made with mode 0660 and answers with no token; it replaces a socket an earlier run left, leaves
// anything else at its path, and goes when the daemon stops (specs.md §25.1).
func TestTheAPISocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "atoll.sock")
	_, c := apiPool(t, "[api]\nsocket = "+strconv.Quote(path)+"\n")
	// What an earlier run left: a socket nobody listens on.
	old, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	old.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = old.Close()
	a := newTestAPI(t, c)
	stop := a.start(t.Context(), c)
	if code := getHealth(t, socketClient(path), "http://atoll/v1/health", ""); code != http.StatusOK {
		t.Errorf("over the socket: %d", code)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o660 {
		t.Errorf("the socket: %v, %v", st.Mode(), err)
	}
	stop()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("the socket stayed: %v", err)
	}
	// Anything else at its path stays, and the socket is off.
	if err := os.WriteFile(path, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	stop = a.start(t.Context(), c)
	defer stop()
	if data, err := os.ReadFile(path); err != nil || string(data) != "mine" {
		t.Errorf("the file at its path: %q, %v", data, err)
	}
}

// Over TCP, on a loopback address, the token is asked for (specs.md §25.1).
func TestTheAPIOverTCP(t *testing.T) {
	free, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := free.Addr().(*net.TCPAddr).Port
	_ = free.Close()
	token := filepath.Join(t.TempDir(), "api.token")
	if err := os.WriteFile(token, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	listen := "127.0.0.1:" + strconv.Itoa(port)
	_, c := apiPool(t, "[api]\nsocket = \"\"\nlisten = "+strconv.Quote(listen)+"\ntoken_file = "+strconv.Quote(token)+"\n")
	stop := newTestAPI(t, c).start(t.Context(), c)
	defer stop()
	client := &http.Client{}
	if code := getHealth(t, client, "http://"+listen+"/v1/health", ""); code != http.StatusUnauthorized {
		t.Errorf("no token: %d", code)
	}
	if code := getHealth(t, client, "http://"+listen+"/v1/health", "s3cret"); code != http.StatusOK {
		t.Errorf("the token: %d", code)
	}
}

// api/openapi.yaml lists exactly the paths the server answers, and each schema it names is there (specs.md §29.3).
func TestTheOpenAPIDocument(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, line := range strings.Split(string(data), "\n") {
		if path, ok := strings.CutPrefix(line, "  /"); ok && strings.HasSuffix(path, ":") {
			listed = append(listed, "/"+strings.TrimSuffix(path, ":"))
		}
		if _, ref, ok := strings.Cut(line, `$ref: "schemas/`); ok {
			name, _, _ := strings.Cut(ref, `"`)
			if _, err := os.Stat(filepath.Join("..", "..", "api", "schemas", name)); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
	}
	_, c := apiPool(t, "")
	served := slices.Sorted(maps.Keys(newTestAPI(t, c).answers()))
	slices.Sort(listed)
	if !slices.Equal(listed, served) {
		t.Errorf("openapi.yaml lists %v, the server answers %v", listed, served)
	}
}
