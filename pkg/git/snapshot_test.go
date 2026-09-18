package git_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/mrueg/git-remote-oci/pkg/git"
)

// A snapshot is defined as self-contained: the tip commit, its tree, and
// every object beneath. One quietly missing a subtree produces a --depth 1
// clone that fails on checkout, which is worse than no snapshot at all --
// publishing one is optional, reading one is not.

// TestSnapshotRefusesAMissingSubtree: go-git's TreeWalker turns a subtree it
// cannot load into io.EOF, the same answer as "done", so a repository missing
// a tree object produced a short snapshot that looked complete. The walk now
// opens every subtree itself and fails on the one it cannot.
func TestSnapshotRefusesAMissingSubtree(t *testing.T) {
	requireGit(t)
	r := newTestRepo(t)
	if err := os.MkdirAll(filepath.Join(r.dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	tip := r.commit(t, filepath.Join("sub", "file.txt"), "nested\n", "add a subdirectory")

	// A complete repository snapshots fine, which keeps the failure below
	// honest.
	var pack bytes.Buffer
	if err := r.repo.CreateSnapshotPackfileTo(&pack, tip); err != nil {
		t.Fatalf("snapshot of a complete repository: %v", err)
	}
	if pack.Len() == 0 {
		t.Fatal("the snapshot is empty")
	}

	// Remove the subtree's loose object. go-git wrote it loose, and nothing
	// has packed it since.
	commit, err := r.git.CommitObject(tip)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := commit.Tree()
	if err != nil {
		t.Fatal(err)
	}
	var subtree plumbing.Hash
	for _, entry := range tree.Entries {
		if entry.Name == "sub" {
			subtree = entry.Hash
		}
	}
	if subtree.IsZero() {
		t.Fatal("fixture error: the tip has no sub/ entry")
	}
	loose := filepath.Join(r.dir, ".git", "objects", subtree.String()[:2], subtree.String()[2:])
	if err := os.Remove(loose); err != nil {
		t.Fatalf("remove the subtree's loose object: %v", err)
	}

	// A fresh handle, so nothing is answered out of the cache the commit
	// filled.
	repo, err := git.OpenRepositoryAt(filepath.Join(r.dir, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	pack.Reset()
	err = repo.CreateSnapshotPackfileTo(&pack, tip)
	if err == nil {
		t.Fatal("a snapshot was produced from a repository missing a subtree; a --depth 1 clone of it fails on checkout")
	}
	if !errors.Is(err, git.ErrSnapshotUnavailable) {
		t.Errorf("error is not ErrSnapshotUnavailable, so the push would fail rather than skip the snapshot: %v", err)
	}
	if !strings.Contains(err.Error(), subtree.String()) {
		t.Errorf("the error does not name the missing tree %s: %v", subtree, err)
	}
}

// TestSnapshotCoversNestedTrees: the explicit walk has to reach every level,
// or it is only a different way of being incomplete.
func TestSnapshotCoversNestedTrees(t *testing.T) {
	requireGit(t)
	r := newTestRepo(t)
	deep := filepath.Join("a", "b", "c")
	if err := os.MkdirAll(filepath.Join(r.dir, deep), 0o755); err != nil {
		t.Fatal(err)
	}
	r.commit(t, "top.txt", "top\n", "top")
	tip := r.commit(t, filepath.Join(deep, "leaf.txt"), "leaf\n", "nested")

	var pack bytes.Buffer
	if err := r.repo.CreateSnapshotPackfileTo(&pack, tip); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// Import the snapshot into an empty repository and check the deepest blob
	// out: that is what a --depth 1 clone does with it.
	dst, into := newBareRepo(t)
	if _, err := into.ImportPackfile(bytes.NewReader(pack.Bytes())); err != nil {
		t.Fatalf("import the snapshot: %v", err)
	}
	commit, err := r.git.CommitObject(tip)
	if err != nil {
		t.Fatal(err)
	}
	file, err := commit.File(filepath.ToSlash(filepath.Join(deep, "leaf.txt")))
	if err != nil {
		t.Fatal(err)
	}
	if out := runGit(t, dst, "cat-file", "-t", file.Hash.String()); out != "blob" {
		t.Errorf("the deepest blob is a %q in the snapshot, want blob", out)
	}
}
