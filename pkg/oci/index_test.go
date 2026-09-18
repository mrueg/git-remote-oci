package oci_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/mrueg/git-remote-oci/pkg/oci"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestOCIImageIndexPushAndFetch(t *testing.T) {
	mock := newMockRegistry()
	server := mock.Server()
	defer server.Close()

	serverURL := strings.TrimPrefix(server.URL, "http://")
	client, err := oci.NewClient(serverURL+"/test-org/index-repo", true)
	if err != nil {
		t.Fatalf("Failed to create OCI client: %v", err)
	}

	ctx := context.Background()

	refs := map[string]oci.RefEntry{
		"refs/heads/main": {
			SHA:    "a1b2c3d4e5f60718293a4b5c6d7e8f901a2b3c4d",
			TagSig: "gpg-signature-bytes-main",
		},
		"refs/tags/v1.0.0": {
			SHA:    "11223344556677889900aabbccddeeff11223344",
			TagSig: "ssh-signature-bytes-tag",
		},
	}

	// Publish real manifests for these refs first. The index references them by
	// digest, and a ref with no manifest is skipped rather than emitted with an
	// empty digest, which no client could follow.
	for refName, entry := range refs {
		if err := pushCommitImage(ctx, client, entry.SHA, refName, refName, []byte("PACK-fake-"+entry.SHA)); err != nil {
			t.Fatalf("failed to seed manifest for %s: %v", refName, err)
		}
	}

	// 1. Test PushOCIImageIndex
	if err := client.PushOCIImageIndex(ctx, "_index", refs, ""); err != nil {
		t.Fatalf("PushOCIImageIndex failed: %v", err)
	}

	// 2. Test FetchOCIImageIndex
	indexObj, err := client.FetchOCIImageIndex(ctx, "_index")
	if err != nil {
		t.Fatalf("FetchOCIImageIndex failed: %v", err)
	}

	if indexObj.MediaType != ocispec.MediaTypeImageIndex {
		t.Errorf("Expected mediaType %s, got %s", ocispec.MediaTypeImageIndex, indexObj.MediaType)
	}

	if len(indexObj.Manifests) != 2 {
		t.Fatalf("Expected 2 manifests in OCI Image Index, got %d", len(indexObj.Manifests))
	}

	// Verify annotations in returned OCI Image Index manifest descriptors
	manifestMap := make(map[string]ocispec.Descriptor)
	for _, m := range indexObj.Manifests {
		ref := m.Annotations[oci.AnnotationGitRef]
		manifestMap[ref] = m
	}

	if mainDesc, ok := manifestMap["refs/heads/main"]; !ok {
		t.Errorf("Missing refs/heads/main in OCI Image Index descriptors")
	} else {
		if mainDesc.Annotations[ocispec.AnnotationRevision] != "a1b2c3d4e5f60718293a4b5c6d7e8f901a2b3c4d" {
			t.Errorf("Incorrect revision annotation: got %s", mainDesc.Annotations[ocispec.AnnotationRevision])
		}
		// The tag signature belongs under the key that only ever means that.
		// It used to be written to org.opencontainers.image.signature as well,
		// which is also where the pushcert option value went — so a reader had
		// no way to tell a real signature from the string "true".
		if mainDesc.Annotations[oci.AnnotationGitTagSig] != "gpg-signature-bytes-main" {
			t.Errorf("Incorrect tag signature annotation: got %s", mainDesc.Annotations[oci.AnnotationGitTagSig])
		}
		if _, ok := mainDesc.Annotations["org.opencontainers.image.signature"]; ok {
			t.Error("org.opencontainers.image.signature is still written; anything trusting that key would read a git tag signature, or the literal \"true\", as an OCI image signature")
		}
	}

	// 3. Test FetchOCIImageIndexRefs
	fetchedRefs, err := client.FetchOCIImageIndexRefs(ctx, "_index")
	if err != nil {
		t.Fatalf("FetchOCIImageIndexRefs failed: %v", err)
	}

	expectedRefs := map[string]oci.RefEntry{
		"refs/heads/main": {
			SHA:    "a1b2c3d4e5f60718293a4b5c6d7e8f901a2b3c4d",
			TagSig: "gpg-signature-bytes-main",
		},
		"refs/tags/v1.0.0": {
			SHA:    "11223344556677889900aabbccddeeff11223344",
			TagSig: "ssh-signature-bytes-tag",
		},
	}

	if !reflect.DeepEqual(fetchedRefs, expectedRefs) {
		t.Errorf("FetchOCIImageIndexRefs mismatch:\nExpected: %+v\nGot:      %+v", expectedRefs, fetchedRefs)
	}
}
