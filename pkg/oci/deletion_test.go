package oci_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// TestDeleteRefOnRegistryThatRefusesManifestDeletion covers the hosted-registry
// case.
//
// GHCR, ECR and Docker Hub restrict or disable manifest deletion to varying
// degrees. Deletion used to fail outright there, deliberately, because a
// surviving tag is rediscovered by the tag-enumeration fallback and the ref
// reappears on the next push. A tombstone keeps that guarantee without needing
// the registry to remove anything.
func TestDeleteRefOnRegistryThatRefusesManifestDeletion(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	client := registrytest.Client(t, ts)
	ctx := context.Background()

	const (
		refName   = "refs/heads/doomed"
		commitSHA = "3333333333333333333333333333333333333333"
	)
	if err := pushCommitImage(ctx, client, refName, refName, true, []byte("PACK-doomed")); err == nil {
		t.Fatal("expected an invalid commit SHA to be rejected")
	}
	if err := pushCommitImage(ctx, client, commitSHA, refName, true, []byte("PACK-doomed")); err != nil {
		t.Fatalf("PushCommitImage: %v", err)
	}

	// Sanity: the ref is discoverable both ways before deletion.
	if refs, err := client.EnumerateTagRefs(ctx); err != nil {
		t.Fatalf("EnumerateTagRefs: %v", err)
	} else if refs[refName] != commitSHA {
		t.Fatalf("setup failed: %s is not enumerable, got %v", refName, refs)
	}

	failDeletesWith(reg, http.StatusMethodNotAllowed)

	if err := client.DeleteRef(ctx, refName); err != nil {
		t.Fatalf("DeleteRef should fall back to a tombstone when the registry refuses deletion, got: %v", err)
	}

	// The index no longer lists it.
	refs, err := client.ListRefs(ctx)
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	if _, present := refs[refName]; present {
		t.Errorf("%s is still in the ref index after deletion", refName)
	}

	// And, crucially, the tag-enumeration fallback must not resurrect it. This
	// is the path that made a "successful" deletion undo itself.
	client.ClearManifestCache()
	enumerated, err := client.EnumerateTagRefs(ctx)
	if err != nil {
		t.Fatalf("EnumerateTagRefs after deletion: %v", err)
	}
	if sha, present := enumerated[refName]; present {
		t.Errorf("tag enumeration resurrected %s as %s after deletion", refName, sha)
	}

	// The tag itself survives, because the registry would not remove it, and
	// what it holds is a tombstone.
	tag := oci.EncodeRefTag(refName)
	raw, ok := reg.ManifestBytes(tag)
	if !ok {
		t.Fatalf("tag %q vanished even though the registry refuses deletion", tag)
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal tombstone: %v", err)
	}
	if m.Annotations[oci.AnnotationGitDeleted] != "true" {
		t.Errorf("tag %q was not replaced with a tombstone; annotations: %v", tag, m.Annotations)
	}
	assertManifestHasLayersArray(t, raw, tag)
	assertLayerBlobsPresent(t, reg, raw, tag)
}

// TestDeleteRefStillFailsOnUnexpectedError pins that the tombstone fallback is
// reserved for registries that genuinely refuse deletion.
//
// Falling back on a transient failure would leave a tombstone over a ref that
// could have been removed properly, and would report success for a deletion
// that half happened.
func TestDeleteRefStillFailsOnUnexpectedError(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	client := registrytest.Client(t, ts)
	ctx := context.Background()

	const (
		refName   = "refs/heads/doomed"
		commitSHA = "4444444444444444444444444444444444444444"
	)
	if err := pushCommitImage(ctx, client, commitSHA, refName, true, []byte("PACK-doomed")); err != nil {
		t.Fatalf("PushCommitImage: %v", err)
	}

	failDeletesWith(reg, http.StatusInternalServerError)

	err := client.DeleteRef(ctx, refName)
	if err == nil {
		t.Fatal("a 500 on delete must be reported, not turned into a tombstone")
	}
	if !strings.Contains(err.Error(), "failed to delete tag") {
		t.Errorf("error should say the delete failed, got: %v", err)
	}
}
