//go:build darwin || linux

package agent

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func tarball(t *testing.T, members ...tar.Header) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "arc-*")
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	tw := tar.NewWriter(zw)
	for _, h := range members {
		h.Mode = 0o755
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return f
}

// A link in the archive that points out of the directory would let a later
// member be written anywhere the user may write.
func TestUnpackRefusesLinksThatLeave(t *testing.T) {
	for _, target := range []string{"/etc", "../outside", "x/../../outside"} {
		dir := filepath.Join(t.TempDir(), "d")
		arc := tarball(t, tar.Header{Name: "a", Typeflag: tar.TypeSymlink, Linkname: target},
			tar.Header{Name: "a/x", Typeflag: tar.TypeReg})
		if err := unpack(arc, 0, dir); err == nil {
			t.Errorf("link to %q: unpacked", target)
		}
		if _, err := os.Lstat(filepath.Join(dir, "a")); err == nil {
			t.Errorf("link to %q was made", target)
		}
	}
	// The runner's own links stay inside (externals/node20/bin/npm -> ../lib/…).
	dir := filepath.Join(t.TempDir(), "d")
	arc := tarball(t, tar.Header{Name: "n/lib/npm.js", Typeflag: tar.TypeReg},
		tar.Header{Name: "n/bin/npm", Typeflag: tar.TypeSymlink, Linkname: "../lib/npm.js"})
	if err := unpack(arc, 0, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "n", "bin", "npm")); err != nil {
		t.Error(err)
	}
}
