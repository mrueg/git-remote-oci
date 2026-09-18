package helper_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// Every push publishes a commit manifest and a commit tag, and neither can be
// removed until the history is repacked to stand alone -- the tag is a pack
// base later pushes were cut against. So the count only grows, and a clone pays
// it: one manifest to fetch and one packfile to index per push.
//
// `gc` fixed that on demand, which meant it happened to repositories somebody
// was minding. These pin the version that happens by itself.

// pushN makes n separate pushes to url, one commit each.
func pushN(t *testing.T, url string, n int, extraConfig ...string) {
	t.Helper()
	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main", src)
	for i := 0; i < n; i++ {
		name := string(rune('a' + i%26))
		if err := os.WriteFile(filepath.Join(src, "f"+name+".txt"),
			[]byte(strings.Repeat(name, 8)+string(rune('0'+i%10))), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, src, "-C", src, "add", ".")
		git(t, src, "-C", src, "commit", "-q", "-m", "commit "+name)

		args := append([]string{"-C", src}, extraConfig...)
		args = append(args, "push", "-q", url, "main")
		git(t, src, args...)
	}
}

// TestAutoCompactTriggersAtTheThreshold.
//
// Six pushes against a threshold of three: the run that crosses it repacks, and
// what is left is one self-contained manifest per ref rather than one per push.
func TestAutoCompactTriggersAtTheThreshold(t *testing.T) {
	url, reg := v2setupRegistry(t)
	pushN(t, url, 6, "-c", "ociremote.compactAfter=3")

	chain := packChainOf(t, reg)
	if len(chain) > 3 {
		t.Errorf("the chain still holds %d commits after crossing a threshold of 3, so nothing compacted: %v",
			len(chain), chain)
	}

	// Compaction is only worth anything if the result still clones. A repack
	// that lost history would satisfy the count above perfectly.
	parent := t.TempDir()
	if out, err := v2run(t, parent, nil, "clone", url, "dst"); err != nil {
		t.Fatalf("clone after automatic compaction: %v\n%s", err, out)
	}
	dst := filepath.Join(parent, "dst")
	if out, err := v2run(t, dst, nil, "fsck"); err != nil {
		t.Fatalf("fsck: %v\n%s", err, out)
	}
	if out, _ := v2run(t, dst, nil, "rev-list", "--count", "HEAD"); strings.TrimSpace(out) != "6" {
		t.Errorf("cloned %s commits, want 6: compaction dropped history", strings.TrimSpace(out))
	}

	// Tag accumulation is the thing compaction exists to stop: one commit tag
	// per push, none of them removable until the history is repacked. Six
	// pushes would leave six.
	//
	// Not zero, and not one: pushes after the last compaction have published
	// their own again, which is the steady state rather than a failure. The
	// point is that the count is bounded by the threshold instead of by how
	// long the repository has existed.
	if commits := commitTagsIn(reg); commits >= 6 {
		t.Errorf("%d commit tags after 6 pushes; compaction pruned nothing", commits)
	}
}

// commitTagsIn counts the commit-id tags the registry holds.
func commitTagsIn(reg *registrytest.Registry) int {
	n := 0
	for _, tag := range reg.Tags() {
		if oci.ClassifyTag(tag) == oci.TagClassCommit {
			n++
		}
	}
	return n
}

// TestAutoCompactLeavesSmallRepositoriesAlone: below the threshold nothing
// should be repacked, because repacking is the expensive thing this schedules.
func TestAutoCompactLeavesSmallRepositoriesAlone(t *testing.T) {
	url, reg := v2setupRegistry(t)
	pushN(t, url, 3, "-c", "ociremote.compactAfter=50")

	chain := packChainOf(t, reg)
	if len(chain) != 3 {
		t.Errorf("the chain holds %d commits after 3 pushes, want 3: something compacted early: %v",
			len(chain), chain)
	}
}

// TestAutoCompactCanBeDisabled pins the escape hatch. Repacking re-uploads the
// whole history, and someone running `gc` on a schedule of their own should not
// also have it happen mid-push.
func TestAutoCompactCanBeDisabled(t *testing.T) {
	url, reg := v2setupRegistry(t)
	pushN(t, url, 5, "-c", "ociremote.compactAfter=0")

	chain := packChainOf(t, reg)
	if len(chain) != 5 {
		t.Errorf("the chain holds %d commits with compaction disabled, want 5: %v", len(chain), chain)
	}
}

// TestAutoCompactFailureDoesNotFailThePush.
//
// Compaction runs after the push has already reported success, and it must stay
// that way: a repository that could not be tidied is untidy, while a push that
// failed because tidying failed is broken -- and the user asked for the push.
func TestAutoCompactFailureDoesNotFailThePush(t *testing.T) {
	url, reg := v2setupRegistry(t)
	pushN(t, url, 3, "-c", "ociremote.compactAfter=2")

	// Break everything compaction needs, and nothing the push needs: a
	// registry that refuses to delete a tag is a real and common
	// configuration, and consolidation writes before it prunes.
	reg.Intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return true
		}
		return false
	})

	src := t.TempDir()
	git(t, src, "clone", "-q", url, src)
	if err := os.WriteFile(filepath.Join(src, "after.txt"), []byte("after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "-C", src, "add", ".")
	git(t, src, "-C", src, "commit", "-q", "-m", "after")

	out, err := v2run(t, src, nil, "-c", "ociremote.compactAfter=2", "push", url, "main")
	if err != nil {
		t.Fatalf("a push whose compaction could not prune should still succeed: %v\n%s", err, out)
	}
}

// TestTimingBreakdownNamesRealPhases.
//
// The unit tests cover the timer; this covers the wiring, which is the part
// that rots. A phase left uninstrumented does not fail anything — the total
// simply stops accounting for it, and the output goes on looking plausible.
func TestTimingBreakdownNamesRealPhases(t *testing.T) {
	url, _ := v2setupRegistry(t)
	pushN(t, url, 3, "-c", "ociremote.compactAfter=0")

	parent := t.TempDir()
	out, err := v2run(t, parent, []string{"GIT_REMOTE_OCI_TIMING=1"}, "clone", url, "dst")
	if err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}

	if !strings.Contains(out, "[timing]") {
		t.Fatalf("GIT_REMOTE_OCI_TIMING=1 produced no breakdown:\n%s", out)
	}
	// The phases a clone must pass through. Resolving the graph is round
	// trips, staging is local CPU, and telling them apart is the entire point.
	for _, phase := range []string{"resolve pack graph", "stage packfiles"} {
		if !strings.Contains(out, phase) {
			t.Errorf("the breakdown does not mention %q, so that phase is uninstrumented:\n%s", phase, out)
		}
	}

	// And silent unless asked, since it goes to the same stderr git shows users.
	quiet := t.TempDir()
	plain, err := v2run(t, quiet, nil, "clone", url, "dst2")
	if err != nil {
		t.Fatalf("clone: %v\n%s", err, plain)
	}
	if strings.Contains(plain, "[timing]") {
		t.Errorf("timing output appeared without the environment variable:\n%s", plain)
	}
}
