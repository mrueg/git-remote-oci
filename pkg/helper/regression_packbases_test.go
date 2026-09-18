package helper_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// git runs a git command in dir and fails the test if it does not succeed.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(gitEnvWithoutGitDir(),
		"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitEnvWithoutGitDir returns the environment with GIT_DIR removed.
//
// The tests point GIT_DIR at whichever repository the helper is operating on,
// but these git calls name their repository explicitly and would otherwise be
// redirected by it. GIT_DIR has to be absent rather than empty: git rejects an
// empty value outright.
func gitEnvWithoutGitDir() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_DIR=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// newWorkRepo creates a repository with one commit and returns its directory.
func newWorkRepo(t *testing.T) string {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}

	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main", ".")
	commitFile(t, dir, "first.txt", "first\n", "commit one")
	return dir
}

// commitFile writes a file and commits it.
func commitFile(t *testing.T, dir, name, content, message string) string {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile %s: %v", name, err)
	}
	git(t, dir, "add", name)
	git(t, dir, "commit", "-q", "-m", message)
	return git(t, dir, "rev-parse", "HEAD")
}

// newBareRepo creates an empty bare repository to fetch into.
func newBareRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	git(t, dir, "init", "-q", "--bare", ".")
	return dir
}

// assertComplete fails unless every object reachable from sha is present in
// gitDir.
//
// It walks trees and blobs, not just commits: the failure this guards against
// leaves the commit objects intact and drops the blobs an earlier packfile
// carried, so a commit-only check sails straight past it.
func assertComplete(t *testing.T, gitDir, sha string) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", "--git-dir="+gitDir, "rev-list", "--objects", "--no-object-names", "--missing=print", sha)
	cmd.Env = gitEnvWithoutGitDir()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rev-list for %s: %v\n%s", sha, err, out)
	}
	var missing []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "?") {
			missing = append(missing, strings.TrimSpace(line))
		}
	}
	if len(missing) > 0 {
		t.Fatalf("history of %s is incomplete: %d objects missing, e.g. %v", sha, len(missing), missing[:min(5, len(missing))])
	}
}

// packBasesOf reads the pack-bases annotation off a tag in the registry.
func packBasesOf(t *testing.T, reg *registrytest.Registry, tag string) (string, bool) {
	t.Helper()

	var m ocispec.Manifest
	if err := json.Unmarshal(reg.RawManifest(t, tag), &m); err != nil {
		t.Fatalf("unmarshal manifest %q: %v", tag, err)
	}
	v, present := m.Annotations[oci.AnnotationGitPackBases]
	return v, present
}

// TestMultiCommitPushCloneHasFullHistory is the regression test for the bug
// that motivated pack-base tracking.
//
// A push publishes a manifest only for the tip of each refspec, and cuts its
// packfile against whatever the remote already had. When the push carries more
// than one commit, the tip's parent is an intermediate commit that was never
// tagged, so a fetcher walking the parent annotation stops there and never
// reaches the commit the packfile was actually cut against. git index-pack
// accepts the truncated result, git updates the ref, and the objects the first
// push contributed are simply absent.
//
// The shape matters: one commit per push, which is what every other test does,
// makes each parent a previous tip and hides the bug entirely.
func TestMultiCommitPushCloneHasFullHistory(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	src := newWorkRepo(t)

	// Push one: the tip is commit one, and its packfile is self-contained.
	t.Setenv("GIT_DIR", filepath.Join(src, ".git"))
	if out, err := runHelper(t, registry, "list for-push\npush refs/heads/main:refs/heads/main\n\n"); err != nil {
		t.Fatalf("first push failed: %v (output %q)", err, out)
	}

	// Push two: *two* commits at once, so the new tip's parent is a commit that
	// never gets a manifest of its own.
	commitFile(t, src, "second.txt", "second\n", "commit two")
	tip := commitFile(t, src, "third.txt", "third\n", "commit three")
	if out, err := runHelper(t, registry, "list for-push\npush refs/heads/main:refs/heads/main\n\n"); err != nil {
		t.Fatalf("second push failed: %v (output %q)", err, out)
	}

	// Clone into an empty repository.
	dst := newBareRepo(t)
	t.Setenv("GIT_DIR", dst)
	out, err := runHelper(t, registry, "list\nfetch "+tip+" refs/heads/main\n\n")
	if err != nil {
		t.Fatalf("fetch failed: %v (output %q)", err, out)
	}

	// Before the fix this passes for the commit objects and fails for the blob
	// that commit one introduced, which is exactly how the bug reaches a user:
	// as a checkout failure rather than a fetch failure.
	assertComplete(t, dst, tip)

	for _, name := range []string{"first.txt", "second.txt", "third.txt"} {
		if got := gitOutput(t, dst, "cat-file", "-e", tip+":"+name); got != "" {
			t.Errorf("%s is not readable from the fetched tip: %s", name, got)
		}
	}
}

// gitOutput runs git against a bare repository and returns any error output
// rather than failing, so callers can assert on it.
func gitOutput(t *testing.T, gitDir string, args ...string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", append([]string{"--git-dir=" + gitDir}, args...)...)
	cmd.Env = gitEnvWithoutGitDir()
	out, err := cmd.CombinedOutput()
	if err != nil {
		if len(out) == 0 {
			return err.Error()
		}
		return strings.TrimSpace(string(out))
	}
	return ""
}

// TestPushRecordsPackBases pins the annotation that fetch depends on.
func TestPushRecordsPackBases(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	src := newWorkRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(src, ".git"))

	first := git(t, src, "rev-parse", "HEAD")
	if out, err := runHelper(t, registry, "list for-push\npush refs/heads/main:refs/heads/main\n\n"); err != nil {
		t.Fatalf("first push failed: %v (output %q)", err, out)
	}

	// A repository with nothing on it yet cannot exclude anything, so the first
	// packfile must declare itself self-contained rather than say nothing.
	if got, present := packBasesOf(t, reg, first); !present || got != oci.PackBasesNone {
		t.Errorf("first push: pack-bases = %q (present %v), want %q", got, present, oci.PackBasesNone)
	}

	tip := commitFile(t, src, "second.txt", "second\n", "commit two")
	if out, err := runHelper(t, registry, "list for-push\npush refs/heads/main:refs/heads/main\n\n"); err != nil {
		t.Fatalf("second push failed: %v (output %q)", err, out)
	}

	if got, present := packBasesOf(t, reg, tip); !present || got != first {
		t.Errorf("second push: pack-bases = %q (present %v), want %q", got, present, first)
	}
	// The ref manifest is what a ref-driven fetch reads, so it has to carry the
	// same declaration as the commit manifest.
	if got, present := packBasesOf(t, reg, "main"); !present || got != first {
		t.Errorf("second push ref manifest: pack-bases = %q (present %v), want %q", got, present, first)
	}
}

// TestFetchFailsWhenBaseManifestMissing pins that an unreachable base is a hard
// error rather than a warning.
//
// Reported as a warning it disappeared entirely under `git fetch -q`, because
// logWarn is gated on the same verbosity as ordinary progress output, so the
// user got a silently truncated repository and a zero exit status.
func TestFetchFailsWhenBaseManifestMissing(t *testing.T) {
	for _, verbosity := range []string{"1", "0"} {
		t.Run("verbosity"+verbosity, func(t *testing.T) {
			reg := registrytest.New()
			ts := reg.Serve(t)

			registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
			t.Setenv("OCI_INSECURE", "1")

			src := newWorkRepo(t)
			t.Setenv("GIT_DIR", filepath.Join(src, ".git"))

			base := git(t, src, "rev-parse", "HEAD")
			if out, err := runHelper(t, registry, "list for-push\npush refs/heads/main:refs/heads/main\n\n"); err != nil {
				t.Fatalf("first push failed: %v (output %q)", err, out)
			}
			tip := commitFile(t, src, "second.txt", "second\n", "commit two")
			if out, err := runHelper(t, registry, "list for-push\npush refs/heads/main:refs/heads/main\n\n"); err != nil {
				t.Fatalf("second push failed: %v (output %q)", err, out)
			}

			// Delete the base the second packfile was cut against.
			reg.DropManifest(base)

			dst := newBareRepo(t)
			t.Setenv("GIT_DIR", dst)
			script := "option verbosity " + verbosity + "\nlist\nfetch " + tip + " refs/heads/main\n\n"
			_, err := runHelper(t, registry, script)
			if err == nil {
				t.Fatal("fetch succeeded even though the packfile's base is gone; it must fail rather than write a truncated repository")
			}
			if !strings.Contains(err.Error(), "packed against") {
				t.Errorf("error should name the missing base, got: %v", err)
			}
		})
	}
}

// TestForcePushProducesFetchableHistory covers the have-set on a force push.
//
// Rewriting history left the old tip in the have-set even though it is not an
// ancestor of the new one, so the packfile excluded objects that no manifest
// reachable from the new tip provides.
func TestForcePushProducesFetchableHistory(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	src := newWorkRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(src, ".git"))

	commitFile(t, src, "second.txt", "second\n", "commit two")
	if out, err := runHelper(t, registry, "list for-push\npush refs/heads/main:refs/heads/main\n\n"); err != nil {
		t.Fatalf("initial push failed: %v (output %q)", err, out)
	}
	old := git(t, src, "rev-parse", "HEAD")

	// Amend, which keeps the tree but produces an unrelated commit id.
	git(t, src, "commit", "-q", "--amend", "-m", "commit two, reworded")
	tip := git(t, src, "rev-parse", "HEAD")
	if tip == old {
		t.Fatal("amend did not change the commit id")
	}

	if out, err := runHelper(t, registry, "list for-push\npush +refs/heads/main:refs/heads/main\n\n"); err != nil {
		t.Fatalf("force push failed: %v (output %q)", err, out)
	}

	if got, _ := packBasesOf(t, reg, tip); strings.Contains(got, old) {
		t.Errorf("force push declared the replaced tip %s as a pack base (%q); it is not an ancestor of %s", old, got, tip)
	}

	dst := newBareRepo(t)
	t.Setenv("GIT_DIR", dst)
	if out, err := runHelper(t, registry, "list\nfetch "+tip+" refs/heads/main\n\n"); err != nil {
		t.Fatalf("fetch after force push failed: %v (output %q)", err, out)
	}
	assertComplete(t, dst, tip)
}

// TestAtomicPushOfSecondBranchIsSelfSufficient covers the atomic path's
// have-set.
//
// It qualified a base by whether the commit merely existed locally, so pushing
// one branch excluded objects reachable from an unrelated branch that happened
// to be on the remote, and a single-branch clone never received them.
func TestAtomicPushOfSecondBranchIsSelfSufficient(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	src := newWorkRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(src, ".git"))

	// Branch "other" shares the root commit's objects with "main" but is not
	// its descendant.
	root := git(t, src, "rev-parse", "HEAD")
	commitFile(t, src, "on-main.txt", "main\n", "main work")
	if out, err := runHelper(t, registry, "list for-push\npush refs/heads/main:refs/heads/main\n\n"); err != nil {
		t.Fatalf("main push failed: %v (output %q)", err, out)
	}

	git(t, src, "checkout", "-q", "-b", "other", root)
	other := commitFile(t, src, "on-other.txt", "other\n", "other work")

	if out, err := runHelper(t, registry, "option atomic true\nlist for-push\npush refs/heads/other:refs/heads/other\n\n"); err != nil {
		t.Fatalf("atomic push failed: %v (output %q)", err, out)
	}

	// Fetch only "other", as `git clone --single-branch --branch other` would.
	dst := newBareRepo(t)
	t.Setenv("GIT_DIR", dst)
	if out, err := runHelper(t, registry, "list\nfetch "+other+" refs/heads/other\n\n"); err != nil {
		t.Fatalf("single-branch fetch failed: %v (output %q)", err, out)
	}
	assertComplete(t, dst, other)
}
