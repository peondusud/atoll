package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/metrics"
	"github.com/peondusud/atoll/internal/value"
)

// What a request may take (specs.md §25.1): its headers, then the whole request, its body included, so that a client
// that sends slowly holds no handler, nor its connection, for long.
const (
	apiHeaderTimeout = 10 * time.Second
	apiReadTimeout   = 15 * time.Second
	apiWriteTimeout  = 60 * time.Second
	apiIdleTimeout   = 60 * time.Second
	apiMaxHeader     = 16 << 10
	apiMaxToken      = 4 << 10
	// apiRefusedWait is how long a request over TCP that brings no token, or another, waits for its 401, so that a
	// token is not guessed at the speed of the network: through the apiMaxConns places of api.listen, 128 tries a second
	// at most. A place is held no longer than a connection that sends nothing holds one already, apiHeaderTimeout, and
	// the wait ends with its client.
	apiRefusedWait = 250 * time.Millisecond
	// apiMinToken is the length under which api.token_file's token gets a warning (specs.md §24.4): 32 bytes, as the
	// 64 of openssl rand -hex 32 are past it.
	apiMinToken = 32
)

// apiServer answers the local API of the daemon that runs for good, with the configuration it runs with (specs.md
// §25.1). It answers questions, and takes one write: an answer to a guard, which atoll guard confirm or reject would
// give.
type apiServer struct {
	current atomic.Pointer[config.Config]
	machine disks.Machine
	log     *slog.Logger
	// metrics is what the daemon recorded for /metrics (specs.md §23.1).
	metrics *metrics.Recorder
	// reading lets one answer at a time read the catalog: each is made in the daemon's own process, beside its passes,
	// and one may plan the whole pool.
	reading chan struct{}
	// checking lets one answer at a time check the disks' mounts: each reads the mount table and each disk's device.
	checking chan struct{}
	// unreadable says when the log last told of a token file that cannot be read, and how many requests it refused
	// since: once a minute at most, as a client with no token could fill the log otherwise.
	unreadable onceAMinute
	// onCatalog runs an answer to a guard on the catalog, under its lock: in the daemon, its loop does, between two
	// steps (answerThrough). Nil takes no answer.
	onCatalog func(ctx context.Context, do func(ctx context.Context, db *sql.DB) error) error
	// apply is false for a daemon in --dry-run, which records no answer.
	apply bool
	// told tells the daemon of an answer recorded, as SIGHUP would, so that its pass takes it at once.
	told func()
	// uid and gid are the account and the group the daemon runs as: with root's, those whose token file an answer
	// over TCP takes (tokenDoubts).
	uid, gid uint32
	// refusedWait is how long a request over TCP waits for its 401: apiRefusedWait.
	refusedWait time.Duration
}

func newAPIServer(c *config.Config, m disks.Machine, log *slog.Logger, rec *metrics.Recorder) *apiServer {
	a := &apiServer{machine: m, log: log, metrics: rec, reading: make(chan struct{}, 1), checking: make(chan struct{}, 1), refusedWait: apiRefusedWait}
	a.uid, a.gid = uint32(os.Geteuid()), uint32(os.Getegid()) //nolint:gosec // an account's number, never negative on Linux
	a.current.Store(c)
	return a
}

// apiAnswer makes the answer to one path into w, and says its status.
type apiAnswer func(r *http.Request, c *config.Config, w io.Writer) (int, error)

// answers gives what each path answers, as api/openapi.yaml lists them. Each command's own writer makes the answer,
// so that it is what the command's --json gives (specs.md §25.1).
func (a *apiServer) answers() map[string]apiAnswer {
	return map[string]apiAnswer{
		"/v1/health": func(_ *http.Request, _ *config.Config, w io.Writer) (int, error) {
			return http.StatusOK, writeJSON(w, struct {
				Version string `json:"version"`
			}{atollVersion()})
		},
		"/v1/status": a.one(func(r *http.Request, c *config.Config, w io.Writer) (int, error) {
			return http.StatusOK, writeStatus(r.Context(), c, statusArgs{asJSON: true}, w)
		}),
		"/v1/disks": a.checks(func(r *http.Request, c *config.Config, w io.Writer) (int, error) {
			verdicts, err := knownChecks(r.Context(), c, a.machine, func(err error) {
				a.log.Warn("which disks' own marker was seen cannot be read: no disk is said missing for its marker gone", "error", err)
			})
			if err != nil {
				return 0, fmt.Errorf("%w", err)
			}
			return http.StatusOK, writeJSON(w, diskListFrom(c.Disks, verdicts))
		}),
		// It reads the catalog too: it takes its turn there first, as a mount check is quick and a catalog's answer
		// may not be, and a check must not wait for one while holding its own turn.
		"/v1/disks/{id}": a.one(a.checks(func(r *http.Request, c *config.Config, w io.Writer) (int, error) {
			at := slices.IndexFunc(c.Disks, func(d config.Disk) bool { return d.ID == r.PathValue("id") })
			if at < 0 {
				return http.StatusNotFound, writeJSON(w, apiError{"no such disk"})
			}
			out, err := showDisk(r.Context(), c, a.machine, &c.Disks[at])
			if err != nil {
				return 0, err
			}
			return http.StatusOK, writeJSON(w, out)
		})),
		"/v1/trash": a.one(func(r *http.Request, c *config.Config, w io.Writer) (int, error) {
			return http.StatusOK, writeTrash(r.Context(), c, true, w)
		}),
		"/v1/guards": a.one(func(r *http.Request, c *config.Config, w io.Writer) (int, error) {
			items, err := readGuards(r.Context(), c)
			if err != nil {
				return 0, err
			}
			return http.StatusOK, writeJSON(w, struct {
				Disks []guardItem `json:"disks"`
			}{items})
		}),
	}
}

// one makes an answer that reads the catalog wait for the one before it, and give up with its request: a client that
// asks again before its answer came adds no work to the daemon (specs.md §25.1).
func (a *apiServer) one(answer apiAnswer) apiAnswer {
	return inTurn(a.reading, answer)
}

// checks makes an answer that checks the disks' mounts wait for the one before it that does, as one does for the
// catalog (specs.md §25.1).
func (a *apiServer) checks(answer apiAnswer) apiAnswer {
	return inTurn(a.checking, answer)
}

// inTurn makes an answer wait for the turn that holds one place, and give up with its request.
func inTurn(turn chan struct{}, answer apiAnswer) apiAnswer {
	return func(r *http.Request, c *config.Config, w io.Writer) (int, error) {
		select {
		case turn <- struct{}{}:
			defer func() { <-turn }()
		case <-r.Context().Done():
			return 0, fmt.Errorf("waiting for another answer: %w", r.Context().Err())
		}
		return answer(r, c, w)
	}
}

// metricsAnswer writes what the daemon recorded, as Prometheus reads it (specs.md §23.1). It reads nothing but memory.
func (a *apiServer) metricsAnswer(_ *http.Request, _ *config.Config, w io.Writer) (int, error) {
	text, err := a.metrics.Text()
	if err != nil {
		return 0, fmt.Errorf("metrics: %w", err)
	}
	_, err = w.Write(text)
	return http.StatusOK, err
}

// writes gives what each path that writes takes, as api/openapi.yaml lists them: an answer to a guard alone (specs.md
// §25.1).
func (a *apiServer) writes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{"/v1/guards/{id}": a.answerGuard}
}

// routes serves each answer, each write and /metrics, and 404 for any other path.
func (a *apiServer) routes() http.Handler {
	mux := a.metricsRoutes()
	for path, answer := range a.answers() {
		mux.HandleFunc(path, a.get(answer, jsonType))
	}
	for path, write := range a.writes() {
		mux.HandleFunc(path, write)
	}
	return mux
}

// metricsRoutes serves /metrics alone, and 404 for any other path: what metrics.listen answers, with no token.
func (a *apiServer) metricsRoutes() *http.ServeMux {
	mux := http.NewServeMux()
	if a.metrics != nil {
		mux.HandleFunc("/metrics", a.get(a.metricsAnswer, metrics.ContentType))
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		send(w, http.StatusNotFound, apiError{"no such path"})
	})
	return mux
}

// jsonType is what each answer of /v1 is.
const jsonType = "application/json"

// apiError is what an answer that could not be made says.
type apiError struct {
	Error string `json:"error"`
}

// get answers GET alone. The answer is made whole before it is sent, so that one that fails halfway is an error, not a
// cut document.
func (a *apiServer) get(answer apiAnswer, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			send(w, http.StatusMethodNotAllowed, apiError{"only GET"})
			return
		}
		// An answer has its 60 s: past them it could not be sent, and it is given up, as it is when its client is gone.
		ctx, cancel := context.WithTimeout(r.Context(), apiWriteTimeout)
		defer cancel()
		var body bytes.Buffer
		status, err := answer(r.WithContext(ctx), a.current.Load(), &body)
		if err != nil {
			a.log.Warn("the local API could not answer", "path", r.URL.Path, "error", err)
			send(w, http.StatusServiceUnavailable, apiError{err.Error()})
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = w.Write(body.Bytes())
	}
}

func send(w http.ResponseWriter, status int, v any) {
	var body bytes.Buffer
	_ = writeJSON(&body, v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body.Bytes())
}

// withToken asks every request for the token api.token_file holds, read again each time, so that a new one takes
// effect at once (specs.md §25.1). Without it, or with another, the answer is 401 with nothing more, after
// refusedWait. A request let through carries whose that file is and its mode, which an answer to a guard looks at
// (tokenRefusal).
func (a *apiServer) withToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		file, _ := a.current.Load().Effective()["api.token_file"].(string)
		want, owner, err := readToken(file)
		got, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if err != nil || want == "" || !bearer || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			if err != nil {
				if refused, due := a.unreadable.due(a.machine.Now()); due {
					a.log.Warn("the local API's token cannot be read: every request over TCP is refused", "file", file, "error", err,
						"refused", refused)
				}
			}
			// The refusal waits before it is sent, so that the client knows nothing sooner; its connection is held that
			// long, and no longer once the client is gone.
			wait := time.NewTimer(a.refusedWait)
			select {
			case <-wait.C:
			case <-r.Context().Done():
			}
			wait.Stop()
			w.Header().Set("WWW-Authenticate", `Bearer realm="atoll"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tokenFileKey{}, tokenFile{path: file, owner: owner})))
	})
}

// onceAMinute counts what happens, and says when the log may tell of it: the first time, then once a minute at most.
type onceAMinute struct {
	mu     sync.Mutex
	told   bool
	lastNS int64
	since  int
}

// due counts one more, and says whether the log may tell of it now, with how many there were since it last did,
// this one included.
func (o *onceAMinute) due(nowNS int64) (int, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.since++
	if o.told && nowNS-o.lastNS < time.Minute.Nanoseconds() {
		return 0, false
	}
	n := o.since
	o.told, o.lastNS, o.since = true, nowNS, 0
	return n, true
}

// tokenFileKey marks a request that came over TCP, with the token file it came with.
type tokenFileKey struct{}

// tokenFile is the token file a request over TCP came with: its path, and whose the file read was, with its mode.
type tokenFile struct {
	path  string
	owner fsx.Owner
}

// tokenRefusal says why an answer to a guard that came over TCP cannot be taken with the token file it came with, ""
// when it can, or when it came over the socket (specs.md §25.1): any account that could set the token could send it.
func (a *apiServer) tokenRefusal(ctx context.Context) string {
	t, overTCP := ctx.Value(tokenFileKey{}).(tokenFile)
	if !overTCP {
		return ""
	}
	folders, err := fsx.SecretFolders(t.path)
	doubts := tokenDoubts(t.owner, folders, a.uid, a.gid)
	if err != nil {
		doubts = append(doubts, "the folders on the way to api.token_file cannot be looked at: "+err.Error())
	}
	if len(doubts) == 0 {
		return ""
	}
	return "an answer over TCP takes a token file that only root or the account the daemon runs as can change, and other " +
		"accounts cannot read: " + strings.Join(doubts, "; ")
}

// tokenDoubts says why a token file cannot be taken for an answer to a guard, each reason in a fixed order, and
// nothing when it can (specs.md §25.1): the file must belong to root or to the account the daemon runs as (uid), with
// no write for its group or other accounts and no read for other accounts; and each folder that holds its name, or
// the name of a symbolic link on the way to it, must belong to root or that account, with no write for other
// accounts, sticky or not, and none for its group unless that group is root's or the daemon's own (gid). Any account
// that could write either could set the token it then sends. Its group may read it: Prometheus's, say.
func tokenDoubts(file fsx.Owner, folders []fsx.Folder, uid, gid uint32) []string {
	ours := func(id uint32) bool { return id == 0 || id == uid }
	var out []string
	if !ours(file.UID) {
		out = append(out, fmt.Sprintf("api.token_file belongs to the account %d, neither root nor the one the daemon runs as (chown root)", file.UID))
	}
	perm := file.Mode.Perm()
	if perm&0o020 != 0 {
		out = append(out, "its group may write api.token_file (chmod g-w)")
	}
	if perm&0o004 != 0 {
		out = append(out, "other accounts may read api.token_file (chmod o-r)")
	}
	if perm&0o002 != 0 {
		out = append(out, "other accounts may write api.token_file (chmod o-w)")
	}
	for _, f := range folders {
		if !ours(f.UID) {
			out = append(out, fmt.Sprintf("the folder %s belongs to the account %d, neither root nor the one the daemon runs as (chown root)", f.Path, f.UID))
		}
		if f.Mode.Perm()&0o002 != 0 {
			out = append(out, fmt.Sprintf("other accounts may write the folder %s (chmod o-w)", f.Path))
		}
		if f.Mode.Perm()&0o020 != 0 && f.GID != 0 && f.GID != gid {
			out = append(out, fmt.Sprintf("the group %d may write the folder %s, neither root's nor the daemon's (chmod g-w)", f.GID, f.Path))
		}
	}
	return out
}

// readToken reads a token file, its last line break left out, and gives whose the file it read is, with its mode.
func readToken(file string) (token string, owner fsx.Owner, err error) {
	if file == "" {
		return "", fsx.Owner{}, errors.New("no token file")
	}
	data, owner, err := fsx.ReadSecretFile(file, apiMaxToken)
	if err != nil {
		return "", fsx.Owner{}, fmt.Errorf("token: %w", err)
	}
	return strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r"), owner, nil
}

// httpServer is a server with the limits of specs.md §25.1.
func httpServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler: h, ReadHeaderTimeout: apiHeaderTimeout, ReadTimeout: apiReadTimeout, WriteTimeout: apiWriteTimeout, IdleTimeout: apiIdleTimeout,
		MaxHeaderBytes: apiMaxHeader,
	}
}

// start opens what the configuration asks for, the socket and TCP, and gives what stops them. One that cannot open is
// off, with a line of the log: answering questions is no reason to stop the daemon.
func (a *apiServer) start(ctx context.Context, c *config.Config) func() {
	var servers []*http.Server
	// Each listener holds apiMaxConns connections at once, its own: a flood over TCP takes no place of the socket's.
	serve := func(s *http.Server, l net.Listener) {
		servers = append(servers, s)
		l = capConns(l, apiMaxConns)
		go func() {
			if err := s.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				a.log.Warn("the local API stopped", "address", l.Addr().String(), "error", err)
			}
		}()
	}
	if path, _ := c.Effective()["api.socket"].(string); path != "" {
		l, err := fsx.ListenSocket(ctx, path)
		if err != nil {
			a.log.Warn("the local API's socket is off", "path", path, "error", err)
		} else {
			a.log.Info("the local API listens", "socket", path)
			s := httpServer(a.routes())
			s.ConnContext = withPeer
			serve(s, l)
		}
	}
	if text, _ := c.Effective()["api.listen"].(string); text != "" {
		l, at, err := listenTCP(ctx, text)
		if err != nil {
			a.log.Warn("the local API over TCP is off", "listen", text, "error", err)
		} else {
			a.log.Info("the local API listens", "tcp", l.Addr().String())
			warnCarrierNAT(a.log, "api.listen", at, machineInterfaces)
			serve(httpServer(a.withToken(a.routes())), l)
		}
	}
	if text, _ := c.Effective()["metrics.listen"].(string); text != "" && a.metrics != nil {
		l, at, err := listenTCP(ctx, text)
		if err != nil {
			a.log.Warn("the metrics over TCP are off", "listen", text, "error", err)
		} else {
			a.log.Info("the metrics listen, with no token", "tcp", l.Addr().String())
			warnCarrierNAT(a.log, "metrics.listen", at, machineInterfaces)
			serve(httpServer(namedOnly(at, a.metricsRoutes())), l)
		}
	}
	return func() {
		stopping, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		for _, s := range servers {
			_ = s.Shutdown(stopping)
		}
	}
}

// listenTCP listens on api.listen or metrics.listen, which the configuration checked is a loopback address or one of
// the Tailscale network (specs.md §29.5), and gives the address it read.
func listenTCP(ctx context.Context, text string) (net.Listener, value.Listen, error) {
	at, err := value.ParseListen(text)
	if err != nil {
		return nil, value.Listen{}, fmt.Errorf("%w", err)
	}
	l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", at.String())
	if err != nil {
		return nil, value.Listen{}, fmt.Errorf("%w", err)
	}
	return l, at, nil
}

// namedOnly answers a request whose Host names the address listened on, and 421 for another (specs.md §23.1): what
// metrics.listen gives with no token must not go to a page a browser loaded from another name, which DNS rebinding
// then points at the address, as the browser takes that page and the metrics for one origin. The answer does not say
// the address, which such a page would read.
func namedOnly(at value.Listen, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !at.NamedBy(r.Host) {
			send(w, http.StatusMisdirectedRequest, apiError{"this server answers a request that names its own address, or localhost for a loopback one, with its port"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// peerKey marks a request that came over the socket, with who connected (fsx.Peer).
type peerKey struct{}

// withPeer gives each request of a connection to the socket who connected, as the kernel recorded it, so that the log
// of an answer to a guard says which account and process gave it (specs.md §25.1). A connection whose peer the kernel
// does not say is given nothing.
func withPeer(ctx context.Context, c net.Conn) context.Context {
	if p, ok := fsx.PeerOf(c); ok {
		return context.WithValue(ctx, peerKey{}, p)
	}
	return ctx
}

// netInterface is a network interface of the machine: its name and the addresses it holds.
type netInterface struct {
	name  string
	addrs []netip.Addr
}

// machineInterfaces lists the machine's network interfaces and their addresses, an IPv4 address mapped in IPv6 read
// as IPv4.
func machineInterfaces() ([]netInterface, error) {
	list, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("the network interfaces: %w", err)
	}
	out := make([]netInterface, 0, len(list))
	for _, i := range list {
		addrs, err := i.Addrs()
		if err != nil {
			return nil, fmt.Errorf("the addresses of %s: %w", i.Name, err)
		}
		item := netInterface{name: i.Name}
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				if ip, ok := netip.AddrFromSlice(n.IP); ok {
					item.addrs = append(item.addrs, ip.Unmap())
				}
			}
		}
		out = append(out, item)
	}
	return out, nil
}

// inCarrierNAT says whether an address is in 100.64.0.0/10, which is Tailscale's and carrier-grade NAT's (RFC 6598).
func inCarrierNAT(at value.Listen) bool {
	return !at.IsIPv6 && at.IPv4[0] == 100 && at.IPv4[1]&0xc0 == 64
}

// carrierNATDoubt says why an address of 100.64.0.0/10 may not be Tailscale's, "" when it is on an interface whose
// name begins with tailscale, and for an address outside that range (specs.md §25.1): the range is carrier-grade NAT's
// too, which a provider's network may give the machine, and the configuration takes it as Tailscale's by the range
// alone. Another name does not mean it is not: headscale or another setup may name its interface otherwise.
func carrierNATDoubt(at value.Listen, ifaces []netInterface) string {
	if !inCarrierNAT(at) {
		return ""
	}
	want := netip.AddrFrom4(at.IPv4)
	var on []string
	for _, i := range ifaces {
		if !slices.Contains(i.addrs, want) {
			continue
		}
		if strings.HasPrefix(i.name, "tailscale") {
			return ""
		}
		on = append(on, i.name)
	}
	if len(on) == 0 {
		return "it is on no interface of the machine"
	}
	return "it is on " + strings.Join(on, ", ") + ", and on no interface whose name begins with tailscale"
}

// warnCarrierNAT writes to the log, at the start, that an address of 100.64.0.0/10 that key listens on may be
// carrier-grade NAT's rather than Tailscale's (carrierNATDoubt), as a network of the provider's would then reach it.
// It listens all the same.
func warnCarrierNAT(log *slog.Logger, key string, at value.Listen, interfaces func() ([]netInterface, error)) {
	if !inCarrierNAT(at) {
		return
	}
	ifaces, err := interfaces()
	why := carrierNATDoubt(at, ifaces)
	if err != nil {
		why = "the interfaces that hold it cannot be listed: " + err.Error()
	}
	if why != "" {
		log.Warn("the address is in 100.64.0.0/10, which Atoll takes as Tailscale's, though carrier-grade NAT uses it too: it listens all the same",
			"key", key, "listen", at.String(), "why", why)
	}
}

// atollVersion is the version the binary was built as: the tag of its commit, which go build reads from git, as a
// release has it; the pseudo-version of that commit otherwise; "(devel)" for a build with no version control.
func atollVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "unknown"
}
