package telemetry

import (
	"encoding"
	"encoding/base64"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Record is a record of the log in OpenTelemetry's log data model, as Atoll writes it (specs.md §23.2): what a line
// of the log holds, and what goes over OTLP.
type Record struct {
	// Time is when it happened, the zero time for none.
	Time time.Time
	// Level is slog's: SeverityNumber is Level + 9, and SeverityText its name.
	Level slog.Level
	Body  string
	// Attrs are the attributes in their order, each key once, each value one of normalize's: a key of a group is
	// dotted, and a string that is not valid UTF-8 is under its key and _b64, in base64.
	Attrs []slog.Attr
	// Resource holds the attributes of the process, and Scope names the package that logged, empty when unknown.
	Resource []slog.Attr
	Scope    string
}

// Severity gives the record's SeverityNumber and SeverityText: slog's levels DEBUG, INFO, WARN and ERROR are 5, 9, 13
// and 17, as OpenTelemetry maps them, and a level in between is a number in between, from 1 to 24.
func (r *Record) Severity() (number int, text string) {
	return min(max(int(r.Level)+9, 1), 24), r.Level.String()
}

// Line writes the record as one line of JSON, with the field names of OpenTelemetry's log data model in its
// specification's example records, in a fixed order, and a line feed (conformance/vectors/log_record.json).
func Line(r *Record) []byte {
	b := make([]byte, 0, 256)
	b = append(b, '{')
	if !r.Time.IsZero() {
		b = append(b, `"Timestamp":"`...)
		b = strconv.AppendInt(b, r.Time.UnixNano(), 10)
		b = append(b, `",`...)
	}
	number, text := r.Severity()
	b = append(b, `"SeverityText":`...)
	b = appendString(b, text)
	b = append(b, `,"SeverityNumber":`...)
	b = strconv.AppendInt(b, int64(number), 10)
	b = append(b, `,"Body":`...)
	b = appendString(b, strings.ToValidUTF8(r.Body, "\uFFFD"))
	if len(r.Attrs) > 0 {
		b = append(b, `,"Attributes":`...)
		b = appendObject(b, r.Attrs)
	}
	b = append(b, `,"Resource":`...)
	b = appendObject(b, r.Resource)
	if r.Scope != "" {
		b = append(b, `,"InstrumentationScope":{"Name":`...)
		b = appendString(b, r.Scope)
		b = append(b, '}')
	}
	return append(b, "}\n"...)
}

func appendObject(b []byte, attrs []slog.Attr) []byte {
	b = append(b, '{')
	for i, a := range attrs {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendString(b, a.Key)
		b = append(b, ':')
		b = appendValue(b, a.Value)
	}
	return append(b, '}')
}

// appendValue writes a value normalize gave: a string, a number, a bool, null, or a list of these.
func appendValue(b []byte, v slog.Value) []byte {
	switch v.Kind() {
	case slog.KindString:
		return appendString(b, v.String())
	case slog.KindInt64:
		return strconv.AppendInt(b, v.Int64(), 10)
	case slog.KindUint64:
		return strconv.AppendUint(b, v.Uint64(), 10)
	case slog.KindFloat64:
		return appendFloat(b, v.Float64())
	case slog.KindBool:
		return strconv.AppendBool(b, v.Bool())
	case slog.KindAny:
		list, ok := v.Any().([]slog.Value)
		if !ok {
			return append(b, "null"...)
		}
		b = append(b, '[')
		for i, item := range list {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendValue(b, item)
		}
		return append(b, ']')
	case slog.KindDuration, slog.KindTime, slog.KindGroup, slog.KindLogValuer:
		// normalize gives none of these: as text all the same.
	}
	return appendString(b, strings.ToValidUTF8(v.String(), "\uFFFD"))
}

// appendFloat writes a float as JavaScript and Go's encoding/json do: without an exponent from 1e-6 to 1e21, the
// shortest digits that read back the same. JSON has no NaN or infinity: those are strings.
func appendFloat(b []byte, f float64) []byte {
	switch {
	case math.IsNaN(f):
		return append(b, `"NaN"`...)
	case math.IsInf(f, 1):
		return append(b, `"+Inf"`...)
	case math.IsInf(f, -1):
		return append(b, `"-Inf"`...)
	}
	format := byte('f')
	if abs := math.Abs(f); abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	b = strconv.AppendFloat(b, f, format, -1, 64)
	if format == 'e' {
		// 1e-07 is 1e-7.
		if n := len(b); n >= 4 && b[n-4] == 'e' && b[n-3] == '-' && b[n-2] == '0' {
			b[n-2] = b[n-1]
			b = b[:n-1]
		}
	}
	return b
}

// appendString writes a valid UTF-8 string as a JSON string: a quote, a backslash and each control character below
// U+0020 escaped, nothing else.
func appendString(b []byte, s string) []byte {
	const hex = "0123456789abcdef"
	b = append(b, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		b = append(b, s[start:i]...)
		switch c {
		case '"', '\\':
			b = append(b, '\\', c)
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		default:
			b = append(b, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
		}
		start = i + 1
	}
	b = append(b, s[start:]...)
	return append(b, '"')
}

// normalize turns the value of an attribute into one Line writes and OTLP sends: a string, an int64, a uint64, a
// float64, a bool, nil, or a list of these as []slog.Value. A duration is its nanoseconds, a time RFC 3339 in UTC with
// nine digits, an error its message, a type of string kind its string, a list its items. A string that is not valid
// UTF-8 goes under the key and _b64, as base64 of its bytes, as specs.md §29.3 writes a path; a string in a list
// takes a U+FFFD for each run of bytes that are not.
func normalize(key string, v slog.Value) (string, slog.Value) {
	v = v.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return stringAttr(key, v.String())
	case slog.KindDuration:
		return key, slog.Int64Value(int64(v.Duration()))
	case slog.KindTime:
		return key, slog.StringValue(v.Time().UTC().Format("2006-01-02T15:04:05.000000000Z"))
	case slog.KindAny:
		return anyAttr(key, v.Any())
	case slog.KindInt64, slog.KindUint64, slog.KindFloat64, slog.KindBool, slog.KindGroup, slog.KindLogValuer:
		// As they are: a group is flattened before, and Resolve leaves no LogValuer.
	}
	return key, v
}

func stringAttr(key, s string) (string, slog.Value) {
	if utf8.ValidString(s) {
		return key, slog.StringValue(s)
	}
	return key + "_b64", slog.StringValue(base64.StdEncoding.EncodeToString([]byte(s)))
}

func anyAttr(key string, x any) (string, slog.Value) {
	switch y := x.(type) {
	case nil:
		return key, slog.AnyValue(nil)
	case error:
		return stringAttr(key, y.Error())
	case []byte:
		return stringAttr(key, string(y))
	case fmt.Stringer:
		return stringAttr(key, y.String())
	case encoding.TextMarshaler:
		text, err := y.MarshalText()
		if err != nil {
			return stringAttr(key, fmt.Sprintf("%+v", x))
		}
		return stringAttr(key, string(text))
	}
	r := reflect.ValueOf(x)
	kind := r.Kind()
	if kind == reflect.String {
		return stringAttr(key, r.String())
	}
	if kind == reflect.Slice || kind == reflect.Array {
		list := make([]slog.Value, 0, r.Len())
		for i := range r.Len() {
			list = append(list, item(r.Index(i).Interface()))
		}
		return key, slog.AnyValue(list)
	}
	if (kind == reflect.Pointer || kind == reflect.Interface) && r.IsNil() {
		return key, slog.AnyValue(nil)
	}
	if v, ok := scalar(r); ok {
		return key, v
	}
	return stringAttr(key, fmt.Sprintf("%+v", x))
}

// item is a value of a list: as an attribute's, with a string made valid UTF-8 where it cannot change its key, and a
// list in a list written as text.
func item(x any) slog.Value {
	_, v := anyAttr("", x)
	if v.Kind() == slog.KindString {
		return slog.StringValue(strings.ToValidUTF8(stringOf(x), "\uFFFD"))
	}
	if _, nested := v.Any().([]slog.Value); v.Kind() == slog.KindAny && nested {
		return slog.StringValue(strings.ToValidUTF8(fmt.Sprintf("%v", x), "\uFFFD"))
	}
	return v
}

// stringOf gives the string an item stands for, before base64.
func stringOf(x any) string {
	switch y := x.(type) {
	case error:
		return y.Error()
	case []byte:
		return string(y)
	case fmt.Stringer:
		return y.String()
	}
	if r := reflect.ValueOf(x); r.Kind() == reflect.String {
		return r.String()
	}
	return fmt.Sprintf("%+v", x)
}

// scalar gives a number or a bool of any type of that kind, and says whether it was one.
func scalar(r reflect.Value) (slog.Value, bool) {
	switch {
	case r.Kind() == reflect.Bool:
		return slog.BoolValue(r.Bool()), true
	case r.CanInt():
		return slog.Int64Value(r.Int()), true
	case r.CanUint():
		return slog.Uint64Value(r.Uint()), true
	case r.CanFloat():
		return slog.Float64Value(r.Float()), true
	}
	return slog.Value{}, false
}

// flatten adds attributes to a list, under the groups open: a key of a group is dotted, a group with an empty key is
// inline, an empty group and an attribute with neither key nor value are none (slog.Handler's rules). A key already
// there takes the new value where it stood.
func flatten(list []slog.Attr, prefix string, attrs ...slog.Attr) []slog.Attr {
	for _, a := range attrs {
		a.Value = a.Value.Resolve()
		if a.Value.Kind() == slog.KindGroup {
			group := a.Value.Group()
			if len(group) == 0 {
				continue
			}
			inner := prefix
			if a.Key != "" {
				inner = prefix + a.Key + "."
			}
			list = flatten(list, inner, group...)
			continue
		}
		if a.Equal(slog.Attr{}) {
			continue
		}
		key, v := normalize(prefix+a.Key, a.Value)
		list = put(list, slog.Attr{Key: key, Value: v})
	}
	return list
}

// put adds an attribute, or gives a key already there its new value.
func put(list []slog.Attr, a slog.Attr) []slog.Attr {
	for i := range list {
		if list[i].Key == a.Key {
			list[i].Value = a.Value
			return list
		}
	}
	return append(list, a)
}
