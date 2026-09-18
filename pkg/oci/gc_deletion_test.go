package oci_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// TestDeleteTagReportsUnsupportedDeletionDistinctly pins the signal gc needs.
//
// DeleteTag returned an indistinguishable error for "this registry will never
// delete anything" and "this delete failed", so gc aborted on the first prune
// against GHCR, ECR or Docker Hub - throwing away the consolidation it had
// already completed, which is the half that actually makes clones cheaper.
func TestDeleteTagReportsUnsupportedDeletionDistinctly(t *testing.T) {
	reg := newMockRegistry()
	ts := reg.Server()
	defer ts.Close()

	client := newTestClient(t, ts.URL)
	ctx := context.Background()

	const commitSHA = "7777777777777777777777777777777777777777"
	if err := pushCommitImage(ctx, client, commitSHA, "refs/heads/main", "main", []byte("PACK")); err != nil {
		t.Fatalf("PushCommitImage: %v", err)
	}

	reg.mu.Lock()
	reg.refuseDelete = true
	reg.mu.Unlock()

	err := client.DeleteTag(ctx, commitSHA)
	if err == nil {
		t.Fatal("expected an error when the registry refuses deletion")
	}
	if !errors.Is(err, oci.ErrDeletionUnsupported) {
		t.Errorf("error should be ErrDeletionUnsupported so callers can skip rather than abort, got: %v", err)
	}

	// A transient failure must stay distinguishable, or gc would silently skip
	// tags it could actually have removed.
	reg.mu.Lock()
	reg.refuseDelete = false
	reg.failDeleteWith = http.StatusInternalServerError
	reg.mu.Unlock()

	err = client.DeleteTag(ctx, commitSHA)
	if err == nil {
		t.Fatal("expected an error on a 500")
	}
	if errors.Is(err, oci.ErrDeletionUnsupported) {
		t.Errorf("a 500 must not be reported as unsupported deletion, got: %v", err)
	}
}

// TestTagClassString covers the names of the four kinds of tag a repository
// holds.
//
// Nothing prints one today — `gc` says "commit manifest" and "released lock",
// which are more specific than the class name. It is kept rather than deleted
// because `TagClass` is exported and any `%v` on one reaches this, which is
// precisely when someone is debugging and least wants to read an integer.
func TestTagClassString(t *testing.T) {
	for _, tc := range []struct {
		class oci.TagClass
		want  string
	}{
		{oci.TagClassMetadata, "metadata"},
		{oci.TagClassLock, "lock"},
		{oci.TagClassCommit, "commit"},
		{oci.TagClassRef, "ref"},
		// A value from outside the set still has to render as something: an
		// unhandled class appearing as an empty string in a message is how a
		// new one gets added and nobody notices.
		{oci.TagClass(99), "unknown"},
	} {
		if got := tc.class.String(); got != tc.want {
			t.Errorf("TagClass(%d).String() = %q, want %q", tc.class, got, tc.want)
		}
	}
}
