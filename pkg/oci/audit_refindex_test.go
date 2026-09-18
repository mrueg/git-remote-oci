package oci_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// These tests pin the ways the _refs index must *not* be read around.
//
// Every one of them is a case where the old code, faced with an index it
// could not use, quietly used something else instead: the tags, the _index
// mirror, or nothing at all. Each of those substitutes is worse than the
// failure it hid.

// TestListRefsDoesNotFallBackOnAnUnsupportedFormat: a repository this build
// refuses must stay refused. Falling back to tag enumeration read the
// repository anyway, which is exactly the read the version check exists to
// prevent.
func TestListRefsDoesNotFallBackOnAnUnsupportedFormat(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	registrytest.SeedRepository(t, client, 1)

	if err := reg.SetManifestAnnotation(oci.TagRefIndex, oci.AnnotationFormatVersion, "2"); err != nil {
		t.Fatalf("SetManifestAnnotation: %v", err)
	}

	// A stranger's client: the seeding one may answer from its caches.
	reader := registrytest.Client(t, ts)
	refs, err := reader.ListRefs(context.Background())
	if err == nil {
		t.Fatalf("ListRefs read a format-version-2 repository via its tags: %v", refs)
	}
	if !errors.Is(err, oci.ErrUnsupportedFormat) {
		t.Errorf("error should be ErrUnsupportedFormat, got: %v", err)
	}
}

// TestRefIndexUpdateFailsWhenTheBaseCannotBeRead: a merge whose base cannot
// be fetched must not proceed with an empty one. It used to, after five
// silent retries, and the index it then published held only this push's refs:
// every truncated-tag ref and every entry's metadata were gone.
func TestRefIndexUpdateFailsWhenTheBaseCannotBeRead(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	_, tip := registrytest.SeedRepository(t, client, 1)
	before := reg.RawManifest(t, oci.TagRefIndex)

	var refused atomic.Int32
	reg.Intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/manifests/"+oci.TagRefIndex) {
			refused.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		return false
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := client.PushRichRefIndexWithHead(ctx, map[string]oci.RefEntry{
		"refs/heads/feature": {SHA: tip},
	}, nil, "")
	if err == nil {
		t.Fatal("the index was updated from a base that could not be read")
	}
	if refused.Load() == 0 {
		t.Fatal("test setup: the _refs fetch was never refused")
	}
	if !bytes.Equal(reg.RawManifest(t, oci.TagRefIndex), before) {
		t.Error("the _refs index was rewritten despite the base being unreadable")
	}
}

// TestRichRefIndexFallsBackToTheMirrorOnlyWhenAbsent: _index stands in for a
// _refs that is *missing*, not for one that could not be read. A mirror can
// be left stale by a failed push, and a momentary error on _refs must not
// make it the base of a read-modify-write.
func TestRichRefIndexFallsBackToTheMirrorOnlyWhenAbsent(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	_, tip := registrytest.SeedRepository(t, client, 1)

	reg.Intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/manifests/"+oci.TagRefIndex) {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		return false
	})
	reader := registrytest.Client(t, ts)
	if refs, err := reader.FetchRichRefIndex(context.Background()); err == nil {
		t.Fatalf("an unreadable _refs was answered from the mirror: %v", refs)
	} else if oci.IsNotFound(err) {
		t.Errorf("a 500 on _refs was reported as not found: %v", err)
	}

	// Absent, on the other hand, is what the mirror is for.
	reg.Intercept(nil)
	reg.DropManifest(oci.TagRefIndex)
	refs, err := reader.FetchRichRefIndex(context.Background())
	if err != nil {
		t.Fatalf("FetchRichRefIndex with _refs absent and _index present: %v", err)
	}
	if refs["refs/heads/main"].SHA != tip {
		t.Errorf("mirror fallback returned %v, want refs/heads/main at %s", refs, tip)
	}
}

// TestIsRefFullyPushedIgnoresFetchedManifests: having read a manifest is not
// having pushed it. The manifest cache is filled by every fetch and every tag
// enumeration, and inferring "already pushed" from it let a push that
// followed a list skip publishing the ref.
func TestIsRefFullyPushedIgnoresFetchedManifests(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	pusher := registrytest.Client(t, ts)
	_, tip := registrytest.SeedRepository(t, pusher, 1)

	if !pusher.IsRefFullyPushed(tip, "refs/heads/main") {
		t.Error("the client that pushed the ref does not report it as pushed")
	}
	if pusher.IsRefFullyPushed(tip, "refs/heads/other") {
		t.Error("a ref this client never pushed is reported as pushed")
	}

	reader := registrytest.Client(t, ts)
	ctx := context.Background()
	if _, err := reader.FetchManifest(ctx, tip); err != nil {
		t.Fatalf("FetchManifest(commit): %v", err)
	}
	if _, err := reader.FetchManifest(ctx, oci.RefManifestTag("refs/heads/main")); err != nil {
		t.Fatalf("FetchManifest(ref): %v", err)
	}
	if !reader.IsCommitManifestCached(tip) {
		t.Fatal("test setup: the fetch did not populate the manifest cache")
	}
	if reader.IsRefFullyPushed(tip, "refs/heads/main") {
		t.Error("a client that only fetched the manifests reports the ref as pushed")
	}
}

// TestTagEnumerationValidatesRefNames: a ref name read from a manifest
// annotation reaches the same place as one read from the index blob -- the
// helper's `list` output, where a newline is another line of protocol -- so it
// gets the same validation.
func TestTagEnumerationValidatesRefNames(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	registrytest.SeedRepository(t, client, 1)

	bad := "refs/heads/main\nrefs/heads/injected"
	if err := reg.SetManifestAnnotation(oci.RefManifestTag("refs/heads/main"), oci.AnnotationGitRef, bad); err != nil {
		t.Fatalf("SetManifestAnnotation: %v", err)
	}
	// No index, so both paths enumerate the tags.
	reg.DropManifest(oci.TagRefIndex)
	reg.DropManifest(oci.TagOCIIndex)

	var mu sync.Mutex
	var warnings []string
	reader := registrytest.Client(t, ts)
	reader.Warnf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		warnings = append(warnings, fmt.Sprintf(format, args...))
	}

	for name, list := range map[string]func(context.Context) (map[string]string, error){
		"ListRefs":         reader.ListRefs,
		"EnumerateTagRefs": reader.EnumerateTagRefs,
	} {
		refs, err := list(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for ref := range refs {
			if strings.ContainsAny(ref, "\n ") {
				t.Errorf("%s returned a ref name with a control character: %q", name, ref)
			}
		}
		if len(refs) != 0 {
			t.Errorf("%s returned %v; the only ref has an invalid name and should be dropped", name, refs)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(warnings) == 0 {
		t.Error("dropping a ref was silent; it should be reported through Warnf")
	}
}

// TestSetHeadRefusesAnythingButABranch: FORMAT.md §6.2. HEAD is a symbolic
// ref to a branch, and a reader that finds it naming a tag has nothing to
// check out.
func TestSetHeadRefusesAnythingButABranch(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	registrytest.SeedRepository(t, client, 1)

	for _, ref := range []string{"refs/tags/v1", "refs/notes/commits", "main"} {
		if _, err := client.SetHead(context.Background(), ref); err == nil {
			t.Errorf("SetHead(%q) succeeded; only refs/heads/ refs may be HEAD", ref)
		} else if !strings.Contains(err.Error(), "refs/heads/") {
			t.Errorf("SetHead(%q) error should say what is allowed, got: %v", ref, err)
		}
	}
}

// TestIndexMirrorOmitsAnEmptyHead: "none recorded" is spelled by absence on
// _refs, and the mirror has to spell it the same way.
func TestIndexMirrorOmitsAnEmptyHead(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	// SeedRepository publishes the index with no head hint.
	registrytest.SeedRepository(t, client, 1)

	for _, tag := range []string{oci.TagOCIIndex, oci.TagRefIndex} {
		var doc struct {
			Annotations map[string]string `json:"annotations"`
		}
		if err := json.Unmarshal(reg.RawManifest(t, tag), &doc); err != nil {
			t.Fatalf("unmarshal %s: %v", tag, err)
		}
		if head, present := doc.Annotations[oci.AnnotationGitHead]; present {
			t.Errorf("%s carries %s=%q; with no HEAD recorded the annotation must be absent", tag, oci.AnnotationGitHead, head)
		}
	}
}

// TestIndexMirrorNamesTheRefManifestTag: the ref.name annotation on an _index
// entry is the tag the entry can be pulled by, which for a ref whose encoding
// looks like a commit id is the "ref-" prefixed form (§3.2), not the bare
// encoding.
func TestIndexMirrorNamesTheRefManifestTag(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	ctx := context.Background()

	const commit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ref := "refs/heads/" + strings.Repeat("b", 40)
	if oci.RefManifestTag(ref) == oci.EncodeRefTag(ref) {
		t.Fatal("test setup: the ref's manifest tag should differ from its bare encoding")
	}
	pack := []byte("not a real packfile")
	if err := client.PushCommitStream(ctx, oci.CommitPush{
		CommitSHA:   commit,
		RefName:     ref,
		WriteRefTag: true,
	}, bytes.NewReader(pack), int64(len(pack))); err != nil {
		t.Fatalf("PushCommitStream: %v", err)
	}
	if err := client.PushRichRefIndex(ctx, map[string]oci.RefEntry{ref: {SHA: commit}}, nil); err != nil {
		t.Fatalf("PushRichRefIndex: %v", err)
	}

	var index ocispec.Index
	if err := json.Unmarshal(reg.RawManifest(t, oci.TagOCIIndex), &index); err != nil {
		t.Fatalf("unmarshal _index: %v", err)
	}
	found := false
	for _, m := range index.Manifests {
		if m.Annotations[oci.AnnotationGitRef] != ref {
			continue
		}
		found = true
		if got, want := m.Annotations[ocispec.AnnotationRefName], oci.RefManifestTag(ref); got != want {
			t.Errorf("_index entry for %s names tag %q, want %q", ref, got, want)
		}
	}
	if !found {
		t.Fatalf("_index has no entry for %s", ref)
	}
}

// TestPackChainIsNotCachedAfterAFailedRead: a read that failed is not an
// answer. Remembering it as "no chain" for the life of the client made the
// next push republish only its own edges.
func TestPackChainIsNotCachedAfterAFailedRead(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	registrytest.SeedRepository(t, client, 3)

	reg.Intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/manifests/"+oci.TagRefIndex) {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		return false
	})
	reader := registrytest.Client(t, ts)
	if chain, ok := reader.FetchPackChain(context.Background()); ok {
		t.Fatalf("a chain was read through a 500: %v", chain)
	}

	reg.Intercept(nil)
	chain, ok := reader.FetchPackChain(context.Background())
	if !ok {
		t.Fatal("the failed read was cached as \"no chain\"; the chain is there and should be read now")
	}
	if len(chain) != 3 {
		t.Errorf("chain has %d edges, want 3: %v", len(chain), chain)
	}
}
