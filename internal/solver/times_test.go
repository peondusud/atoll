package solver

import (
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

type timeVector struct {
	Seconds     *int64 `json:"seconds"`
	Nanoseconds *int64 `json:"nanoseconds"`
	NS          *int64 `json:"ns"`
	Error       bool   `json:"error"`
	Note        string `json:"note"`
}

func TestTimeNSVectors(t *testing.T) {
	for i, v := range vectors.Load[timeVector](t, "time_ns.json") {
		if v.Seconds == nil || v.Nanoseconds == nil {
			t.Errorf("vector %d (%s): want seconds and nanoseconds", i, v.Note)
			continue
		}
		if v.Error == (v.NS != nil) {
			t.Errorf("vector %d (%s): want exactly one of ns and error", i, v.Note)
			continue
		}
		ns, err := TimeNS(*v.Seconds, *v.Nanoseconds)
		switch {
		case v.Error:
			if err == nil {
				t.Errorf("vector %d (%s): got %d, want an error", i, v.Note, ns)
			}
		case err != nil || ns != *v.NS:
			t.Errorf("vector %d (%s): got %d, %v, want %d", i, v.Note, ns, err, *v.NS)
		}
	}
}

type creationVector struct {
	Seconds     *int64 `json:"seconds"`
	Nanoseconds *int64 `json:"nanoseconds"`
	NS          *int64 `json:"ns"`
	Absent      bool   `json:"absent"`
	Error       bool   `json:"error"`
	Note        string `json:"note"`
}

func TestCreationTimeNSVectors(t *testing.T) {
	for i, v := range vectors.Load[creationVector](t, "creation_time.json") {
		if v.Seconds == nil || v.Nanoseconds == nil {
			t.Errorf("vector %d (%s): want seconds and nanoseconds", i, v.Note)
			continue
		}
		outcomes := 0
		for _, set := range []bool{v.NS != nil, v.Absent, v.Error} {
			if set {
				outcomes++
			}
		}
		if outcomes != 1 {
			t.Errorf("vector %d (%s): want exactly one of ns, absent and error", i, v.Note)
			continue
		}
		ns, ok, err := CreationTimeNS(*v.Seconds, *v.Nanoseconds)
		switch {
		case v.Error:
			if err == nil {
				t.Errorf("vector %d (%s): got %d, %v, want an error", i, v.Note, ns, ok)
			}
		case err != nil:
			t.Errorf("vector %d (%s): %v", i, v.Note, err)
		case v.Absent:
			if ok {
				t.Errorf("vector %d (%s): got %d, want absent", i, v.Note, ns)
			}
		case !ok || ns != *v.NS:
			t.Errorf("vector %d (%s): got %d, %v, want %d", i, v.Note, ns, ok, *v.NS)
		}
	}
}

type sameTimeVector struct {
	A         *int64 `json:"a"`
	B         *int64 `json:"b"`
	Precision *int64 `json:"precision_ns"`
	Same      *bool  `json:"same"`
	Note      string `json:"note"`
}

func TestSameTimeVectors(t *testing.T) {
	for i, v := range vectors.Load[sameTimeVector](t, "same_time.json") {
		if v.A == nil || v.B == nil || v.Precision == nil || v.Same == nil {
			t.Errorf("vector %d (%s): want a, b, precision_ns and same", i, v.Note)
			continue
		}
		if got := SameTime(*v.A, *v.B, *v.Precision); got != *v.Same {
			t.Errorf("vector %d (%s): got %v", i, v.Note, got)
		}
		if got := SameTime(*v.B, *v.A, *v.Precision); got != *v.Same {
			t.Errorf("vector %d (%s), the other way round: got %v", i, v.Note, got)
		}
	}
}

// The coarser precision wins, and an unknown one is exact.
func TestCoarser(t *testing.T) {
	for _, c := range [][3]int64{{1, 100, 100}, {100, 1, 100}, {0, 100, 100}, {0, 0, 1}, {1_000_000_000, 100, 1_000_000_000}} {
		if got := Coarser(c[0], c[1]); got != c[2] {
			t.Errorf("Coarser(%d, %d) = %d, want %d", c[0], c[1], got, c[2])
		}
	}
}
