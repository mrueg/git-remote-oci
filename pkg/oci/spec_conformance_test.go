package oci_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	opencontainers "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// failDeletesWith makes the registry answer every manifest DELETE with status
// from now on: 405 is how a hosted registry says deletion is disabled, and
// anything else stands in for a transient failure rather than a policy.
func failDeletesWith(reg *registrytest.Registry, status int) {
	reg.Intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodDelete {
			return false
		}
		if status == http.StatusMethodNotAllowed {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"errors":[{"code":"UNSUPPORTED","message":"manifest delete is disabled"}]}`))
			return true
		}
		w.WriteHeader(status)
		return true
	})
}

// assertManifestHasLayersArray fails if the stored manifest serialises "layers"
// as anything other than a JSON array.
//
// ocispec.Manifest tags Layers as `layers` with no omitempty, so a manifest
// built without one marshals to `"layers": null`. The image-spec requires an
// array, and registries that validate manifests reject null - which broke ref
// locking and LFS locking on them while ordinary push and fetch kept working.
func assertManifestHasLayersArray(t *testing.T, raw []byte, tag string) {
	t.Helper()

	var generic map[string]json.RawMessage
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("manifest %q is not valid JSON: %v", tag, err)
	}
	layers, present := generic["layers"]
	if !present {
		t.Errorf("manifest %q has no \"layers\" field", tag)
		return
	}
	if string(layers) == "null" {
		t.Errorf("manifest %q serialises \"layers\": null, which registries that validate will reject", tag)
		return
	}
	var descs []ocispec.Descriptor
	if err := json.Unmarshal(layers, &descs); err != nil {
		t.Errorf("manifest %q has a non-array \"layers\": %s", tag, layers)
	}
}

func TestRefLockManifestIsSpecConformant(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	client := registrytest.Client(t, ts)
	ctx := context.Background()

	if _, err := client.AcquireRefLock(ctx, "refs/heads/main", time.Minute); err != nil {
		t.Fatalf("AcquireRefLock: %v", err)
	}

	tag := oci.LockTag("refs/heads/main")
	raw, ok := reg.ManifestBytes(tag)
	if !ok {
		t.Fatalf("no lock manifest stored under %q", tag)
	}
	assertManifestHasLayersArray(t, raw, tag)

	// The layer it names has to actually be in the registry, or a validating
	// registry rejects the manifest for a dangling reference instead.
	assertLayerBlobsPresent(t, reg, raw, tag)

	// Releasing writes a tombstone manifest, which must be conformant too.
	if err := client.ReleaseRefLock(ctx, "refs/heads/main"); err != nil {
		t.Fatalf("ReleaseRefLock: %v", err)
	}
	raw = reg.RawManifest(t, tag)
	assertManifestHasLayersArray(t, raw, tag+" (released)")
	assertLayerBlobsPresent(t, reg, raw, tag+" (released)")
}

func TestLFSLockManifestIsSpecConformant(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	client := registrytest.Client(t, ts)
	ctx := context.Background()

	if _, err := client.AcquireLFSLock(ctx, "big/asset.bin", "tester"); err != nil {
		t.Fatalf("AcquireLFSLock: %v", err)
	}

	var tag string
	var raw []byte
	for _, candidate := range reg.Tags() {
		if strings.Contains(candidate, "lfs_locks") {
			tag, raw = candidate, reg.RawManifest(t, candidate)
		}
	}
	if raw == nil {
		t.Fatal("no LFS lock manifest was stored")
	}
	assertManifestHasLayersArray(t, raw, tag)
	assertLayerBlobsPresent(t, reg, raw, tag)
}

// assertLayerBlobsPresent checks that every layer a manifest names exists.
func assertLayerBlobsPresent(t *testing.T, reg *registrytest.Registry, raw []byte, tag string) {
	t.Helper()

	var m ocispec.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal %q: %v", tag, err)
	}
	for _, layer := range m.Layers {
		if !reg.HasBlob(layer.Digest.String()) {
			t.Errorf("manifest %q names layer %s, which was never uploaded", tag, layer.Digest)
		}
	}
}

// TestConfigDiffIDMatchesUncompressedLayer pins that rootfs.diff_ids describes
// the uncompressed packfile.
//
// It was set to the layer digest, which is the *compressed* bytes. That is only
// correct for OCI_COMPRESSION=none, the default, so the config was silently
// inconsistent for anyone using gzip or zstd.
func TestConfigDiffIDMatchesUncompressedLayer(t *testing.T) {
	payload := bytes.Repeat([]byte("PACK-compressible-payload-"), 64)
	want := opencontainers.FromBytes(payload)

	for _, mode := range []string{"none", "gzip", "zstd"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("OCI_COMPRESSION", mode)

			reg := registrytest.New()
			ts := reg.Serve(t)

			client := registrytest.Client(t, ts)
			commitSHA := "1111111111111111111111111111111111111111"
			err := client.PushCommitStream(context.Background(), oci.CommitPush{
				CommitSHA: commitSHA,
				RefName:   "refs/heads/main",
				RefTag:    "main",
			}, bytes.NewReader(payload), int64(len(payload)))
			if err != nil {
				t.Fatalf("push: %v", err)
			}

			raw := reg.RawManifest(t, commitSHA)

			var m ocispec.Manifest
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("unmarshal commit manifest: %v", err)
			}

			if !reg.HasBlob(m.Config.Digest.String()) {
				t.Fatal("config blob was not uploaded")
			}
			configBytes := reg.BlobBytes(m.Config.Digest.String())

			var cfg ocispec.Image
			if err := json.Unmarshal(configBytes, &cfg); err != nil {
				t.Fatalf("unmarshal config: %v", err)
			}
			if len(cfg.RootFS.DiffIDs) != 1 {
				t.Fatalf("expected exactly one diff id, got %v", cfg.RootFS.DiffIDs)
			}
			if cfg.RootFS.DiffIDs[0] != want {
				t.Errorf("diff id = %s, want the uncompressed digest %s", cfg.RootFS.DiffIDs[0], want)
			}

			// Under compression the layer digest must differ from the diff id;
			// if they match, nothing was compressed and the test proves nothing.
			if mode != "none" && m.Layers[0].Digest == want {
				t.Skipf("%s did not change the bytes, so this case is vacuous", mode)
			}
		})
	}
}

// TestOCIImageIndexEntriesArePlatformQualified pins that index children can be
// selected.
//
// Without a platform, an entry cannot be matched by `docker pull` or any other
// client that selects on one.
func TestOCIImageIndexEntriesArePlatformQualified(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	client := registrytest.Client(t, ts)
	ctx := context.Background()

	commitSHA := "2222222222222222222222222222222222222222"
	if err := pushCommitImage(ctx, client, commitSHA, "refs/heads/main", "main", []byte("PACK-x")); err != nil {
		t.Fatalf("PushCommitImage: %v", err)
	}
	if err := client.PushOCIImageIndex(ctx, oci.TagOCIIndex, map[string]oci.RefEntry{
		"refs/heads/main": {SHA: commitSHA},
	}, ""); err != nil {
		t.Fatalf("PushOCIImageIndex: %v", err)
	}

	raw := reg.RawManifest(t, oci.TagOCIIndex)

	var idx ocispec.Index
	if err := json.Unmarshal(raw, &idx); err != nil {
		t.Fatalf("unmarshal index: %v", err)
	}
	if len(idx.Manifests) == 0 {
		t.Fatal("index has no entries")
	}
	for _, entry := range idx.Manifests {
		if entry.Platform == nil {
			t.Errorf("index entry %s has no platform, so no client can select it", entry.Digest)
			continue
		}
		if entry.Platform.OS != "unknown" || entry.Platform.Architecture != "unknown" {
			t.Errorf("index entry %s has platform %s/%s, want unknown/unknown",
				entry.Digest, entry.Platform.OS, entry.Platform.Architecture)
		}
	}
}
