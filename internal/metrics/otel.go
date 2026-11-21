package metrics

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

var (
	otelName = regexp.MustCompile(`^atoll(\.[a-z][a-z0-9_]*)+$`)
	// otelUnit is a unit of UCUM the contract uses: bytes, seconds, degrees Celsius, or a count in braces, which adds
	// no word to the Prometheus name. "1" would add _ratio to a gauge.
	otelUnit = regexp.MustCompile(`^(By|s|Cel|\{[a-z][a-z_]*\})$`)
)

// checkOTelNames applies the rules of an OpenTelemetry name to the contract (specs.md §23.1): lowercase words under
// atoll., dotted where they name a namespace; none twice; none the namespace of another, as a dotted name is a path of
// objects to Elasticsearch, where a metric and a namespace of one name cannot both be; and a unit of UCUM.
func checkOTelNames(families []Family) error {
	seen := map[string]bool{}
	for _, f := range families {
		switch {
		case !otelName.MatchString(f.OTelName):
			return fmt.Errorf("%s: %q is no OpenTelemetry name of a metric of Atoll", f.Name, f.OTelName)
		case !otelUnit.MatchString(f.Unit):
			return fmt.Errorf("%s: unit %q", f.Name, f.Unit)
		case seen[f.OTelName]:
			return fmt.Errorf("%s: %q twice", f.Name, f.OTelName)
		}
		seen[f.OTelName] = true
	}
	for _, f := range families {
		for space := f.OTelName; ; {
			at := strings.LastIndexByte(space, '.')
			if at < 0 {
				break
			}
			space = space[:at]
			if seen[space] {
				return fmt.Errorf("%s: %q is the name of a metric and a namespace of %q", f.Name, space, f.OTelName)
			}
		}
	}
	return nil
}

// The words OpenTelemetry's Prometheus compatibility translation gives a unit of UCUM, and a unit after a slash; a
// unit of neither table is kept as written.
var (
	unitWords = map[string]string{
		"d": "days", "h": "hours", "min": "minutes", "s": "seconds", "ms": "milliseconds", "us": "microseconds", "ns": "nanoseconds",
		"By": "bytes", "KiBy": "kibibytes", "MiBy": "mebibytes", "GiBy": "gibibytes", "TiBy": "tibibytes",
		"KBy": "kilobytes", "MBy": "megabytes", "GBy": "gigabytes", "TBy": "terabytes",
		"m": "meters", "V": "volts", "A": "amperes", "J": "joules", "W": "watts", "g": "grams",
		"Cel": "celsius", "Hz": "hertz", "1": "", "%": "percent",
	}
	perUnitWords = map[string]string{"s": "second", "m": "minute", "h": "hour", "d": "day", "w": "week", "mo": "month", "y": "year"}
)

// PrometheusName gives the name OpenTelemetry's Prometheus compatibility translation gives a metric of that name and
// unit, a monotonic sum or not (its specification, Prometheus and OpenMetrics Compatibility, with the suffixes, as the
// OpenTelemetry Collector and Prometheus apply it): the name's words, split at any character but a letter, a digit or
// a colon; the unit's word, unless a word of the name is that already; per_ and the word of a unit after a slash; total
// at the end of a monotonic sum, and ratio at the end of a gauge of unit 1, each once; joined by underscores, with one
// before a leading digit. A unit in braces adds nothing (conformance/vectors/otel_names.json).
func PrometheusName(name, unit string, monotonic bool) string {
	words := strings.FieldsFunc(name, func(r rune) bool { return !metricRune(r) })
	main, per, _ := strings.Cut(unit, "/")
	if word := unitWord(main, unitWords); word != "" && !slices.Contains(words, word) {
		words = append(words, word)
	}
	if word := unitWord(per, perUnitWords); word != "" && !slices.Contains(words, "per_"+word) {
		words = append(words, "per_"+word)
	}
	if monotonic {
		words = append(slices.DeleteFunc(words, func(w string) bool { return w == "total" }), "total")
	}
	if unit == "1" && !monotonic {
		words = append(slices.DeleteFunc(words, func(w string) bool { return w == "ratio" }), "ratio")
	}
	out := strings.Join(words, "_")
	if out != "" && out[0] >= '0' && out[0] <= '9' {
		out = "_" + out
	}
	return out
}

// metricRune says whether a rune may stay in a word of a Prometheus name: a letter, a digit or a colon.
func metricRune(r rune) bool {
	return 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || r == ':'
}

// unitWord gives the word of a unit, as a table gives it or as written, with its other characters made underscores:
// none for a unit in braces, or none at all.
func unitWord(unit string, table map[string]string) string {
	unit = strings.TrimSpace(unit)
	if unit == "" || strings.ContainsAny(unit, "{}") {
		return ""
	}
	word, ok := table[unit]
	if !ok {
		word = unit
	}
	return strings.Join(strings.FieldsFunc(word, func(r rune) bool { return !metricRune(r) }), "_")
}
