package ops

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/peondusud/atoll/internal/fsx"
	"github.com/peondusud/atoll/internal/layout"
)

// ErrOtherMarker says that a disk holds a marker other than the one to write.
var ErrOtherMarker = errors.New("the disk holds another marker")

// ReadMarker reads the disk's Atoll/disk.toml, and says whether there is one.
func (d *Disk) ReadMarker() (text []byte, found bool, err error) {
	f, err := d.open(components(d.config.Workdir))
	if fsx.IsAbsent(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.close()
	file, err := f.OpenFile(layout.MarkerName)
	if fsx.IsAbsent(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, classify(fmt.Errorf("open %q: %w", layout.MarkerName, err))
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, 64<<10))
	if err != nil {
		return nil, false, classify(fmt.Errorf("read %q: %w", layout.MarkerName, err))
	}
	return data, true, nil
}

// WriteMarker writes the disk's Atoll/disk.toml (specs.md §10): the work folder made when it is missing, then the
// text in <name>.new, synced, renamed to its name without replacing anything, and the folder synced. A marker
// already there with the same text is left as it is; one with another is ErrOtherMarker.
func (d *Disk) WriteMarker(text string) error {
	old, found, err := d.ReadMarker()
	switch {
	case err != nil:
		return err
	case found && bytes.Equal(old, []byte(text)):
		return nil
	case found:
		return ErrOtherMarker
	}
	f, err := d.makeFolders(components(d.config.Workdir), nil)
	if err != nil {
		return err
	}
	defer f.close()
	part := layout.MarkerName + ".new"
	if err := f.Unlink(part); err != nil && !fsx.IsAbsent(err) {
		return classify(fmt.Errorf("unlink %q: %w", part, err))
	}
	w, err := f.CreateFile(part, 0o644)
	if err != nil {
		return classify(fmt.Errorf("create %q: %w", part, err))
	}
	_, err = w.Write([]byte(text))
	if err == nil {
		err = w.Sync()
	}
	if closeErr := w.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = f.Unlink(part)
		return classify(fmt.Errorf("write %q: %w", part, err))
	}
	if err := f.Rename(part, f.Dir, layout.MarkerName); err != nil {
		_ = f.Unlink(part)
		return classify(fmt.Errorf("rename %q: %w", part, err))
	}
	if err := f.Sync(); err != nil {
		return classify(fmt.Errorf("sync: %w", err))
	}
	return nil
}

// CatalogFolder holds the latest catalog backup, in the work folder of each disk, with maintenance.catalog_on_disks
// (specs.md §9).
const CatalogFolder = "catalog"

// CatalogName is that backup's name.
const CatalogName = "catalog.db"

// PutCatalog writes a catalog backup in the disk's Atoll/catalog/: the folders made when missing, then the copy in
// <name>.new, synced, then in place of the one before, and the folder synced. A crash leaves the one before, or the
// new one, and at worst a <name>.new the next backup replaces.
func (d *Disk) PutCatalog(src io.Reader) error {
	f, err := d.makeFolders(components(d.config.Workdir, CatalogFolder), nil)
	if err != nil {
		return err
	}
	defer f.close()
	part := CatalogName + ".new"
	if err := f.Unlink(part); err != nil && !fsx.IsAbsent(err) {
		return classify(fmt.Errorf("unlink %q: %w", part, err))
	}
	w, err := f.CreateFile(part, 0o600)
	if err != nil {
		return classify(fmt.Errorf("create %q: %w", part, err))
	}
	_, err = io.Copy(w, src)
	if err == nil {
		err = w.Sync()
	}
	if closeErr := w.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = f.Unlink(part)
		return classify(fmt.Errorf("write %q: %w", part, err))
	}
	if err := f.Unlink(CatalogName); err != nil && !fsx.IsAbsent(err) {
		return classify(fmt.Errorf("unlink %q: %w", CatalogName, err))
	}
	if err := f.Rename(part, f.Dir, CatalogName); err != nil {
		return classify(fmt.Errorf("rename %q: %w", part, err))
	}
	if err := f.Sync(); err != nil {
		return classify(fmt.Errorf("sync: %w", err))
	}
	return nil
}

// HighestOpID gives the highest operation id the disk's work folder names: a folder of its trash,
// trash/<date>/<op id>/, or a copy in progress, tmp/<op id>.part; 0 when there is none. A new catalog starts its
// operations past it, so that a new one never meets an old one's folder or part (specs.md §9, §17).
func (d *Disk) HighestOpID() (int64, error) {
	highest := int64(0)
	trash, err := d.open(components(d.config.Workdir, layout.TrashFolder))
	switch {
	case fsx.IsAbsent(err):
	case err != nil:
		return 0, err
	default:
		days, err := trash.Names()
		if err != nil {
			trash.close()
			return 0, classify(fmt.Errorf("list the trash: %w", err))
		}
		for _, day := range days {
			below, err := trash.OpenDir(day)
			if err != nil {
				continue
			}
			ids, err := below.Names()
			_ = below.Close()
			if err != nil {
				trash.close()
				return 0, classify(fmt.Errorf("list the trash: %w", err))
			}
			for _, id := range ids {
				if n, err := strconv.ParseInt(id, 10, 64); err == nil && n > highest {
					highest = n
				}
			}
		}
		trash.close()
	}
	tmp, err := d.open(components(d.config.Workdir, layout.TmpFolder))
	switch {
	case fsx.IsAbsent(err):
		return highest, nil
	case err != nil:
		return 0, err
	}
	defer tmp.close()
	parts, err := tmp.Names()
	if err != nil {
		return 0, classify(fmt.Errorf("list tmp: %w", err))
	}
	for _, name := range parts {
		if id, ok := strings.CutSuffix(name, ".part"); ok {
			if n, err := strconv.ParseInt(id, 10, 64); err == nil && n > highest {
				highest = n
			}
		}
	}
	return highest, nil
}
