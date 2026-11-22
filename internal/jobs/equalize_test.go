package jobs

import (
	"testing"

	"github.com/peondusud/atoll/internal/watch"
)

// With balance.equalize = "percent", Atoll's copy goes to the disk with the most free share of its capacity, whatever
// placement.new_files says: ordered would take t2, the first below fill_to, and the most free t2 too, which has more
// bytes free. Off, new_files decides (specs.md §12.4).
func TestEqualizePlacesAtollsCopies(t *testing.T) {
	const tib = int64(1) << 40
	for _, tc := range []struct{ equalize, want string }{{"percent", "t3"}, {"off", "t2"}} {
		t.Run(tc.equalize, func(t *testing.T) {
			b := newBenchWith(t, "[defaults]\nsettle = \"0s\"\n[placement]\nnew_files = \"ordered\"\n[balance]\nequalize = \""+tc.equalize+"\"\n", "t1", "t2", "t3")
			b.spaces = map[string][2]int64{b.dirs["t1"]: {tib, tib * 9 / 10}, b.dirs["t2"]: {4 * tib, tib}, b.dirs["t3"]: {tib, tib * 8 / 10}}
			b.write("t1", "a", "x")
			b.scan(false)
			b.pass()
			if got := b.holders("a"); got != "t1:user:ok "+tc.want+":atoll:ok" {
				t.Errorf("copies %q, want the copy on %s", got, tc.want)
			}
		})
	}
}

// atoll capacity plans as Atoll would place the copies, balance.equalize included (specs.md §13.3): the copy of a's goes
// to t3, the most free share, rather than t2, the most free, and the free space that plan leaves is not the same.
func TestCapacityPlansWithEqualize(t *testing.T) {
	reach := map[string]int64{}
	for _, equalize := range []string{"percent", "off"} {
		b := newBenchWith(t, "[placement]\nmin_free = [\"0%\"]\n[balance]\nequalize = \""+equalize+"\"\n", "t1", "t2", "t3")
		b.spaces = map[string][2]int64{b.dirs["t1"]: {1000, 100}, b.dirs["t2"]: {4000, 1000}, b.dirs["t3"]: {110, 100}}
		b.write("t1", "a", "0123456789")
		b.scan(false)
		levels, err := watch.Capacity(t.Context(), b.db, b.config, b.Now())
		if err != nil {
			t.Fatal(err)
		}
		reach[equalize] = levels[1].Reachable
	}
	if reach["percent"] == reach["off"] {
		t.Errorf("the same reach at 2 copies, %d, with balance.equalize and without", reach["off"])
	}
}
