package helper_test

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// refTagRevision reads the commit a ref tag currently resolves to.
func refTagRevision(t *testing.T, reg *registrytest.Registry, refName string) (string, bool) {
	t.Helper()

	raw, ok := reg.ManifestBytes(oci.EncodeRefTag(refName))
	if !ok {
		return "", false
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", refName, err)
	}
	return m.Annotations[ocispec.AnnotationRevision], true
}

// TestAtomicPushRollsBackRefTagsOnFailure pins that a failed --atomic batch
// does not leave refs half moved.
//
// Phase 3 pushes each ref's manifest in turn and used to stop at the first
// failure, leaving the refs before it pointing at their new commits. The _refs
// index was never updated, so `list` still reported the old values while the
// ref tags resolved to the new ones - and the next push then ran its
// fast-forward check against a ref that had only half moved.
func TestAtomicPushRollsBackRefTagsOnFailure(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	src := newWorkRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(src, ".git"))

	// Two branches, both published.
	root := git(t, src, "rev-parse", "HEAD")
	mainOld := commitFile(t, src, "on-main.txt", "main v1\n", "main one")
	git(t, src, "checkout", "-q", "-b", "second", root)
	secondOld := commitFile(t, src, "on-second.txt", "second v1\n", "second one")
	git(t, src, "checkout", "-q", "main")

	if out, err := runHelper(t, registry,
		"list for-push\npush refs/heads/main:refs/heads/main\npush refs/heads/second:refs/heads/second\n\n"); err != nil {
		t.Fatalf("seeding push failed: %v (output %q)", err, out)
	}

	// Advance both branches.
	mainNew := commitFile(t, src, "on-main.txt", "main v2\n", "main two")
	git(t, src, "checkout", "-q", "second")
	secondNew := commitFile(t, src, "on-second.txt", "second v2\n", "second two")
	git(t, src, "checkout", "-q", "main")
	if mainNew == mainOld || secondNew == secondOld {
		t.Fatal("branches did not advance")
	}

	// Fail the *second* ref's manifest write, so the batch stops after the
	// first has already landed. Only that tag is failed: failing every write
	// would also break the rollback, which is the thing under test.
	reg.Intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/manifests/"+oci.EncodeRefTag("refs/heads/second")) {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		return false
	})

	out, err := runHelper(t, registry,
		"option atomic true\nlist for-push\npush refs/heads/main:refs/heads/main\npush refs/heads/second:refs/heads/second\n\n")
	if err != nil {
		t.Fatalf("push returned a batch-level error: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "error refs/heads/") {
		t.Fatalf("expected the atomic batch to fail, got:\n%s", out)
	}

	reg.Intercept(nil)

	// Neither ref tag may be left advanced.
	for refName, want := range map[string]string{
		"refs/heads/main":   mainOld,
		"refs/heads/second": secondOld,
	} {
		got, present := refTagRevision(t, reg, refName)
		if !present {
			continue // never existed, nothing to roll back
		}
		if got != want {
			t.Errorf("%s resolves to %s after a failed atomic push; it should still be %s",
				refName, shortID(got), shortID(want))
		}
	}

	// And the index must agree with the tags.
	listOut, err := runHelper(t, registry, "list\n\n")
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if strings.Contains(listOut, mainNew) || strings.Contains(listOut, secondNew) {
		t.Errorf("list reports a commit from the failed atomic batch:\n%s", listOut)
	}
}

func shortID(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
