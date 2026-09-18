package cli_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// fsck --repair rebuilds _refs from the ref tags. The tags are authoritative:
// a push writes the ref manifest before the index and a deletion removes the
// tag before the entry, so after an interrupted write the index is the thing
// that lags, and nothing but fsck ever compares the two.
//
// Each test puts the registry into one of those intermediate states directly,
// checks that fsck describes the change a repair would make, applies it, and
// checks that a second run finds nothing left to do.

const (
	movedSHA   = "abcdef0123456789abcdef0123456789abcdef01"
	unknownSHA = "0123456789abcdef0123456789abcdef01234567"
)

// fabricateRef publishes a ref manifest for refName by copying the seeded
// main ref manifest and rewriting its annotations, so the layers and pack
// bases it names are ones the registry already serves. It is exactly what a
// push leaves behind when it dies after writing the tag and before the index.
func fabricateRef(t *testing.T, reg *registrytest.Registry, refName, sha string, extra map[string]string) string {
	t.Helper()

	var m ocispec.Manifest
	if err := json.Unmarshal(reg.RawManifest(t, mainTag), &m); err != nil {
		t.Fatalf("unmarshal the main ref manifest: %v", err)
	}
	m.Annotations[oci.AnnotationGitRef] = refName
	m.Annotations[ocispec.AnnotationRevision] = sha
	m.Annotations[ocispec.AnnotationTitle] = refName
	for k, v := range extra {
		m.Annotations[k] = v
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal the fabricated manifest: %v", err)
	}
	tag := oci.RefManifestTag(refName)
	if tag == "" {
		t.Fatalf("%q has no manifest tag", refName)
	}
	reg.PutManifest(tag, out)
	return tag
}

// indexRefs adds entries to _refs the way a push does, through the client.
func indexRefs(t *testing.T, reg *registrytest.Registry, refs map[string]oci.RefEntry) {
	t.Helper()
	client := registrytest.Client(t, reg.LastServer())
	if err := client.PushRichRefIndex(context.Background(), refs, nil); err != nil {
		t.Fatalf("PushRichRefIndex: %v", err)
	}
}

// publishedIndex reads _refs back the way a stranger would: a fresh client
// with nothing cached.
func publishedIndex(t *testing.T, reg *registrytest.Registry) map[string]oci.RefEntry {
	t.Helper()
	client := registrytest.Client(t, reg.LastServer())
	refs, err := client.FetchRichRefIndex(context.Background())
	if err != nil {
		t.Fatalf("FetchRichRefIndex: %v", err)
	}
	return refs
}

// assertRepaired runs fsck --repair and checks the summary it prints.
func assertRepaired(t *testing.T, url, summary string) string {
	t.Helper()
	stdout, stderr, err := runCLI(t, "fsck", "--repair", url)
	if err != nil {
		t.Fatalf("fsck --repair failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "repaired _refs: "+summary) {
		t.Errorf("fsck --repair did not report %q:\nstdout: %s\nstderr: %s", summary, stdout, stderr)
	}
	return stdout
}

// assertClean runs fsck and checks it finds nothing to report or repair: the
// second run after a repair, and the proof that a repair converges.
func assertClean(t *testing.T, url string) {
	t.Helper()
	stdout, stderr, err := runCLI(t, "fsck", "--repair", url)
	if err != nil {
		t.Fatalf("fsck after repair still fails: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	if strings.Contains(stdout, "repaired") {
		t.Errorf("a second repair found something to do:\nstdout: %s", stdout)
	}
	if strings.Contains(stderr, "_refs:") {
		t.Errorf("a second run still reports drift:\nstderr: %s", stderr)
	}
	if !strings.Contains(stdout, "fetchable") {
		t.Errorf("fsck did not report overall fetchability:\n%s", stdout)
	}
}

// TestFsckRepairsATagAheadOfTheIndex: the push wrote the ref manifest and died
// before _refs, so the tag names a newer commit than the index does.
func TestFsckRepairsATagAheadOfTheIndex(t *testing.T) {
	reg, url, tip := seeded(t)
	reg.SetRefRevision(t, "refs/heads/main", movedSHA)

	stdout, stderr, err := runCLI(t, "fsck", url)
	if err == nil {
		t.Fatalf("fsck reported a stale index as healthy\nstdout: %s", stdout)
	}
	if !strings.Contains(stderr, "refs/heads/main is listed at a different commit than its tag") || !strings.Contains(stderr, "repair updates it") {
		t.Errorf("fsck did not describe the update a repair would make:\nstderr: %s", stderr)
	}
	if !strings.Contains(err.Error(), "fsck --repair") {
		t.Errorf("the failure should point at --repair, got: %v", err)
	}
	if got := publishedIndex(t, reg)["refs/heads/main"].SHA; got != tip {
		t.Fatalf("a plain fsck changed the index: main = %s", got)
	}

	assertRepaired(t, url, "0 added, 1 updated, 0 removed")
	if got := publishedIndex(t, reg)["refs/heads/main"].SHA; got != movedSHA {
		t.Errorf("after repair main = %s, want the tag's %s", got, movedSHA)
	}
	assertClean(t, url)
}

// TestFsckRepairsAnEntryWithNoTag: the deletion removed the tag and died
// before rewriting _refs, so the index lists a ref nobody can fetch.
func TestFsckRepairsAnEntryWithNoTag(t *testing.T) {
	reg, url, tip := seeded(t)
	fabricateRef(t, reg, "refs/heads/gone", tip, nil)
	indexRefs(t, reg, map[string]oci.RefEntry{"refs/heads/gone": {SHA: tip}})
	reg.DropManifest(oci.RefManifestTag("refs/heads/gone"))

	_, stderr, err := runCLI(t, "fsck", url)
	if err == nil {
		t.Fatal("fsck reported an index listing a ref with no tag as healthy")
	}
	if !strings.Contains(stderr, "refs/heads/gone is listed at") || !strings.Contains(stderr, "repair removes it") {
		t.Errorf("fsck did not describe the removal a repair would make:\nstderr: %s", stderr)
	}

	assertRepaired(t, url, "0 added, 0 updated, 1 removed")
	final := publishedIndex(t, reg)
	if _, still := final["refs/heads/gone"]; still {
		t.Error("the ref with no tag is still listed after repair")
	}
	if final["refs/heads/main"].SHA != tip {
		t.Errorf("repair disturbed main: %s", final["refs/heads/main"].SHA)
	}
	assertClean(t, url)
}

// TestFsckRepairsAnEntryWithATombstonedTag: on a registry that refuses
// deletion the tag is overwritten with a tombstone (FORMAT.md §8), and a
// deletion that died there leaves the entry behind just the same.
func TestFsckRepairsAnEntryWithATombstonedTag(t *testing.T) {
	reg, url, tip := seeded(t)
	tag := fabricateRef(t, reg, "refs/heads/dev", tip, nil)
	indexRefs(t, reg, map[string]oci.RefEntry{"refs/heads/dev": {SHA: tip}})
	if err := reg.SetManifestAnnotation(tag, oci.AnnotationGitDeleted, "true"); err != nil {
		t.Fatal(err)
	}

	_, stderr, err := runCLI(t, "fsck", url)
	if err == nil {
		t.Fatal("fsck reported an index listing a tombstoned ref as healthy")
	}
	if !strings.Contains(stderr, "refs/heads/dev is listed at") || !strings.Contains(stderr, "gone or deleted") {
		t.Errorf("fsck did not describe the tombstone:\nstderr: %s", stderr)
	}

	assertRepaired(t, url, "0 added, 0 updated, 1 removed")
	if _, still := publishedIndex(t, reg)["refs/heads/dev"]; still {
		t.Error("the tombstoned ref is still listed after repair")
	}
	assertClean(t, url)
}

// TestFsckRepairsATagWithNoEntry: the push wrote the ref manifest for a new
// branch and died before _refs, so readers cannot see the branch at all.
func TestFsckRepairsATagWithNoEntry(t *testing.T) {
	reg, url, tip := seeded(t)
	fabricateRef(t, reg, "refs/heads/dev", tip, nil)

	_, stderr, err := runCLI(t, "fsck", url)
	if err == nil {
		t.Fatal("fsck reported an index missing a live ref as healthy")
	}
	if !strings.Contains(stderr, "refs/heads/dev is not listed, but its tag is live") || !strings.Contains(stderr, "repair adds it") {
		t.Errorf("fsck did not describe the addition a repair would make:\nstderr: %s", stderr)
	}

	assertRepaired(t, url, "1 added, 0 updated, 0 removed")
	if got := publishedIndex(t, reg)["refs/heads/dev"].SHA; got != tip {
		t.Errorf("after repair dev = %q, want %s", got, tip)
	}
	assertClean(t, url)
}

// TestFsckRepairRecoversATruncatedRef: a ref whose encoding exceeds the OCI
// tag limit is stored under a lossy `_h_` tag (FORMAT.md §3.1) and is
// discoverable only through _refs. The repair has to recover its name from
// the manifest's annotation, because the tag cannot be decoded.
func TestFsckRepairRecoversATruncatedRef(t *testing.T) {
	reg, url, tip := seeded(t)
	long := "refs/heads/" + strings.Repeat("feature-with-a-very-long-name/", 6) + "end"
	tag := fabricateRef(t, reg, long, tip, nil)
	if !strings.HasPrefix(tag, "_h_") {
		t.Fatalf("fixture error: %q should have a truncated tag, got %q", long, tag)
	}

	_, stderr, err := runCLI(t, "fsck", url)
	if err == nil {
		t.Fatal("fsck reported an index missing a truncated ref as healthy")
	}
	if !strings.Contains(stderr, long+" is not listed") {
		t.Errorf("fsck did not name the truncated ref by its full name:\nstderr: %s", stderr)
	}

	assertRepaired(t, url, "1 added, 0 updated, 0 removed")
	if got := publishedIndex(t, reg)[long].SHA; got != tip {
		t.Errorf("after repair the truncated ref is %q in the index, want %s", got, tip)
	}
	assertClean(t, url)
}

// TestFsckRepairKeepsHEADAndTagMetadata: a repair rewrites the whole index, so
// everything it does not mean to change has to come through untouched -- the
// recorded HEAD and the annotated-tag fields of an entry it did not touch --
// and an entry it rebuilds recovers the tag fields the manifest carries.
func TestFsckRepairKeepsHEADAndTagMetadata(t *testing.T) {
	reg, url, tip := seeded(t)
	client := registrytest.Client(t, reg.LastServer())
	ctx := context.Background()

	if _, err := client.SetHead(ctx, "refs/heads/main"); err != nil {
		t.Fatalf("SetHead: %v", err)
	}

	signed := oci.RefEntry{
		SHA:        tip,
		Tagger:     "T <t@example.com>",
		TagMessage: "release 1.0",
		TagSig:     "-----BEGIN PGP SIGNATURE-----\nv1\n-----END PGP SIGNATURE-----\n",
		TagObject:  unknownSHA,
	}
	fabricateRef(t, reg, "refs/tags/v1", tip, map[string]string{
		oci.AnnotationGitTagger:     signed.Tagger,
		oci.AnnotationGitTagMessage: signed.TagMessage,
		oci.AnnotationGitTagSig:     signed.TagSig,
		oci.AnnotationGitTagObj:     signed.TagObject,
	})
	indexRefs(t, reg, map[string]oci.RefEntry{"refs/tags/v1": signed})

	// A second annotated tag the index never learned about: its metadata is
	// recoverable from the manifest and must come back with it.
	fabricateRef(t, reg, "refs/tags/v2", tip, map[string]string{
		oci.AnnotationGitTagger:     "U <u@example.com>",
		oci.AnnotationGitTagMessage: "release 2.0",
		oci.AnnotationGitTagObj:     movedSHA,
	})

	assertRepaired(t, url, "1 added, 0 updated, 0 removed")

	head, err := client.FetchHead(ctx)
	if err != nil {
		t.Fatalf("FetchHead: %v", err)
	}
	if head != "refs/heads/main" {
		t.Errorf("repair lost the recorded HEAD: %q", head)
	}
	final := publishedIndex(t, reg)
	if got := final["refs/tags/v1"]; got != signed {
		t.Errorf("repair disturbed the untouched tag entry:\n got %+v\nwant %+v", got, signed)
	}
	if got, want := final["refs/tags/v2"], (oci.RefEntry{SHA: tip, Tagger: "U <u@example.com>", TagMessage: "release 2.0", TagObject: movedSHA}); got != want {
		t.Errorf("repair did not recover the tag metadata for the added entry:\n got %+v\nwant %+v", got, want)
	}
	assertClean(t, url)
}

// TestFsckRepairDoesNotClobberAConcurrentPush: a push that lands while the
// repair is running is the case the digest compare-and-swap exists for. The
// plan was made against an index that push has since moved; it must be
// recomputed, not applied over the top.
//
// The repair's plan is to move main's entry up to its tag, which is ahead, and
// to add dev. The push is injected at the moment the repair takes the index
// lock -- after its plan is made and before its write -- and moves main's tag
// and entry together to a third commit, as a real push does. A repair that
// applied its original plan would write the tag's old commit over the push.
func TestFsckRepairDoesNotClobberAConcurrentPush(t *testing.T) {
	reg, url, tip := seeded(t)
	reg.SetRefRevision(t, "refs/heads/main", movedSHA)
	fabricateRef(t, reg, "refs/heads/dev", tip, nil)

	var fired atomic.Bool
	reg.Observe(func(method, path string) {
		if method != http.MethodPut || !strings.Contains(path, "/manifests/"+oci.LockTagPrefix) {
			return
		}
		if !fired.CompareAndSwap(false, true) {
			return
		}
		if err := reg.SetManifestAnnotation(mainTag, ocispec.AnnotationRevision, unknownSHA); err != nil {
			panic(err)
		}
		if err := reg.SetIndexedRef("refs/heads/main", unknownSHA); err != nil {
			panic(err)
		}
	})
	t.Cleanup(func() { reg.Observe(nil) })

	// By the time the write goes through, main agrees with its tag again and
	// only dev is left to add: the summary describes what was written, not
	// what was planned.
	assertRepaired(t, url, "1 added, 0 updated, 0 removed")
	if !fired.Load() {
		t.Fatal("fixture error: the concurrent push was never injected")
	}

	final := publishedIndex(t, reg)
	if got := final["refs/heads/main"].SHA; got != unknownSHA {
		t.Errorf("the concurrent push was clobbered: main = %s, want %s", got, unknownSHA)
	}
	if got := final["refs/heads/dev"].SHA; got != tip {
		t.Errorf("the repair's own change was lost: dev = %q, want %s", got, tip)
	}
	reg.Observe(nil)
	assertClean(t, url)
}

// TestFsckRepairRewritesAStaleMirror: when the tags and _refs agree but the
// _index mirror is behind, --repair republishes the index unchanged, which is
// how a push would rewrite the mirror.
func TestFsckRepairRewritesAStaleMirror(t *testing.T) {
	reg, url, _ := seeded(t)
	reg.DropManifest(oci.TagOCIIndex)

	stdout := assertRepaired(t, url, "_index mirror rewritten")
	if !strings.Contains(stdout, "_index mirror matches _refs") {
		t.Errorf("the mirror was not rewritten:\nstdout: %s", stdout)
	}
	if _, ok := reg.ManifestBytes(oci.TagOCIIndex); !ok {
		t.Error("no _index manifest after the repair")
	}
	assertClean(t, url)
}

// TestFsckRepairRefusesAnUnsupportedFormat: a repair that rewrote an index in
// a format this build does not implement would stamp version 1 over a layout
// it cannot read. It is refused before anything is written.
func TestFsckRepairRefusesAnUnsupportedFormat(t *testing.T) {
	reg, url, tip := seeded(t)
	fabricateRef(t, reg, "refs/heads/dev", tip, nil)
	if err := reg.SetManifestAnnotation(oci.TagRefIndex, oci.AnnotationFormatVersion, "99"); err != nil {
		t.Fatal(err)
	}
	before := reg.RawManifest(t, oci.TagRefIndex)

	_, stderr, err := runCLI(t, "fsck", "--repair", url)
	if err == nil {
		t.Fatal("fsck --repair rewrote an index in an unsupported format")
	}
	if !strings.Contains(err.Error(), "format") {
		t.Errorf("the refusal should name the format version, got: %v\nstderr: %s", err, stderr)
	}
	if after := reg.RawManifest(t, oci.TagRefIndex); string(after) != string(before) {
		t.Error("the _refs manifest was rewritten despite the refusal")
	}
}

// TestFsckRepairCreatesAMissingIndex: a repository whose _refs was lost is
// served the mirror or a tag enumeration, which cannot see truncated names.
// The repair writes the index back from the tags.
func TestFsckRepairCreatesAMissingIndex(t *testing.T) {
	reg, url, tip := seeded(t)
	reg.DropManifest(oci.TagRefIndex)
	reg.DropManifest(oci.TagOCIIndex)

	_, stderr, err := runCLI(t, "fsck", url)
	if err == nil {
		t.Fatal("fsck reported a repository with no index as healthy")
	}
	if !strings.Contains(stderr, "_refs: absent") {
		t.Errorf("fsck did not say the index was absent:\nstderr: %s", stderr)
	}

	assertRepaired(t, url, "1 added, 0 updated, 0 removed")
	if got := publishedIndex(t, reg)["refs/heads/main"].SHA; got != tip {
		t.Errorf("after repair main = %q, want %s", got, tip)
	}
	assertClean(t, url)
}
