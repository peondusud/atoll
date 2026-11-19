package telemetry

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/vectors"
)

// typed is a value of a vector: one of its fields says which.
type typed struct {
	Key        string   `json:"key"`
	String     *string  `json:"string"`
	StringB64  *string  `json:"string_b64"`
	Int        *int64   `json:"int"`
	Float      *string  `json:"float"`
	Bool       *bool    `json:"bool"`
	Strings    []string `json:"strings"`
	StringsB64 []string `json:"strings_b64"`
}

func (v *typed) attr(t *testing.T) slog.Attr {
	t.Helper()
	decode := func(s string) string {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	switch {
	case v.String != nil:
		return slog.String(v.Key, *v.String)
	case v.StringB64 != nil:
		return slog.String(v.Key, decode(*v.StringB64))
	case v.Int != nil:
		return slog.Int64(v.Key, *v.Int)
	case v.Float != nil:
		f, err := strconv.ParseFloat(*v.Float, 64)
		if err != nil {
			t.Fatal(err)
		}
		return slog.Float64(v.Key, f)
	case v.Bool != nil:
		return slog.Bool(v.Key, *v.Bool)
	case v.Strings != nil:
		return slog.Any(v.Key, v.Strings)
	case v.StringsB64 != nil:
		list := make([]string, 0, len(v.StringsB64))
		for _, s := range v.StringsB64 {
			list = append(list, decode(s))
		}
		return slog.Any(v.Key, list)
	}
	t.Fatalf("%s: no value", v.Key)
	return slog.Attr{}
}

// A record a line, as the vectors write it, in the Go and the Rust versions alike (specs.md §23.2).
func TestLineVectors(t *testing.T) {
	type vector struct {
		Note        string  `json:"note"`
		TimestampNS *int64  `json:"timestamp_ns"`
		Level       int     `json:"level"`
		Body        *string `json:"body"`
		BodyB64     *string `json:"body_b64"`
		Attributes  []typed `json:"attributes"`
		Resource    []typed `json:"resource"`
		Scope       string  `json:"scope"`
		Line        string  `json:"line"`
	}
	for _, v := range vectors.Load[vector](t, "log_record.json") {
		r := Record{Level: slog.Level(v.Level), Scope: v.Scope}
		if v.TimestampNS != nil {
			r.Time = time.Unix(0, *v.TimestampNS)
		}
		body, err := vectors.Bytes("body", v.Body, v.BodyB64)
		if err != nil {
			t.Fatal(err)
		}
		r.Body = body
		for i := range v.Attributes {
			r.Attrs = flatten(r.Attrs, "", v.Attributes[i].attr(t))
		}
		for i := range v.Resource {
			r.Resource = flatten(r.Resource, "", v.Resource[i].attr(t))
		}
		if got := string(Line(&r)); got != v.Line {
			t.Errorf("%s:\n%s\nwant:\n%s", v.Note, got, v.Line)
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(v.Line), &parsed); err != nil {
			t.Errorf("%s: the line is no JSON: %v", v.Note, err)
		}
	}
}

// syncBuffer is a writer several goroutines may write to.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// lines gives each line the handler wrote, decoded.
func lines(t *testing.T, text string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSuffix(text, "\n"), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// What slog gives the handler: groups and attributes added before, values of every kind, and the package that logged.
func TestLogsWriteWhatSlogGives(t *testing.T) {
	var out bytes.Buffer
	logs := NewLogs(&out, slog.String("service.name", "atoll"), slog.String("host.name", "nas"))
	at := time.Date(2026, 11, 19, 10, 0, 0, 5, time.UTC)
	r := slog.NewRecord(at, slog.LevelWarn, "a copy did not happen", 0)
	type state string
	r.AddAttrs(
		slog.String("path", "Films/\xffa.mkv"),
		slog.Any("error", errors.New("open Films/\xffa.mkv: input/output error")),
		slog.Any("from", state("online")),
		slog.Duration("for", 1500*time.Millisecond),
		slog.Time("at", time.Date(2026, 1, 2, 3, 4, 5, 6, time.FixedZone("x", 3600))),
		slog.Group("disk", slog.String("id", "t1"), slog.Int("copies", 4)),
		slog.Group("", slog.String("inline", "yes")),
		slog.Group("empty"),
		slog.Any("because", []string{"start", "events"}),
		slog.Any("nothing", nil),
		slog.Any("struct", struct{ A int }{3}),
		slog.Uint64("big", 1<<63),
	)
	h := logs.WithAttrs([]slog.Attr{slog.String("op", "7")}).WithGroup("view").WithAttrs([]slog.Attr{slog.String("point", "/srv/pool")})
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	want := `{"Timestamp":"` + strconv.FormatInt(at.UnixNano(), 10) + `","SeverityText":"WARN","SeverityNumber":13,"Body":"a copy did not happen",` +
		`"Attributes":{"op":"7","view.point":"/srv/pool","view.path_b64":"` + base64.StdEncoding.EncodeToString([]byte("Films/\xffa.mkv")) + `",` +
		`"view.error_b64":"` + base64.StdEncoding.EncodeToString([]byte("open Films/\xffa.mkv: input/output error")) + `","view.from":"online","view.for":1500000000,` +
		`"view.at":"2026-01-02T02:04:05.000000006Z","view.disk.id":"t1","view.disk.copies":4,"view.inline":"yes","view.because":["start","events"],` +
		`"view.nothing":null,"view.struct":"{A:3}","view.big":9223372036854775808},"Resource":{"service.name":"atoll","host.name":"nas"}}` + "\n"
	if out.String() != want {
		t.Errorf("\n%s\nwant\n%s", out.String(), want)
	}
}

// The package that logged is the record's scope; Atoll's main package by its path.
func TestLogsNameThePackageThatLogged(t *testing.T) {
	var out bytes.Buffer
	slog.New(NewLogs(&out)).Info("here")
	got := lines(t, out.String())
	scope, _ := got[0]["InstrumentationScope"].(map[string]any)
	if scope["Name"] != "github.com/peondusud/atoll/internal/telemetry" {
		t.Errorf("%v", got[0])
	}
	for function, want := range map[string]string{
		"github.com/peondusud/atoll/internal/jobs.(*scrub).verify":  "github.com/peondusud/atoll/internal/jobs",
		"github.com/peondusud/atoll/internal/daemon.(*loop).reload": "github.com/peondusud/atoll/internal/daemon",
		"main.runForGood.func2":                           "main",
		"github.com/peondusud/atoll/cmd/atoll.Test.func1": "github.com/peondusud/atoll/cmd/atoll",
		"nodot": "nodot",
	} {
		if got := packageOf(function); got != want {
			t.Errorf("%s: %s, want %s", function, got, want)
		}
	}
}

// INFO and more are written, DEBUG is not, as with slog's own handlers.
func TestLogsLeaveDebugOut(t *testing.T) {
	var out bytes.Buffer
	log := slog.New(NewLogs(&out))
	log.Debug("no")
	log.Info("yes")
	if got := lines(t, out.String()); len(got) != 1 || got[0]["Body"] != "yes" {
		t.Errorf("%q", out.String())
	}
}

// log_format = "text" writes as slog's TextHandler does, groups and attributes added before included, and JSON again
// once asked: a configuration read again changes it while the process runs.
func TestLogsSwitchToTextAndBack(t *testing.T) {
	var out, want bytes.Buffer
	logs := NewLogs(&out)
	logs.SetFormat(FormatText)
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	r := slog.NewRecord(at, slog.LevelInfo, "a pass begins", 0)
	r.AddAttrs(slog.String("path", "a b"), slog.Int("n", 2))
	h := logs.WithAttrs([]slog.Attr{slog.String("op", "7")}).WithGroup("g")
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if err := slog.NewTextHandler(&want, nil).WithAttrs([]slog.Attr{slog.String("op", "7")}).WithGroup("g").Handle(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if out.String() != want.String() {
		t.Errorf("%q, want %q", out.String(), want.String())
	}
	out.Reset()
	logs.SetFormat("anything else")
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if got := lines(t, out.String()); got[0]["Body"] != "a pass begins" {
		t.Errorf("%q", out.String())
	}
}

// recordSink keeps what it is given.
type recordSink struct {
	mu   sync.Mutex
	list []Record
}

func (s *recordSink) Emit(r *Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list = append(s.list, *r)
}

// The sink takes each record, in either format, but those of Local: the export never sends what it says of itself.
func TestLogsGiveEachRecordToTheSink(t *testing.T) {
	for _, format := range []string{FormatJSON, FormatText} {
		var out syncBuffer
		logs := NewLogs(&out, slog.String("service.name", "atoll"))
		logs.SetFormat(format)
		sink := &recordSink{}
		logs.SetSink(sink)
		log := slog.New(logs)
		log.Warn("a corrupt copy", "path", "Films/\xff")
		logs.Local().Warn("the OTLP export fails")
		logs.SetSink(nil)
		log.Info("after")
		if len(sink.list) != 1 {
			t.Fatalf("%s: %d records", format, len(sink.list))
		}
		r := sink.list[0]
		if r.Body != "a corrupt copy" || r.Level != slog.LevelWarn || len(r.Attrs) != 1 || r.Attrs[0].Key != "path_b64" ||
			r.Scope != "github.com/peondusud/atoll/internal/telemetry" || r.Resource[0].Value.String() != "atoll" {
			t.Errorf("%s: %+v", format, r)
		}
		if n := strings.Count(out.String(), "\n"); n != 3 {
			t.Errorf("%s: %d lines: %s", format, n, out.String())
		}
	}
}

// Records written from many goroutines at once are whole lines, each.
func TestLogsWriteWholeLines(t *testing.T) {
	var out syncBuffer
	log := slog.New(NewLogs(&out))
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 100 {
				log.Info("a record", "goroutine", g, "i", i, "padding", strings.Repeat("x", 200))
			}
		})
	}
	wg.Wait()
	if got := lines(t, out.String()); len(got) != 800 {
		t.Errorf("%d lines", len(got))
	}
}
