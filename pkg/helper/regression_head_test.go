package helper_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// headAdvertised returns the "@<ref> HEAD" target from a list response.
func headAdvertised(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "@") && strings.HasSuffix(line, " HEAD") {
			return strings.TrimSuffix(strings.TrimPrefix(line, "@"), " HEAD")
		}
	}
	return ""
}

// TestHeadIsRecordedNotGuessed pins that the remote's default branch survives.
//
// HEAD used to be guessed on every listing - main, else master, else the
// alphabetically first branch - so a repository whose default branch was
// neither cloned onto the wrong branch. Here the only branch ever pushed is
// "develop", and an alphabetically earlier "aaa" is added afterwards precisely
// to break the old heuristic.
func TestHeadIsRecordedNotGuessed(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	src := newWorkRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(src, ".git"))

	git(t, src, "branch", "-m", "develop")
	if out, err := runHelper(t, registry, "list for-push\npush refs/heads/develop:refs/heads/develop\n\n"); err != nil {
		t.Fatalf("push of develop failed: %v (output %q)", err, out)
	}

	out, err := runHelper(t, registry, "list\n\n")
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if got := headAdvertised(out); got != "refs/heads/develop" {
		t.Errorf("HEAD = %q, want refs/heads/develop\n%s", got, out)
	}

	// A branch that sorts first must not steal HEAD.
	git(t, src, "checkout", "-q", "-b", "aaa")
	commitFile(t, src, "aaa.txt", "aaa\n", "on aaa")
	if out, err := runHelper(t, registry, "list for-push\npush refs/heads/aaa:refs/heads/aaa\n\n"); err != nil {
		t.Fatalf("push of aaa failed: %v (output %q)", err, out)
	}

	out, err = runHelper(t, registry, "list\n\n")
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if got := headAdvertised(out); got != "refs/heads/develop" {
		t.Errorf("HEAD moved to %q after pushing an unrelated branch; it should still be refs/heads/develop\n%s", got, out)
	}
}

// TestHeadFallsBackWhenNoneRecorded checks repositories written before the
// annotation existed still advertise something sensible.
func TestHeadFallsBackWhenNoneRecorded(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	src := newWorkRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(src, ".git"))
	if out, err := runHelper(t, registry, "list for-push\npush refs/heads/main:refs/heads/main\n\n"); err != nil {
		t.Fatalf("push failed: %v (output %q)", err, out)
	}

	stripHeadAnnotation(t, reg)

	out, err := runHelper(t, registry, "list\n\n")
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if got := headAdvertised(out); got != "refs/heads/main" {
		t.Errorf("HEAD = %q, want the refs/heads/main fallback\n%s", got, out)
	}
}

// TestHeadIsDroppedWhenItsRefIsDeleted pins that HEAD never dangles.
func TestHeadIsDroppedWhenItsRefIsDeleted(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	src := newWorkRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(src, ".git"))
	git(t, src, "branch", "-m", "develop")
	if out, err := runHelper(t, registry, "list for-push\npush refs/heads/develop:refs/heads/develop\n\n"); err != nil {
		t.Fatalf("push failed: %v (output %q)", err, out)
	}
	git(t, src, "checkout", "-q", "-b", "keeper")
	commitFile(t, src, "k.txt", "k\n", "keeper")
	if out, err := runHelper(t, registry, "list for-push\npush refs/heads/keeper:refs/heads/keeper\n\n"); err != nil {
		t.Fatalf("push failed: %v (output %q)", err, out)
	}
	if out, err := runHelper(t, registry, "list for-push\npush :refs/heads/develop\n\n"); err != nil {
		t.Fatalf("delete failed: %v (output %q)", err, out)
	}

	out, err := runHelper(t, registry, "list\n\n")
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if got := headAdvertised(out); got == "refs/heads/develop" {
		t.Errorf("HEAD still points at the deleted refs/heads/develop\n%s", out)
	}
}

// stripHeadAnnotation removes the recorded HEAD from every stored manifest,
// producing what a repository written before HEAD was recorded looks like.
func stripHeadAnnotation(t *testing.T, reg *registrytest.Registry) {
	t.Helper()

	for _, tag := range reg.Tags() {
		raw := reg.RawManifest(t, tag)
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		annotations, ok := m["annotations"].(map[string]any)
		if !ok {
			continue
		}
		if _, present := annotations[oci.AnnotationGitHead]; !present {
			continue
		}
		delete(annotations, oci.AnnotationGitHead)
		updated, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("re-marshal %q: %v", tag, err)
		}
		reg.PutManifest(tag, updated)
	}
}
