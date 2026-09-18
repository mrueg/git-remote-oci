package git_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrueg/git-remote-oci/pkg/git"
)

// A linked worktree is a repository whose git directory is not where its
// objects are. git hands a remote helper GIT_DIR=<repo>/.git/worktrees/<name>,
// which holds HEAD, the index and a `commondir` file, and nothing else; the
// object store, the refs, the shallow file and the LFS cache are all in the
// directory commondir names. Everything here that built a path under GIT_DIR
// was looking in the wrong place from a linked worktree.

// runGit runs git in dir and fails the test on error.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// linkedWorktree creates a repository with one commit and a linked worktree
// on a new branch, and returns the main repository's directory and the linked
// worktree's git directory -- the one git exports as GIT_DIR from there.
func linkedWorktree(t *testing.T) (mainDir, worktreeDir, worktreeGitDir string) {
	t.Helper()
	requireGit(t)

	mainDir = filepath.Join(t.TempDir(), "main")
	if err := os.MkdirAll(mainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, mainDir, "init", "-q", "-b", "main", ".")
	if err := os.WriteFile(filepath.Join(mainDir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, mainDir, "add", "README.md")
	runGit(t, mainDir, "commit", "-q", "-m", "initial")

	worktreeDir = filepath.Join(t.TempDir(), "linked")
	runGit(t, mainDir, "worktree", "add", "-q", "-b", "feature", worktreeDir)

	worktreeGitDir = runGit(t, worktreeDir, "rev-parse", "--absolute-git-dir")
	if !strings.Contains(worktreeGitDir, filepath.Join(".git", "worktrees")) {
		t.Fatalf("fixture error: %q is not a linked worktree's git directory", worktreeGitDir)
	}
	return mainDir, worktreeDir, worktreeGitDir
}

func TestLinkedWorktreeResolvesTheCommonDir(t *testing.T) {
	mainDir, worktreeDir, worktreeGitDir := linkedWorktree(t)
	t.Setenv("GIT_DIR", worktreeGitDir)

	// GIT_DIR keeps its meaning: it is what git said, and what --git-dir is
	// handed. The common directory is where the shared state is.
	gitDir, ok := git.GitDir()
	if !ok || gitDir != worktreeGitDir {
		t.Errorf("GitDir() = %q (ok=%v), want GIT_DIR %q", gitDir, ok, worktreeGitDir)
	}
	wantCommon := filepath.Join(mainDir, ".git")
	if common, ok := git.CommonDir(); !ok || common != wantCommon {
		t.Errorf("CommonDir() = %q (ok=%v), want %q", common, ok, wantCommon)
	}
	if objects, ok := git.ObjectsDir(); !ok || objects != filepath.Join(wantCommon, "objects") {
		t.Errorf("ObjectsDir() = %q (ok=%v), want the main repository's object store", objects, ok)
	}

	repo, err := git.OpenRepository()
	if err != nil {
		t.Fatalf("OpenRepository from a linked worktree: %v", err)
	}
	if got := repo.CommonDir(); got != wantCommon {
		t.Errorf("Repository.CommonDir() = %q, want %q", got, wantCommon)
	}
	if got := repo.ObjectsDir(); got != filepath.Join(wantCommon, "objects") {
		t.Errorf("Repository.ObjectsDir() = %q, want the main repository's object store", got)
	}
	// The worktree's own branch resolves through the common refs.
	if _, err := repo.ResolveRef("refs/heads/feature"); err != nil {
		t.Errorf("the linked worktree's branch does not resolve: %v", err)
	}
	_ = worktreeDir
}

// TestLinkedWorktreeIsFoundFromItsDirectory covers the no-GIT_DIR case: the
// worktree has a ".git" *file* naming its git directory, which used to be
// declined outright.
func TestLinkedWorktreeIsFoundFromItsDirectory(t *testing.T) {
	mainDir, worktreeDir, worktreeGitDir := linkedWorktree(t)
	unsetGitDir(t)
	t.Chdir(worktreeDir)

	gitDir, ok := git.GitDir()
	if !ok {
		t.Fatal("GitDir() could not resolve a linked worktree from inside it")
	}
	gotEval, _ := filepath.EvalSymlinks(gitDir)
	wantEval, _ := filepath.EvalSymlinks(worktreeGitDir)
	if gotEval != wantEval {
		t.Errorf("GitDir() = %q, want %q", gitDir, worktreeGitDir)
	}
	common, _ := git.CommonDir()
	commonEval, _ := filepath.EvalSymlinks(common)
	wantCommon, _ := filepath.EvalSymlinks(filepath.Join(mainDir, ".git"))
	if commonEval != wantCommon {
		t.Errorf("CommonDir() = %q, want %q", common, filepath.Join(mainDir, ".git"))
	}
	if _, err := git.OpenRepository(); err != nil {
		t.Errorf("OpenRepository from inside a linked worktree: %v", err)
	}
}

// TestImportPackfileFromALinkedWorktreeReportsTheRealKeepFile is the bug
// this is all for. git index-pack honours commondir and writes the pack and
// its .keep under the common object store; the helper looked for the .keep
// under GIT_DIR, found nothing, reported no lock line, and the .keep -- which
// git would have unlinked once the refs were updated -- stayed forever.
func TestImportPackfileFromALinkedWorktreeReportsTheRealKeepFile(t *testing.T) {
	packBytes := buildPackfile(t)
	mainDir, _, worktreeGitDir := linkedWorktree(t)
	t.Setenv("GIT_DIR", worktreeGitDir)

	repo, err := git.OpenRepository()
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}
	lockPath, err := repo.ImportPackfile(bytes.NewReader(packBytes))
	if err != nil {
		t.Fatalf("ImportPackfile: %v", err)
	}
	if lockPath == "" {
		t.Fatal("no lock path reported: the .keep index-pack wrote under the common dir was not found")
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Errorf("the reported lock path %q does not exist: %v", lockPath, err)
	}
	wantDir := filepath.Join(mainDir, ".git", "objects", "pack")
	if got := filepath.Dir(lockPath); got != wantDir {
		t.Errorf("lock path is in %q, want the common object store %q", got, wantDir)
	}
	// Nothing was created under the worktree's own git directory.
	if _, err := os.Stat(filepath.Join(worktreeGitDir, "objects")); err == nil {
		t.Errorf("an objects directory was created under the linked worktree's git directory %q", worktreeGitDir)
	}
}

// TestImportPackfileLeavesNoSpoolBehind: the incoming pack is staged in the
// pack directory, and the staging file has to be gone afterwards -- and named
// so that `git prune` would remove it if it were not.
func TestImportPackfileLeavesNoSpoolBehind(t *testing.T) {
	requireGit(t)
	packBytes := buildPackfile(t)
	dir, repo := newBareRepo(t)

	if _, err := repo.ImportPackfile(bytes.NewReader(packBytes)); err != nil {
		t.Fatalf("ImportPackfile: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "objects", "pack"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "pack-") {
			t.Errorf("unexpected file left in objects/pack: %s", e.Name())
		}
	}
}

// TestImportPackfileIgnoresStderrNoise: index-pack's answer is on stdout and
// its commentary on stderr. Reading the two together, anything git said first
// -- a trace line, a warning -- displaced the `keep <sha>` line, the id was not
// found, and the .keep was never reported.
func TestImportPackfileIgnoresStderrNoise(t *testing.T) {
	requireGit(t)
	packBytes := buildPackfile(t)
	_, repo := newBareRepo(t)

	// GIT_TRACE makes every git subprocess narrate itself on stderr before
	// doing anything, which is exactly the interleaving that broke the parse.
	t.Setenv("GIT_TRACE", "1")

	lockPath, err := repo.ImportPackfile(bytes.NewReader(packBytes))
	if err != nil {
		t.Fatalf("ImportPackfile: %v", err)
	}
	if lockPath == "" {
		t.Fatal("no lock path reported: stderr output displaced index-pack's answer")
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Errorf("reported lock path %q does not exist: %v", lockPath, err)
	}
}

// IsShallow and IsPartial are what stops gc packing from a clone that does
// not hold the history.

func TestIsShallow(t *testing.T) {
	requireGit(t)
	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, src, "init", "-q", "-b", "main", ".")
	for _, name := range []string{"a", "b", "c"} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, src, "add", name)
		runGit(t, src, "commit", "-q", "-m", name)
	}

	full, err := git.OpenRepositoryAt(filepath.Join(src, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if full.IsShallow() {
		t.Error("a complete repository reports itself as shallow")
	}
	if full.IsPartial() {
		t.Error("a complete repository reports itself as partial")
	}

	shallowDir := filepath.Join(t.TempDir(), "shallow")
	runGit(t, t.TempDir(), "clone", "-q", "--depth", "1", "file://"+src, shallowDir)
	shallow, err := git.OpenRepositoryAt(filepath.Join(shallowDir, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if !shallow.IsShallow() {
		t.Error("a --depth 1 clone does not report itself as shallow")
	}
	if shallow.IsPartial() {
		t.Error("a --depth 1 clone reports itself as partial")
	}

	// A shallow file that is present but empty is what --unshallow leaves
	// behind on some versions; it grafts nothing.
	if err := os.WriteFile(filepath.Join(shallowDir, ".git", "shallow"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if shallow.IsShallow() {
		t.Error("an empty shallow file reads as shallow")
	}

	partialDir := filepath.Join(t.TempDir(), "partial")
	runGit(t, t.TempDir(), "clone", "-q", "--no-checkout", "--filter=blob:none", "file://"+src, partialDir)
	partial, err := git.OpenRepositoryAt(filepath.Join(partialDir, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if !partial.IsPartial() {
		t.Error("a --filter=blob:none clone does not report itself as partial")
	}
	if partial.IsShallow() {
		t.Error("a --filter=blob:none clone reports itself as shallow")
	}
}

// TestIsShallowFromALinkedWorktree: the shallow file is shared state, so it is
// read from the common directory, not the worktree's own.
func TestIsShallowFromALinkedWorktree(t *testing.T) {
	mainDir, _, worktreeGitDir := linkedWorktree(t)
	t.Setenv("GIT_DIR", worktreeGitDir)
	repo, err := git.OpenRepository()
	if err != nil {
		t.Fatal(err)
	}
	if repo.IsShallow() {
		t.Fatal("a complete repository reports itself as shallow")
	}
	if err := os.WriteFile(filepath.Join(mainDir, ".git", "shallow"),
		[]byte("0123456789abcdef0123456789abcdef01234567\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !repo.IsShallow() {
		t.Error("the common directory's shallow file was not consulted from the linked worktree")
	}
}
