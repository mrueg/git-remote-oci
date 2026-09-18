package oci_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
)

func TestManifestCaching(t *testing.T) {
	var fetchCount atomic.Int32

	testManifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Annotations: map[string]string{
			ocispec.AnnotationRevision: "4b825dc642cb6eb9a060e54bf8d69288fbee4904",
		},
	}
	manifestData, _ := json.Marshal(testManifest)

	reg := registrytest.New()
	ts := reg.Serve(t)
	reg.PutManifest("v1.0.0", manifestData)
	reg.Observe(func(_, path string) {
		if strings.Contains(path, "/manifests/") {
			fetchCount.Add(1)
		}
	})

	client := registrytest.Client(t, ts)
	ctx := context.Background()

	// 1. Initial fetch (cache miss -> network fetch)
	m1, err := client.FetchManifest(ctx, "v1.0.0")
	if err != nil {
		t.Fatalf("First FetchManifest failed: %v", err)
	}
	if fetchCount.Load() != 1 {
		t.Errorf("Expected 1 network fetch, got %d", fetchCount.Load())
	}
	if m1.Annotations[ocispec.AnnotationRevision] != "4b825dc642cb6eb9a060e54bf8d69288fbee4904" {
		t.Errorf("Unexpected manifest revision: %v", m1.Annotations)
	}

	// 2. Second fetch of same tag (cache hit -> no network request)
	m2, err := client.FetchManifest(ctx, "v1.0.0")
	if err != nil {
		t.Fatalf("Second FetchManifest failed: %v", err)
	}
	if fetchCount.Load() != 1 {
		t.Errorf("Expected cache hit (1 total network fetch), got %d", fetchCount.Load())
	}
	if m2.Annotations[ocispec.AnnotationRevision] != "4b825dc642cb6eb9a060e54bf8d69288fbee4904" {
		t.Errorf("Unexpected manifest revision: %v", m2.Annotations)
	}

	// 3. Invalidate cache for tag
	client.InvalidateManifestCache("v1.0.0")

	// 4. Fetch after invalidation (cache miss -> network fetch #2)
	_, err = client.FetchManifest(ctx, "v1.0.0")
	if err != nil {
		t.Fatalf("FetchManifest after invalidation failed: %v", err)
	}
	if fetchCount.Load() != 2 {
		t.Errorf("Expected 2 total network fetches after invalidation, got %d", fetchCount.Load())
	}

	// 5. Clear entire cache
	client.ClearManifestCache()

	// 6. Fetch after clear (cache miss -> network fetch #3)
	_, err = client.FetchManifest(ctx, "v1.0.0")
	if err != nil {
		t.Fatalf("FetchManifest after ClearManifestCache failed: %v", err)
	}
	if fetchCount.Load() != 3 {
		t.Errorf("Expected 3 total network fetches after clear, got %d", fetchCount.Load())
	}
}
