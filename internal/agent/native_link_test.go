//go:build darwin || linux || windows

package agent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Every runner of a host links to the same unpacked files. A job must not be
// able to change one in place, and one that was changed is not handed on.
func TestSharedRunnerFilesAreReadOnlyLinks(t *testing.T) {
	root := t.TempDir()
	dist := filepath.Join(root, "dist")
	for rel, mode := range map[string]os.FileMode{"run.sh": 0o755, "bin/Runner.Listener": 0o755, "externals/node/lib/x.js": 0o644} {
		p := filepath.Join(dist, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(rel), mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := sealShared(dist); err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(root, "r1")
	if err := linkTree(dist, clone); err != nil {
		t.Fatal(err)
	}
	same := func(rel string) bool {
		a, err1 := os.Stat(filepath.Join(dist, rel))
		b, err2 := os.Stat(filepath.Join(clone, rel))
		if err1 != nil || err2 != nil {
			t.Fatalf("%s: %v %v", rel, err1, err2)
		}
		return os.SameFile(a, b)
	}
	bin := filepath.Join("bin", "Runner.Listener")
	if !same(bin) || !same(filepath.Join("externals", "node", "lib", "x.js")) {
		t.Error("shared files are copies, want hard links")
	}
	if same("run.sh") {
		t.Error("run.sh is a hard link, want a copy: the runner rewrites files at the top")
	}
	if os.Getuid() != 0 { // root writes through any mode
		if f, err := os.OpenFile(filepath.Join(clone, bin), os.O_WRONLY, 0); err == nil {
			f.Close()
			t.Error("a runner's link to a shared file can be written in place")
		}
	}
	if f, err := os.OpenFile(filepath.Join(clone, "run.sh"), os.O_WRONLY, 0); err != nil {
		t.Errorf("the runner's own copy is not writable: %v", err)
	} else {
		f.Close()
	}
	// Replacing its own link is the runner's business and touches nobody else.
	if err := os.Remove(filepath.Join(clone, bin)); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dist, bin)); err != nil || string(b) != "bin/Runner.Listener" {
		t.Errorf("the shared file after a runner removed its link: %q %v", b, err)
	}
	if err := os.RemoveAll(clone); err != nil {
		t.Fatal(err)
	}

	// Someone made a shared file writable again: it may have been changed.
	if err := os.Chmod(filepath.Join(dist, bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := linkTree(dist, filepath.Join(root, "r2")); !errors.Is(err, errDistChanged) {
		t.Errorf("a writable shared file: err %v, want errDistChanged", err)
	}
}
