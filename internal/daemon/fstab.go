package daemon

import (
	"path"
	"strconv"
	"strings"
)

// fstabHas says whether a table in the form of /etc/fstab has an entry whose mount point is that path: the
// remount after a USB reset goes through it, with the administrator's options (specs.md §15.13). Fields are
// split on blanks, and \040, \011, \012 and \134 are read back as fstab(5) writes them.
func fstabHas(table []byte, mount string) bool {
	want := path.Clean(mount)
	for _, line := range strings.Split(string(table), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if path.Clean(fstabUnescape(fields[1])) == want {
			return true
		}
	}
	return false
}

// fstabUnescape reads the octal escapes of fstab(5).
func fstabUnescape(field string) string {
	var b strings.Builder
	for i := 0; i < len(field); i++ {
		if field[i] == '\\' && i+3 < len(field) {
			if n, err := strconv.ParseUint(field[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(field[i])
	}
	return b.String()
}
