package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/catalog"
	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/metrics"
	"github.com/peondusud/atoll/internal/value"
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
	rec := metrics.NewRecorder("v0.1.0")
	rec.Update(func(s *metrics.Snapshot) {
		s.Configure([]metrics.Disk{{ID: "t1", FS: "ext4", Role: "data", Mount: "/mnt/t1", State: "online"}})
	})
	return newAPIServer(c, m, slog.New(slog.DiscardHandler), rec)
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

// A request over TCP that brings no token, or another, waits a quarter of a second before its 401, so that a token is
// not guessed at the speed of the network; the right token waits for nothing; and the wait ends with its client, so
// that it holds its place of api.listen no longer than its client does (specs.md §25.1).
func TestARefusedTokenWaits(t *testing.T) {
	token := filepath.Join(t.TempDir(), "api.token")
	if err := os.WriteFile(token, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, c := apiPool(t, "[api]\nlisten = \"127.0.0.1:7341\"\ntoken_file = "+strconv.Quote(token)+"\n")
	a := newTestAPI(t, c)
	h := a.withToken(a.routes())
	if apiRefusedWait < 100*time.Millisecond || apiRefusedWait > 500*time.Millisecond {
		t.Errorf("a refusal waits %s: a few hundred milliseconds", apiRefusedWait)
	}
	for _, header := range []string{"", "Bearer guess"} {
		start := time.Now()
		if w := ask(h, http.MethodGet, "/v1/health", header); w.Code != http.StatusUnauthorized {
			t.Errorf("%q: %d", header, w.Code)
		}
		if waited := time.Since(start); waited < apiRefusedWait {
			t.Errorf("%q was refused after %s, want %s at least", header, waited, apiRefusedWait)
		}
	}
	// With a refusal that waits half a minute, the right token is answered before the request's own 10 s are over.
	a.refusedWait = 30 * time.Second
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/health", nil)
	r.Header.Set("Authorization", "Bearer s3cret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || ctx.Err() != nil {
		t.Errorf("the right token: %d, %v", w.Code, ctx.Err())
	}
	// A client that gives up ends the wait of its refusal, over a real connection.
	done := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
		done <- struct{}{}
	}))
	defer server.Close()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/v1/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer guess")
	if resp, err := (&http.Client{Timeout: 200 * time.Millisecond}).Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatalf("answered %d before the wait was over", resp.StatusCode)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Error("the refusal still waits, its client gone")
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

// A request whose client is gone is given up, not answered for nobody: its context reaches what reads the catalog
// (specs.md §25.1).
func TestTheAPIGivesUpARequestThatIsGone(t *testing.T) {
	_, c := apiPool(t, "")
	h := newTestAPI(t, c).routes()
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	for _, path := range []string{"/v1/status", "/v1/trash", "/v1/guards"} {
		if code := serve(gone, h, path); code != http.StatusServiceUnavailable {
			t.Errorf("%s: %d", path, code)
		}
	}
}

// The answers that read the catalog are made one at a time, as each is made in the daemon's own process and one may
// plan the whole pool: another waits for it, and gives up with its request (specs.md §25.1).
func TestTheAPIReadsTheCatalogOneAnswerAtATime(t *testing.T) {
	_, c := apiPool(t, "")
	a := newTestAPI(t, c)
	var running, most atomic.Int32
	entered, release := make(chan struct{}, 3), make(chan struct{})
	slow := a.get(a.one(func(*http.Request, *config.Config, io.Writer) (int, error) {
		most.Store(max(most.Load(), running.Add(1)))
		defer running.Add(-1)
		entered <- struct{}{}
		<-release
		return http.StatusOK, nil
	}), jsonType)
	codes := make(chan int, 3)
	for range 3 {
		go func() { codes <- serve(t.Context(), slow, "/v1/status") }()
	}
	<-entered
	// One whose client gives up while another answer is made never starts.
	gone, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if code := serve(gone, slow, "/v1/status"); code != http.StatusServiceUnavailable {
		t.Errorf("a request that gave up while waiting: %d", code)
	}
	if n := len(entered); n != 0 {
		t.Errorf("%d more answers started while one was made", n)
	}
	close(release)
	for range 3 {
		if code := <-codes; code != http.StatusOK {
			t.Errorf("%d", code)
		}
	}
	if most.Load() != 1 {
		t.Errorf("%d answers read the catalog at once", most.Load())
	}
}

// The limits of a request (specs.md §25.1). A connection that asks nothing more is closed after a minute: one left
// open for good holds a descriptor of the daemon, which needs them for its disks.
func TestTheAPIServerLimits(t *testing.T) {
	s := httpServer(nil)
	if s.ReadHeaderTimeout != 10*time.Second || s.ReadTimeout != 15*time.Second || s.WriteTimeout != time.Minute || s.IdleTimeout != time.Minute ||
		s.MaxHeaderBytes != 16<<10 {
		t.Errorf("headers %v, request %v, answer %v, idle %v, %d bytes of headers", s.ReadHeaderTimeout, s.ReadTimeout, s.WriteTimeout, s.IdleTimeout,
			s.MaxHeaderBytes)
	}
	if apiMaxConns != 32 {
		t.Errorf("%d connections at once", apiMaxConns)
	}
}

// A client that sends its body slowly is cut off once the request's time is over, rather than holding its handler,
// and its connection, for as long as it likes: the API's server has a time for the whole request, its body included,
// shortened here (specs.md §25.1).
func TestASlowClientTimesOut(t *testing.T) {
	_, c := apiPool(t, "")
	a := newTestAPI(t, c)
	a.onCatalog, a.apply = directly(c), true
	s := httpServer(a.routes())
	s.ReadTimeout = 300 * time.Millisecond
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve(l) }()
	defer func() { _ = s.Close() }()
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	// The headers, then a byte of a body of 20, and nothing more.
	if _, err := conn.Write([]byte("POST /v1/guards/t1 HTTP/1.1\r\nHost: atoll\r\nContent-Type: application/json\r\nContent-Length: 20\r\n\r\n{")); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := conn.SetReadDeadline(start.Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	answer, _ := io.ReadAll(conn)
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the slow client held its request %v", took)
	}
	if len(answer) > 0 && !bytes.HasPrefix(answer, []byte("HTTP/1.1 400")) {
		t.Errorf("%q", answer)
	}
}

// Each listener of the API holds so many connections at once, and no more: past them, a connection waits, unaccepted,
// until one of them closes. A connection closed twice frees its place once, and a listener closed while one waits
// gives up the wait (specs.md §25.1).
func TestTheConnectionsAreCapped(t *testing.T) {
	inner, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := capConns(inner, 2)
	defer func() { _ = l.Close() }()
	accepted := make(chan net.Conn, 8)
	failed := make(chan error, 1)
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				failed <- err
				return
			}
			accepted <- conn
		}
	}()
	for range 4 {
		conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", inner.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
	}
	next := func() net.Conn {
		select {
		case conn := <-accepted:
			return conn
		case <-time.After(5 * time.Second):
			t.Fatal("no connection taken")
			return nil
		}
	}
	none := func(why string) {
		select {
		case conn := <-accepted:
			t.Errorf("%s: a connection past the cap was taken: %v", why, conn.RemoteAddr())
		case <-time.After(200 * time.Millisecond):
		}
	}
	first, _ := next(), next()
	none("two held")
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	third := next()
	none("a place freed once, though its connection closed twice")
	_ = third.Close()
	next()
	none("four taken")
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-failed:
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("a wait given up: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait was not given up")
	}
}

// The API's servers take the cap: once every place of the socket is held, a request waits until one closes, then is
// answered (specs.md §25.1).
func TestTheAPIWaitsPastItsConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "atoll.sock")
	_, c := apiPool(t, "[api]\nsocket = "+strconv.Quote(path)+"\n")
	stop := newTestAPI(t, c).start(t.Context(), c)
	defer stop()
	var idle []net.Conn
	for range apiMaxConns {
		conn, err := (&net.Dialer{}).DialContext(t.Context(), "unix", path)
		if err != nil {
			t.Fatal(err)
		}
		idle = append(idle, conn)
	}
	defer func() {
		for _, conn := range idle {
			_ = conn.Close()
		}
	}()
	codes := make(chan int, 1)
	go func() { codes <- getHealth(t, socketClient(path), "http://atoll/v1/health", "") }()
	select {
	case code := <-codes:
		t.Fatalf("answered past the cap: %d", code)
	case <-time.After(300 * time.Millisecond):
	}
	_ = idle[0].Close()
	select {
	case code := <-codes:
		if code != http.StatusOK {
			t.Errorf("%d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a place freed, and the request still waits")
	}
}

// clockMachine is a machine whose clock the test sets.
type clockMachine struct {
	disks.Machine
	now atomic.Int64
}

func (m *clockMachine) Now() int64 { return m.now.Load() }

// A token file that cannot be read refuses every request over TCP, and the log says so once a minute at most, with the
// requests refused since: a client with no token could fill it otherwise (specs.md §25.1).
func TestAnUnreadableTokenIsLoggedOnceAMinute(t *testing.T) {
	_, c := apiPool(t, "[api]\nlisten = \"127.0.0.1:7341\"\ntoken_file = "+strconv.Quote(filepath.Join(t.TempDir(), "none"))+"\n")
	a := newTestAPI(t, c)
	m := &clockMachine{Machine: a.machine}
	m.now.Store(time.Hour.Nanoseconds())
	a.machine = m
	var log syncLog
	a.log = slog.New(slog.NewTextHandler(&log, nil))
	h := a.withToken(a.routes())
	for range 50 {
		if w := ask(h, http.MethodGet, "/v1/health", "Bearer x"); w.Code != http.StatusUnauthorized {
			t.Fatalf("%d", w.Code)
		}
	}
	if n := strings.Count(log.String(), "token cannot be read"); n != 1 || !strings.Contains(log.String(), "refused=1") {
		t.Errorf("%d lines:\n%s", n, log.String())
	}
	m.now.Add(time.Minute.Nanoseconds() - 1)
	ask(h, http.MethodGet, "/v1/health", "Bearer x")
	if n := strings.Count(log.String(), "token cannot be read"); n != 1 {
		t.Errorf("before the minute: %d lines", n)
	}
	m.now.Add(1)
	ask(h, http.MethodGet, "/v1/health", "Bearer x")
	if n := strings.Count(log.String(), "token cannot be read"); n != 2 || !strings.Contains(log.String(), "refused=51") {
		t.Errorf("a minute on: %d lines:\n%s", n, log.String())
	}
}

// syncLog is a log that requests write at once.
type syncLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// slowMounts is a machine whose mount checks wait to be let go, and count how many run at once.
type slowMounts struct {
	disks.Machine
	running, most atomic.Int32
	entered       chan struct{}
	release       chan struct{}
}

func (m *slowMounts) Mount(mount string) disks.MountSeen {
	n := m.running.Add(1)
	defer m.running.Add(-1)
	for {
		most := m.most.Load()
		if n <= most || m.most.CompareAndSwap(most, n) {
			break
		}
	}
	m.entered <- struct{}{}
	<-m.release
	return m.Machine.Mount(mount)
}

// Every answer that reads the catalog waits for the one that reads it before, through the routes themselves, not one()
// alone: /v1/disks/{id} read it outside that turn on 2026-11-14's main (scrutineer F060, fixed on beta since). One that
// reads no catalog, as /v1/disks, does not wait for it (specs.md §25.1).
func TestEveryAnswerThatReadsTheCatalogWaitsItsTurn(t *testing.T) {
	_, c := apiPool(t, "")
	a := newTestAPI(t, c)
	h := a.routes()
	a.reading <- struct{}{}
	defer func() { <-a.reading }()
	for _, path := range []string{"/v1/status", "/v1/disks/t1", "/v1/trash", "/v1/guards"} {
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		if code := serve(ctx, h, path); code != http.StatusServiceUnavailable {
			t.Errorf("%s answered %d while another answer read the catalog", path, code)
		}
		cancel()
	}
	if code := serve(t.Context(), h, "/v1/disks"); code != http.StatusOK {
		t.Errorf("/v1/disks: %d", code)
	}
}

// The answers that check the disks' mounts, /v1/disks and /v1/disks/{id}, check them one at a time: each reads the
// mount table and every disk's device, and a client that asks again and again adds no checks at once (specs.md §25.1).
func TestTheAPIChecksTheMountsOneAnswerAtATime(t *testing.T) {
	_, c := apiPool(t, "")
	a := newTestAPI(t, c)
	m := &slowMounts{Machine: a.machine, entered: make(chan struct{}, 64), release: make(chan struct{})}
	a.machine = m
	h := a.routes()
	codes := make(chan int, 4)
	for _, path := range []string{"/v1/disks", "/v1/disks", "/v1/disks/t1", "/v1/disks"} {
		go func() { codes <- serve(t.Context(), h, path) }()
	}
	<-m.entered
	select {
	case <-m.entered:
		t.Error("two mount checks at once")
	case <-time.After(200 * time.Millisecond):
	}
	close(m.release)
	for range 4 {
		if code := <-codes; code != http.StatusOK {
			t.Errorf("%d", code)
		}
	}
	if most := m.most.Load(); most != 1 {
		t.Errorf("%d mount checks at once", most)
	}
}

// The unit closes the catalog's folder to other accounts. The catalog lists every path of the library, its trash and
// its history: what the local API gives only through its socket, of mode 0660, or with its token (specs.md §25.1).
func TestTheUnitClosesTheCatalogsFolder(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "deploy", "atoll.service"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	for _, want := range []string{"StateDirectory=atoll", "StateDirectoryMode=0750"} {
		if !slices.Contains(lines, want) {
			t.Errorf("deploy/atoll.service has no line %q", want)
		}
	}
}

// No unit of deploy/ gives its service a mount namespace of its own: in one, what the service mounts stays its own,
// and the host never sees it, whatever MountFlags says (systemd.exec(5)). The daemon's remount after a USB reset then
// mounted the disk again for Atoll alone, mergerfs and the media servers keeping the dead mount, and atoll view's
// mount would reach no one (specs.md §15.13, §21.2). Each of these settings makes such a namespace. ProtectClock and
// ProtectHostname make none, nor a device policy: measured with systemd 252 and 257, and in qemu, where
// test/loopdev/units_test.go checks the mount namespace of each unit's process.
func TestNoUnitKeepsItsMountsToItself(t *testing.T) {
	units, err := filepath.Glob(filepath.Join("..", "..", "deploy", "*.service"))
	if err != nil || len(units) == 0 {
		t.Fatalf("units %v, %v", units, err)
	}
	namespacing := []string{"PrivateTmp", "PrivateMounts", "PrivateDevices", "ProtectHome", "ProtectSystem", "ProtectKernelTunables",
		"ProtectKernelModules", "ProtectKernelLogs", "ProtectControlGroups", "ProtectProc", "ProcSubset", "ReadOnlyPaths",
		"ReadWritePaths", "InaccessiblePaths", "ExecPaths", "NoExecPaths", "TemporaryFileSystem", "BindPaths", "BindReadOnlyPaths",
		"RootDirectory", "RootImage", "MountFlags", "MountAPIVFS", "PrivateNetwork", "PrivateIPC", "PrivateUsers", "NetworkNamespacePath"}
	for _, unit := range units {
		data, err := os.ReadFile(unit)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			key, _, ok := strings.Cut(strings.TrimSpace(line), "=")
			if ok && slices.Contains(namespacing, key) {
				t.Errorf("%s: %q gives the service a mount namespace of its own", filepath.Base(unit), line)
			}
		}
	}
}

// Each unit of deploy/ keeps the hardening measured in qemu, where test/loopdev/units_test.go starts them as they are
// (docs/threat-model.md, G18): its filter of calls, its sockets, its namespaces and its mask. Neither gives its account
// a supplementary group, which the capabilities make useless, nor refuses setuid and setgid, which a copy, and a
// folder made 2775, take with the rest of their mode (specs.md §14), nor sets a device policy, which would refuse a disk
// that comes back under another number.
func TestTheUnitsAreHardened(t *testing.T) {
	units, err := filepath.Glob(filepath.Join("..", "..", "deploy", "*.service"))
	if err != nil || len(units) == 0 {
		t.Fatalf("units %v, %v", units, err)
	}
	want := []string{"NoNewPrivileges=yes", "SystemCallArchitectures=native", "SystemCallErrorNumber=EPERM",
		"RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK", "RestrictNamespaces=yes", "LockPersonality=yes",
		"MemoryDenyWriteExecute=yes", "RestrictRealtime=yes", "ProtectClock=yes", "ProtectHostname=yes", "UMask=0027"}
	refused := []string{"SupplementaryGroups", "RestrictSUIDSGID", "DevicePolicy", "DeviceAllow"}
	for _, unit := range units {
		data, err := os.ReadFile(unit)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(data), "\n")
		for _, line := range want {
			if !slices.Contains(lines, line) {
				t.Errorf("%s has no line %q", filepath.Base(unit), line)
			}
		}
		filters := 0
		for _, line := range lines {
			key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
			switch {
			case !ok:
			case slices.Contains(refused, key):
				t.Errorf("%s: %q", filepath.Base(unit), line)
			case key == "SystemCallFilter":
				filters++
				if !strings.HasPrefix(value, "@system-service @mount") {
					t.Errorf("%s: %q, want systemd's set for services and @mount first", filepath.Base(unit), line)
				}
			}
		}
		if filters != 1 {
			t.Errorf("%s has %d lines SystemCallFilter=, want 1", filepath.Base(unit), filters)
		}
	}
}

// Each unit of deploy/ sets the PATH its service finds smartctl, mount, umount and fusermount3 on: the system's own
// folders, which root alone writes and dpkg fills, whatever systemd's default or a DefaultEnvironment= says (specs.md
// §26; docs/threat-model.md, G19). /usr/local is not on it: a binary there, as the staff group of an older Debian
// could write, would run with the service's capabilities. test/loopdev/units_test.go reads the PATH of each running
// service in qemu.
func TestTheUnitsSetTheirPath(t *testing.T) {
	units, err := filepath.Glob(filepath.Join("..", "..", "deploy", "*.service"))
	if err != nil || len(units) == 0 {
		t.Fatalf("units %v, %v", units, err)
	}
	for _, unit := range units {
		data, err := os.ReadFile(unit)
		if err != nil {
			t.Fatal(err)
		}
		var paths []string
		for _, line := range strings.Split(string(data), "\n") {
			if key, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok && key == "Environment" && strings.HasPrefix(value, "PATH=") {
				paths = append(paths, value)
			}
		}
		if want := []string{"PATH=" + unitPath}; !slices.Equal(paths, want) {
			t.Errorf("%s sets %q, want %q", filepath.Base(unit), paths, want)
		}
	}
}

// unitPath is the PATH deploy/'s units set.
const unitPath = "/usr/sbin:/usr/bin:/sbin:/bin"

// deploy/50-atoll.rules lets the account atoll start a mount unit, and nothing else: one action, one account, the verb
// start alone, a unit whose name ends in .mount (specs.md §15.13). The .deb installs it where polkitd reads it, and the
// archive holds it. Measured with polkitd 122 (Debian 12) and 126 (Debian 13) through pkcheck, and in qemu by the
// daemon's own remount (test/loopdev/units_test.go, TestTheRemountUnderSystemd).
func TestThePolkitRuleLetsAtollStartAMountUnitAlone(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "deploy", "50-atoll.rules"))
	if err != nil {
		t.Fatal(err)
	}
	rule := string(data)
	var code []string
	for _, line := range strings.Split(rule, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "//") {
			code = append(code, line)
		}
	}
	program := strings.Join(code, "\n")
	for _, want := range []string{`action.id === "org.freedesktop.systemd1.manage-units"`, `subject.user === "atoll"`,
		`action.lookup("verb") === "start"`, `\.mount$/.test(action.lookup("unit") || "")`, "return polkit.Result.YES;",
		"return polkit.Result.NOT_HANDLED;"} {
		if !strings.Contains(program, want) {
			t.Errorf("deploy/50-atoll.rules has no %s", want)
		}
	}
	if n := strings.Count(program, "polkit.Result.YES"); n != 1 {
		t.Errorf("deploy/50-atoll.rules says yes %d times", n)
	}
	if n := strings.Count(program, "action.lookup(\"verb\")"); n != 1 {
		t.Errorf("deploy/50-atoll.rules reads the verb %d times", n)
	}
	release, err := os.ReadFile(filepath.Join("..", "..", ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(release), "      - src: deploy/50-atoll.rules\n        dst: /usr/share/polkit-1/rules.d/50-atoll.rules\n") {
		t.Error(".goreleaser.yaml does not install deploy/50-atoll.rules in /usr/share/polkit-1/rules.d/")
	}
	if strings.Count(string(release), "- src: deploy/50-atoll.rules\n") != 2 {
		t.Error(".goreleaser.yaml does not put deploy/50-atoll.rules in both the archive and the package")
	}
}

// serve asks a handler for a path, with a request of that context, and gives the status of its answer.
func serve(ctx context.Context, h http.Handler, path string) int {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil))
	return w.Code
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
	// /metrics is no command's: it is what the daemon recorded (specs.md §23.1).
	a := newTestAPI(t, c)
	served := slices.Sorted(maps.Keys(a.answers()))
	served = append(served, slices.Collect(maps.Keys(a.writes()))...)
	served = append(served, "/metrics")
	slices.Sort(served)
	slices.Sort(listed)
	if !slices.Equal(listed, served) {
		t.Errorf("openapi.yaml lists %v, the server answers %v", listed, served)
	}
}

// /metrics gives what the daemon recorded, as Prometheus reads it, on the socket with no token and over TCP with it,
// GET alone (specs.md §23.1).
func TestTheAPIGivesTheMetrics(t *testing.T) {
	token := filepath.Join(t.TempDir(), "api.token")
	if err := os.WriteFile(token, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, c := apiPool(t, "[api]\nlisten = \"127.0.0.1:7341\"\ntoken_file = "+strconv.Quote(token)+"\n")
	a := newTestAPI(t, c)
	a.metrics.Update(func(s *metrics.Snapshot) { s.Passes = 3 })
	want, err := a.metrics.Text()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(want), "atoll_passes_total 3\n") || !strings.Contains(string(want), `atoll_disk_state{disk="t1",state="online"} 1`) {
		t.Fatalf("the recorder gives\n%s", want)
	}
	socket, tcp := a.routes(), a.withToken(a.routes())
	for name, w := range map[string]*httptest.ResponseRecorder{
		"the socket": ask(socket, http.MethodGet, "/metrics", ""), "TCP with the token": ask(tcp, http.MethodGet, "/metrics", "Bearer s3cret"),
	} {
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" || w.Body.String() != string(want) {
			t.Errorf("%s: %d %q\n%s", name, w.Code, w.Header().Get("Content-Type"), w.Body.String())
		}
	}
	if w := ask(tcp, http.MethodGet, "/metrics", ""); w.Code != http.StatusUnauthorized || w.Body.Len() != 0 {
		t.Errorf("TCP with no token: %d %q", w.Code, w.Body.String())
	}
	if w := ask(socket, http.MethodPost, "/metrics", ""); w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodGet {
		t.Errorf("POST: %d", w.Code)
	}
	// What the daemon records next is in the next answer.
	a.metrics.Update(func(s *metrics.Snapshot) { s.Passes = 4 })
	if w := ask(socket, http.MethodGet, "/metrics", ""); !strings.Contains(w.Body.String(), "atoll_passes_total 4\n") {
		t.Errorf("after another pass:\n%s", w.Body.String())
	}
}

// metrics.listen gives /metrics with no token, and nothing else: no answer of /v1 goes out with no token (specs.md
// §23.1).
func TestMetricsListenGivesTheMetricsAlone(t *testing.T) {
	ports := make([]string, 2)
	for i := range ports {
		free, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ports[i] = strconv.Itoa(free.Addr().(*net.TCPAddr).Port)
		_ = free.Close()
	}
	token := filepath.Join(t.TempDir(), "api.token")
	if err := os.WriteFile(token, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	api, open := "127.0.0.1:"+ports[0], "127.0.0.1:"+ports[1]
	_, c := apiPool(t, "[api]\nsocket = \"\"\nlisten = "+strconv.Quote(api)+"\ntoken_file = "+strconv.Quote(token)+"\n[metrics]\nlisten = "+strconv.Quote(open)+"\n")
	stop := newTestAPI(t, c).start(t.Context(), c)
	defer stop()
	client := &http.Client{}
	for _, tc := range []struct {
		url, token string
		code       int
	}{
		{"http://" + open + "/metrics", "", http.StatusOK},
		{"http://" + open + "/v1/health", "", http.StatusNotFound},
		{"http://" + open + "/v1/status", "s3cret", http.StatusNotFound},
		{"http://" + api + "/metrics", "", http.StatusUnauthorized},
		{"http://" + api + "/metrics", "s3cret", http.StatusOK},
	} {
		if code := getHealth(t, client, tc.url, tc.token); code != tc.code {
			t.Errorf("%s with token %q: %d, want %d", tc.url, tc.token, code, tc.code)
		}
	}
}

// An address of 100.64.0.0/10, which carrier-grade NAT uses too, is taken as Tailscale's by its range alone: at the
// start, the daemon's log warns when it is on no interface whose name begins with tailscale, and the API listens all
// the same, as headscale or another setup may name it otherwise (specs.md §25.1).
func TestAnAddressOfCarrierGradeNATIsDoubted(t *testing.T) {
	ts, other := netip.MustParseAddr("100.101.102.103"), netip.MustParseAddr("100.101.102.104")
	lan, lo := netip.MustParseAddr("192.168.1.2"), netip.MustParseAddr("127.0.0.1")
	for _, tc := range []struct {
		listen string
		ifaces []netInterface
		want   string
	}{
		{"127.0.0.1:7341", []netInterface{{"wwan0", []netip.Addr{lo}}}, ""},
		{"[fd7a:115c:a1e0::1]:7341", nil, ""},
		{"100.101.102.103:7341", []netInterface{{"lo", []netip.Addr{lo}}, {"tailscale0", []netip.Addr{ts}}}, ""},
		{"100.101.102.103:7341", []netInterface{{"wwan0", []netip.Addr{other}}, {"tailscale0", []netip.Addr{lan, ts}}}, ""},
		{"100.64.0.0:7341", []netInterface{{"tailscale", []netip.Addr{netip.MustParseAddr("100.64.0.0")}}}, ""},
		{"100.101.102.103:7341", []netInterface{{"wwan0", []netip.Addr{ts}}}, "it is on wwan0, and on no interface whose name begins with tailscale"},
		{"100.101.102.103:7341", []netInterface{{"ts0", []netip.Addr{ts}}, {"wg0", []netip.Addr{ts}}}, "it is on ts0, wg0, and on no interface whose name begins with tailscale"},
		{"100.101.102.103:7341", []netInterface{{"Tailscale0", []netip.Addr{ts}}}, "it is on Tailscale0, and on no interface"},
		{"100.101.102.103:7341", []netInterface{{"tailscale0", []netip.Addr{other}}}, "it is on no interface of the machine"},
		{"100.127.255.255:7341", nil, "it is on no interface of the machine"},
	} {
		at, err := value.ParseListen(tc.listen)
		if err != nil {
			t.Fatal(err)
		}
		if got := carrierNATDoubt(at, tc.ifaces); !strings.HasPrefix(got, tc.want) || (tc.want == "") != (got == "") {
			t.Errorf("%s on %v: %q, want %q", tc.listen, tc.ifaces, got, tc.want)
		}
	}
	var log bytes.Buffer
	at, err := value.ParseListen("100.101.102.103:7341")
	if err != nil {
		t.Fatal(err)
	}
	warnCarrierNAT(slog.New(slog.NewTextHandler(&log, nil)), "api.listen", at, func() ([]netInterface, error) {
		return []netInterface{{"eth0", []netip.Addr{ts}}}, nil
	})
	if !strings.Contains(log.String(), "level=WARN") || !strings.Contains(log.String(), "carrier-grade NAT") || !strings.Contains(log.String(), "key=api.listen") ||
		!strings.Contains(log.String(), "eth0") {
		t.Errorf("the log says:\n%s", log.String())
	}
	log.Reset()
	warnCarrierNAT(slog.New(slog.NewTextHandler(&log, nil)), "metrics.listen", at, func() ([]netInterface, error) {
		return nil, errors.New("no netlink")
	})
	if !strings.Contains(log.String(), "level=WARN") || !strings.Contains(log.String(), "no netlink") {
		t.Errorf("the log says:\n%s", log.String())
	}
	log.Reset()
	loopback, err := value.ParseListen("127.0.0.1:7341")
	if err != nil {
		t.Fatal(err)
	}
	warnCarrierNAT(slog.New(slog.NewTextHandler(&log, nil)), "api.listen", loopback, func() ([]netInterface, error) {
		t.Error("the interfaces were listed for a loopback address")
		return nil, nil
	})
	if log.Len() != 0 {
		t.Errorf("a loopback address: %s", log.String())
	}
	// The machine's own interfaces, as the daemon lists them: the loopback one holds 127.0.0.1.
	ifaces, err := machineInterfaces()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(ifaces, func(i netInterface) bool { return slices.Contains(i.addrs, lo) }) {
		t.Errorf("no interface holds 127.0.0.1: %v", ifaces)
	}
}

// metrics.listen answers a request whose Host names its address, and 421 for another, so that a page a browser
// loaded from a name that DNS rebinding points at the address reads nothing; api.listen, which asks for its token,
// answers any Host (specs.md §23.1, conformance/vectors/listen_host.json).
func TestTheMetricsAnswerOnlyTheirAddress(t *testing.T) {
	ports := make([]string, 2)
	for i := range ports {
		free, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ports[i] = strconv.Itoa(free.Addr().(*net.TCPAddr).Port)
		_ = free.Close()
	}
	token := filepath.Join(t.TempDir(), "api.token")
	if err := os.WriteFile(token, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	api, open := "127.0.0.1:"+ports[0], "127.0.0.1:"+ports[1]
	_, c := apiPool(t, "[api]\nsocket = \"\"\nlisten = "+strconv.Quote(api)+"\ntoken_file = "+strconv.Quote(token)+"\n[metrics]\nlisten = "+strconv.Quote(open)+"\n")
	stop := newTestAPI(t, c).start(t.Context(), c)
	defer stop()
	askAs := func(at, host, token string) (int, string) {
		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+at+"/metrics", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Host = host
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	for _, host := range []string{open, "localhost:" + ports[1], "LOCALHOST:" + ports[1], "[::1]:" + ports[1]} {
		if code, body := askAs(open, host, ""); code != http.StatusOK || !strings.Contains(body, "atoll_disk_info") {
			t.Errorf("Host %q: %d %s", host, code, body)
		}
	}
	for _, host := range []string{"rebind.example:" + ports[1], "rebind.example", "127.0.0.1", "127.0.0.1:" + ports[0], "localhost", "127.0.0.2:" + ports[1]} {
		code, body := askAs(open, host, "")
		if code != http.StatusMisdirectedRequest || !strings.Contains(body, `"error"`) || strings.Contains(body, "atoll_") || strings.Contains(body, ports[1]) {
			t.Errorf("Host %q: %d %s", host, code, body)
		}
	}
	if code, _ := askAs(api, "rebind.example:"+ports[0], "s3cret"); code != http.StatusOK {
		t.Errorf("api.listen with its token and another Host: %d", code)
	}
}
