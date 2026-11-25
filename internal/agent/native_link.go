//go:build darwin || linux || windows

package agent

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// shared reports whether a file of the unpacked runner is hard-linked into
// every runner's directory: all of bin and externals, hundreds of megabytes
// that are only read. The few files at the top are copied; the runner
// rewrites some of them.
func shared(rel string) bool { return strings.ContainsRune(rel, filepath.Separator) }

// sealShared takes the write permission off the shared files of an unpacked
// runner. A job then cannot change one in place and with it every later
// runner of the host; it can still replace its own link.
func sealShared(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		if rel, _ := filepath.Rel(dir, p); !shared(rel) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return os.Chmod(p, info.Mode().Perm()&^0o222)
	})
}

// linkTree makes a runner directory out of the unpacked runner: hard links
// to the shared files, copies of the rest. A shared file that is writable is
// not as sealShared left it: errDistChanged. Where a link cannot be made
// (another filesystem) the file is copied.
func linkTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		to := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(to, 0o755)
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(target, to)
		case !d.Type().IsRegular():
			return nil
		case shared(rel):
			if info.Mode().Perm()&0o222 != 0 {
				return errDistChanged
			}
			if os.Link(p, to) == nil {
				return nil
			}
		}
		return copyFile(p, to, info.Mode().Perm()|0o200)
	})
}

func copyFile(from, to string, mode fs.FileMode) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
