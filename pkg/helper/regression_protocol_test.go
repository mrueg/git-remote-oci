package helper_test

import (
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
)

// TestAtomicPushCanDeleteRef covers the missing delete branch in the atomic
// path: `git push --atomic origin :branch` used to fall through to
// ResolveRef("") and abort the entire batch.
func TestAtomicPushCanDeleteRef(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	srcDir := newCommitRepo(t)
	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	branch := currentBranch(t, srcDir)
	if out, err := runHelper(t, registry, "push "+branch+":refs/heads/doomed\n\n"); err != nil {
		t.Fatalf("seeding push failed: %v (%s)", err, out)
	}

	out, err := runHelper(t, registry, "option atomic true\nlist\npush :refs/heads/doomed\n\n")
	if err != nil {
		t.Fatalf("atomic delete failed: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "ok refs/heads/doomed") {
		t.Fatalf("expected 'ok refs/heads/doomed', got:\n%s", out)
	}

	listOut, err := runHelper(t, registry, "list\n\n")
	if err != nil {
		t.Fatalf("list after delete failed: %v", err)
	}
	if strings.Contains(listOut, "refs/heads/doomed") {
		t.Errorf("deleted ref is still listed:\n%s", listOut)
	}
}

// TestDeletedRefIsNotResurrected checks that a ref deleted in one session stays
// deleted after a subsequent unrelated push. The _refs index is rebuilt by
// merging in a tag enumeration, which used to re-add the deleted ref.
func TestDeletedRefIsNotResurrected(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	srcDir := newCommitRepo(t)
	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	branch := currentBranch(t, srcDir)
	seed := "push " + branch + ":refs/heads/keep\npush " + branch + ":refs/heads/doomed\n\n"
	if out, err := runHelper(t, registry, seed); err != nil {
		t.Fatalf("seeding push failed: %v (%s)", err, out)
	}

	if out, err := runHelper(t, registry, "list\npush :refs/heads/doomed\n\n"); err != nil {
		t.Fatalf("delete failed: %v (%s)", err, out)
	}

	// An unrelated push in a fresh session rebuilds the index.
	if out, err := runHelper(t, registry, "push "+branch+":refs/heads/another\n\n"); err != nil {
		t.Fatalf("follow-up push failed: %v (%s)", err, out)
	}

	listOut, err := runHelper(t, registry, "list\n\n")
	if err != nil {
		t.Fatalf("final list failed: %v", err)
	}
	if strings.Contains(listOut, "refs/heads/doomed") {
		t.Errorf("deleted ref was resurrected by a later push:\n%s", listOut)
	}
	if !strings.Contains(listOut, "refs/heads/keep") {
		t.Errorf("unrelated ref went missing:\n%s", listOut)
	}
}

// TestQuitFlushesPendingBatch verifies that a batch terminated by "quit"
// instead of a blank line is still executed rather than silently dropped.
func TestQuitFlushesPendingBatch(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	srcDir := newCommitRepo(t)
	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	branch := currentBranch(t, srcDir)
	out, err := runHelper(t, registry, "push "+branch+":refs/heads/viaquit\nquit\n")
	if err != nil {
		t.Fatalf("push terminated by quit failed: %v", err)
	}
	if !strings.Contains(out, "ok refs/heads/viaquit") {
		t.Fatalf("batch was dropped on quit, got:\n%s", out)
	}
}

// TestListEmitsNoPeelLines pins the list grammar. "^<oid> <ref>^{}" is
// dumb-HTTP info/refs syntax, not remote-helper syntax; emitting it registered
// a bogus ref named "<ref>^{}".
func TestListEmitsNoPeelLines(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	srcDir := newCommitRepo(t)
	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	// An annotated tag is what used to trigger the peel line.
	repo, err := gogit.PlainOpen(srcDir)
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if _, err := repo.CreateTag("v1.0.0", head.Hash(), &gogit.CreateTagOptions{
		Tagger:  &object.Signature{Name: "T", Email: "t@example.com", When: time.Now()},
		Message: "release",
	}); err != nil {
		t.Fatalf("CreateTag: %v", err)
	}

	if out, err := runHelper(t, registry, "push refs/tags/v1.0.0:refs/tags/v1.0.0\n\n"); err != nil {
		t.Fatalf("tag push failed: %v (%s)", err, out)
	}

	out, err := runHelper(t, registry, "list\n\n")
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "^") {
			t.Errorf("list emitted a peel line, which is not valid remote-helper grammar: %q", line)
		}
		if strings.Contains(line, "^{}") {
			t.Errorf("list emitted a ref name containing ^{}: %q", line)
		}
	}
}
