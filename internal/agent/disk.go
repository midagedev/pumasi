package agent

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
)

// diskFree is what a statfs says of one filesystem (0, 0 = not known) and
// which filesystem it is: two paths with the same id are one disk.
type diskFree struct {
	free, total int64
	id          string
}

// fullest is the fullest of the filesystems that hold paths: the lowest share
// free, and of two alike the fewer bytes free. That is the disk a host is
// closed by. path is the first of paths on it, as given. Paths on one
// filesystem count once; a path not there yet is measured where it would be
// made (its nearest parent that is there). ("", 0, 0 = none could be measured.)
func fullest(paths []string, stat func(string) (diskFree, error)) (path string, free, total int64) {
	seen := map[string]bool{}
	for _, p := range paths {
		if p == "" {
			continue
		}
		d, err := statUp(p, stat)
		if err != nil || d.total <= 0 || seen[d.id] {
			continue
		}
		seen[d.id] = true
		if path == "" || lower(d.free, d.total, free, total) {
			path, free, total = p, d.free, d.total
		}
	}
	return path, free, total
}

// lower: a free of at total is a fuller disk than b free of bt.
func lower(a, at, b, bt int64) bool {
	pa, pb := float64(a)/float64(at), float64(b)/float64(bt)
	if pa != pb {
		return pa < pb
	}
	return a < b
}

// statUp measures p, or the nearest parent of p that is there.
func statUp(p string, stat func(string) (diskFree, error)) (diskFree, error) {
	p = filepath.Clean(p)
	for {
		d, err := stat(p)
		if !errors.Is(err, fs.ErrNotExist) {
			return d, err
		}
		up := filepath.Dir(p)
		if up == p {
			return d, err
		}
		p = up
	}
}

// diskDirs are the directories outside the driver's own where a runner of s
// writes: its work directory and the slot's directories ({slot} in the
// class's work, env and path). The host's disk is measured there too.
func (s Spec) diskDirs() []string {
	var out []string
	if w := s.expand(s.WorkDir, ""); w != "" && filepath.IsAbs(w) && !strings.Contains(w, "{slot}") && !strings.Contains(w, "{runner}") {
		out = append(out, w)
	}
	return append(out, s.SlotDirs()...)
}
