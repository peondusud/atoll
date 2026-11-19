package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/peondusud/atoll/internal/config"
	"github.com/peondusud/atoll/internal/metrics"
)

// metricsScope names the instrumentation scope of the metrics: the package that holds them.
const metricsScope = "github.com/peondusud/atoll/internal/metrics"

// stopWithin is the most a stop waits for what is left to send: a collector that is gone must not hold up a
// systemctl stop.
const stopWithin = 5 * time.Second

// Options is what an export needs besides its settings.
type Options struct {
	// Logs is the handler of the process: its records go out with Settings.Logs, and the export tells of its own
	// trouble through it, never sent.
	Logs *Logs
	// Metrics is what /metrics gives, sent with Settings.Metrics; nil sends no metrics, as atoll mount has none.
	Metrics *metrics.Recorder
	// Version is Atoll's, the version of the scope of the metrics.
	Version string
}

// Export sends the metrics and the log over OTLP/HTTP with protobuf, as a configuration's [telemetry] asks (specs.md
// §23.1, §23.2).
type Export struct {
	settings config.Telemetry
	meters   *sdkmetric.MeterProvider
	loggers  *sdklog.LoggerProvider
	sink     *logSink
	opt      Options
}

// Start starts what settings ask: nothing, with no endpoint or no signal. Nothing it does waits on the collector.
func Start(ctx context.Context, settings config.Telemetry, opt Options) (*Export, error) {
	e := &Export{settings: settings, opt: opt}
	wantMetrics := settings.Metrics && opt.Metrics != nil
	wantLogs := settings.Logs && opt.Logs != nil
	if settings.Endpoint == "" || !wantMetrics && !wantLogs {
		return e, nil
	}
	report := newReporter(opt.Logs.Local())
	listenToTheSDK(report)
	client := newClient(settings.Timeout, settings.HeadersFile)
	base := strings.TrimSuffix(settings.Endpoint, "/")
	res := resource.NewSchemaless(attributes(opt.Logs.core.resource)...)
	if wantMetrics {
		exporter, err := otlpmetrichttp.New(ctx,
			otlpmetrichttp.WithEndpointURL(base+"/v1/metrics"), otlpmetrichttp.WithHTTPClient(client),
			otlpmetrichttp.WithHeaders(map[string]string{}), otlpmetrichttp.WithCompression(otlpmetrichttp.NoCompression),
			otlpmetrichttp.WithTimeout(settings.Timeout))
		if err != nil {
			return nil, fmt.Errorf("the OTLP export of the metrics: %w", err)
		}
		reader := sdkmetric.NewPeriodicReader(&metricExporter{Exporter: exporter, report: report, timeout: settings.Timeout},
			sdkmetric.WithInterval(settings.Interval), sdkmetric.WithTimeout(settings.Timeout))
		e.meters = sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(reader))
		if err := observe(e.meters.Meter(metricsScope, metric.WithInstrumentationVersion(opt.Version)), opt.Metrics); err != nil {
			return nil, errors.Join(err, e.meters.Shutdown(ctx))
		}
	}
	if wantLogs {
		exporter, err := otlploghttp.New(ctx,
			otlploghttp.WithEndpointURL(base+"/v1/logs"), otlploghttp.WithHTTPClient(client),
			otlploghttp.WithHeaders(map[string]string{}), otlploghttp.WithCompression(otlploghttp.NoCompression),
			otlploghttp.WithTimeout(settings.Timeout))
		if err != nil {
			return nil, errors.Join(fmt.Errorf("the OTLP export of the log: %w", err), e.shutdownMeters(ctx))
		}
		e.loggers = sdklog.NewLoggerProvider(sdklog.WithResource(res),
			sdklog.WithProcessor(newQueue(ctx, exporter, report, settings.Timeout, sendEvery, queueSize)))
		e.sink = &logSink{provider: e.loggers}
		opt.Logs.SetSink(e.sink)
	}
	return e, nil
}

func (e *Export) shutdownMeters(ctx context.Context) error {
	if e.meters == nil {
		return nil
	}
	return e.meters.Shutdown(ctx)
}

// Shutdown stops the export, sending within ctx what is left: the last metrics, and the log records queued.
func (e *Export) Shutdown(ctx context.Context) error {
	if e == nil {
		return nil
	}
	var errs []error
	if e.loggers != nil {
		e.sink.stop()
		errs = append(errs, e.loggers.Shutdown(ctx))
	}
	errs = append(errs, e.shutdownMeters(ctx))
	return errors.Join(errs...)
}

// attributes gives slog's attributes as OpenTelemetry's.
func attributes(attrs []slog.Attr) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(attrs))
	for _, a := range attrs {
		if kv, ok := keyValue(a); ok {
			out = append(out, kv)
		}
	}
	return out
}

// keyValue gives an attribute normalize made as OpenTelemetry's; nil has none.
func keyValue(a slog.Attr) (attribute.KeyValue, bool) {
	v, ok := value(a.Value)
	return attribute.KeyValue{Key: attribute.Key(a.Key), Value: v}, ok
}

func value(v slog.Value) (attribute.Value, bool) {
	switch v.Kind() {
	case slog.KindString:
		return attribute.StringValue(v.String()), true
	case slog.KindInt64:
		return attribute.Int64Value(v.Int64()), true
	case slog.KindUint64:
		if u := v.Uint64(); u <= math.MaxInt64 {
			return attribute.Int64Value(int64(u)), true
		}
		return attribute.StringValue(strconv.FormatUint(v.Uint64(), 10)), true
	case slog.KindFloat64:
		return attribute.Float64Value(v.Float64()), true
	case slog.KindBool:
		return attribute.BoolValue(v.Bool()), true
	case slog.KindAny:
		list, ok := v.Any().([]slog.Value)
		if !ok {
			return attribute.Value{}, false
		}
		values := make([]attribute.Value, 0, len(list))
		for _, item := range list {
			if one, ok := value(item); ok {
				values = append(values, one)
			}
		}
		return attribute.SliceValue(values...), true
	case slog.KindDuration, slog.KindTime, slog.KindGroup, slog.KindLogValuer:
		// normalize gives none of these: as text all the same.
	}
	return attribute.StringValue(strings.ToValidUTF8(v.String(), "\uFFFD")), true
}

// logSink gives each record of the handler to the SDK's logger of its scope, whose processor queues it.
type logSink struct {
	provider *sdklog.LoggerProvider
	loggers  sync.Map
	mu       sync.RWMutex
	stopped  bool
}

func (s *logSink) Emit(r *Record) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.stopped {
		return
	}
	scope := r.Scope
	if scope == "" {
		scope = "github.com/peondusud/atoll"
	}
	logger, ok := s.loggers.Load(scope)
	if !ok {
		logger, _ = s.loggers.LoadOrStore(scope, s.provider.Logger(scope))
	}
	var rec otellog.Record
	rec.SetTimestamp(r.Time)
	rec.SetObservedTimestamp(r.Time)
	number, text := r.Severity()
	rec.SetSeverity(otellog.Severity(number))
	rec.SetSeverityText(text)
	rec.SetBody(attribute.StringValue(strings.ToValidUTF8(r.Body, "\uFFFD")))
	rec.AddAttributes(attributes(r.Attrs)...)
	logger.(otellog.Logger).Emit(context.Background(), rec)
}

// stop makes the records that come next go nowhere: the provider is shut down.
func (s *logSink) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
}

// observe registers the metrics of the contract with the meter, each under its OpenTelemetry name and unit, gauges as
// gauges and counters as monotonic sums, values in seconds as floats; one callback reads the recorder's snapshot once
// for every metric, so each export holds what /metrics would have given at that moment (specs.md §23.1).
func observe(meter metric.Meter, rec *metrics.Recorder) error {
	families, err := metrics.Families()
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	type instrument struct {
		family  metrics.Family
		integer metric.Int64Observable
		float   metric.Float64Observable
	}
	list := make([]instrument, 0, len(families))
	var all []metric.Observable
	for _, f := range families {
		one := instrument{family: f}
		var err error
		switch {
		case f.Unit == "s" && f.Type == "gauge":
			one.float, err = meter.Float64ObservableGauge(f.OTelName, metric.WithUnit(f.Unit), metric.WithDescription(f.Help))
		case f.Unit == "s":
			one.float, err = meter.Float64ObservableCounter(f.OTelName, metric.WithUnit(f.Unit), metric.WithDescription(f.Help))
		case f.Type == "gauge":
			one.integer, err = meter.Int64ObservableGauge(f.OTelName, metric.WithUnit(f.Unit), metric.WithDescription(f.Help))
		default:
			one.integer, err = meter.Int64ObservableCounter(f.OTelName, metric.WithUnit(f.Unit), metric.WithDescription(f.Help))
		}
		if err != nil {
			return fmt.Errorf("metric %s: %w", f.OTelName, err)
		}
		if one.float != nil {
			all = append(all, one.float)
		} else {
			all = append(all, one.integer)
		}
		list = append(list, one)
	}
	_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		points := rec.Points()
		for _, one := range list {
			for _, p := range points[one.family.Name] {
				kvs := make([]attribute.KeyValue, 0, len(one.family.Labels))
				for i, label := range one.family.Labels {
					kvs = append(kvs, attribute.String(label, strings.ToValidUTF8(p.Labels[i], "\uFFFD")))
				}
				set := metric.WithAttributes(kvs...)
				if one.float != nil {
					o.ObserveFloat64(one.float, float64(p.Value)/1e9, set)
				} else {
					o.ObserveInt64(one.integer, p.Value, set)
				}
			}
		}
		return nil
	}, all...)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	return nil
}

// metricExporter gives each export timeout in all, its retries included, the last one at a stop too; and counts what
// the exporter of the metrics could not send, and tells of it: a collection that failed is dropped, the next one
// holding every counter whole. It says nil to the reader, which would tell the SDK's error handler again.
type metricExporter struct {
	sdkmetric.Exporter
	report  *reporter
	timeout time.Duration
}

func (m *metricExporter) Export(ctx context.Context, data *metricdata.ResourceMetrics) error {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	err := m.Exporter.Export(ctx, data)
	m.report.sent(signalMetrics, err, 1)
	return nil
}

// listenToTheSDK sends what the SDK says of its own problems, through its error handler and its logger, to the
// reporter: by default it would write lines of its own on standard error.
func listenToTheSDK(report *reporter) {
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) { report.internal(err.Error()) }))
	otel.SetLogger(logr.New(&sdkSink{report: report}))
}

// sdkSink takes what the SDK logs of its errors and warnings; what it says of its normal work is left out.
type sdkSink struct {
	report *reporter
}

func (s *sdkSink) Init(logr.RuntimeInfo) {}

// Enabled takes the SDK's warnings, which it logs at level 1, and its errors.
func (s *sdkSink) Enabled(level int) bool { return level <= 1 }

func (s *sdkSink) Info(_ int, msg string, _ ...any) { s.report.internal(msg) }

func (s *sdkSink) Error(err error, msg string, _ ...any) {
	if err != nil {
		msg += ": " + err.Error()
	}
	s.report.internal(msg)
}

func (s *sdkSink) WithValues(...any) logr.LogSink { return s }

func (s *sdkSink) WithName(string) logr.LogSink { return s }
