package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/disks"
	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/value"
)

// What a request may take (specs.md §25.1).
const (
	apiHeaderTimeout = 10 * time.Second
	apiWriteTimeout  = 60 * time.Second
	apiIdleTimeout   = 60 * time.Second
	apiMaxHeader     = 16 << 10
	apiMaxToken      = 4 << 10
)

// apiServer answers the local API of the daemon that runs for good, with the configuration it runs with (specs.md
// §25.1). It only answers: nothing it does writes.
type apiServer struct {
	current atomic.Pointer[config.Config]
	machine disks.Machine
	log     *slog.Logger
	// reading lets one answer at a time read the catalog: each is made in the daemon's own process, beside its passes,
	// and one may plan the whole pool.
	reading chan struct{}
}

func newAPIServer(c *config.Config, m disks.Machine, log *slog.Logger) *apiServer {
	a := &apiServer{machine: m, log: log, reading: make(chan struct{}, 1)}
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
		"/v1/disks": func(r *http.Request, c *config.Config, w io.Writer) (int, error) {
			verdicts, err := disks.CheckAll(r.Context(), a.machine, c.Disks)
			if err != nil {
				return 0, fmt.Errorf("%w", err)
			}
			return http.StatusOK, writeJSON(w, diskListFrom(c.Disks, verdicts))
		},
		"/v1/disks/{id}": func(r *http.Request, c *config.Config, w io.Writer) (int, error) {
			at := slices.IndexFunc(c.Disks, func(d config.Disk) bool { return d.ID == r.PathValue("id") })
			if at < 0 {
				return http.StatusNotFound, writeJSON(w, apiError{"no such disk"})
			}
			out, err := showDisk(r.Context(), c, a.machine, &c.Disks[at])
			if err != nil {
				return 0, err
			}
			return http.StatusOK, writeJSON(w, out)
		},
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
	return func(r *http.Request, c *config.Config, w io.Writer) (int, error) {
		select {
		case a.reading <- struct{}{}:
			defer func() { <-a.reading }()
		case <-r.Context().Done():
			return 0, fmt.Errorf("waiting for another answer: %w", r.Context().Err())
		}
		return answer(r, c, w)
	}
}

// routes serves each answer, and 404 for any other path.
func (a *apiServer) routes() http.Handler {
	mux := http.NewServeMux()
	for path, answer := range a.answers() {
		mux.HandleFunc(path, a.get(answer))
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		send(w, http.StatusNotFound, apiError{"no such path"})
	})
	return mux
}

// apiError is what an answer that could not be made says.
type apiError struct {
	Error string `json:"error"`
}

// get answers GET alone. The answer is made whole before it is sent, so that one that fails halfway is an error, not a
// cut document.
func (a *apiServer) get(answer apiAnswer) http.HandlerFunc {
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
		w.Header().Set("Content-Type", "application/json")
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
// effect at once (specs.md §25.1). Without it, or with another, the answer is 401 with nothing more.
func (a *apiServer) withToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		file, _ := a.current.Load().Effective()["api.token_file"].(string)
		want, err := readToken(file)
		got, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if err != nil || want == "" || !bearer || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			if err != nil {
				a.log.Warn("the local API's token cannot be read: every request over TCP is refused", "file", file, "error", err)
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="atoll"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// readToken reads a token file, its last line break left out.
func readToken(file string) (string, error) {
	if file == "" {
		return "", errors.New("no token file")
	}
	data, err := fsx.ReadSmallFile(file, apiMaxToken)
	if err != nil {
		return "", fmt.Errorf("token: %w", err)
	}
	return strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r"), nil
}

// httpServer is a server with the limits of specs.md §25.1.
func httpServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler: h, ReadHeaderTimeout: apiHeaderTimeout, WriteTimeout: apiWriteTimeout, IdleTimeout: apiIdleTimeout, MaxHeaderBytes: apiMaxHeader,
	}
}

// start opens what the configuration asks for, the socket and TCP, and gives what stops them. One that cannot open is
// off, with a line of the log: answering questions is no reason to stop the daemon.
func (a *apiServer) start(ctx context.Context, c *config.Config) func() {
	var servers []*http.Server
	serve := func(s *http.Server, l net.Listener) {
		servers = append(servers, s)
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
			serve(httpServer(a.routes()), l)
		}
	}
	if text, _ := c.Effective()["api.listen"].(string); text != "" {
		l, err := listenTCP(ctx, text)
		if err != nil {
			a.log.Warn("the local API over TCP is off", "listen", text, "error", err)
		} else {
			a.log.Info("the local API listens", "tcp", l.Addr().String())
			serve(httpServer(a.withToken(a.routes())), l)
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

// listenTCP listens on api.listen, which the configuration checked is a loopback address or one of the Tailscale
// network (specs.md §29.5).
func listenTCP(ctx context.Context, text string) (net.Listener, error) {
	at, err := value.ParseListen(text)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", at.String())
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	return l, nil
}

// atollVersion is the version the binary was built as, "(devel)" for a build from a working tree.
func atollVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "unknown"
}
