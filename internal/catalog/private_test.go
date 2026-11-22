package catalog

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The catalog lists every path of the library, its trash and its history: the folders made for it, the database, its
// write-ahead log, its shared memory and its backups are closed to other accounts, whatever the umask, the account's
// group reading them, as the unit's StateDirectoryMode=0750 has /var/lib/atoll (specs.md §25, §26;
// docs/threat-model.md, G20). Under the umask 0022 of a shell, a catalog elsewhere was a folder 0755 and a database
// 0644, readable by every account; under 0000, so were its log and its backups. A folder that was there is left as it
// is: it may hold more than the catalog.
func TestTheCatalogIsClosedToOtherAccounts(t *testing.T) {
	for _, mask := range []int{0o000, 0o022, 0o077} {
		old := syscall.Umask(mask)
		top := t.TempDir()
		if err := os.Chmod(top, 0o755); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(top, "deep", "er", "catalog.db")
		if _, err := Create(t.Context(), file, "p"); err != nil {
			syscall.Umask(old)
			t.Fatal(err)
		}
		db, err := OpenPool(t.Context(), file, "p")
		if err != nil {
			syscall.Umask(old)
			t.Fatal(err)
		}
		exec(t, db, `INSERT INTO files(lpath, size, mtime_ns, replicas_target, mode, state, first_seen_ns, updated_ns)
			VALUES (x'61', 0, 0, 1, 'deferred', 'ok', 0, 0)`)
		written, _, err := Backup(t.Context(), db, file, "p", time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC).UnixNano())
		if err != nil {
			syscall.Umask(old)
			t.Fatal(err)
		}
		want := map[string]os.FileMode{
			filepath.Join(top, "deep"):       os.ModeDir | 0o750,
			filepath.Join(top, "deep", "er"): os.ModeDir | 0o750,
			file:                             0o640,
			file + "-wal":                    0o640,
			file + "-shm":                    0o640,
			filepath.Join(top, "deep", "er", BackupFolder): os.ModeDir | 0o750,
			written: 0o640,
			top:     os.ModeDir | 0o755,
		}
		for path, mode := range want {
			switch info, err := os.Lstat(path); {
			case err != nil:
				t.Errorf("umask %04o: %v", mask, err)
			case info.Mode() != mode:
				t.Errorf("umask %04o: %s is %v, want %v", mask, path, info.Mode(), mode)
			}
		}
		_ = db.Close()
		syscall.Umask(old)
	}
}
