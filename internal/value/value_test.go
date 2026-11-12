package value

import (
	"testing"
	"time"

	"github.com/peondusud/atoll/internal/vectors"
)

func TestDurationVectors(t *testing.T) {
	type vector struct {
		Input       string `json:"input"`
		Nanoseconds *int64 `json:"nanoseconds"`
		Error       bool   `json:"error"`
		Note        string `json:"note"`
	}
	for _, v := range vectors.Load[vector](t, "duration.json") {
		got, err := ParseDuration(v.Input)
		switch {
		case v.Error == (v.Nanoseconds != nil):
			t.Errorf("vector %q: want exactly one of nanoseconds and error", v.Input)
		case v.Error && err == nil:
			t.Errorf("ParseDuration(%q) = %d, want an error", v.Input, got)
		case !v.Error && err != nil:
			t.Errorf("ParseDuration(%q): %v", v.Input, err)
		case !v.Error && int64(got) != *v.Nanoseconds:
			t.Errorf("ParseDuration(%q) = %d, want %d", v.Input, got, *v.Nanoseconds)
		}
	}
}

func TestSizeVectors(t *testing.T) {
	type vector struct {
		Input string `json:"input"`
		Bytes *int64 `json:"bytes"`
		Error bool   `json:"error"`
		Note  string `json:"note"`
	}
	for _, v := range vectors.Load[vector](t, "size.json") {
		got, err := ParseSize(v.Input)
		switch {
		case v.Error == (v.Bytes != nil):
			t.Errorf("vector %q: want exactly one of bytes and error", v.Input)
		case v.Error && err == nil:
			t.Errorf("ParseSize(%q) = %d, want an error", v.Input, got)
		case !v.Error && err != nil:
			t.Errorf("ParseSize(%q): %v", v.Input, err)
		case !v.Error && got != *v.Bytes:
			t.Errorf("ParseSize(%q) = %d, want %d", v.Input, got, *v.Bytes)
		}
	}
}

func TestPercentVectors(t *testing.T) {
	type vector struct {
		Input  string `json:"input"`
		Of     *int64 `json:"of"`
		Result *int64 `json:"result"`
		Error  bool   `json:"error"`
		Note   string `json:"note"`
	}
	for _, v := range vectors.Load[vector](t, "percent.json") {
		p, err := ParsePercent(v.Input)
		switch {
		case v.Error == (v.Of != nil) || (v.Of == nil) != (v.Result == nil):
			t.Errorf("vector %q: want of and result, or error", v.Input)
		case v.Error && err == nil:
			t.Errorf("ParsePercent(%q) succeeded, want an error", v.Input)
		case !v.Error && err != nil:
			t.Errorf("ParsePercent(%q): %v", v.Input, err)
		case !v.Error:
			if got := p.Of(*v.Of); got != *v.Result {
				t.Errorf("ParsePercent(%q).Of(%d) = %d, want %d", v.Input, *v.Of, got, *v.Result)
			}
			if p.String() != v.Input {
				t.Errorf("ParsePercent(%q).String() = %q", v.Input, p.String())
			}
		}
	}
}

func TestWindowVectors(t *testing.T) {
	type vector struct {
		Input  string  `json:"input"`
		At     *string `json:"at"`
		Inside *bool   `json:"inside"`
		Next   *string `json:"next"`
		Error  bool    `json:"error"`
		Note   string  `json:"note"`
	}
	for _, v := range vectors.Load[vector](t, "window.json") {
		w, err := ParseWindow(v.Input)
		switch {
		case v.Error == (v.Inside != nil) || (v.At == nil) != (v.Inside == nil):
			t.Errorf("vector %q: want at and inside, or error", v.Input)
		case v.Error && err == nil:
			t.Errorf("ParseWindow(%q) = %v, want an error", v.Input, w)
		case !v.Error && err != nil:
			t.Errorf("ParseWindow(%q): %v", v.Input, err)
		case !v.Error:
			at, err := time.Parse(time.RFC3339Nano, *v.At)
			if err != nil {
				t.Errorf("vector %q: at: %v", v.Input, err)
				continue
			}
			if got := w.Contains(at); got != *v.Inside {
				t.Errorf("ParseWindow(%q).Contains(%s) = %v, want %v", v.Input, *v.At, got, *v.Inside)
			}
			if v.Next != nil {
				next, err := time.Parse(time.RFC3339Nano, *v.Next)
				if err != nil {
					t.Errorf("vector %q: next: %v", v.Input, err)
				} else if got := w.Next(at); !got.Equal(next) {
					t.Errorf("ParseWindow(%q).Next(%s) = %s, want %s", v.Input, *v.At, got.Format(time.RFC3339Nano), *v.Next)
				}
			}
			if w.String() != v.Input {
				t.Errorf("ParseWindow(%q).String() = %q", v.Input, w.String())
			}
		}
	}
}

// The daylight-saving vectors of window.json stand for real Europe/Paris
// instants. This checks that their offsets are the right ones.
func TestWindowVectorsDaylightSaving(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Skipf("no time zone database: %v", err)
	}
	for _, at := range []string{
		"2026-03-29T01:59:59+01:00",
		"2026-03-29T03:00:00+02:00",
		"2026-10-25T02:30:00+02:00",
		"2026-10-25T02:30:00+01:00",
		"2026-10-25T02:15:00+02:00",
		"2026-10-25T02:15:00+01:00",
	} {
		ts, err := time.Parse(time.RFC3339, at)
		if err != nil {
			t.Fatal(err)
		}
		if got := ts.In(paris).Format(time.RFC3339); got != at {
			t.Errorf("%s in Europe/Paris is %s", at, got)
		}
	}
}

func TestWindowZeroValue(t *testing.T) {
	var w Window
	at := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	for range 24 * 60 {
		if w.Contains(at) {
			t.Fatalf("the zero Window contains %s", at.Format("15:04"))
		}
		at = at.Add(time.Minute)
	}
}

func TestPercentZeroValue(t *testing.T) {
	var p Percent
	if got := p.Of(1000); got != 0 {
		t.Errorf("Percent{}.Of(1000) = %d, want 0", got)
	}
	if got := p.String(); got != "0%" {
		t.Errorf("Percent{}.String() = %q, want 0%%", got)
	}
}
