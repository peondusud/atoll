package fsx

// Fault wraps a folder so that a test can make any call fail (CLAUDE.md, "Testing rules"). fail
// gets the name of the call and the name it is about, and gives the error to return, or nil.
func Fault(d Dir, fail func(call, name string) error) Dir {
	return &faultDir{Dir: d, fail: fail}
}

type faultDir struct {
	Dir
	fail func(call, name string) error
}

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
	return &faultDir{Dir: below, fail: d.fail}, nil
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

func (f *faultFile) Info() (Stat, error) {
	if err := f.fail("fileinfo", f.name); err != nil {
		return Stat{}, err
	}
	return f.File.Info()
}
