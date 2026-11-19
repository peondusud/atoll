package telemetry

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// Sink takes the records of the log as they come, to send them on: it must never wait (specs.md §23.2).
type Sink interface {
	Emit(r *Record)
}

// Logs is the handler of every log of a process (specs.md §23.2): each record a line on its writer, standard error, in
// OpenTelemetry's log data model as JSON, or as Go's slog writes text; and, with a sink, each record to it as well.
// The format and the sink may change while it runs, as a configuration read again says.
type Logs struct {
	core *logsCore
	// prefix is the groups open, each key followed by a dot; attrs the attributes added, flattened and normalized.
	prefix string
	attrs  []slog.Attr
	// steps replays WithGroup and WithAttrs on the text handler, which formats them itself.
	steps []step
	// local says that its records go to the writer alone, never to the sink: what the export says of itself.
	local bool
}

type step struct {
	group string
	attrs []slog.Attr
}

type logsCore struct {
	mu       sync.Mutex
	w        io.Writer
	resource []slog.Attr
	format   atomic.Pointer[string]
	text     atomic.Pointer[slog.Handler]
	sink     atomic.Pointer[Sink]
	scopes   sync.Map
	main     string
}

// NewLogs gives the handler of a process's log, writing to w in JSON, the process described by resource (service.name
// and the others): what each record carries.
func NewLogs(w io.Writer, resource ...slog.Attr) *Logs {
	c := &logsCore{w: w, resource: flatten(nil, "", resource...), main: "main"}
	if info, ok := debug.ReadBuildInfo(); ok && info.Path != "" {
		c.main = info.Path
	}
	l := &Logs{core: c}
	l.SetFormat(FormatJSON)
	return l
}

// The formats of the log (specs.md §23.2).
const (
	FormatJSON = "json"
	FormatText = "text"
)

// SetFormat writes the records that come next as JSON lines or as text; any other format is JSON. A nil Logs does
// nothing.
func (l *Logs) SetFormat(format string) {
	if l == nil {
		return
	}
	if format != FormatText {
		format = FormatJSON
	}
	if was := l.core.format.Load(); was != nil && *was == format {
		return
	}
	var text slog.Handler = slog.NewTextHandler(l.core.w, nil)
	l.core.text.Store(&text)
	l.core.format.Store(&format)
}

// Format says how the records are written.
func (l *Logs) Format() string {
	return *l.core.format.Load()
}

// SetSink sends each record that comes next to s as well, or to none with nil.
func (l *Logs) SetSink(s Sink) {
	if s == nil {
		l.core.sink.Store(nil)
		return
	}
	l.core.sink.Store(&s)
}

// Local gives a logger whose records go to the writer alone, never to the sink: the export tells of its own
// trouble so, as a record it sent of it could only add to it.
func (l *Logs) Local() *slog.Logger {
	local := *l
	local.local = true
	return slog.New(&local)
}

// Enabled says that a record of INFO or more is written, as slog's own handlers do by default.
func (l *Logs) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo
}

// Handle writes a record, and gives it to the sink.
func (l *Logs) Handle(ctx context.Context, r slog.Record) error {
	var sink Sink
	if p := l.core.sink.Load(); p != nil && !l.local {
		sink = *p
	}
	json := l.Format() == FormatJSON
	var err error
	if !json {
		h := *l.core.text.Load()
		for _, s := range l.steps {
			if s.group != "" {
				h = h.WithGroup(s.group)
			} else {
				h = h.WithAttrs(s.attrs)
			}
		}
		err = h.Handle(ctx, r)
	}
	if !json && sink == nil {
		return err
	}
	rec := Record{Time: r.Time, Level: r.Level, Body: r.Message, Attrs: slices.Clone(l.attrs), Resource: l.core.resource, Scope: l.core.scope(r.PC)}
	r.Attrs(func(a slog.Attr) bool {
		rec.Attrs = flatten(rec.Attrs, l.prefix, a)
		return true
	})
	if json {
		line := Line(&rec)
		l.core.mu.Lock()
		_, err = l.core.w.Write(line)
		l.core.mu.Unlock()
	}
	if sink != nil {
		sink.Emit(&rec)
	}
	return err
}

// WithAttrs gives a handler that adds these attributes to each record.
func (l *Logs) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return l
	}
	next := *l
	next.attrs = flatten(slices.Clone(l.attrs), l.prefix, attrs...)
	next.steps = append(slices.Clip(l.steps), step{attrs: attrs})
	return &next
}

// WithGroup gives a handler that puts the attributes that come next in a group.
func (l *Logs) WithGroup(name string) slog.Handler {
	if name == "" {
		return l
	}
	next := *l
	next.prefix = l.prefix + name + "."
	next.steps = append(slices.Clip(l.steps), step{group: name})
	return &next
}

// scope names the package of the function that logged, from its program counter: what OpenTelemetry calls the
// instrumentation scope. Atoll's main package is named by its path.
func (c *logsCore) scope(pc uintptr) string {
	if pc == 0 {
		return ""
	}
	if known, ok := c.scopes.Load(pc); ok {
		return known.(string)
	}
	frame, _ := runtime.CallersFrames([]uintptr{pc}).Next()
	name := packageOf(frame.Function)
	if name == "main" {
		name = c.main
	}
	c.scopes.Store(pc, name)
	return name
}

// packageOf gives the package of a function's full name, such as github.com/peondusud/atoll/internal/jobs for
// github.com/peondusud/atoll/internal/jobs.(*scrub).verify.
func packageOf(function string) string {
	slash := strings.LastIndexByte(function, '/')
	dot := strings.IndexByte(function[slash+1:], '.')
	if dot < 0 {
		return function
	}
	return function[:slash+1+dot]
}
