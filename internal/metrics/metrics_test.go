package metrics

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

func TestMetricsVectors(t *testing.T) {
	type vector struct {
		Note     string   `json:"note"`
		Snapshot Snapshot `json:"snapshot"`
		Text     string   `json:"text"`
	}
	for _, v := range vectors.Load[vector](t, "metrics.json") {
		before, err := json.Marshal(v.Snapshot)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Render(&v.Snapshot)
		if err != nil {
			t.Errorf("%s: %v", v.Note, err)
			continue
		}
		if string(got) != v.Text {
			t.Errorf("%s:\n%s\nwant:\n%s", v.Note, got, v.Text)
		}
		if after, _ := json.Marshal(v.Snapshot); !bytes.Equal(before, after) {
			t.Errorf("%s: Render changed the snapshot", v.Note)
		}
	}
}

// Every metric of the contract has a sample in some case of the vectors: none is pinned by its name alone.
func TestEveryMetricHasAVector(t *testing.T) {
	type vector struct {
		Note     string          `json:"note"`
		Snapshot json.RawMessage `json:"snapshot"`
		Text     string          `json:"text"`
	}
	families, err := Families()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, v := range vectors.Load[vector](t, "metrics.json") {
		for line := range strings.SplitSeq(v.Text, "\n") {
			if name, _, ok := strings.Cut(line, " "); ok && !strings.HasPrefix(line, "#") {
				name, _, _ = strings.Cut(name, "{")
				seen[name] = true
			}
		}
	}
	for _, f := range families {
		if !seen[f.Name] {
			t.Errorf("%s has no sample in conformance/vectors/metrics.json", f.Name)
		}
	}
}

// The contract is the one in the repository, and the binary reads it whole: what api/metrics.json says is what /metrics
// writes.
func TestFamiliesAreTheContract(t *testing.T) {
	families, err := Families()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Metrics []Family `json:"metrics"`
	}
	if err := json.Unmarshal(vectors.RepoFile(t, "api/metrics.json"), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Metrics) != len(families) {
		t.Fatalf("%d metrics, the contract has %d", len(families), len(doc.Metrics))
	}
	for i := range families {
		if f, want := families[i], doc.Metrics[i]; f.Name != want.Name || f.Type != want.Type || f.Help != want.Help || !slices.Equal(f.Labels, want.Labels) ||
			f.OTelName != want.OTelName || f.Unit != want.Unit {
			t.Errorf("%+v, the contract says %+v", f, want)
		}
	}
}

// The lists of states are those of the schemas: a state added there and not here would never be counted.
func TestStatesAreTheSchemas(t *testing.T) {
	var status struct {
		Defs struct {
			Disk struct {
				Properties struct {
					State struct {
						Enum []string `json:"enum"`
					} `json:"state"`
				} `json:"properties"`
			} `json:"disk"`
			File struct {
				Properties struct {
					State struct {
						Enum []string `json:"enum"`
					} `json:"state"`
				} `json:"properties"`
			} `json:"file"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(vectors.RepoFile(t, "api/schemas/status.schema.json"), &status); err != nil {
		t.Fatal(err)
	}
	if got, want := slices.Sorted(slices.Values(FileStates)), slices.Sorted(slices.Values(status.Defs.File.Properties.State.Enum)); !slices.Equal(got, want) {
		t.Errorf("file states %v, the status schema has %v", got, want)
	}
	// A disk of the configuration is never retired: atoll disk retire takes it out of the configuration.
	want := slices.DeleteFunc(slices.Clone(status.Defs.Disk.Properties.State.Enum), func(s string) bool { return s == "retired" })
	if got := slices.Sorted(slices.Values(DiskStates)); !slices.Equal(got, slices.Sorted(slices.Values(want))) {
		t.Errorf("disk states %v, the status schema has %v and retired", got, want)
	}
}

func TestSeconds(t *testing.T) {
	for ns, want := range map[int64]string{
		0: "0.000000000", 1: "0.000000001", 1_500_000_000: "1.500000000", -1: "-0.000000001",
		-9223372036854775808: "-9223372036.854775808", 9223372036854775807: "9223372036.854775807",
	} {
		if got := Seconds(ns); got != want {
			t.Errorf("Seconds(%d) = %s, want %s", ns, got, want)
		}
	}
}

// A sample of a metric the contract does not have, or with other labels, is an error, never a line Prometheus would
// read as another metric.
func TestRenderRefusesWhatTheContractLacks(t *testing.T) {
	families, err := Families()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := write(families, map[string][]Point{"atoll_passes_total": {{Value: 1}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := write(families, map[string][]Point{"atoll_nope_total": {{Value: 1}}}); err == nil {
		t.Error("a metric the contract does not have is written")
	}
	if _, err := write(families, map[string][]Point{"atoll_passes_total": {{Labels: []string{"t1"}, Value: 1}}}); err == nil {
		t.Error("a label the contract does not give the metric is written")
	}
}

// A disk that stays in the configuration keeps what was recorded of it; one that went is forgotten.
func TestConfigureKeepsTheDisksThatStay(t *testing.T) {
	var s Snapshot
	s.Configure([]Disk{{ID: "t1", FS: "ext4", Mount: "/mnt/t1"}, {ID: "t2", FS: "ext4", Mount: "/mnt/t2"}})
	s.DiskOf("t1").ScrubReadCopies = 4
	s.DiskOf("t2").ScrubReadCopies = 5
	s.Configure([]Disk{{ID: "t3", FS: "xfs", Mount: "/mnt/t3"}, {ID: "t1", FS: "ext4", Mount: "/srv/t1"}})
	if len(s.Disks) != 2 || s.Disks[0].ID != "t3" || s.Disks[1].ID != "t1" {
		t.Fatalf("disks %+v", s.Disks)
	}
	if d := s.DiskOf("t1"); d.ScrubReadCopies != 4 || d.Mount != "/srv/t1" {
		t.Errorf("t1 %+v: it keeps its counts and takes its new mount", d)
	}
	if s.DiskOf("t2") != nil || s.DiskOf("t3").ScrubReadCopies != 0 {
		t.Errorf("disks %+v", s.Disks)
	}
}

func TestAddOperationsCounts(t *testing.T) {
	var s Snapshot
	s.AddOperations(map[[2]string]int64{{"replicate", "replicated"}: 2})
	s.AddOperations(map[[2]string]int64{{"replicate", "replicated"}: 1, {"trash", "trashed"}: 3})
	slices.SortFunc(s.Operations, func(a, b Operation) int { return strings.Compare(a.Kind, b.Kind) })
	if want := []Operation{{"replicate", "replicated", 3}, {"trash", "trashed", 3}}; !slices.Equal(s.Operations, want) {
		t.Errorf("%+v, want %+v", s.Operations, want)
	}
}

// A request writes the snapshot while the daemon changes it.
func TestRecorderIsSafeToShare(t *testing.T) {
	r := NewRecorder("v")
	r.Update(func(s *Snapshot) { s.Configure([]Disk{{ID: "t1", State: "online"}}) })
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 200 {
			r.Update(func(s *Snapshot) {
				s.Passes++
				s.DiskOf("t1").ScrubReadBytes += 10
			})
		}
	})
	for range 200 {
		if _, err := r.Text(); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	text, err := r.Text()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(text), "atoll_passes_total 200\n") || !strings.Contains(string(text), `atoll_scrub_read_bytes_total{disk="t1"} 2000`+"\n") {
		t.Errorf("%s", text)
	}
	var none *Recorder
	none.Update(func(*Snapshot) { t.Error("a nil recorder records") })
}

// OpenTelemetry's Prometheus compatibility translation, as its specification gives it (specs.md §23.1).
func TestPrometheusNameVectors(t *testing.T) {
	type vector struct {
		Note       string `json:"note"`
		Name       string `json:"name"`
		Unit       string `json:"unit"`
		Type       string `json:"type"`
		Prometheus string `json:"prometheus"`
	}
	for _, v := range vectors.Load[vector](t, "otel_names.json") {
		if v.Type != "gauge" && v.Type != "counter" {
			t.Fatalf("%s: type %q", v.Name, v.Type)
		}
		if got := PrometheusName(v.Name, v.Unit, v.Type == "counter"); got != v.Prometheus {
			t.Errorf("%s %q %s: %s, want %s", v.Name, v.Unit, v.Type, got, v.Prometheus)
		}
	}
}

// Each metric's OpenTelemetry name and unit give its Prometheus name back, exactly: what an OTLP collector writes for
// Prometheus is what /metrics writes, so one query reads both. The Prometheus name is the contract (specs.md §29.3);
// none is an exception.
func TestEachOpenTelemetryNameGivesThePrometheusName(t *testing.T) {
	families, err := Families()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if got := PrometheusName(f.OTelName, f.Unit, f.Type == "counter"); got != f.Name {
			t.Errorf("%s (%s, %s) translates to %s", f.OTelName, f.Unit, f.Type, got)
		}
	}
}

// The rules of an OpenTelemetry name: dotted, lowercase, under atoll., none twice, none the namespace of another, as
// Elasticsearch maps a dotted name to objects; a unit of UCUM, a count in braces.
func TestOpenTelemetryNamesFollowTheRules(t *testing.T) {
	good := Family{Name: "atoll_a", Type: "gauge", Labels: []string{}, Help: "h", OTelName: "atoll.a", Unit: "{file}"}
	for _, tc := range []struct {
		note string
		list []Family
		ok   bool
	}{
		{"one", []Family{good}, true},
		{"no name", []Family{{Name: "atoll_a", Unit: "By"}}, false},
		{"not under atoll.", []Family{{Name: "atoll_a", OTelName: "a.b", Unit: "By"}}, false},
		{"atoll alone", []Family{{Name: "atoll_a", OTelName: "atoll", Unit: "By"}}, false},
		{"uppercase", []Family{{Name: "atoll_a", OTelName: "atoll.A", Unit: "By"}}, false},
		{"an empty part", []Family{{Name: "atoll_a", OTelName: "atoll..a", Unit: "By"}}, false},
		{"no unit", []Family{{Name: "atoll_a", OTelName: "atoll.a"}}, false},
		{"unit 1, which gives _ratio", []Family{{Name: "atoll_a", OTelName: "atoll.a", Unit: "1"}}, false},
		{"twice", []Family{good, {Name: "atoll_b", OTelName: "atoll.a", Unit: "By"}}, false},
		{"the namespace of another", []Family{good, {Name: "atoll_a_b", OTelName: "atoll.a.b", Unit: "By"}}, false},
		{"another's namespace, written after", []Family{{Name: "atoll_a_b", OTelName: "atoll.a.b", Unit: "By"}, good}, false},
		{"a word of another", []Family{good, {Name: "atoll_a_b", OTelName: "atoll.a_b", Unit: "By"}}, true},
	} {
		if err := checkOTelNames(tc.list); (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.note, err)
		}
	}
}

// Points are the samples /metrics writes, as numbers: OTLP sends the same values from the same snapshot (specs.md
// §23.1).
func TestPointsAreTheText(t *testing.T) {
	type vector struct {
		Note     string   `json:"note"`
		Snapshot Snapshot `json:"snapshot"`
		Text     string   `json:"text"`
	}
	families, err := Families()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vectors.Load[vector](t, "metrics.json") {
		points := Points(&v.Snapshot)
		var lines []string
		for _, f := range families {
			for _, p := range points[f.Name] {
				var b strings.Builder
				b.WriteString(f.Name)
				if len(f.Labels) > 0 {
					b.WriteByte('{')
					for i, l := range f.Labels {
						if i > 0 {
							b.WriteByte(',')
						}
						b.WriteString(l + `="` + escapeLabel(p.Labels[i]) + `"`)
					}
					b.WriteByte('}')
				}
				value := strconv.FormatInt(p.Value, 10)
				if p.Seconds {
					value = Seconds(p.Value)
				}
				lines = append(lines, b.String()+" "+value)
			}
		}
		var want []string
		for line := range strings.SplitSeq(v.Text, "\n") {
			if line != "" && !strings.HasPrefix(line, "#") {
				want = append(want, line)
			}
		}
		slices.Sort(lines)
		slices.Sort(want)
		if !slices.Equal(lines, want) {
			t.Errorf("%s:\n%s\nwant:\n%s", v.Note, strings.Join(lines, "\n"), strings.Join(want, "\n"))
		}
	}
}
