package telemetry

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/metrics"
)

// collector is an OTLP/HTTP collector in the test's process: it decodes what it is sent, and answers as status says,
// or never while hang is set.
type collector struct {
	mu      sync.Mutex
	metrics []*colmetrics.ExportMetricsServiceRequest
	logs    []*collogs.ExportLogsServiceRequest
	headers []http.Header
	paths   []string
	status  int
	hang    chan struct{}
}

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	c.headers = append(c.headers, r.Header.Clone())
	c.paths = append(c.paths, r.URL.Path)
	status, hang := c.status, c.hang
	switch r.URL.Path {
	case "/otlp/v1/metrics":
		var req colmetrics.ExportMetricsServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil || r.Header.Get("Content-Type") != "application/x-protobuf" {
			c.mu.Unlock()
			http.Error(w, "not protobuf", http.StatusBadRequest)
			return
		}
		c.metrics = append(c.metrics, &req)
	case "/otlp/v1/logs":
		var req collogs.ExportLogsServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil || r.Header.Get("Content-Type") != "application/x-protobuf" {
			c.mu.Unlock()
			http.Error(w, "not protobuf", http.StatusBadRequest)
			return
		}
		c.logs = append(c.logs, &req)
	}
	c.mu.Unlock()
	if hang != nil {
		select {
		case <-hang:
		case <-r.Context().Done():
		}
		return
	}
	if status != 0 {
		http.Error(w, "the collector is down", status)
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
}

func (c *collector) counts() (metrics, logs int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.metrics), len(c.logs)
}

// eventually waits for what is said to hold, 10 s at most.
func eventually(t *testing.T, what string, holds func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if holds() {
			return
		}
	}
	t.Fatalf("%s: not within 10 s", what)
}

// pool is the snapshot of a daemon on one disk after two passes.
func pool() *metrics.Recorder {
	rec := metrics.NewRecorder("v0.1.0")
	rec.Update(func(s *metrics.Snapshot) {
		s.Configure([]metrics.Disk{{ID: "t1", FS: "ext4", Role: "data", Mount: "/mnt/t1"}})
		d := s.DiskOf("t1")
		d.State, d.Space = "online", &metrics.Space{Capacity: 1000, Free: 400}
		d.ScrubReadBytes = 77
		s.Passes, s.CopiedBytes = 2, 4096
		s.LastPass = &metrics.LastPass{EndNS: 1_795_000_000_500_000_000, DurationNS: 1_500_000_000}
		s.Files = map[string]metrics.Count{"ok": {Files: 5, Bytes: 50}}
		s.AddOperations(map[[2]string]int64{{"replicate", "replicated"}: 3})
		s.Alerts = []metrics.Alert{{Key: "disk-missing:t2", Count: 1}}
	})
	return rec
}

func attrsOf(list []*common.KeyValue) map[string]string {
	out := map[string]string{}
	for _, kv := range list {
		out[kv.Key] = kv.Value.GetStringValue()
	}
	return out
}

func headersFile(t *testing.T, text string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "otlp.headers")
	if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// The metrics of /metrics, by their OpenTelemetry names, with the same values from the same snapshot, and the
// records of the log, each with the process's resource, the headers of the file on each request (specs.md §23.1,
// §23.2).
func TestExportSendsTheMetricsAndTheLog(t *testing.T) {
	c := &collector{}
	server := httptest.NewServer(c)
	defer server.Close()
	var stderr syncBuffer
	logs := NewLogs(&stderr, slog.String("service.name", "atoll"), slog.String("service.version", "v0.1.0"), slog.String("host.name", "nas"))
	rec := pool()
	settings := config.Telemetry{LogFormat: "json", Endpoint: server.URL + "/otlp/", Metrics: true, Logs: true, Interval: 50 * time.Millisecond,
		Timeout: 2 * time.Second, HeadersFile: headersFile(t, "# for the collector\nAuthorization: ApiKey s3cr3t-value\r\nX-Scope-OrgID:  home \n\n")}
	m := NewManager(Options{Logs: logs, Metrics: rec, Version: "v0.1.0"})
	if err := m.Apply(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	slog.New(logs).Warn("a corrupt copy", "disk", "t1", "path", "Films/\xffa.mkv", "chunk", 3)
	eventually(t, "both signals sent", func() bool { n, l := c.counts(); return n > 0 && l > 0 })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := m.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for i, h := range c.headers {
		if h.Get("Authorization") != "ApiKey s3cr3t-value" || h.Get("X-Scope-OrgID") != "home" {
			t.Errorf("request %d (%s): headers %v", i, c.paths[i], h)
		}
	}
	// The last export of the metrics holds every point of /metrics.
	last := c.metrics[len(c.metrics)-1]
	if len(last.ResourceMetrics) != 1 {
		t.Fatalf("%d resources", len(last.ResourceMetrics))
	}
	rm := last.ResourceMetrics[0]
	if got := attrsOf(rm.Resource.Attributes); got["service.name"] != "atoll" || got["host.name"] != "nas" || got["service.version"] != "v0.1.0" {
		t.Errorf("resource %v", got)
	}
	if len(rm.ScopeMetrics) != 1 || rm.ScopeMetrics[0].Scope.Name != metricsScope || rm.ScopeMetrics[0].Scope.Version != "v0.1.0" {
		t.Fatalf("scopes %v", rm.ScopeMetrics)
	}
	sent := map[string]*metricspb.Metric{}
	for _, metric := range rm.ScopeMetrics[0].Metrics {
		sent[metric.Name] = metric
	}
	families, err := metrics.Families()
	if err != nil {
		t.Fatal(err)
	}
	points := rec.Points()
	for _, f := range families {
		list := points[f.Name]
		metric := sent[f.OTelName]
		if len(list) == 0 {
			if metric != nil {
				t.Errorf("%s sent with no sample", f.OTelName)
			}
			continue
		}
		if metric == nil {
			t.Errorf("%s not sent", f.OTelName)
			continue
		}
		if metric.Unit != f.Unit || metric.Description != f.Help {
			t.Errorf("%s: unit %q, description %q", f.OTelName, metric.Unit, metric.Description)
		}
		var data []*metricspb.NumberDataPoint
		switch f.Type {
		case "counter":
			sum := metric.GetSum()
			if sum == nil || !sum.IsMonotonic || sum.AggregationTemporality != metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE {
				t.Errorf("%s: no cumulative monotonic sum: %v", f.OTelName, metric)
				continue
			}
			data = sum.DataPoints
		default:
			if metric.GetGauge() == nil {
				t.Errorf("%s: no gauge: %v", f.OTelName, metric)
				continue
			}
			data = metric.GetGauge().DataPoints
		}
		if len(data) != len(list) {
			t.Errorf("%s: %d points, /metrics has %d", f.OTelName, len(data), len(list))
		}
		for _, p := range list {
			at := slices.IndexFunc(data, func(d *metricspb.NumberDataPoint) bool {
				got := attrsOf(d.Attributes)
				for i, label := range f.Labels {
					if got[label] != p.Labels[i] {
						return false
					}
				}
				return len(got) == len(f.Labels)
			})
			if at < 0 {
				t.Errorf("%s: no point for %v", f.OTelName, p.Labels)
				continue
			}
			if p.Seconds && data[at].GetAsDouble() != float64(p.Value)/1e9 || !p.Seconds && data[at].GetAsInt() != p.Value {
				t.Errorf("%s %v: %v, /metrics has %d", f.OTelName, p.Labels, data[at], p.Value)
			}
		}
	}
	// The record of the log, as the line says it.
	var found bool
	for _, req := range c.logs {
		for _, rl := range req.ResourceLogs {
			if got := attrsOf(rl.Resource.Attributes); got["service.name"] != "atoll" || got["host.name"] != "nas" {
				t.Errorf("resource %v", got)
			}
			for _, sl := range rl.ScopeLogs {
				for _, r := range sl.LogRecords {
					if r.Body.GetStringValue() != "a corrupt copy" {
						continue
					}
					found = true
					attrs := map[string]*common.AnyValue{}
					for _, kv := range r.Attributes {
						attrs[kv.Key] = kv.Value
					}
					if sl.Scope.Name != "github.com/peondusud/atoll/internal/telemetry" || r.SeverityNumber != 13 || r.SeverityText != "WARN" ||
						r.TimeUnixNano == 0 || attrs["disk"].GetStringValue() != "t1" || attrs["chunk"].GetIntValue() != 3 ||
						attrs["path_b64"].GetStringValue() != base64.StdEncoding.EncodeToString([]byte("Films/\xffa.mkv")) {
						t.Errorf("record %v in scope %v", r, sl.Scope)
					}
				}
			}
		}
	}
	if !found {
		t.Error("the record was not sent")
	}
	if strings.Contains(stderr.String(), "s3cr3t") {
		t.Errorf("the log holds the token:\n%s", stderr.String())
	}
}

// A collector that answers 500, or never, holds nothing up: each record goes at once, the oldest dropped once the
// queue is full, and the log tells of it once, with no token, never sending what it says of itself; a stop waits
// stopWithin at most (specs.md §23.2).
func TestADeadCollectorHoldsNothingUp(t *testing.T) {
	for _, hang := range []bool{false, true} {
		c := &collector{status: http.StatusInternalServerError}
		if hang {
			c.hang = make(chan struct{})
		}
		server := httptest.NewServer(c)
		var stderr syncBuffer
		logs := NewLogs(&stderr, slog.String("service.name", "atoll"))
		settings := config.Telemetry{Endpoint: server.URL + "/otlp", Metrics: true, Logs: true, Interval: 20 * time.Millisecond,
			Timeout: 300 * time.Millisecond, HeadersFile: headersFile(t, "Authorization: Bearer s3cr3t-value\n")}
		m := NewManager(Options{Logs: logs, Metrics: pool(), Version: "v"})
		if err := m.Apply(t.Context(), settings); err != nil {
			t.Fatal(err)
		}
		log := slog.New(logs)
		start := time.Now()
		for i := range 3 * queueSize {
			log.Info("a record", "i", i)
		}
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("hang %v: %d records took %s", hang, 3*queueSize, took)
		}
		eventually(t, "both signals tried", func() bool { n, l := c.counts(); return n > 1 && l > 1 })
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		start = time.Now()
		if err := m.Shutdown(ctx); err != nil && !hang {
			t.Errorf("hang %v: %v", hang, err)
		}
		if took := time.Since(start); took > stopWithin+time.Second {
			t.Errorf("hang %v: the stop took %s", hang, took)
		}
		cancel()
		if c.hang != nil {
			close(c.hang)
		}
		server.Close()
		said := stderr.String()
		for _, signal := range []string{signalMetrics, signalLogs} {
			if n := strings.Count(said, `"signal":"`+signal+`"`); n < 1 || n > 2 {
				t.Errorf("hang %v: %d lines of the log about %s:\n%s", hang, n, signal, said)
			}
		}
		if !strings.Contains(said, "the OTLP export fails") || strings.Contains(said, "s3cr3t") {
			t.Errorf("hang %v:\n%s", hang, said)
		}
		c.mu.Lock()
		for _, req := range c.logs {
			for _, rl := range req.ResourceLogs {
				for _, sl := range rl.ScopeLogs {
					for _, r := range sl.LogRecords {
						if strings.Contains(r.Body.GetStringValue(), "OTLP") {
							t.Errorf("hang %v: the export sent what it said of itself: %v", hang, r)
						}
					}
				}
			}
		}
		c.mu.Unlock()
	}
}

// A redirect is not followed: the headers would go wherever it leads.
func TestARedirectIsNotFollowed(t *testing.T) {
	elsewhere := &collector{}
	other := httptest.NewServer(elsewhere)
	defer other.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	client := newClient(time.Second, headersFile(t, "Authorization: Bearer s3cr3t-value\n"))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, redirect.URL+"/v1/logs", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Errorf("status %d", resp.StatusCode)
	}
	if _, l := elsewhere.counts(); len(elsewhere.paths) != 0 || l != 0 {
		t.Errorf("the redirect was followed: %v", elsewhere.paths)
	}
}

// gate is an exporter that waits for its gate on each export, and keeps the bodies of what it was given.
type gate struct {
	open   chan struct{}
	mu     sync.Mutex
	bodies []string
}

func (g *gate) Export(ctx context.Context, records []sdklog.Record) error {
	select {
	case <-g.open:
	case <-ctx.Done():
		return ctx.Err()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, r := range records {
		g.bodies = append(g.bodies, r.Body().AsString())
	}
	return nil
}

func (g *gate) Shutdown(context.Context) error   { return nil }
func (g *gate) ForceFlush(context.Context) error { return nil }

// A full queue drops its oldest record, counts it, and keeps the newest; what it holds goes once the exporter takes it.
func TestAFullQueueDropsItsOldest(t *testing.T) {
	var out syncBuffer
	g := &gate{open: make(chan struct{})}
	q := newQueue(t.Context(), g, newReporter(slog.New(NewLogs(&out))), 5*time.Second, time.Hour, 4)
	for i := range 10 {
		var r sdklog.Record
		r.SetBody(attribute.StringValue(strconv.Itoa(i)))
		if err := q.OnEmit(t.Context(), &r); err != nil {
			t.Fatal(err)
		}
	}
	close(g.open)
	if err := q.ForceFlush(t.Context()); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	got := slices.Clone(g.bodies)
	g.mu.Unlock()
	if want := []string{"6", "7", "8", "9"}; !slices.Equal(got, want) {
		t.Errorf("%v, want %v", got, want)
	}
	if !strings.Contains(out.String(), `"Body":"the OTLP export drops what it cannot send in time","Attributes":{"signal":"logs","dropped":6}`) {
		t.Errorf("%s", out.String())
	}
	if err := q.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	var late sdklog.Record
	if err := q.OnEmit(t.Context(), &late); err != nil || q.n != 0 {
		t.Errorf("a record after the stop: %v, %d queued", err, q.n)
	}
}

func TestReadHeaders(t *testing.T) {
	for _, tc := range []struct {
		text string
		want []header
		ok   bool
	}{
		{"", nil, true},
		{"# a comment\n\n  \r\n", nil, true},
		{"Authorization: ApiKey abc==\r\n", []header{{"Authorization", "ApiKey abc=="}}, true},
		{"X-Scope-OrgID:home\nX-Two:  a\tb  \n", []header{{"X-Scope-OrgID", "home"}, {"X-Two", "a\tb"}}, true},
		{"X-Empty:\n", []header{{"X-Empty", ""}}, true},
		{"no colon s3cr3t\n", nil, false},
		{": s3cr3t\n", nil, false},
		{"Bad Name: s3cr3t\n", nil, false},
		{"X-Bad: s3cr3t\x00\n", nil, false},
		{"X-Bad: s3cr3t\x7f\n", nil, false},
		{"content-type: text/s3cr3t\n", nil, false},
		{"Host: s3cr3t\n", nil, false},
		{strings.Repeat("X-A: b\n", maxHeadersFile/7+1), nil, false},
	} {
		file := headersFile(t, tc.text)
		got, err := readHeaders(file)
		if (err == nil) != tc.ok || tc.ok && !slices.Equal(got, tc.want) {
			t.Errorf("%q: %v, %v", tc.text, got, err)
		}
		if err != nil && (strings.Contains(err.Error(), "s3cr3t") || !errors.Is(err, errHeaders)) {
			t.Errorf("%q: %v", tc.text, err)
		}
	}
	if _, err := readHeaders(filepath.Join(t.TempDir(), "none")); err == nil {
		t.Error("a file that is not there")
	}
}

// The log tells of a signal that fails at once, then at most once every reportEvery, with what failed and was
// dropped since, and when it works again.
func TestReporterTellsOnceAWhile(t *testing.T) {
	var out syncBuffer
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	r := newReporter(slog.New(NewLogs(&out)))
	r.now = func() time.Time { return now }
	down := errors.New("connection refused")
	r.sent(signalLogs, down, 512)
	r.sent(signalLogs, down, 512)
	r.dropped(signalLogs, 7)
	r.sent(signalMetrics, down, 1)
	now = now.Add(reportEvery)
	r.sent(signalLogs, down, 100)
	r.sent(signalLogs, nil, 0)
	r.sent(signalLogs, nil, 0)
	got := lines(t, out.String())
	type line struct {
		body, signal      string
		failures, dropped float64
	}
	var said []line
	for _, l := range got {
		a, _ := l["Attributes"].(map[string]any)
		f, _ := a["failures"].(float64)
		d, _ := a["dropped"].(float64)
		s, _ := a["signal"].(string)
		said = append(said, line{l["Body"].(string), s, f, d})
	}
	want := []line{
		{"the OTLP export fails: what it could not send is dropped", signalLogs, 1, 512},
		{"the OTLP export fails: what it could not send is dropped", signalMetrics, 1, 1},
		{"the OTLP export fails: what it could not send is dropped", signalLogs, 2, 619},
		{"the OTLP export works again", signalLogs, 0, 0},
	}
	if !slices.Equal(said, want) {
		t.Errorf("%+v\nwant %+v", said, want)
	}
}

// What a configuration read again asks: the same export goes on, another starts in its place, none stops it; the
// format of the log changes at once.
func TestManagerFollowsTheConfiguration(t *testing.T) {
	c := &collector{}
	server := httptest.NewServer(c)
	defer server.Close()
	var stderr syncBuffer
	logs := NewLogs(&stderr)
	m := NewManager(Options{Logs: logs, Version: "v"})
	settings := config.Telemetry{LogFormat: "text", Endpoint: server.URL, Logs: true, Interval: time.Minute, Timeout: time.Second}
	if err := m.Apply(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	first := m.current
	if logs.Format() != FormatText || logs.core.sink.Load() == nil {
		t.Fatal("not applied")
	}
	settings.LogFormat = "json"
	if err := m.Apply(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	if m.current != first || logs.Format() != FormatJSON {
		t.Error("the same export started again")
	}
	settings.Timeout = 2 * time.Second
	if err := m.Apply(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	if m.current == first || logs.core.sink.Load() == nil {
		t.Error("another export did not start")
	}
	// Metrics with no recorder, as in atoll mount: nothing to send.
	if err := m.Apply(t.Context(), config.Telemetry{Endpoint: server.URL, Metrics: true, Interval: time.Minute, Timeout: time.Second}); err != nil {
		t.Fatal(err)
	}
	if logs.core.sink.Load() != nil || m.current.meters != nil || m.current.loggers != nil {
		t.Error("an export with nothing to send")
	}
	if err := m.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}
