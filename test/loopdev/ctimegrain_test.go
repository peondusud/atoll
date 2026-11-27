//go:build loopdev

package loopdev

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestTheChangeTimeGranularityOfEachDriver measures, on each filesystem and driver, how far apart two changes of one
// file may be and still carry one change time, as a look between them sees it (docs/research/ctime-granularity.md): a
// byte written in place, then the file looked at, 4,000 times in a row, then again with a pause of 1 ms between two. A
// kernel without the multigrain timestamps of Linux 6.13 gives a change the time of its clock's coarse tick, whatever
// the filesystem holds; one with them gives a change after a look a time of its own. It logs what it found and fails only
// when it cannot measure: what it finds is the kernel's, for docs/research/ to record.
func TestTheChangeTimeGranularityOfEachDriver(t *testing.T) {
	needRoot(t)
	var u syscall.Utsname
	mustDo(t, syscall.Uname(&u))
	b := make([]byte, 0, len(u.Release))
	for _, c := range u.Release {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	release := string(b)
	for _, tc := range repairDrivers {
		t.Run(tc.name, func(t *testing.T) {
			options := driverOptions(t, tc.driver)
			v := newVolume(t, tc.fs)
			at := mountPoint(t)
			mountVolume(t, v, tc.driver, at, options...)
			path := filepath.Join(at, "f")
			mustDo(t, os.WriteFile(path, []byte("x"), 0o644))
			for _, pause := range []time.Duration{0, time.Millisecond} {
				f, err := os.OpenFile(path, os.O_WRONLY, 0)
				mustDo(t, err)
				const n = 4000
				var last int64
				var lastWall time.Time
				same, maxSame, minStep := 0, time.Duration(0), int64(0)
				for i := range n {
					if pause > 0 {
						time.Sleep(pause)
					}
					_, err := f.WriteAt([]byte{byte(i)}, 0)
					mustDo(t, err)
					wall := time.Now()
					var st syscall.Stat_t
					mustDo(t, syscall.Stat(path, &st))
					ctime := st.Ctim.Nano()
					if i > 0 {
						switch gap := wall.Sub(lastWall); {
						case ctime == last:
							same++
							maxSame = max(maxSame, gap)
						case minStep == 0 || ctime-last < minStep:
							minStep = ctime - last
						}
					}
					last, lastWall = ctime, wall
				}
				mustDo(t, f.Close())
				t.Logf("Linux %s, %s, a pause of %v: %d of %d changes after a look carried the change time of the one before, "+
					"the farthest apart %v; the smallest step between two change times %d ns",
					release, tc.name, pause, same, n-1, maxSame, minStep)
			}
		})
	}
}
