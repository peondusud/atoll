package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/peondusud/atoll/internal/telemetry"
)

// childConfig names, to TestDaemonChild, the configuration of the daemon it runs.
const childConfig = "ATOLL_TEST_DAEMON_CONFIG"

// TestDaemonChild runs the daemon that runs for good in a process of its own, as main does, for the tests below: it
// does nothing in a run of the tests.
func TestDaemonChild(t *testing.T) {
	path := os.Getenv(childConfig)
	if path == "" {
		t.Skip("the process of a daemon the tests below start")
	}
	processLogs = telemetry.NewLogs(os.Stderr, logResource()...)
	slog.SetDefault(slog.New(processLogs))
	os.Exit(run([]string{"--config", path, "daemon"}, os.Stdout, os.Stderr))
}

// otlpCollector keeps the names of the metrics and the bodies of the records it is sent, and answers 200.
type otlpCollector struct {
	mu      sync.Mutex
	metrics map[string]bool
	bodies  []string
	passes  int64
}

func (c *otlpCollector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch r.URL.Path {
	case "/v1/metrics":
		var req colmetrics.ExportMetricsServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for _, rm := range req.ResourceMetrics {
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					c.metrics[m.Name] = true
					if m.Name == "atoll.passes" {
						c.passes = m.GetSum().DataPoints[0].GetAsInt()
					}
				}
			}
		}
	case "/v1/logs":
		var req collogs.ExportLogsServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for _, rl := range req.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				for _, lr := range sl.LogRecords {
					c.bodies = append(c.bodies, lr.Body.GetStringValue())
				}
			}
		}
	default:
		http.NotFound(w, r)
	}
}

// daemonProcess is a daemon TestDaemonChild runs, and what it writes on standard error; err is what it ended with,
// once exited is closed.
type daemonProcess struct {
	cmd    *exec.Cmd
	stderr *syncedBuffer
	exited chan struct{}
	err    error
}

type syncedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func startDaemon(t *testing.T, configPath string) *daemonProcess {
	t.Helper()
	p := &daemonProcess{stderr: &syncedBuffer{}, exited: make(chan struct{})}
	p.cmd = exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestDaemonChild$")
	p.cmd.Env = append(os.Environ(), childConfig+"="+configPath)
	p.cmd.Stderr = p.stderr
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		p.err = p.cmd.Wait()
		close(p.exited)
	}()
	t.Cleanup(func() {
		_ = p.cmd.Process.Kill()
		<-p.exited
	})
	return p
}

// stop sends SIGTERM, and waits for the daemon to exit.
func (p *daemonProcess) stop(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.exited:
		if p.err != nil {
			t.Errorf("the daemon exited with %v:\n%s", p.err, p.stderr.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the daemon did not stop:\n%s", p.stderr.String())
	}
}

// memory reads the resident memory of a process and its peak, in kB.
func memory(pid int) (rss, hwm int64) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0, 0
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		name, value, _ := strings.Cut(scanner.Text(), ":")
		kb, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(value), " kB"), 10, 64)
		switch name {
		case "VmRSS":
			rss = kb
		case "VmHWM":
			hwm = kb
		}
	}
	return rss, hwm
}

// The daemon writes its log as JSON lines of OpenTelemetry's data model by default, and with [telemetry] sends its
// metrics and the same records to an OTLP/HTTP collector (specs.md §23.2).
func TestTheDaemonLogsInJSONAndExports(t *testing.T) {
	c := &otlpCollector{metrics: map[string]bool{}}
	server := httptest.NewServer(c)
	defer server.Close()
	configPath, _ := apiPool(t, "[api]\nsocket = \"\"\n[telemetry]\notlp_endpoint = "+strconv.Quote(server.URL)+
		"\notlp_metrics = true\notlp_logs = true\notlp_interval = \"1s\"\n")
	p := startDaemon(t, configPath)
	deadline := time.Now().Add(30 * time.Second)
	for {
		c.mu.Lock()
		ready := c.passes > 0 && c.metrics["atoll.disk.state"] && len(c.bodies) > 0
		c.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing sent within 30 s:\n%s", p.stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	p.stop(t)
	said := p.stderr.String()
	var bodies []string
	for line := range strings.SplitSeq(strings.TrimSuffix(said, "\n"), "\n") {
		var record struct {
			Timestamp      string
			SeverityText   string
			SeverityNumber int
			Body           string
			Resource       map[string]string
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Errorf("a line that is no JSON: %q", line)
			continue
		}
		if _, err := strconv.ParseInt(record.Timestamp, 10, 64); err != nil || record.SeverityNumber == 0 ||
			record.Resource["service.name"] != "atoll" || record.Resource["service.version"] == "" {
			t.Errorf("%q", line)
		}
		bodies = append(bodies, record.Body)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, body := range c.bodies {
		if !strings.Contains(said, `"Body":`+strconv.Quote(body)) {
			t.Errorf("sent %q, which the log does not hold", body)
		}
	}
	if !slices.Contains(bodies, "a pass ended") {
		t.Errorf("no pass in the log:\n%s", said)
	}
}

// log_format = "text" brings back slog's text, for whoever reads a terminal.
func TestTheDaemonLogsInText(t *testing.T) {
	configPath, _ := apiPool(t, "[api]\nsocket = \"\"\n[telemetry]\nlog_format = \"text\"\n")
	p := startDaemon(t, configPath)
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(p.stderr.String(), `msg="a pass ended"`) {
		if time.Now().After(deadline) {
			t.Fatalf("no pass within 30 s:\n%s", p.stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	p.stop(t)
	for line := range strings.SplitSeq(strings.TrimSuffix(p.stderr.String(), "\n"), "\n") {
		if !strings.HasPrefix(line, "time=") || !strings.Contains(line, " level=") {
			t.Errorf("%q", line)
		}
	}
}

// The resident memory of the daemon, with OTLP off and on, sending every second to a collector that takes it, and to
// one that is gone (specs.md §26). It runs with ATOLL_MEASURE_MEMORY=1 alone, as it takes a minute and a half.
func TestDaemonMemoryWithOTLP(t *testing.T) {
	if os.Getenv("ATOLL_MEASURE_MEMORY") != "1" {
		t.Skip("ATOLL_MEASURE_MEMORY=1 measures it")
	}
	c := &otlpCollector{metrics: map[string]bool{}}
	server := httptest.NewServer(c)
	defer server.Close()
	gone := httptest.NewServer(http.NotFoundHandler())
	goneURL := gone.URL
	gone.Close()
	for _, tc := range []struct{ name, more string }{
		{"OTLP off", ""},
		{"OTLP on", "[telemetry]\notlp_endpoint = " + strconv.Quote(server.URL) + "\notlp_metrics = true\notlp_logs = true\notlp_interval = \"1s\"\n"},
		{"OTLP on, its collector gone", "[telemetry]\notlp_endpoint = " + strconv.Quote(goneURL) + "\notlp_metrics = true\notlp_logs = true\notlp_interval = \"1s\"\n"},
	} {
		configPath, _ := apiPool(t, "[api]\nsocket = \"\"\n"+tc.more)
		p := startDaemon(t, configPath)
		var most int64
		for range 30 {
			time.Sleep(time.Second)
			if rss, _ := memory(p.cmd.Process.Pid); rss > most {
				most = rss
			}
		}
		rss, hwm := memory(p.cmd.Process.Pid)
		p.stop(t)
		t.Logf("%s: resident %.1f MiB after 30 s, at most %.1f MiB sampled, peak %.1f MiB", tc.name, float64(rss)/1024, float64(most)/1024, float64(hwm)/1024)
	}
}
