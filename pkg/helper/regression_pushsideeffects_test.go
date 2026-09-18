package helper_test

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// seedPush publishes refs/heads/main to registry.
func seedPush(t *testing.T, registry string) {
	t.Helper()

	if out, err := runHelper(t, registry, "list for-push\npush refs/heads/main:refs/heads/main\n\n"); err != nil {
		t.Fatalf("seeding push failed: %v (output %q)", err, out)
	}
}

// requestsMatching returns the recorded requests containing substr.
func requestsMatching(reg *mockRegistry, substr string) []string {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	var hits []string
	for _, req := range reg.requests {
		if strings.Contains(req, substr) {
			hits = append(hits, req)
		}
	}
	return hits
}

// TestDryRunDoesNotDeleteRemoteRef pins that --dry-run is read-only.
//
// The delete branch of the non-atomic push path ran before any dry-run check,
// so `git push --dry-run origin :branch` really deleted the branch and the
// underlying OCI manifest. The atomic path already got this right.
func TestDryRunDoesNotDeleteRemoteRef(t *testing.T) {
	reg := newMockRegistry()
	ts := reg.Server()
	defer ts.Close()

	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	src := newWorkRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(src, ".git"))
	seedPush(t, registry)

	reg.mu.Lock()
	reg.requests = nil
	reg.mu.Unlock()

	out, err := runHelper(t, registry, "option dry-run true\nlist for-push\npush :refs/heads/main\n\n")
	if err != nil {
		t.Fatalf("dry-run delete failed: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "ok refs/heads/main") {
		t.Errorf("dry-run delete should report success, got:\n%s", out)
	}

	if deletes := requestsMatching(reg, "DELETE "); len(deletes) > 0 {
		t.Errorf("dry-run issued %d DELETE request(s): %v", len(deletes), deletes)
	}

	// And the ref must still be there afterwards.
	listOut, err := runHelper(t, registry, "list\n\n")
	if err != nil {
		t.Fatalf("list after dry-run failed: %v", err)
	}
	if !strings.Contains(listOut, "refs/heads/main") {
		t.Errorf("refs/heads/main disappeared after a dry-run delete; list returned:\n%s", listOut)
	}
}

// TestDryRunDoesNotRewriteRefsIndex pins the other unguarded write.
//
// The _refs index rewrite ran unconditionally at the end of every non-atomic
// batch: it takes the index lock, pushes blobs and pushes a manifest, none of
// which belongs in a dry run.
func TestDryRunDoesNotRewriteRefsIndex(t *testing.T) {
	reg := newMockRegistry()
	ts := reg.Server()
	defer ts.Close()

	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	src := newWorkRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(src, ".git"))
	seedPush(t, registry)

	reg.mu.Lock()
	before := string(reg.manifests[oci.TagRefIndex])
	reg.requests = nil
	reg.mu.Unlock()

	commitFile(t, src, "second.txt", "second\n", "commit two")
	out, err := runHelper(t, registry, "option dry-run true\nlist for-push\npush refs/heads/main:refs/heads/main\n\n")
	if err != nil {
		t.Fatalf("dry-run push failed: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "ok refs/heads/main") {
		t.Errorf("dry-run push should report success, got:\n%s", out)
	}

	for _, req := range requestsMatching(reg, "/manifests/") {
		if strings.HasPrefix(req, "PUT ") {
			t.Errorf("dry-run wrote a manifest: %s", req)
		}
	}

	reg.mu.Lock()
	after := string(reg.manifests[oci.TagRefIndex])
	reg.mu.Unlock()
	if before != after {
		t.Error("dry-run rewrote the _refs index")
	}
}

// TestPushReportsErrorWhenRefsIndexUpdateFails pins that a push is not called
// successful until the ref is actually discoverable.
//
// "ok" used to be written from inside the worker, before the _refs index was
// rewritten, and a failed rewrite was only a warning. Since list prefers the
// index over tag enumeration, that left the remote advertising the old SHA
// while git recorded the push as done - and the next push then made its
// fast-forward and force-with-lease decisions from the stale value.
func TestPushReportsErrorWhenRefsIndexUpdateFails(t *testing.T) {
	reg := newMockRegistry()
	ts := reg.Server()
	defer ts.Close()

	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	src := newWorkRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(src, ".git"))
	seedPush(t, registry)

	// Fail only the index write, leaving the commit and ref manifests to land
	// normally, which is exactly the partial-failure shape that used to be
	// reported as success.
	reg.mu.Lock()
	reg.intercept = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/manifests/"+oci.TagRefIndex) {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		return false
	}
	reg.mu.Unlock()

	commitFile(t, src, "second.txt", "second\n", "commit two")
	out, err := runHelper(t, registry, "list for-push\npush refs/heads/main:refs/heads/main\n\n")
	if err != nil {
		t.Fatalf("push returned a batch-level error: %v (output %q)", err, out)
	}

	if strings.Contains(out, "ok refs/heads/main") {
		t.Errorf("push reported success even though the _refs index update failed:\n%s", out)
	}
	if !strings.Contains(out, "error refs/heads/main") {
		t.Errorf("push should report an error for the ref, got:\n%s", out)
	}
}

// TestNonAtomicPushAcquiresRefLock pins that an ordinary push takes the lock.
//
// Only the --atomic path used to acquire it; the non-atomic path merely tested
// whether one was held. Two concurrent `git push` runs therefore both saw an
// unlocked ref and both went ahead, so the lock constrained nothing except
// other --atomic pushers.
func TestNonAtomicPushAcquiresRefLock(t *testing.T) {
	reg := newMockRegistry()
	ts := reg.Server()
	defer ts.Close()

	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	src := newWorkRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(src, ".git"))
	seedPush(t, registry)

	// Another client holds the lock.
	client, err := oci.NewClient(registry, true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.AcquireRefLock(context.Background(), "refs/heads/main", time.Minute); err != nil {
		t.Fatalf("AcquireRefLock: %v", err)
	}

	commitFile(t, src, "second.txt", "second\n", "commit two")
	out, err := runHelper(t, registry, "list for-push\npush refs/heads/main:refs/heads/main\n\n")
	if err != nil {
		t.Fatalf("push returned a batch-level error: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "error refs/heads/main") || !strings.Contains(out, "reference is locked") {
		t.Errorf("push should have been refused while the ref is locked, got:\n%s", out)
	}

	// Once released, the same push must succeed - and be able to take the lock
	// itself, which only works if the refused attempt did not leave one behind.
	if err := client.ReleaseRefLock(context.Background(), "refs/heads/main"); err != nil {
		t.Fatalf("ReleaseRefLock: %v", err)
	}

	reg.mu.Lock()
	reg.requests = nil
	reg.mu.Unlock()

	out, err = runHelper(t, registry, "list for-push\npush refs/heads/main:refs/heads/main\n\n")
	if err != nil {
		t.Fatalf("push after release failed: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "ok refs/heads/main") {
		t.Errorf("push should succeed once the lock is released, got:\n%s", out)
	}

	// The discriminating assertion. Refusing while someone else holds the lock
	// only needs a read, which the old code did; actually taking the lock is
	// what stops two concurrent pushes from both proceeding, and that shows up
	// as a write to the ref's lock tag.
	lockTag := oci.LockTag("refs/heads/main")
	var acquired bool
	for _, req := range requestsMatching(reg, "/manifests/"+lockTag) {
		if strings.HasPrefix(req, "PUT ") {
			acquired = true
			break
		}
	}
	if !acquired {
		t.Errorf("a successful non-atomic push never wrote %s, so it only tested the lock instead of holding it", lockTag)
	}
}

// TestPushReleasesRefLockOnFailure checks that a refused push does not strand
// the lock it took.
func TestPushReleasesRefLockOnFailure(t *testing.T) {
	reg := newMockRegistry()
	ts := reg.Server()
	defer ts.Close()

	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	src := newWorkRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(src, ".git"))
	seedPush(t, registry)

	// Diverge from the remote so the push is rejected *after* the lock has been
	// taken: advance the remote by one commit, then replace that commit locally
	// with a different one on the same base.
	base := git(t, src, "rev-parse", "HEAD")
	commitFile(t, src, "remote-side.txt", "remote\n", "commit that reaches the remote")
	if out, err := runHelper(t, registry, "list for-push\npush refs/heads/main:refs/heads/main\n\n"); err != nil {
		t.Fatalf("advancing the remote failed: %v (output %q)", err, out)
	}
	git(t, src, "reset", "-q", "--hard", base)
	commitFile(t, src, "local-side.txt", "local\n", "divergent local commit")

	out, err := runHelper(t, registry, "list for-push\npush refs/heads/main:refs/heads/main\n\n")
	if err != nil {
		t.Fatalf("push returned a batch-level error: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "error refs/heads/main") {
		t.Fatalf("expected the non-fast-forward push to be rejected, got:\n%s", out)
	}

	client, err := oci.NewClient(registry, true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	locked, info, err := client.IsLocked(context.Background(), "refs/heads/main")
	if err != nil {
		t.Fatalf("IsLocked: %v", err)
	}
	if locked {
		owner := ""
		if info != nil {
			owner = info.Owner
		}
		t.Errorf("a rejected push left the ref locked by %q", owner)
	}
}
