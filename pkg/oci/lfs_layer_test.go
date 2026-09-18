package oci_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/lfs"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

func lfsTestClient(t *testing.T) (*oci.Client, *registrytest.Registry) {
	t.Helper()

	reg := registrytest.New()
	return registrytest.Client(t, reg.Serve(t)), reg
}

// TestPushLFSLayerPublishesUnderTheObjectId checks the happy path: the layer is
// stored under a digest derived from the object id, not from a re-hash of
// whatever was read.
func TestPushLFSLayerPublishesUnderTheObjectId(t *testing.T) {
	client, _ := lfsTestClient(t)

	payload := []byte("the quick brown fox")
	sum := sha256.Sum256(payload)
	oid := hex.EncodeToString(sum[:])

	desc, err := client.PushLFSLayer(context.Background(), oid, bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("PushLFSLayer: %v", err)
	}
	if got, want := desc.Digest.String(), "sha256:"+oid; got != want {
		t.Errorf("descriptor digest = %s, want %s", got, want)
	}
	if desc.Size != int64(len(payload)) {
		t.Errorf("descriptor size = %d, want %d", desc.Size, len(payload))
	}
	if desc.Annotations[lfs.AnnotationLFSOID] != oid {
		t.Errorf("OID annotation = %q, want %q", desc.Annotations[lfs.AnnotationLFSOID], oid)
	}
	// The digest and the annotation must agree, which is exactly what the old
	// implementation could not guarantee.
	if desc.Digest.Encoded() != desc.Annotations[lfs.AnnotationLFSOID] {
		t.Errorf("digest %s disagrees with the OID annotation %s",
			desc.Digest.Encoded(), desc.Annotations[lfs.AnnotationLFSOID])
	}
}

// TestPushLFSLayerRejectsCorruptObject is the regression test.
//
// The old implementation hashed whatever it read and pushed under that digest,
// so an object whose bytes did not match the id it was filed under got
// published anyway, with a digest that disagreed with its own OID annotation.
func TestPushLFSLayerRejectsCorruptObject(t *testing.T) {
	client, mock := lfsTestClient(t)

	announced := sha256.Sum256([]byte("what the pointer claims"))
	oid := hex.EncodeToString(announced[:])
	corrupt := []byte("what is actually on disk")

	_, err := client.PushLFSLayer(context.Background(), oid, bytes.NewReader(corrupt), int64(len(corrupt)))
	if err == nil {
		t.Fatal("PushLFSLayer accepted an object that does not match its own id")
	}

	// Nothing may have been published under the announced digest.
	if mock.HasBlob("sha256:" + oid) {
		t.Errorf("a mismatched blob was published under sha256:%s", oid)
	}
}

// TestPushLFSLayerRejectsSizeMismatch: the pointer's size is part of the
// descriptor, so content of a different length must not be accepted either.
func TestPushLFSLayerRejectsSizeMismatch(t *testing.T) {
	client, _ := lfsTestClient(t)

	payload := []byte("exactly this")
	sum := sha256.Sum256(payload)
	oid := hex.EncodeToString(sum[:])

	// Correct content, wrong advertised size.
	if _, err := client.PushLFSLayer(context.Background(), oid, bytes.NewReader(payload), int64(len(payload))+10); err == nil {
		t.Error("PushLFSLayer accepted content shorter than the advertised size")
	}
}

// TestPushLFSLayerRejectsInvalidOID: the id is used to build the digest, so it
// has to be well formed before it gets there.
func TestPushLFSLayerRejectsInvalidOID(t *testing.T) {
	client, _ := lfsTestClient(t)

	for _, oid := range []string{"", "abcd", "../../escape", strings.Repeat("z", 64)} {
		if _, err := client.PushLFSLayer(context.Background(), oid, bytes.NewReader([]byte("x")), 1); err == nil {
			t.Errorf("PushLFSLayer accepted the invalid object id %q", oid)
		}
	}
}
