package fsx

import (
	"testing"
	"time"
)

// What is no SCSI disk answers SG_IO with an error, which says nothing of a power mode; a name that is no disk's is
// refused before anything is read.
func TestCheckPowerModeOfWhatIsNoDisk(t *testing.T) {
	if sense, err := CheckPowerMode("/dev/null", time.Second); err == nil {
		t.Errorf("/dev/null answered %x", sense)
	}
	for _, name := range []string{"", "..", "sda/../../etc"} {
		if _, err := ReadBlockStat(name); err == nil {
			t.Errorf("%q was read", name)
		}
		if _, err := Rotational(name); err == nil {
			t.Errorf("%q was read", name)
		}
	}
}
