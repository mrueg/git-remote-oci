package gc_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/gc"
	"github.com/mrueg/git-remote-oci/pkg/oci"

	opencontainers "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// phantomBase is a well-formed commit id no registry in these tests serves.
const phantomBase = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

// TestRunWithoutALocalCloneSurvivesAPhantomChainEntry.
//
// Hydration walks the published pack chain the way a fetch does, and used to
// fail the same way on a chain entry naming a manifest the registry no longer
// serves -- so the one tool that could have repaired a repository left that
// way could not run against it. The chain is advisory (§6.1): an entry no
// manifest's own pack-bases backs is skipped, not fatal.
func TestRunWithoutALocalCloneSurvivesAPhantomChainEntry(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	_, tip := registrytest.SeedRepository(t, client, 3)

	addPhantomChainEdges(t, reg)

	reader := registrytest.Client(t, ts)
	res, err := gc.Run(context.Background(), reader, nil, gc.Options{Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("gc failed because the published pack chain names a manifest the registry does not serve: %v", err)
	}
	if res.RefsConsolidated != 1 {
		t.Errorf("consolidated %d refs, want 1", res.RefsConsolidated)
	}

	verifier := registrytest.Client(t, ts)
	manifest, err := verifier.FetchManifest(context.Background(), oci.EncodeRefTag("refs/heads/main"))
	if err != nil {
		t.Fatalf("the ref manifest did not survive gc: %v", err)
	}
	index, ok := verifier.FetchPackIndex(context.Background(), manifest)
	if !ok {
		t.Fatal("no pack index on the consolidated manifest")
	}
	if !oci.PackIndexContains(index, []string{tip}) {
		t.Errorf("the repacked history does not contain the tip %s", tip)
	}
}

// TestRunWithoutALocalCloneStillRefusesAMissingDeclaredBase: skipping a chain
// entry must not extend to a base a manifest's annotation declares. With the
// first push's manifest gone, the second push's declared base cannot be
// fetched, and hydration has to say so rather than repack a truncated history.
func TestRunWithoutALocalCloneStillRefusesAMissingDeclaredBase(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	registrytest.SeedRepository(t, client, 2)

	var commits []string
	for _, tag := range reg.Tags() {
		if oci.IsCommitID(tag) {
			commits = append(commits, tag)
		}
	}
	if len(commits) != 2 {
		t.Fatalf("fixture error: expected two commit tags, got %v", commits)
	}
	reg.DropManifest(commits[0])

	reader := registrytest.Client(t, ts)
	_, err := gc.Run(context.Background(), reader, nil, gc.Options{Logf: func(string, ...any) {}})
	if err == nil {
		t.Fatal("gc succeeded although the tip's declared pack base is gone; a declared base that cannot be fetched is an error, whatever the chain says")
	}
	if !strings.Contains(err.Error(), commits[0][:7]) {
		t.Errorf("the failure should name the missing base %s, got: %v", commits[0], err)
	}
}

// addPhantomChainEdges rewrites the published pack chain so every entry also
// names a manifest that does not exist.
func addPhantomChainEdges(t *testing.T, reg *registrytest.Registry) {
	t.Helper()
	raw, ok := reg.ManifestBytes(oci.TagRefIndex)
	if !ok {
		t.Fatal("no _refs manifest was ever pushed")
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("_refs manifest: %v", err)
	}
	for i, layer := range manifest.Layers {
		if layer.MediaType != oci.MediaTypePackChain {
			continue
		}
		var chain map[string][]string
		if err := json.Unmarshal(reg.BlobBytes(layer.Digest.String()), &chain); err != nil {
			t.Fatalf("the published chain is not readable: %v", err)
		}
		if len(chain) == 0 {
			t.Fatal("fixture error: the published chain is empty")
		}
		for sha, bases := range chain {
			chain[sha] = append(bases, phantomBase)
		}
		replacement, err := json.Marshal(chain)
		if err != nil {
			t.Fatal(err)
		}
		manifest.Layers[i] = ocispec.Descriptor{
			MediaType: oci.MediaTypePackChain,
			Digest:    opencontainers.Digest(reg.PutBlob(replacement)),
			Size:      int64(len(replacement)),
		}
		raw, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		reg.PutManifest(oci.TagRefIndex, raw)
		return
	}
	t.Fatal("the _refs manifest carries no pack chain layer")
}
