package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/peondusud/atoll/internal/daemon"
	"github.com/peondusud/atoll/internal/jobs"
)

// What the body of an answer to a guard may hold: a few fields (specs.md §25.1).
const apiMaxBody = 4 << 10

// upToLayout is how an output writes a time (specs.md §29.3), and so how up_to is given back.
const upToLayout = "2006-01-02T15:04:05.000000000Z"

// answerGuard is POST /v1/guards/{id}, the one write of the local API: an answer to a guard of a disk, made by the code
// of atoll guard confirm and reject, under the catalog's lock, with the events they write (specs.md §15.6, §25.1). As
// every command that lets a deletion go (invariant 17), it says what it would do with "dry_run": true, and does it
// only with "yes": true; it refuses, and writes nothing, otherwise.
func (a *apiServer) answerGuard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		send(w, http.StatusMethodNotAllowed, apiError{"only POST"})
		return
	}
	req, status, err := readGuardBody(w, r)
	if err != nil {
		send(w, status, apiError{err.Error()})
		return
	}
	req.disk = r.PathValue("id")
	c := a.current.Load()
	if !hasDisk(c, req.disk) {
		send(w, http.StatusNotFound, apiError{"no such disk"})
		return
	}
	if !req.dryRun {
		if !a.apply {
			send(w, http.StatusConflict, apiError{"the daemon runs with --dry-run, and records no answer: atoll guard " + req.what + " records it"})
			return
		}
		if why := a.tokenRefusal(r.Context()); why != "" {
			a.log.Warn("the local API refused an answer to a guard: another account could set its token", "disk", req.disk, "why", why, "from", r.RemoteAddr)
			send(w, http.StatusForbidden, apiError{why})
			return
		}
	}
	if a.onCatalog == nil {
		send(w, http.StatusServiceUnavailable, apiError{"this server takes no answer"})
		return
	}
	// An answer has its 60 s, as a question has: one the daemon could not take by then was not given.
	ctx, cancel := context.WithTimeout(r.Context(), apiWriteTimeout)
	defer cancel()
	var answer *guardAnswer
	err = a.onCatalog(ctx, func(ctx context.Context, db *sql.DB) error {
		var err error
		answer, err = answerGuardOn(ctx, db, c, req, a.machine.Now())
		return err
	})
	switch {
	case errors.Is(err, errGuardMoved):
		send(w, http.StatusConflict, apiError{err.Error()})
		return
	case err != nil:
		a.log.Warn("the local API could not answer a guard", "disk", req.disk, "kind", req.kind, "answer", req.what, "error", err)
		send(w, http.StatusServiceUnavailable, apiError{err.Error()})
		return
	}
	if answer.Done {
		a.log.Info("the local API answered a guard", "disk", req.disk, "kind", req.kind, "answer", req.what, "up_to", *answer.UpTo, "from", r.RemoteAddr)
		if a.told != nil {
			a.told()
		}
	}
	var body bytes.Buffer
	if err := writeJSON(&body, answer); err != nil {
		send(w, http.StatusServiceUnavailable, apiError{err.Error()})
		return
	}
	w.Header().Set("Content-Type", jsonType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body.Bytes())
}

// readGuardBody reads the body of POST /v1/guards/{id}: one JSON object of the fields api/openapi.yaml names, each at
// most once and written as there, of application/json, of apiMaxBody bytes at most. It gives the status of a body it
// refuses: 415, 413, or 400.
func readGuardBody(w http.ResponseWriter, r *http.Request) (*guardRequest, int, error) {
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != jsonType {
		return nil, http.StatusUnsupportedMediaType, errors.New("the body of an answer is application/json")
	}
	var answer, kind string
	var dryRun, yes bool
	var upTo *string
	targets := map[string]any{"answer": &answer, "kind": &kind, "dry_run": &dryRun, "yes": &yes, "up_to": &upTo}
	if err := readObject(http.MaxBytesReader(w, r.Body, apiMaxBody), targets); err != nil {
		if tooLarge := (*http.MaxBytesError)(nil); errors.As(err, &tooLarge) {
			return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("the body of an answer holds %d bytes at most", apiMaxBody)
		}
		return nil, http.StatusBadRequest, err
	}
	req := &guardRequest{what: answer, kind: kind, dryRun: dryRun}
	switch answer {
	case "confirm", "reject":
	case "":
		return nil, http.StatusBadRequest, errors.New(`want "answer": "confirm" or "reject"`)
	default:
		return nil, http.StatusBadRequest, fmt.Errorf(`"answer" is "confirm" or "reject", not %q`, answer)
	}
	switch kind {
	case "":
		req.kind = jobs.GuardDeletions
	case jobs.GuardDeletions, jobs.GuardModifications:
	default:
		return nil, http.StatusBadRequest, fmt.Errorf(`"kind" is "deletions" or "modifications", not %q`, kind)
	}
	switch {
	case dryRun && yes:
		return nil, http.StatusBadRequest, errors.New(`"dry_run" and "yes" go one without the other, as --dry-run and --yes do`)
	case !dryRun && !yes:
		return nil, http.StatusBadRequest, errors.New(`nothing is answered without "yes": true; "dry_run": true says what the answer would be about`)
	}
	if upTo != nil {
		at, err := time.Parse(upToLayout, *upTo)
		if err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf(`"up_to" is a time as the answer gives it, such as "2026-01-01T00:00:00.000000000Z", not %q`, *upTo)
		}
		req.upTo = sql.NullInt64{Int64: at.UnixNano(), Valid: true}
	}
	return req, 0, nil
}

// readObject reads one JSON object whose fields are those of targets, each decoded into its target, with nothing after
// it. A field targets does not name, one of another case, and one given twice are errors: a request that could be read
// two ways is not read at all.
func readObject(body io.Reader, targets map[string]any) error {
	dec := json.NewDecoder(body)
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return bodyError("want one JSON object", err)
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return bodyError("a field", err)
		}
		name, _ := tok.(string)
		target, ok := targets[name]
		switch {
		case !ok:
			return fmt.Errorf("unknown field %q", name)
		case seen[name]:
			return fmt.Errorf("field %q given twice", name)
		}
		seen[name] = true
		if err := dec.Decode(target); err != nil {
			return bodyError(fmt.Sprintf("field %q", name), err)
		}
	}
	if _, err := dec.Token(); err != nil {
		return bodyError("the end of the object", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return bodyError("something after the object", err)
	}
	return nil
}

// bodyError says what is wrong with a body, keeping a body too large for what it is.
func bodyError(what string, err error) error {
	if err == nil {
		return errors.New(what)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// answerThrough makes the API's answers to a guard go through the daemon's loop, which runs each on the catalog, under
// its lock, between two steps (specs.md §25.1), and tells the loop of one recorded as SIGHUP would, so that its next
// pass takes it at once. A daemon in --dry-run records none.
func (a *apiServer) answerThrough(work chan<- *daemon.CatalogWork, reload chan<- struct{}, apply bool) {
	a.apply = apply
	a.onCatalog = func(ctx context.Context, do func(ctx context.Context, db *sql.DB) error) error {
		return daemon.Hand(ctx, work, do)
	}
	a.told = func() {
		select {
		case reload <- struct{}{}:
		default:
		}
	}
}
