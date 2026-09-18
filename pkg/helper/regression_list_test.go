package helper_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/helper"
)

// newCommitRepo creates a git repository with a single commit and returns its
// path. GIT_DIR is pointed at it for the duration of the test.
func newCommitRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0644); err != nil {
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

	t.Setenv("GIT_DIR", filepath.Join(dir, ".git"))
	return dir
}

// runHelper feeds script to a Helper pointed at registryURL and returns stdout.
func runHelper(t *testing.T, registryURL, script string) (string, error) {
	t.Helper()

	var out strings.Builder
	h, err := helper.NewHelper("origin", registryURL, strings.NewReader(script), &out)
	if err != nil {
		t.Fatalf("NewHelper: %v", err)
	}
	runErr := h.Run(context.Background())
	return out.String(), runErr
}

// TestListFailsLoudlyWhenRegistryUnreachable is the regression test for the
// force-overwrite bug: a registry that answers 500 (or 401) must not be
// reported to git as an empty remote, because git would then treat every push
// as a create and skip the fast-forward check.
func TestListFailsLoudlyWhenRegistryUnreachable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"server error", http.StatusInternalServerError},
		{"unauthorized", http.StatusUnauthorized},
		{"forbidden", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := registrytest.New()
			ts := reg.Serve(t)
			reg.Intercept(func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path == "/v2/" {
					return false
				}
				w.WriteHeader(tc.status)
				return true
			})

			newCommitRepo(t)
			registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
			t.Setenv("OCI_INSECURE", "1")

			out, err := runHelper(t, registry, "list\n\n")
			if err == nil {
				t.Fatalf("expected list to fail against a %d registry, got output %q", tc.status, out)
			}
			if strings.Contains(err.Error(), "no such host") {
				t.Fatalf("unexpected transport failure: %v", err)
			}
		})
	}
}

// TestListReportsEmptyForFreshRepository is the counterpart: a registry that
// genuinely has nothing must still list cleanly, otherwise the very first push
// to a new repository would fail.
func TestListReportsEmptyForFreshRepository(t *testing.T) {
	// A repository that has never been pushed to: everything 404s.
	ts := registrytest.New().Serve(t)

	newCommitRepo(t)
	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	out, err := runHelper(t, registry, "list\n\n")
	if err != nil {
		t.Fatalf("list against an empty registry should succeed, got: %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("expected an empty ref list, got %q", out)
	}
}

// TestDryRunSingleSpecPackfileFailureDoesNotPanic covers the nil-mutex
// dereference that used to crash `git push --dry-run` for a single refspec.
//
// Reaching that code needs a ref that resolves but whose objects cannot be
// packed, so the ref is left in place while the object store is emptied. Before
// the fix, the dry-run failure branch called mu.Unlock() unconditionally even
// though single-spec pushes pass a nil mutex.
func TestDryRunSingleSpecPackfileFailureDoesNotPanic(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	dir := newCommitRepo(t)

	// Drop the object database but keep refs: the ref still resolves, packfile
	// generation cannot.
	objectsDir := filepath.Join(dir, ".git", "objects")
	entries, err := os.ReadDir(objectsDir)
	if err != nil {
		t.Fatalf("read objects dir: %v", err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(objectsDir, e.Name())); err != nil {
			t.Fatalf("remove %s: %v", e.Name(), err)
		}
	}

	branch := currentBranch(t, dir)
	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	script := "option dry-run true\npush " + branch + ":" + branch + "\n\n"
	out, err := runHelper(t, registry, script)
	if err != nil {
		t.Fatalf("Run returned an error instead of reporting a per-ref failure: %v", err)
	}
	if !strings.Contains(out, "error "+branch) {
		t.Fatalf("expected a per-ref error line for %s, got %q", branch, out)
	}
}

// currentBranch returns the fully-qualified HEAD branch name of a repository.
func currentBranch(t *testing.T, dir string) string {
	t.Helper()

	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	return head.Name().String()
}
