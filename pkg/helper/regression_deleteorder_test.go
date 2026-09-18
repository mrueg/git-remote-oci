package helper_test

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// Deleting a ref is the one push step a registry cannot take back, which makes
// *when* it is recorded and *when* it runs matter more than for an upload.

// TestFailedDeletionKeepsRefInIndex pins that a refused deletion is not
// recorded as one.
//
// The non-atomic path marked the ref deleted before asking the registry, so
// when the registry refused, the batch still told the index update to drop
// it. The listing then said the ref was gone while its tag stayed exactly
// where it was -- and git had been told `error`, so the user was left with a
// remote that disagreed with itself about a ref they believed still existed.
func TestFailedDeletionKeepsRefInIndex(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	src := newWorkRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(src, ".git"))

	git(t, src, "branch", "dev")
	if out, err := runHelper(t, registry,
		"list for-push\npush refs/heads/main:refs/heads/main\npush refs/heads/dev:refs/heads/dev\n\n"); err != nil {
		t.Fatalf("seeding push failed: %v (output %q)", err, out)
	}

	// The registry refuses the deletion outright. Not with 405, which the
	// client treats as "deletion unsupported" and answers with a tombstone,
	// but with a failure it has to report.
	reg.Intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		return false
	})

	// main moves in the same batch, so the index *is* rewritten -- the case
	// in which a wrongly recorded deletion would reach it.
	commitFile(t, src, "second.txt", "second\n", "commit two")
	out, err := runHelper(t, registry,
		"list for-push\npush refs/heads/main:refs/heads/main\npush :refs/heads/dev\n\n")
	if err != nil {
		t.Fatalf("push returned a batch-level error: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "error refs/heads/dev") {
		t.Errorf("git was not told the deletion failed:\n%s", out)
	}
	if !strings.Contains(out, "ok refs/heads/main") {
		t.Errorf("the update of main should still have gone through:\n%s", out)
	}

	reg.Intercept(nil)

	listOut, err := runHelper(t, registry, "list\n\n")
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if !strings.Contains(listOut, "refs/heads/dev") {
		t.Errorf("a deletion the registry refused was recorded in the index anyway; list returned:\n%s", listOut)
	}
	if _, present := refTagRevision(t, reg, "refs/heads/dev"); !present {
		t.Errorf("the ref tag for refs/heads/dev is gone even though the registry refused the DELETE")
	}
}

// TestAtomicPushKeepsDeletedRefWhenAnotherRefFails pins that --atomic covers
// deletions.
//
// The atomic path ran deletions inline, in batch order, and its rollback only
// restored the tags it had snapshotted before overwriting. So
// `git push --atomic origin :old new` deleted old, failed on new, and left old
// deleted -- the half-applied batch --atomic exists to rule out. Deletions now
// run only once every upload has landed.
func TestAtomicPushKeepsDeletedRefWhenAnotherRefFails(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	src := newWorkRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(src, ".git"))

	git(t, src, "branch", "old")
	if out, err := runHelper(t, registry,
		"list for-push\npush refs/heads/main:refs/heads/main\npush refs/heads/old:refs/heads/old\n\n"); err != nil {
		t.Fatalf("seeding push failed: %v (output %q)", err, out)
	}
	oldSHA, present := refTagRevision(t, reg, "refs/heads/old")
	if !present {
		t.Fatal("refs/heads/old was not published")
	}

	git(t, src, "checkout", "-q", "-b", "new")
	commitFile(t, src, "new.txt", "new\n", "on new")
	git(t, src, "checkout", "-q", "main")

	// Fail the ref manifest write for new, and nothing else.
	reg.Intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/manifests/"+oci.EncodeRefTag("refs/heads/new")) {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		return false
	})

	out, err := runHelper(t, registry,
		"option atomic true\nlist for-push\npush :refs/heads/old\npush refs/heads/new:refs/heads/new\n\n")
	if err != nil {
		t.Fatalf("push returned a batch-level error: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "error refs/heads/new") || !strings.Contains(out, "error refs/heads/old") {
		t.Fatalf("expected the atomic batch to fail as a whole, got:\n%s", out)
	}
	if strings.Contains(out, "could not be rolled back") {
		t.Errorf("the batch reports a ref it could not roll back, so a deletion ran before the failure:\n%s", out)
	}

	reg.Intercept(nil)

	got, present := refTagRevision(t, reg, "refs/heads/old")
	if !present {
		t.Fatal("refs/heads/old was deleted by an atomic batch that failed")
	}
	if got != oldSHA {
		t.Errorf("refs/heads/old resolves to %s, want %s", shortID(got), shortID(oldSHA))
	}

	listOut, err := runHelper(t, registry, "list\n\n")
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if !strings.Contains(listOut, "refs/heads/old") {
		t.Errorf("refs/heads/old is missing from the index after a failed atomic batch:\n%s", listOut)
	}
	if strings.Contains(listOut, "refs/heads/new") {
		t.Errorf("refs/heads/new is listed although its push failed:\n%s", listOut)
	}
}
