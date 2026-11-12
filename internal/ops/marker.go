package ops

import (
	"bytes"
	"errors"
	"fmt"
	"io"

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
