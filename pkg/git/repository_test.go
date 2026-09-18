package git_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/mrueg/git-remote-oci/pkg/git"
)

// testRepo is a repository with a small known history, opened through the
// package under test.
type testRepo struct {
	dir  string
	repo *git.Repository
	git  *gogit.Repository
	wt   *gogit.Worktree
}

func newTestRepo(tb testing.TB) *testRepo {
	tb.Helper()

	dir := tb.TempDir()
	g, err := gogit.PlainInit(dir, false)
	if err != nil {
		tb.Fatalf("PlainInit: %v", err)
	}
	wt, err := g.Worktree()
	if err != nil {
		tb.Fatalf("Worktree: %v", err)
	}
	tb.Setenv("GIT_DIR", filepath.Join(dir, ".git"))

	repo, err := git.OpenRepository()
	if err != nil {
		tb.Fatalf("OpenRepository: %v", err)
	}
	return &testRepo{dir: dir, repo: repo, git: g, wt: wt}
}

// commit writes a file and commits it, returning the new commit hash.
func (r *testRepo) commit(tb testing.TB, name, content, message string) plumbing.Hash {
	tb.Helper()

	if err := os.WriteFile(filepath.Join(r.dir, name), []byte(content), 0644); err != nil {
		tb.Fatalf("WriteFile: %v", err)
	}
	if _, err := r.wt.Add(name); err != nil {
		tb.Fatalf("Add: %v", err)
	}
	h, err := r.wt.Commit(message, &gogit.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.com", When: time.Now()},
	})
	if err != nil {
		tb.Fatalf("Commit: %v", err)
	}
	return h
}

func TestOpenRepositoryFromGitDir(t *testing.T) {
	r := newTestRepo(t)
	r.commit(t, "a.txt", "a\n", "first")

	// GIT_DIR is what git hands the helper, so opening from it must work.
	if _, err := git.OpenRepository(); err != nil {
		t.Fatalf("OpenRepository with GIT_DIR set: %v", err)
	}
}

func TestOpenRepositoryFailsOutsideARepo(t *testing.T) {
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "definitely-not-a-repo"))
	if _, err := git.OpenRepository(); err == nil {
		t.Error("OpenRepository succeeded outside a repository")
	}
}

func TestResolveRef(t *testing.T) {
	r := newTestRepo(t)
	first := r.commit(t, "a.txt", "a\n", "first")

	head, err := r.git.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	branch := head.Name().String()

	// Lightweight tag and annotated tag on the same commit.
	if _, err := r.git.CreateTag("light", first, nil); err != nil {
		t.Fatalf("CreateTag light: %v", err)
	}
	if _, err := r.git.CreateTag("annotated", first, &gogit.CreateTagOptions{
		Tagger:  &object.Signature{Name: "T", Email: "t@example.com", When: time.Now()},
		Message: "annotated",
	}); err != nil {
		t.Fatalf("CreateTag annotated: %v", err)
	}

	tests := []struct {
		name string
		ref  string
		want plumbing.Hash
	}{
		{"fully qualified branch", branch, first},
		{"short tag name", "light", first},
		{"annotated tag peels to the commit", "refs/tags/annotated", first},
		{"short annotated tag name", "annotated", first},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := r.repo.ResolveRef(tt.ref)
			if err != nil {
				t.Fatalf("ResolveRef(%q): %v", tt.ref, err)
			}
			if got != tt.want {
				t.Errorf("ResolveRef(%q) = %s, want %s", tt.ref, got, tt.want)
			}
		})
	}

	if _, err := r.repo.ResolveRef("refs/heads/does-not-exist"); err == nil {
		t.Error("ResolveRef succeeded for a missing ref")
	}
}

func TestGetCommitInfo(t *testing.T) {
	r := newTestRepo(t)
	h := r.commit(t, "a.txt", "a\n", "the message")

	commit, err := r.repo.GetCommitInfo(h)
	if err != nil {
		t.Fatalf("GetCommitInfo: %v", err)
	}
	if !strings.Contains(commit.Message, "the message") {
		t.Errorf("commit message = %q", commit.Message)
	}
	if commit.Author.Email != "t@example.com" {
		t.Errorf("author = %q", commit.Author.Email)
	}

	if _, err := r.repo.GetCommitInfo(plumbing.NewHash("0123456789012345678901234567890123456789")); err == nil {
		t.Error("GetCommitInfo succeeded for an absent commit")
	}
}

func TestIsAncestor(t *testing.T) {
	r := newTestRepo(t)
	first := r.commit(t, "a.txt", "a\n", "first")
	second := r.commit(t, "a.txt", "b\n", "second")
	third := r.commit(t, "a.txt", "c\n", "third")

	tests := []struct {
		name              string
		ancestor, descend plumbing.Hash
		want              bool
	}{
		{"direct parent", second, third, true},
		{"transitive ancestor", first, third, true},
		{"same commit", third, third, true},
		{"descendant is not an ancestor", third, first, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := r.repo.IsAncestor(tt.ancestor, tt.descend)
			if err != nil {
				t.Fatalf("IsAncestor: %v", err)
			}
			if got != tt.want {
				t.Errorf("IsAncestor(%s, %s) = %v, want %v", tt.ancestor, tt.descend, got, tt.want)
			}
		})
	}

	// An unknown descendant cannot be walked at all.
	if _, err := r.repo.IsAncestor(first, plumbing.NewHash("0123456789012345678901234567890123456789")); err == nil {
		t.Error("IsAncestor succeeded for an absent descendant")
	}
}

func TestScanLFSPointers(t *testing.T) {
	r := newTestRepo(t)

	oid := strings.Repeat("ab", 32)
	pointer := "version https://git-lfs.github.com/spec/v1\noid sha256:" + oid + "\nsize 1234\n"
	if err := os.WriteFile(filepath.Join(r.dir, "big.bin"), []byte(pointer), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(r.dir, "plain.txt"), []byte("just a file\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := r.wt.Add("."); err != nil {
		t.Fatalf("Add: %v", err)
	}
	h, err := r.wt.Commit("add pointer", &gogit.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	pointers, err := r.repo.ScanLFSPointers(h, nil)
	if err != nil {
		t.Fatalf("ScanLFSPointers: %v", err)
	}
	if len(pointers) != 1 {
		t.Fatalf("expected exactly one pointer, got %d", len(pointers))
	}
	if pointers[0].Oid != oid {
		t.Errorf("oid = %q, want %q", pointers[0].Oid, oid)
	}
	if pointers[0].Size != 1234 {
		t.Errorf("size = %d, want 1234", pointers[0].Size)
	}

	if _, err := r.repo.ScanLFSPointers(plumbing.NewHash("0123456789012345678901234567890123456789"), nil); err == nil {
		t.Error("ScanLFSPointers succeeded for an absent commit")
	}
}

func TestGetReachableTags(t *testing.T) {
	r := newTestRepo(t)
	first := r.commit(t, "a.txt", "a\n", "first")
	second := r.commit(t, "a.txt", "b\n", "second")

	if _, err := r.git.CreateTag("v1", first, nil); err != nil {
		t.Fatalf("CreateTag v1: %v", err)
	}
	if _, err := r.git.CreateTag("v2", second, nil); err != nil {
		t.Fatalf("CreateTag v2: %v", err)
	}

	// Reachable from the first commit: only v1.
	tags, err := r.repo.GetReachableTags([]plumbing.Hash{first})
	if err != nil {
		t.Fatalf("GetReachableTags: %v", err)
	}
	names := tagNames(tags)
	if !names["refs/tags/v1"] {
		t.Errorf("v1 should be reachable from the first commit, got %v", names)
	}
	if names["refs/tags/v2"] {
		t.Errorf("v2 is on a later commit and should not be reachable, got %v", names)
	}

	// Reachable from the tip: both.
	tags, err = r.repo.GetReachableTags([]plumbing.Hash{second})
	if err != nil {
		t.Fatalf("GetReachableTags: %v", err)
	}
	names = tagNames(tags)
	if !names["refs/tags/v1"] || !names["refs/tags/v2"] {
		t.Errorf("both tags should be reachable from the tip, got %v", names)
	}

	// No input hashes means nothing to report.
	if tags, err := r.repo.GetReachableTags(nil); err != nil || len(tags) != 0 {
		t.Errorf("GetReachableTags(nil) = %v, %v; want empty, nil", tags, err)
	}
}

func tagNames(tags []git.TagInfo) map[string]bool {
	out := make(map[string]bool, len(tags))
	for _, tag := range tags {
		out[tag.Name] = true
	}
	return out
}

func TestSetReference(t *testing.T) {
	r := newTestRepo(t)
	h := r.commit(t, "a.txt", "a\n", "first")

	if err := r.repo.SetReference("refs/heads/created", h); err != nil {
		t.Fatalf("SetReference: %v", err)
	}
	got, err := r.repo.ResolveRef("refs/heads/created")
	if err != nil {
		t.Fatalf("ResolveRef after SetReference: %v", err)
	}
	if got != h {
		t.Errorf("created ref points at %s, want %s", got, h)
	}
}

func TestCreatePackfileToExcludesHaves(t *testing.T) {
	r := newTestRepo(t)
	first := r.commit(t, "a.txt", "a\n", "first")
	second := r.commit(t, "a.txt", "b\n", "second")

	full, err := packBytes(t, r.repo, second, nil)
	if err != nil {
		t.Fatalf("CreatePackfile full: %v", err)
	}
	thin, err := packBytes(t, r.repo, second, []plumbing.Hash{first})
	if err != nil {
		t.Fatalf("CreatePackfile thin: %v", err)
	}

	if len(thin) >= len(full) {
		t.Errorf("excluding a known commit should shrink the packfile: thin=%d full=%d", len(thin), len(full))
	}
	if !strings.HasPrefix(string(full), "PACK") {
		t.Errorf("packfile does not start with the PACK signature")
	}
}

// packBytes buffers CreatePackfileTo. Repository.CreatePackfile used to do this
// and was public API with no caller outside the tests.
func packBytes(t *testing.T, r *git.Repository, want plumbing.Hash, haves []plumbing.Hash) ([]byte, error) {
	t.Helper()

	var buf bytes.Buffer
	if err := r.CreatePackfileTo(&buf, want, haves); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
