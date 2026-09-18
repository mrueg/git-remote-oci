package git_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/mrueg/git-remote-oci/pkg/git"
)

// buildPackfile creates a repository with one commit and returns a packfile
// containing all of its objects.
func buildPackfile(t *testing.T) []byte {
	t.Helper()

	srcDir := t.TempDir()
	repo, err := gogit.PlainInit(srcDir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "README.md"), []byte("hello\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := wt.Commit("initial", &gogit.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.com", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	gitDir := filepath.Join(srcDir, ".git")
	revList := exec.CommandContext(t.Context(), "git", "--git-dir="+gitDir, "rev-list", "--objects", "--all")
	objects, err := revList.Output()
	if err != nil {
		t.Fatalf("rev-list: %v", err)
	}

	prefix := filepath.Join(t.TempDir(), "pack")
	packCmd := exec.CommandContext(t.Context(), "git", "--git-dir="+gitDir, "pack-objects", prefix)
	packCmd.Stdin = bytes.NewReader(objects)
	if out, err := packCmd.CombinedOutput(); err != nil {
		t.Fatalf("pack-objects: %v: %s", err, out)
	}

	packs, err := filepath.Glob(prefix + "-*.pack")
	if err != nil || len(packs) == 0 {
		t.Fatalf("no packfile produced: %v", err)
	}
	data, err := os.ReadFile(packs[0])
	if err != nil {
		t.Fatalf("read pack: %v", err)
	}
	return data
}

// newBareRepo initialises a bare repository, points GIT_DIR at it, and opens it.
func newBareRepo(t *testing.T) (string, *git.Repository) {
	t.Helper()

	dir := t.TempDir()
	if out, err := exec.CommandContext(t.Context(), "git", "init", "--bare", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, out)
	}
	t.Setenv("GIT_DIR", dir)

	repo, err := git.OpenRepository()
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}
	return dir, repo
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
}

// TestImportPackfileLockPathIsKeepFile pins the "lock <file>" contract from
// gitremote-helpers(7): the reported path must be absolute, must live under
// $GIT_DIR/objects/pack, and must end in .keep. Git unlinks exactly that path
// after refs are updated, so reporting the .idx would risk deleting a pack
// index, and reporting a relative path silently leaks the .keep file forever.
func TestImportPackfileLockPathIsKeepFile(t *testing.T) {
	requireGit(t)

	packBytes := buildPackfile(t)
	dstDir, repo := newBareRepo(t)

	lockPath, err := repo.ImportPackfile(bytes.NewReader(packBytes))
	if err != nil {
		t.Fatalf("ImportPackfile: %v", err)
	}
	if lockPath == "" {
		t.Skip("git index-pack kept no pack for this input")
	}

	if !filepath.IsAbs(lockPath) {
		t.Errorf("lock path must be absolute, got %q", lockPath)
	}
	if !strings.HasSuffix(lockPath, ".keep") {
		t.Errorf("lock path must end in .keep, got %q", lockPath)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Errorf("lock path %q does not exist on disk: %v", lockPath, err)
	}
	wantDir := filepath.Join(dstDir, "objects", "pack")
	if got := filepath.Dir(lockPath); got != wantDir {
		t.Errorf("lock path should live in %q, got %q", wantDir, got)
	}
}

// TestImportPackfileRejectsGarbage verifies that a failed import is reported as
// an error. Returning nil here would tell git that objects landed when they
// did not, leaving the caller with a silently incomplete object graph.
func TestImportPackfileRejectsGarbage(t *testing.T) {
	requireGit(t)

	_, repo := newBareRepo(t)

	if _, err := repo.ImportPackfile(strings.NewReader("this is definitely not a packfile")); err == nil {
		t.Fatal("ImportPackfile reported success for data that is not a packfile")
	}
}

// TestImportPackfileEmptyInput treats an empty stream as a no-op rather than an
// error: a thin packfile with nothing new in it is legitimate.
func TestImportPackfileEmptyInput(t *testing.T) {
	requireGit(t)

	_, repo := newBareRepo(t)

	lockPath, err := repo.ImportPackfile(bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("empty input should not be an error: %v", err)
	}
	if lockPath != "" {
		t.Errorf("expected no lock path for empty input, got %q", lockPath)
	}
}
