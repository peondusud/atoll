package fsx

// Fault wraps a folder so that a test can make any call fail (CLAUDE.md, "Testing rules"). fail
// gets the name of the call and the name it is about, and gives the error to return, or nil. A call on
// a folder itself is about the name it was opened by, which the top does not have.
func Fault(d Dir, fail func(call, name string) error) Dir {
	return &faultDir{Dir: d, fail: fail}
}

type faultDir struct {
	Dir
	name string
	fail func(call, name string) error
}

// Unwrap gives the folder it wraps.
func (d *faultDir) Unwrap() Dir { return d.Dir }

func (d *faultDir) Names() ([]string, error) {
	if err := d.fail("names", ""); err != nil {
		return nil, err
	}
	return d.Dir.Names()
}

func (d *faultDir) Stat(name string) (Stat, error) {
	if err := d.fail("stat", name); err != nil {
		return Stat{}, err
	}
	return d.Dir.Stat(name)
}

func (d *faultDir) OpenDir(name string) (Dir, error) {
	if err := d.fail("opendir", name); err != nil {
		return nil, err
	}
	below, err := d.Dir.OpenDir(name)
	if err != nil {
		return nil, err
	}
	return &faultDir{Dir: below, name: name, fail: d.fail}, nil
}

func (d *faultDir) OpenFile(name string) (File, error) {
	if err := d.fail("openfile", name); err != nil {
		return nil, err
	}
	f, err := d.Dir.OpenFile(name)
	if err != nil {
		return nil, err
	}
	return &faultFile{File: f, name: name, fail: d.fail}, nil
}

func (d *faultDir) ReadLink(name string) (string, error) {
	if err := d.fail("readlink", name); err != nil {
		return "", err
	}
	return d.Dir.ReadLink(name)
}

func (d *faultDir) Mkdir(name string, mode uint32) error {
	if err := d.fail("mkdir", name); err != nil {
		return err
	}
	return d.Dir.Mkdir(name, mode)
}

func (d *faultDir) Adopt(name string, like *Stat) error {
	if err := d.fail("adopt", name); err != nil {
		return err
	}
	return d.Dir.Adopt(name, like)
}

// Rename and Link are about the name they make, which is what a driver refuses.
func (d *faultDir) Rename(name string, to Dir, newName string) error {
	if err := d.fail("rename", newName); err != nil {
		return err
	}
	return d.Dir.Rename(name, to, newName)
}

func (d *faultDir) RenamePlain(name string, to Dir, newName string) error {
	if err := d.fail("plainrename", newName); err != nil {
		return err
	}
	return d.Dir.RenamePlain(name, to, newName)
}

func (d *faultDir) Link(name string, to Dir, newName string) error {
	if err := d.fail("link", newName); err != nil {
		return err
	}
	return d.Dir.Link(name, to, newName)
}

func (d *faultDir) Unlink(name string) error {
	if err := d.fail("unlink", name); err != nil {
		return err
	}
	return d.Dir.Unlink(name)
}

func (d *faultDir) Rmdir(name string) error {
	if err := d.fail("rmdir", name); err != nil {
		return err
	}
	return d.Dir.Rmdir(name)
}

func (d *faultDir) Sync() error {
	if err := d.fail("sync", d.name); err != nil {
		return err
	}
	return d.Dir.Sync()
}

// CreateFile and OpenWritable are about the name they open, and so is every call on what they open.
func (d *faultDir) CreateFile(name string, mode uint32) (WritableFile, error) {
	if err := d.fail("create", name); err != nil {
		return nil, err
	}
	f, err := d.Dir.CreateFile(name, mode)
	if err != nil {
		return nil, err
	}
	return &faultWritable{WritableFile: f, name: name, fail: d.fail}, nil
}

func (d *faultDir) OpenWritable(name string) (WritableFile, error) {
	if err := d.fail("openwrite", name); err != nil {
		return nil, err
	}
	f, err := d.Dir.OpenWritable(name)
	if err != nil {
		return nil, err
	}
	return &faultWritable{WritableFile: f, name: name, fail: d.fail}, nil
}

// SyncEntry is about the name it syncs.
func (d *faultDir) SyncEntry(name string) error {
	if err := d.fail("syncentry", name); err != nil {
		return err
	}
	return d.Dir.SyncEntry(name)
}

func (d *faultDir) SetMtime(t Time) error {
	if err := d.fail("setmtime", d.name); err != nil {
		return err
	}
	return d.Dir.SetMtime(t)
}

type faultFile struct {
	File
	name string
	fail func(call, name string) error
}

func (f *faultFile) Read(p []byte) (int, error) {
	if err := f.fail("read", f.name); err != nil {
		return 0, err
	}
	return f.File.Read(p)
}

func (f *faultFile) ReadAt(p []byte, off int64) (int, error) {
	if err := f.fail("read", f.name); err != nil {
		return 0, err
	}
	return f.File.ReadAt(p, off)
}

func (f *faultFile) Info() (Stat, error) {
	if err := f.fail("fileinfo", f.name); err != nil {
		return Stat{}, err
	}
	return f.File.Info()
}

type faultWritable struct {
	WritableFile
	name string
	fail func(call, name string) error
}

func (f *faultWritable) Write(p []byte) (int, error) {
	if err := f.fail("write", f.name); err != nil {
		return 0, err
	}
	return f.WritableFile.Write(p)
}

func (f *faultWritable) WriteAt(p []byte, off int64) (int, error) {
	if err := f.fail("write", f.name); err != nil {
		return 0, err
	}
	return f.WritableFile.WriteAt(p, off)
}

func (f *faultWritable) ReadAt(p []byte, off int64) (int, error) {
	if err := f.fail("readback", f.name); err != nil {
		return 0, err
	}
	return f.WritableFile.ReadAt(p, off)
}

func (f *faultWritable) Info() (Stat, error) {
	if err := f.fail("fileinfo", f.name); err != nil {
		return Stat{}, err
	}
	return f.WritableFile.Info()
}

func (f *faultWritable) Truncate(size int64) error {
	if err := f.fail("truncate", f.name); err != nil {
		return err
	}
	return f.WritableFile.Truncate(size)
}

func (f *faultWritable) Sync() error {
	if err := f.fail("filesync", f.name); err != nil {
		return err
	}
	return f.WritableFile.Sync()
}

func (f *faultWritable) SetTimes(atime, mtime Time) error {
	if err := f.fail("settimes", f.name); err != nil {
		return err
	}
	return f.WritableFile.SetTimes(atime, mtime)
}

func (f *faultWritable) SetMode(mode uint32) error {
	if err := f.fail("setmode", f.name); err != nil {
		return err
	}
	return f.WritableFile.SetMode(mode)
}

func (f *faultWritable) SetOwner(uid, gid uint32) error {
	if err := f.fail("setowner", f.name); err != nil {
		return err
	}
	return f.WritableFile.SetOwner(uid, gid)
}

func (f *faultWritable) DropCache() error {
	if err := f.fail("dropcache", f.name); err != nil {
		return err
	}
	return f.WritableFile.DropCache()
}
