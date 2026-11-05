package names

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/peondusud/atoll/internal/vectors"
)

// The generated table (upcase_table.go, from upcase_gen.go) must be the $UpCase that Windows 7 to 10 and mkntfs
// write, byte for byte: 65536 little-endian 16-bit entries. The MD5 is the one
// ntfs-3g records for it (libntfs-3g/unistr.c); both sums were checked on
// volumes formatted by Windows 10 (docs/research/nfc-and-case-folding.md §4.3).
func TestUpcaseTableIsWindows7To10(t *testing.T) {
	const (
		wantSHA256 = "41c26bc7a12bdaeb26025c93118697c7e3ef81ee048b00fe5cce2a472e0e0742"
		wantMD5    = "7ff498a44e45e77374cc7c962b1b92f2"
	)
	table := make([]byte, 2*65536)
	changed := 0
	for u := range 65536 {
		up := ntfsUpcase(rune(u))
		if up < 0 || up > 0xFFFF {
			t.Fatalf("ntfsUpcase(%U) = %U, outside the BMP", u, up)
		}
		if up != rune(u) {
			changed++
		}
		binary.LittleEndian.PutUint16(table[2*u:], uint16(up))
	}
	if changed != upcaseEntries {
		t.Errorf("%d characters change, want %d", changed, upcaseEntries)
	}
	if sum := sha256.Sum256(table); hex.EncodeToString(sum[:]) != wantSHA256 {
		t.Errorf("SHA-256 of the table = %x, want %s", sum, wantSHA256)
	}
	if sum := md5.Sum(table); hex.EncodeToString(sum[:]) != wantMD5 {
		t.Errorf("MD5 of the table = %x, want %s", sum, wantMD5)
	}
}

type keyVector struct {
	Path    *string `json:"path"`
	PathB64 *string `json:"path_b64"`
	Key     *string `json:"key"`
	NoKey   bool    `json:"no_key"`
	Note    string  `json:"note"`
}

func TestCollisionKeyVectors(t *testing.T) {
	for _, v := range vectors.Load[keyVector](t, "lpath_key.json") {
		path, err := vectors.Path(v.Path, v.PathB64)
		if err != nil {
			t.Errorf("vector: %v", err)
			continue
		}
		if (v.Key == nil) == !v.NoKey {
			t.Errorf("vector %q: want exactly one of key and no_key", path)
			continue
		}
		key, ok := CollisionKey(path)
		switch {
		case v.NoKey && ok:
			t.Errorf("CollisionKey(%q) = %q, want no key", path, key)
		case !v.NoKey && !ok:
			t.Errorf("CollisionKey(%q): no key, want %q", path, *v.Key)
		case !v.NoKey && key != *v.Key:
			t.Errorf("CollisionKey(%q) = %q, want %q", path, key, *v.Key)
		}
	}
}

// Every pair of characters the collision table merges is also merged by
// Unicode simple case folding, so ignore_case rules treat them alike
// (docs/research/nfc-and-case-folding.md §4.5).
func TestUpcaseWithinSimpleFolding(t *testing.T) {
	for u := range rune(0x10000) {
		if up := collisionUpcase(u); up != u && !FoldEqual(u, up) {
			t.Errorf("collisionUpcase(%U) = %U, which simple case folding keeps apart", u, up)
		}
	}
}
