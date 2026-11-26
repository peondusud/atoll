package integrity

import (
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

type mismatchVector struct {
	Note        string  `json:"note"`
	Verified    *bool   `json:"verified"`
	PrecisionNS *int64  `json:"precision_ns"`
	Verdict     *string `json:"verdict"`
}

func TestScrubMismatchVectors(t *testing.T) {
	for i, v := range vectors.Load[mismatchVector](t, "scrub_mismatch.json") {
		if v.Verified == nil || v.PrecisionNS == nil || v.Verdict == nil {
			t.Fatalf("vector %d: want verified, precision_ns and verdict", i)
		}
		if got := ScrubMismatch(*v.Verified, *v.PrecisionNS); string(got) != *v.Verdict {
			t.Errorf("vector %d (%s): %q, want %q", i, v.Note, got, *v.Verdict)
		}
		if vouch := TimesVouch(*v.PrecisionNS); *v.Verified && vouch != (*v.Verdict == string(MismatchCorrupt)) {
			t.Errorf("vector %d (%s): times vouch %v", i, v.Note, vouch)
		}
	}
}
