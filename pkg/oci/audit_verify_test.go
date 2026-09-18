package oci_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	opencontainers "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// Content fetched from a registry is untrusted (AGENTS.md), and the registry
// client verifies only the digest *header*. These tests serve bodies that do
// not hash to their digest and require the reader to notice.

// corruptBlob flips the first byte of a stored blob, keeping its length so
// that only the digest check can catch it.
func corruptBlob(t *testing.T, reg *registrytest.Registry, digest string) {
	t.Helper()
	data := reg.BlobBytes(digest)
	if len(data) == 0 {
		t.Fatalf("test setup: no blob stored under %s", digest)
	}
	data[0] ^= 0xff
	reg.SetBlobBytes(digest, data)
}

func TestPackfileStreamFailsOnADigestMismatch(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	_, tip := registrytest.SeedRepository(t, client, 1)

	var manifest ocispec.Manifest
	if err := json.Unmarshal(reg.RawManifest(t, tip), &manifest); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	corruptBlob(t, reg, manifest.Layers[0].Digest.String())

	reader := registrytest.Client(t, ts)
	m, err := reader.FetchManifest(context.Background(), tip)
	if err != nil {
		t.Fatalf("FetchManifest: %v", err)
	}
	rc, err := reader.FetchPackfileStream(context.Background(), m)
	if err != nil {
		t.Fatalf("FetchPackfileStream: %v", err)
	}
	defer func() { _ = rc.Close() }()
	if _, err := io.ReadAll(rc); err == nil {
		t.Fatal("a packfile whose bytes do not match its digest was read to EOF without error")
	}
}

func TestRefIndexBlobIsVerified(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	registrytest.SeedRepository(t, client, 1)

	var manifest ocispec.Manifest
	if err := json.Unmarshal(reg.RawManifest(t, oci.TagRefIndex), &manifest); err != nil {
		t.Fatalf("unmarshal _refs: %v", err)
	}
	var indexDigest string
	for _, layer := range manifest.Layers {
		if layer.MediaType == oci.MediaTypeGitIndex {
			indexDigest = layer.Digest.String()
		}
	}
	corruptBlob(t, reg, indexDigest)

	reader := registrytest.Client(t, ts)
	if refs, err := reader.FetchRichRefIndex(context.Background()); err == nil {
		t.Fatalf("a _refs blob that does not match its digest was accepted: %v", refs)
	} else if oci.IsNotFound(err) {
		t.Errorf("a digest mismatch was reported as not found: %v", err)
	}
	// And ListRefs must not repair around it by enumerating tags: the index
	// is there, it is just not what the registry claims it is.
	if refs, err := reader.ListRefs(context.Background()); err == nil {
		t.Fatalf("ListRefs fell back to the tags past a corrupt index: %v", refs)
	}
}

func TestLFSLayerIsVerified(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	ctx := context.Background()

	content := []byte("large file contents")
	oid := opencontainers.FromBytes(content).Encoded()
	desc, err := client.PushLFSLayer(ctx, oid, strings.NewReader(string(content)), int64(len(content)))
	if err != nil {
		t.Fatalf("PushLFSLayer: %v", err)
	}
	corruptBlob(t, reg, desc.Digest.String())

	rc, err := registrytest.Client(t, ts).FetchLFSLayer(ctx, desc)
	if err != nil {
		t.Fatalf("FetchLFSLayer: %v", err)
	}
	defer func() { _ = rc.Close() }()
	if _, err := io.ReadAll(rc); err == nil {
		t.Fatal("an LFS object whose bytes do not match its digest was read to EOF without error")
	}
}

// TestManifestFetchedByDigestIsVerified: the registry client checks that the
// digest header matches the digest asked for, and nothing checks the body
// against either. A body that hashes differently is served under a header
// that satisfies the client.
func TestManifestFetchedByDigestIsVerified(t *testing.T) {
	genuine := ocispec.Manifest{
		MediaType:   ocispec.MediaTypeImageManifest,
		Config:      ocispec.DescriptorEmptyJSON,
		Layers:      []ocispec.Descriptor{ocispec.DescriptorEmptyJSON},
		Annotations: map[string]string{ocispec.AnnotationRevision: "4b825dc642cb6eb9a060e54bf8d69288fbee4904"},
	}
	genuineBytes, _ := json.Marshal(genuine)
	claimed := opencontainers.FromBytes(genuineBytes)

	served := genuine
	served.Annotations = map[string]string{ocispec.AnnotationRevision: "0000000000000000000000000000000000000000"}
	servedBytes, _ := json.Marshal(served)

	// The shared fake addresses a manifest by the digest of what it holds, so
	// serving the wrong bytes under the right digest has to be done by hand.
	reg := registrytest.New()
	ts := reg.Serve(t)
	reg.Intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/manifests/"+claimed.String()) {
			return false
		}
		w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
		w.Header().Set("Docker-Content-Digest", claimed.String())
		_, _ = w.Write(servedBytes)
		return true
	})

	client := registrytest.Client(t, ts)
	m, err := client.FetchManifest(context.Background(), claimed.String())
	if err == nil {
		t.Fatalf("a manifest body that does not hash to the digest it was fetched by was accepted: %+v", m.Annotations)
	}
	if strings.Contains(err.Error(), "not found") {
		t.Errorf("the mismatch was reported as not found: %v", err)
	}
}
