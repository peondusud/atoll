package fsx

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// UserNamespace is the namespace of the extended attributes Atoll's own view keeps and the copy pipeline copies: those
// any program may write, as Samba's user.DOSATTRIB and macOS's through Samba (specs.md §14, §21.2). Those of security.,
// trusted. and system., ACLs among them, are never read nor written.
const UserNamespace = "user."

// The flags of setxattr(2): a value set only where the attribute is new, or only where it is there.
const (
	XattrCreate  = unix.XATTR_CREATE
	XattrReplace = unix.XATTR_REPLACE
)

// xattrTries bounds how often a listing or a value is read again when it grew between asking its size and reading it.
const xattrTries = 8

// IsXattrRefused says whether a filesystem refused an extended attribute: none it holds (ENOTSUP), a name or a value
// too long for it (ERANGE, E2BIG, ENAMETOOLONG), no room for it (ENOSPC, EDQUOT), or a name it does not take (EINVAL,
// EPERM, as on a symbolic link). It says nothing about the disk.
func IsXattrRefused(err error) bool {
	return errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ERANGE) || errors.Is(err, unix.E2BIG) ||
		errors.Is(err, unix.ENAMETOOLONG) || errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EDQUOT) || errors.Is(err, unix.EINVAL) ||
		errors.Is(err, unix.EPERM)
}

// IsNoXattr says whether an attribute asked for is not there, ENODATA.
func IsNoXattr(err error) bool {
	return errors.Is(err, unix.ENODATA)
}

// readXattrs reads the attributes whose names begin with namespace: list gives the names, get a value, each filling
// a buffer, or saying how large one must be when it is empty, as the system calls do. A filesystem that holds none,
// ENOTSUP, holds an empty set.
func readXattrs(list func([]byte) (int, error), get func(string, []byte) (int, error), namespace string) (map[string][]byte, error) {
	raw, err := grown(list)
	switch {
	case errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EOPNOTSUPP):
		return map[string][]byte{}, nil
	case err != nil:
		return nil, fmt.Errorf("list attributes: %w", err)
	}
	out := map[string][]byte{}
	for _, name := range strings.Split(string(raw), "\x00") {
		if name == "" || !strings.HasPrefix(name, namespace) {
			continue
		}
		value, err := grown(func(buf []byte) (int, error) { return get(name, buf) })
		switch {
		case IsNoXattr(err):
			// Gone since it was listed.
			continue
		case err != nil:
			return nil, fmt.Errorf("attribute %q: %w", name, err)
		}
		out[name] = value
	}
	return out, nil
}

// grown asks a call for the size of what it gives, then reads it, again when it grew in between.
func grown(read func([]byte) (int, error)) ([]byte, error) {
	for range xattrTries {
		size, err := read(nil)
		if err != nil {
			return nil, err //nolint:wrapcheck // the caller says what it read
		}
		if size == 0 {
			return []byte{}, nil
		}
		buf := make([]byte, size)
		n, err := read(buf)
		switch {
		case errors.Is(err, unix.ERANGE):
			continue
		case err != nil:
			return nil, err //nolint:wrapcheck // the caller says what it read
		}
		return buf[:n], nil
	}
	return nil, unix.ERANGE
}

// procPath names a name of a folder through the folder's handle, as a call that takes no handle and a name needs: the
// path stays short however deep the folder is, and leads where the handle does (specs.md §15.1).
func (d *realDir) procPath(name string) string {
	return "/proc/self/fd/" + strconv.Itoa(d.fd()) + "/" + name
}

func (d *realDir) Xattrs(name, namespace string) (map[string][]byte, error) {
	path := d.procPath(name)
	out, err := readXattrs(func(buf []byte) (int, error) { return unix.Llistxattr(path, buf) },
		func(attr string, buf []byte) (int, error) { return unix.Lgetxattr(path, attr, buf) }, namespace)
	if err != nil {
		return nil, fmt.Errorf("%q: %w", name, err)
	}
	return out, nil
}

func (d *realDir) SetXattr(name, attr string, value []byte, flags int) error {
	if err := unix.Lsetxattr(d.procPath(name), attr, value, flags); err != nil {
		return fmt.Errorf("set attribute %q of %q: %w", attr, name, err)
	}
	return nil
}

func (d *realDir) RemoveXattr(name, attr string) error {
	if err := unix.Lremovexattr(d.procPath(name), attr); err != nil {
		return fmt.Errorf("remove attribute %q of %q: %w", attr, name, err)
	}
	return nil
}

func fdXattrs(fd int, what, namespace string) (map[string][]byte, error) {
	out, err := readXattrs(func(buf []byte) (int, error) { return unix.Flistxattr(fd, buf) },
		func(attr string, buf []byte) (int, error) { return unix.Fgetxattr(fd, attr, buf) }, namespace)
	if err != nil {
		return nil, fmt.Errorf("%q: %w", what, err)
	}
	return out, nil
}

func fdSetXattr(fd int, what, attr string, value []byte, flags int) error {
	if err := unix.Fsetxattr(fd, attr, value, flags); err != nil {
		return fmt.Errorf("set attribute %q of %q: %w", attr, what, err)
	}
	return nil
}

func fdRemoveXattr(fd int, what, attr string) error {
	if err := unix.Fremovexattr(fd, attr); err != nil {
		return fmt.Errorf("remove attribute %q of %q: %w", attr, what, err)
	}
	return nil
}

func (f *realFile) Xattrs(namespace string) (map[string][]byte, error) {
	return fdXattrs(int(f.file.Fd()), f.file.Name(), namespace)
}

// SetXattr and RemoveXattr need no write access to the file through its handle: setxattr asks for the right to write
// the file, which Atoll's account has, whatever the handle is open for.
func (f *realFile) SetXattr(attr string, value []byte, flags int) error {
	return fdSetXattr(int(f.file.Fd()), f.file.Name(), attr, value, flags)
}

func (f *realFile) RemoveXattr(attr string) error {
	return fdRemoveXattr(int(f.file.Fd()), f.file.Name(), attr)
}

func (f *realWritable) Xattrs(namespace string) (map[string][]byte, error) {
	return fdXattrs(f.fd(), f.file.Name(), namespace)
}

func (f *realWritable) SetXattr(attr string, value []byte, flags int) error {
	return fdSetXattr(f.fd(), f.file.Name(), attr, value, flags)
}

func (f *realWritable) RemoveXattr(attr string) error {
	return fdRemoveXattr(f.fd(), f.file.Name(), attr)
}
