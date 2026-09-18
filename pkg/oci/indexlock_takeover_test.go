package oci_test

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	opencontainers "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// The pseudo-ref whose lock serialises _refs updates (FORMAT.md §9).
const refsIndexLockRef = "_refs_index_lock"

const (
	takeoverSHAMain    = "1111111111111111111111111111111111111111"
	takeoverSHAFeature = "2222222222222222222222222222222222222222"
)

// foreignLock is a live lock manifest as another client would have written
// it: a different owner, a different id, minutes of TTL left.
func foreignLock(t *testing.T, ref string) []byte {
	t.Helper()
	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    ocispec.Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: opencontainers.FromBytes([]byte("{}")), Size: 2},
		Annotations: map[string]string{
			oci.AnnotationLockRef:       ref,
			oci.AnnotationLockOwner:     "someone@elsewhere",
			oci.AnnotationLockID:        "4242-someone@elsewhere",
			oci.AnnotationLockExpiresAt: time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339),
		},
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal lock manifest: %v", err)
	}
	return body
}

func lockOwner(t *testing.T, reg *registrytest.Registry, tag string) string {
	t.Helper()
	var manifest ocispec.Manifest
	if err := json.Unmarshal(reg.RawManifest(t, tag), &manifest); err != nil {
		t.Fatalf("parse lock manifest: %v", err)
	}
	return manifest.Annotations[oci.AnnotationLockOwner]
}

func countRequests(reg *registrytest.Registry, method, suffix string) int {
	n := 0
	for _, r := range reg.Requests() {
		if strings.HasPrefix(r, method+" ") && strings.HasSuffix(r, suffix) {
			n++
		}
	}
	return n
}

// seededIndexRegistry serves a repository whose _refs already lists main, with
// a client whose registry-side warnings go to the test log.
func seededIndexRegistry(t *testing.T) (*registrytest.Registry, *oci.Client) {
	t.Helper()
	reg := registrytest.New()
	ts := reg.Serve(t)
	client := registrytest.Client(t, ts)
	client.Warnf = func(format string, args ...any) { t.Logf("warning: "+format, args...) }
	client.Verbosef = func(format string, args ...any) { t.Logf("verbose: "+format, args...) }
	if err := client.PushRichRefIndex(t.Context(), map[string]oci.RefEntry{"refs/heads/main": {SHA: takeoverSHAMain}}, nil); err != nil {
		t.Fatalf("seed _refs: %v", err)
	}
	return reg, client
}

// TestRefIndexUpdateVerifiesItselfAfterALockTakeover pins the common outcome
// of the window FORMAT.md §9 leaves open: acquisition is check, write, read
// back, so another client can overwrite the _refs index lock after this
// client's read-back and both believe they hold it. Here that happens just as
// this client's index write lands. Release rightly refuses to touch the
// other client's lock; the update must then be judged by what the registry
// serves, not by the lock. The write did land, so the update succeeds --
// without writing again, and without touching the foreign lock.
func TestRefIndexUpdateVerifiesItselfAfterALockTakeover(t *testing.T) {
	reg, client := seededIndexRegistry(t)
	lockTag := oci.LockTag(refsIndexLockRef)

	var fired atomic.Bool
	reg.Observe(func(method, path string) {
		if method == "PUT" && strings.HasSuffix(path, "/manifests/"+oci.TagRefIndex) && fired.CompareAndSwap(false, true) {
			reg.PutManifest(lockTag, foreignLock(t, refsIndexLockRef))
		}
	})

	err := client.PushRichRefIndex(t.Context(), map[string]oci.RefEntry{"refs/heads/feature": {SHA: takeoverSHAFeature}}, nil)
	reg.Observe(nil)
	if err != nil {
		t.Fatalf("PushRichRefIndex whose lock was taken over after a successful write: %v", err)
	}
	if !fired.Load() {
		t.Fatal("test setup: the takeover never happened")
	}

	published, err := client.FetchRichRefIndex(t.Context())
	if err != nil {
		t.Fatalf("FetchRichRefIndex: %v", err)
	}
	for ref, want := range map[string]string{"refs/heads/main": takeoverSHAMain, "refs/heads/feature": takeoverSHAFeature} {
		if got := published[ref].SHA; got != want {
			t.Errorf("%s = %q in the published index, want %s", ref, got, want)
		}
	}
	// Seed plus this update: the verified write was not repeated.
	if n := countRequests(reg, "PUT", "/manifests/"+oci.TagRefIndex); n != 2 {
		t.Errorf("_refs was written %d times, want 2 (seed and one update): the verified write was redone", n)
	}
	if owner := lockOwner(t, reg, lockTag); owner != "someone@elsewhere" {
		t.Errorf("the foreign lock now belongs to %q: release wrote over a lock that was not ours", owner)
	}
}

// TestRefIndexUpdateRedoneWhenATakeoverClobbersIt is the other outcome of the
// same window: the other holder writes its own merge of _refs after this
// client's, from a base that predates it, so this client's entry is gone from
// the published index. The read-back finds it missing and the update is redone
// against what the other client left, once its lock is gone.
func TestRefIndexUpdateRedoneWhenATakeoverClobbersIt(t *testing.T) {
	reg, client := seededIndexRegistry(t)
	lockTag := oci.LockTag(refsIndexLockRef)

	var (
		stage          atomic.Int32
		beforeOurWrite []byte
	)
	reg.Observe(func(method, path string) {
		if !strings.HasSuffix(path, "/manifests/"+oci.TagRefIndex) {
			return
		}
		switch {
		case method == "PUT" && stage.CompareAndSwap(0, 1):
			// Just before this client's write lands: remember the index it
			// is about to replace, and let another client take the lock.
			before, ok := reg.ManifestBytes(oci.TagRefIndex)
			if !ok {
				panic("no _refs to remember")
			}
			beforeOurWrite = before
			reg.PutManifest(lockTag, foreignLock(t, refsIndexLockRef))
		case method != "PUT" && stage.CompareAndSwap(1, 2):
			// The first read after the write is the read-back. The other
			// holder has meanwhile published its own index from the older
			// base, dropping this client's entry, and has released its lock.
			reg.PutManifest(oci.TagRefIndex, beforeOurWrite)
			reg.DropManifest(lockTag)
		}
	})

	err := client.PushRichRefIndex(t.Context(), map[string]oci.RefEntry{"refs/heads/feature": {SHA: takeoverSHAFeature}}, nil)
	reg.Observe(nil)
	if err != nil {
		t.Fatalf("PushRichRefIndex whose write was clobbered under a shared lock: %v", err)
	}
	if stage.Load() != 2 {
		t.Fatalf("test setup: reached stage %d, want 2", stage.Load())
	}

	published, err := client.FetchRichRefIndex(t.Context())
	if err != nil {
		t.Fatalf("FetchRichRefIndex: %v", err)
	}
	for ref, want := range map[string]string{"refs/heads/main": takeoverSHAMain, "refs/heads/feature": takeoverSHAFeature} {
		if got := published[ref].SHA; got != want {
			t.Errorf("%s = %q in the published index, want %s", ref, got, want)
		}
	}
	// Seed, the clobbered write, and the redo.
	if n := countRequests(reg, "PUT", "/manifests/"+oci.TagRefIndex); n != 3 {
		t.Errorf("_refs was written %d times, want 3 (seed, clobbered write, redo)", n)
	}
}

// TestSetHeadVerifiesItselfAfterALockTakeover: SetHead shares the commit path
// and the same window, and judges itself by the recorded HEAD.
func TestSetHeadVerifiesItselfAfterALockTakeover(t *testing.T) {
	reg, client := seededIndexRegistry(t)
	lockTag := oci.LockTag(refsIndexLockRef)

	var fired atomic.Bool
	reg.Observe(func(method, path string) {
		if method == "PUT" && strings.HasSuffix(path, "/manifests/"+oci.TagRefIndex) && fired.CompareAndSwap(false, true) {
			reg.PutManifest(lockTag, foreignLock(t, refsIndexLockRef))
		}
	})

	_, err := client.SetHead(t.Context(), "refs/heads/main")
	reg.Observe(nil)
	if err != nil {
		t.Fatalf("SetHead whose lock was taken over after a successful write: %v", err)
	}
	if !fired.Load() {
		t.Fatal("test setup: the takeover never happened")
	}
	head, err := client.FetchHead(t.Context())
	if err != nil {
		t.Fatalf("FetchHead: %v", err)
	}
	if head != "refs/heads/main" {
		t.Errorf("recorded HEAD is %q, want refs/heads/main", head)
	}
}
