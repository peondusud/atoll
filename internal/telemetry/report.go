package telemetry

import (
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"
)

// reportEvery is how often the log tells, at most, of a signal the export cannot send: the first failure at once, then
// what failed and was dropped since.
const reportEvery = 10 * time.Minute

// The signals the export sends, and what the SDK says of itself.
const (
	signalMetrics = "metrics"
	signalLogs    = "logs"
	signalSDK     = "sdk"
)

// reporter tells of what the export could not do, through the process's log but never through the export, at most
// once every reportEvery for each signal (specs.md §23.2).
type reporter struct {
	log   *slog.Logger
	every time.Duration
	now   func() time.Time

	mu    sync.Mutex
	state map[string]*signalState
}

type signalState struct {
	// failing says that the last try failed and was told of; told is when the log last said so.
	failing bool
	told    time.Time
	// failures and dropped count what failed, and what was lost, since the log last told.
	failures, dropped int64
	lastError         string
}

func newReporter(log *slog.Logger) *reporter {
	return &reporter{log: log, every: reportEvery, now: time.Now, state: map[string]*signalState{}}
}

func (r *reporter) get(signal string) *signalState {
	st := r.state[signal]
	if st == nil {
		st = &signalState{}
		r.state[signal] = st
	}
	return st
}

// sent records an export of a signal: a success says that the export works again after a failure, and a failure
// loses what it held, lost records or one collection of the metrics.
func (r *reporter) sent(signal string, err error, lost int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.get(signal)
	if err == nil {
		if st.failing {
			r.log.Info("the OTLP export works again", "signal", signal, "failures", st.failures, "dropped", st.dropped)
			*st = signalState{}
		}
		return
	}
	st.failures++
	st.dropped += lost
	st.lastError = clip(err.Error())
	if st.failing && r.now().Sub(st.told) < r.every {
		return
	}
	r.log.Warn("the OTLP export fails: what it could not send is dropped", "signal", signal, "failures", st.failures,
		"dropped", st.dropped, "error", st.lastError)
	st.failing, st.told = true, r.now()
	st.failures, st.dropped = 0, 0
}

// dropped records records a full queue dropped, the collector being too slow or gone.
func (r *reporter) dropped(signal string, n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.get(signal)
	st.dropped += n
	if !st.told.IsZero() && r.now().Sub(st.told) < r.every {
		return
	}
	r.log.Warn("the OTLP export drops what it cannot send in time", "signal", signal, "dropped", st.dropped)
	st.told = r.now()
	st.dropped = 0
}

// internal records what the SDK says of a problem of its own, through its error handler or its logger.
func (r *reporter) internal(message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.get(signalSDK)
	st.failures++
	if !st.told.IsZero() && r.now().Sub(st.told) < r.every {
		return
	}
	r.log.Warn("the OpenTelemetry SDK tells of a problem", "times", st.failures, "error", clip(message))
	st.told = r.now()
	st.failures = 0
}

// clip keeps the first 512 bytes of an error, on one line: a collector's answer goes in it, as long as it likes.
func clip(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	if len(s) <= 512 {
		return s
	}
	cut := 512
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func utf8Start(b byte) bool {
	return b&0xC0 != 0x80
}
