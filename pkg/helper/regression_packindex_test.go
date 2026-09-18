package helper_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/oci"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// A lazy fetch asks for a blob, and a blob is not something the pack-base graph
// can be walked from — there is no annotation saying which ref a blob belongs
// to, because a blob belongs to as many as reach it. So the search goes ref by
// ref, and before pack indexes existed the only way to ask a ref whether it had
// the object was to download its packfiles and index them.
//
// That is the wrong unit of work by orders of magnitude: a checkout of a branch
// in a repository with a dozen refs would download most of the repository to
// deliver one blob. The index makes the question answerable in a few kilobytes.
//
// The test below is about the packs that are *not* fetched, which is the whole
// point and the part no functional check would notice: a helper that downloads
// everything still produces a correct checkout.

// packfileLayerOf returns the digest of the packfile a ref's manifest carries.
func packfileLayerOf(t *testing.T, reg *registrytest.Registry, refName string) string {
	t.Helper()
	raw, ok := reg.ManifestBytes(oci.EncodeRefTag(refName))
	if !ok {
		t.Fatalf("%s was never pushed", refName)
	}

	var manifest ocispec.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("manifest for %s: %v", refName, err)
	}
	for _, layer := range manifest.Layers {
		if strings.HasPrefix(layer.MediaType, "application/vnd.git.repository.packfile") {
			return layer.Digest.String()
		}
	}
	t.Fatalf("%s has no packfile layer", refName)
	return ""
}

// hasPackIndexLayer reports whether a ref's manifest published an object index.
func hasPackIndexLayer(t *testing.T, reg *registrytest.Registry, refName string) bool {
	t.Helper()
	raw, _ := reg.ManifestBytes(oci.EncodeRefTag(refName))

	var manifest ocispec.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("manifest for %s: %v", refName, err)
	}
	_, ok := oci.PackIndexLayer(&manifest)
	return ok
}

// TestPackIndexSkipsRefsThatCannotHoldTheObject pins the saving.
//
// Four refs, each pushed separately so each has its own packfile: main, and
// alpha/beta/gamma branching off it. A lazy fetch searches HEAD first and then
// branches alphabetically, so a blob that exists only on gamma is the last
// thing found — main, alpha and beta are all tried before it.
//
// Without the index those three packfiles are downloaded and indexed to
// discover they were the wrong ones. With it, alpha's and beta's are never
// requested at all. (main's is, but as gamma's pack base rather than as a
// guess: gamma's packfile is thin and cannot be applied without it.)
func TestPackIndexSkipsRefsThatCannotHoldTheObject(t *testing.T) {
	url, reg := v2setupRegistry(t)
	src := v2seed(t, url, 2)

	for _, branch := range []string{"alpha", "beta", "gamma"} {
		git(t, src, "-C", src, "checkout", "-q", "-b", branch, "main")
		if err := os.WriteFile(filepath.Join(src, branch+".txt"), []byte(branch+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, src, "-C", src, "add", ".")
		git(t, src, "-C", src, "commit", "-q", "-m", "on "+branch)
		git(t, src, "-C", src, "push", "-q", url, branch)
	}

	// The skip is only possible if the push published the index in the first
	// place. Checking that here separates "the reader declined to skip" from
	// "the writer never gave it anything to read", which are the same symptom.
	for _, branch := range []string{"main", "alpha", "beta", "gamma"} {
		if !hasPackIndexLayer(t, reg, "refs/heads/"+branch) {
			t.Fatalf("push of %s published no pack index layer", branch)
		}
	}

	parent := t.TempDir()
	if out, err := v2run(t, parent, nil, "-c", "protocol.version=2", "-c", "ociremote.protocolV2=true",
		"clone", "--filter=blob:none", url, "dst"); err != nil {
		t.Fatalf("partial clone: %v\n%s", err, out)
	}
	dst := filepath.Join(parent, "dst")

	alphaPack := packfileLayerOf(t, reg, "refs/heads/alpha")
	betaPack := packfileLayerOf(t, reg, "refs/heads/beta")
	gammaPack := packfileLayerOf(t, reg, "refs/heads/gamma")

	// Measure only the lazy fetch. The clone legitimately touches everything.
	mark := len(reg.Requests())
	out, err := v2run(t, dst, nil, "-c", "protocol.version=2", "-c", "ociremote.protocolV2=true",
		"checkout", "-f", "gamma")
	if err != nil {
		t.Fatalf("lazy fetch on gamma: %v\n%s", err, out)
	}

	// It still has to be correct. A skip that loses the object is the failure
	// this optimisation risks, and it would look like success everywhere else.
	body, err := os.ReadFile(filepath.Join(dst, "gamma.txt"))
	if err != nil || strings.TrimSpace(string(body)) != "gamma" {
		t.Fatalf("gamma.txt = %q (err=%v), want \"gamma\"", string(body), err)
	}

	fetched := reg.Requests()[mark:]
	wasFetched := func(digest string) bool {
		for _, req := range fetched {
			if strings.HasPrefix(req, "GET ") && strings.HasSuffix(req, "/blobs/"+digest) {
				return true
			}
		}
		return false
	}

	for _, tc := range []struct{ branch, digest string }{
		{"alpha", alphaPack},
		{"beta", betaPack},
	} {
		if wasFetched(tc.digest) {
			t.Errorf("the lazy fetch downloaded %s's packfile, which cannot contain gamma.txt;\n"+
				"the index should have ruled it out. Requests:\n%s",
				tc.branch, strings.Join(fetched, "\n"))
		}
	}
	// And the ref that does hold it was not skipped by an over-eager index —
	// without this the test would pass just as well if nothing were fetched.
	if !wasFetched(gammaPack) {
		t.Errorf("gamma's own packfile was never fetched, so the blob came from somewhere unexpected.\n"+
			"Requests:\n%s", strings.Join(fetched, "\n"))
	}

	if out, err := v2run(t, dst, nil, "fsck"); err != nil {
		t.Fatalf("fsck: %v\n%s", err, out)
	}
}

// TestPackIndexAbsentFallsBackToStaging is the compatibility half.
//
// A repository pushed before pack indexes existed has manifests with no index
// layer, and the reader must treat that as "cannot say" rather than "holds
// nothing" — the second reading would declare every object in that repository
// missing. Stripping the layers from an already-pushed repository reproduces
// exactly that, and the clone has to keep working, just without the shortcut.
func TestPackIndexAbsentFallsBackToStaging(t *testing.T) {
	url, reg := v2setupRegistry(t)
	src := v2seed(t, url, 2)

	git(t, src, "-C", src, "checkout", "-q", "-b", "gamma", "main")
	if err := os.WriteFile(filepath.Join(src, "gamma.txt"), []byte("gamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "-C", src, "add", ".")
	git(t, src, "-C", src, "commit", "-q", "-m", "on gamma")
	git(t, src, "-C", src, "push", "-q", url, "gamma")

	// Rewrite every manifest as an older build would have written it.
	if err := reg.StripLayers(oci.MediaTypeGitPackIndex); err != nil {
		t.Fatalf("StripLayers: %v", err)
	}

	parent := t.TempDir()
	if out, err := v2run(t, parent, nil, "-c", "protocol.version=2", "-c", "ociremote.protocolV2=true",
		"clone", "--filter=blob:none", url, "dst"); err != nil {
		t.Fatalf("partial clone from an index-less repository: %v\n%s", err, out)
	}
	dst := filepath.Join(parent, "dst")

	if out, err := v2run(t, dst, nil, "-c", "protocol.version=2", "-c", "ociremote.protocolV2=true",
		"checkout", "-f", "gamma"); err != nil {
		t.Fatalf("lazy fetch without indexes: %v\n%s", err, out)
	}
	body, err := os.ReadFile(filepath.Join(dst, "gamma.txt"))
	if err != nil || strings.TrimSpace(string(body)) != "gamma" {
		t.Errorf("gamma.txt = %q (err=%v); an index-less repository lost an object", string(body), err)
	}
	if out, err := v2run(t, dst, nil, "fsck"); err != nil {
		t.Fatalf("fsck: %v\n%s", err, out)
	}
}

// TestPackIndexIsPublishedByTheAtomicPushPath.
//
// The ordinary and the `--atomic` push paths build their manifests separately
// and have drifted before — an earlier bug had the atomic one publishing refs
// whose LFS layers were never uploaded. The pack index is attached in both, and
// a reader cannot tell which path wrote a manifest, so a ref pushed atomically
// has to carry one just the same. Nothing else here exercises that half.
func TestPackIndexIsPublishedByTheAtomicPushPath(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	src := newWorkRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(src, ".git"))

	root := git(t, src, "rev-parse", "HEAD")
	commitFile(t, src, "on-main.txt", "main\n", "main one")
	git(t, src, "checkout", "-q", "-b", "second", root)
	commitFile(t, src, "on-second.txt", "second\n", "second one")
	git(t, src, "checkout", "-q", "main")

	if out, err := runHelper(t, registry,
		"option atomic true\nlist for-push\n"+
			"push refs/heads/main:refs/heads/main\n"+
			"push refs/heads/second:refs/heads/second\n\n"); err != nil {
		t.Fatalf("atomic push failed: %v (output %q)", err, out)
	}

	for _, branch := range []string{"main", "second"} {
		if !hasPackIndexLayer(t, reg, "refs/heads/"+branch) {
			t.Errorf("the atomic push of %s published no pack index layer", branch)
		}
	}
}
