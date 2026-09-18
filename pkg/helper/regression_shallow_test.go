package helper_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// blobFetches counts packfile blob downloads the registry served.
func blobFetches(reg *mockRegistry) int {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	n := 0
	for _, req := range reg.requests {
		if strings.HasPrefix(req, "GET ") && strings.Contains(req, "/blobs/") {
			n++
		}
	}
	return n
}

// shallowFixture publishes `generations` pushes of one commit each and returns
// the tip.
func shallowFixture(t *testing.T, registry string, generations int) string {
	t.Helper()

	src := newWorkRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(src, ".git"))

	tip := git(t, src, "rev-parse", "HEAD")
	for i := range generations {
		if i > 0 {
			tip = commitFile(t, src, "f"+strconv.Itoa(i)+".txt", "content\n", "commit "+strconv.Itoa(i))
		}
		if out, err := runHelper(t, registry, "list for-push\npush refs/heads/main:refs/heads/main\n\n"); err != nil {
			t.Fatalf("push %d failed: %v (output %q)", i, err, out)
		}
	}
	return tip
}

// TestShallowDepthTruncatesAtTheRequestedCommit pins that `--depth n` shows
// exactly n commits.
//
// The boundary used to be marked during the fetch walk, at the point where the
// recursion depth equalled the requested depth. That recursion descends one hop
// per *push*, not per commit, so any repository that ever received a
// multi-commit push - or any depth above 1 - truncated in the wrong place. It is
// now computed from the real commit graph once everything has been imported.
func TestShallowDepthTruncatesAtTheRequestedCommit(t *testing.T) {
	reg := newMockRegistry()
	ts := reg.Server()
	defer ts.Close()

	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	tip := shallowFixture(t, registry, 6)

	for _, depth := range []int{1, 2, 3} {
		t.Run("depth"+strconv.Itoa(depth), func(t *testing.T) {
			dst := newBareRepo(t)
			t.Setenv("GIT_DIR", dst)

			script := "option depth " + strconv.Itoa(depth) + "\nlist\nfetch " + tip + " refs/heads/main\n\n"
			if out, err := runHelper(t, registry, script); err != nil {
				t.Fatalf("fetch failed: %v (output %q)", err, out)
			}

			if !fileHasLines(t, filepath.Join(dst, "shallow")) {
				t.Fatal("no shallow boundary was recorded")
			}
			got := strings.TrimSpace(runGitOut(t, dst, "rev-list", "--count", tip))
			if got != strconv.Itoa(depth) {
				t.Errorf("depth %d produced %s commits, want %d", depth, got, depth)
			}
		})
	}
}

// TestShallowBoundaryIsWrittenInABareRepository pins the GIT_DIR handling.
//
// markShallowBoundary used to append ".git" to GIT_DIR whenever the basename
// was not already ".git". In a bare repository - the shape every clone target
// has - that pointed at a directory which does not exist, so the boundary was
// never written and the repository was not shallow at all.
func TestShallowBoundaryIsWrittenInABareRepository(t *testing.T) {
	reg := newMockRegistry()
	ts := reg.Server()
	defer ts.Close()

	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	tip := shallowFixture(t, registry, 3)

	dst := newBareRepo(t) // no ".git" suffix
	t.Setenv("GIT_DIR", dst)
	if out, err := runHelper(t, registry, "option depth 1\nlist\nfetch "+tip+" refs/heads/main\n\n"); err != nil {
		t.Fatalf("fetch failed: %v (output %q)", err, out)
	}

	if !fileHasLines(t, filepath.Join(dst, "shallow")) {
		t.Errorf("no shallow boundary at %s", filepath.Join(dst, "shallow"))
	}
}

// TestShallowCloneFetchesOnlyTheSnapshot pins the point of the snapshot layer.
//
// A shallow clone needs the boundary commit's *complete tree*, and the stored
// packfiles are incremental: a file untouched since the first commit lives in
// the first packfile, so stopping the pack-base walk early yields the commit
// without its content and git rejects the result. That is why --depth used to
// transfer the whole history and save nothing.
//
// Compacting with gc does not fix it either: it collapses the chain to one
// packfile, but that packfile still contains every commit, so a depth-1 clone
// costs what a full clone costs.
//
// The snapshot is the missing artifact — a self-contained packfile holding
// exactly the objects reachable from the tip, with no ancestry — and taking it
// is what makes --depth 1 cheap.
func TestShallowCloneFetchesOnlyTheSnapshot(t *testing.T) {
	reg := newMockRegistry()
	ts := reg.Server()
	defer ts.Close()

	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	tip := shallowFixtureWithSnapshots(t, registry, 6)

	measure := func(script string) int {
		dst := newBareRepo(t)
		t.Setenv("GIT_DIR", dst)
		reg.mu.Lock()
		reg.requests = nil
		reg.mu.Unlock()
		if out, err := runHelper(t, registry, script); err != nil {
			t.Fatalf("fetch failed: %v (output %q)", err, out)
		}
		return blobFetches(reg)
	}

	full := measure("list\nfetch " + tip + " refs/heads/main\n\n")
	shallow := measure("option depth 1\nlist\nfetch " + tip + " refs/heads/main\n\n")

	if shallow >= full {
		t.Errorf("shallow fetch downloaded %d blobs and the full fetch %d; "+
			"the snapshot layer is not being used", shallow, full)
	}
	// Six pushes, so a walk costs six packs and a snapshot costs one.
	if shallow > 2 {
		t.Errorf("shallow fetch downloaded %d blobs; a depth-1 clone should need "+
			"the snapshot and little else", shallow)
	}
	t.Logf("depth-1 fetched %d blobs, full fetched %d", shallow, full)
}

// TestShallowCloneIsCompleteAtTheBoundary: cheaper is worthless if the result
// does not check out. The snapshot must carry the tip's whole tree.
func TestShallowCloneIsCompleteAtTheBoundary(t *testing.T) {
	reg := newMockRegistry()
	ts := reg.Server()
	defer ts.Close()

	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	tip := shallowFixtureWithSnapshots(t, registry, 6)

	dst := newBareRepo(t)
	t.Setenv("GIT_DIR", dst)
	if out, err := runHelper(t, registry, "option depth 1\nlist\nfetch "+tip+" refs/heads/main\n\n"); err != nil {
		t.Fatalf("shallow fetch failed: %v (output %q)", err, out)
	}

	// Every path in the tip tree must be readable, including files last
	// touched by the very first commit.
	if out := gitOutput(t, dst, "cat-file", "-e", tip+"^{tree}"); out != "" {
		t.Fatalf("the tip tree is not present after a shallow fetch: %s", out)
	}
	if out := gitOutput(t, dst, "ls-tree", "-r", tip); out != "" {
		t.Fatalf("the tip tree is not fully readable: %s", out)
	}
	if !fileHasLines(t, filepath.Join(dst, "shallow")) {
		t.Error("no shallow boundary was recorded")
	}
}

// TestShallowSnapshotIsOffByDefault pins the default and the fallback.
//
// The snapshot costs a second copy of the tip on every push, which most
// repositories should not pay for, so it is opt-in. With it off, --depth 1
// still works: it walks the packfiles as it always did and produces a correct,
// checkoutable repository.
func TestShallowSnapshotIsOffByDefault(t *testing.T) {
	reg := newMockRegistry()
	ts := reg.Server()
	defer ts.Close()

	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	src := newWorkRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(src, ".git"))
	// No config: this is the out-of-the-box behaviour.

	var tip string
	for i := range 3 {
		tip = commitFile(t, src, fmt.Sprintf("f%d.txt", i), fmt.Sprintf("v%d\n", i), "commit")
		if out, err := runHelper(t, registry, "list for-push\npush refs/heads/main:refs/heads/main\n\n"); err != nil {
			t.Fatalf("push %d failed: %v (output %q)", i, err, out)
		}
	}

	// Nothing published, so a depth-1 fetch falls back to the pack-base walk
	// and still produces a correct, checkoutable repository.
	dst := newBareRepo(t)
	t.Setenv("GIT_DIR", dst)
	if out, err := runHelper(t, registry, "option depth 1\nlist\nfetch "+tip+" refs/heads/main\n\n"); err != nil {
		t.Fatalf("shallow fetch without a snapshot failed: %v (output %q)", err, out)
	}
	if out := gitOutput(t, dst, "ls-tree", "-r", tip); out != "" {
		t.Fatalf("the fallback produced an incomplete tree: %s", out)
	}
}

// shallowFixtureWithSnapshots is shallowFixture with the snapshot layer turned
// on, which is opt-in.
func shallowFixtureWithSnapshots(t *testing.T, registry string, commits int) string {
	t.Helper()

	src := newWorkRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(src, ".git"))
	git(t, src, "config", "ociremote.shallowSnapshot", "true")

	var tip string
	for i := range commits {
		tip = commitFile(t, src, fmt.Sprintf("f%d.txt", i), fmt.Sprintf("v%d\n", i), "commit")
		if out, err := runHelper(t, registry, "list for-push\npush refs/heads/main:refs/heads/main\n\n"); err != nil {
			t.Fatalf("push %d failed: %v (output %q)", i, err, out)
		}
	}
	return tip
}

// fileHasLines reports whether path exists and has any non-blank content.
func fileHasLines(t *testing.T, path string) bool {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) != ""
}

// runGitOut runs git against a bare repository and returns stdout.
func runGitOut(t *testing.T, gitDir string, args ...string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", append([]string{"--git-dir=" + gitDir}, args...)...)
	cmd.Env = gitEnvWithoutGitDir()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}
