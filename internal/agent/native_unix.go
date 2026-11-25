//go:build darwin || linux

package agent

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const distExt = ".tar.gz"

// runScript is the shell around run.sh ($1 = the runner's directory). It
// leaves the exit code in a file, because a later agent is not the runner's
// parent and cannot wait for it.
const runScript = `cd "$1/runner" && ./run.sh; echo $? > "$1/exit.tmp" && mv "$1/exit.tmp" "$1/exit"`

func (t *tree) exitCode() (int, bool) {
	b, err := os.ReadFile(filepath.Join(t.dir, "exit"))
	if err != nil {
		return 0, false
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return code, err == nil
}

func unpack(arc *os.File, _ int64, dir string) error {
	if _, err := arc.Seek(0, io.SeekStart); err != nil {
		return err
	}
	zr, err := gzip.NewReader(arc)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		p, err := inside(dir, h.Name)
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(p, 0o755)
		case tar.TypeSymlink:
			if err = os.MkdirAll(filepath.Dir(p), 0o755); err == nil {
				err = os.Symlink(h.Linkname, p)
			}
		case tar.TypeLink:
			var target string
			if target, err = inside(dir, h.Linkname); err == nil {
				err = os.Link(target, p)
			}
		case tar.TypeReg:
			if err = os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				break
			}
			var f *os.File
			if f, err = os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_EXCL, h.FileInfo().Mode().Perm()); err != nil {
				break
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
		if err != nil {
			return err
		}
	}
}
