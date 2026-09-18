package oci_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	opencontainers "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/lfs"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// A lock's expiry is recorded at second precision (RFC 3339), so a TTL under
// a second can floor to "already expired" on the read-back that confirms the
// acquisition. lockTestTTL clears that; lockTestExpiryWait is past it.
const (
	lockTestTTL        = 1500 * time.Millisecond
	lockTestExpiryWait = 2 * time.Second
)

// TestReleaseAfterExpiryLeavesTheNewHolderAlone is the interleaving that lost
// locks: A's lock expires, B legitimately takes the ref, and A's release --
// finding "no live lock of mine" -- wrote its tombstone anyway, over B's lock.
func TestReleaseAfterExpiryLeavesTheNewHolderAlone(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	ctx := context.Background()
	const ref = "refs/heads/main"

	// The expiry is written at RFC 3339 second precision, so the TTL has to
	// clear the next second boundary to be a lock at all.
	a := registrytest.Client(t, ts)
	if _, err := a.AcquireRefLock(ctx, ref, lockTestTTL); err != nil {
		t.Fatalf("A acquire: %v", err)
	}
	time.Sleep(lockTestExpiryWait)

	b := registrytest.Client(t, ts)
	bLock, err := b.AcquireRefLock(ctx, ref, time.Minute)
	if err != nil {
		t.Fatalf("B acquire after A's expiry: %v", err)
	}
	bManifest := reg.RawManifest(t, oci.LockTag(ref))

	if err := a.ReleaseRefLock(ctx, ref); err == nil {
		t.Error("A released a ref that B now holds")
	}

	locked, info, err := b.IsLocked(ctx, ref)
	if err != nil {
		t.Fatalf("IsLocked: %v", err)
	}
	if !locked || info == nil || info.LockID != bLock.LockID {
		t.Fatalf("B's lock is gone after A's release: locked=%v info=%+v", locked, info)
	}
	if !bytes.Equal(reg.RawManifest(t, oci.LockTag(ref)), bManifest) {
		t.Error("A's release rewrote the lock manifest")
	}

	// The refusal must clear A's stale bookkeeping, or A can never lock the
	// ref again in this process.
	if err := b.ReleaseRefLock(ctx, ref); err != nil {
		t.Fatalf("B release: %v", err)
	}
	if _, err := a.AcquireRefLock(ctx, ref, time.Minute); err != nil {
		t.Errorf("A cannot re-acquire after its release was refused: %v", err)
	}
}

// TestReleaseOfAnExpiredLockWritesNothing: with nothing of ours published
// there is nothing to release, and writing a tombstone is how the previous
// test's bug happened.
func TestReleaseOfAnExpiredLockWritesNothing(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	ctx := context.Background()
	const ref = "refs/heads/main"

	client := registrytest.Client(t, ts)
	if _, err := client.AcquireRefLock(ctx, ref, lockTestTTL); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	time.Sleep(lockTestExpiryWait)
	before := reg.RawManifest(t, oci.LockTag(ref))

	if err := client.ReleaseRefLock(ctx, ref); err != nil {
		t.Fatalf("releasing an expired lock: %v", err)
	}
	if !bytes.Equal(reg.RawManifest(t, oci.LockTag(ref)), before) {
		t.Error("releasing an expired lock rewrote the lock manifest")
	}
	if _, err := client.AcquireRefLock(ctx, ref, time.Minute); err != nil {
		t.Errorf("re-acquire after the no-op release: %v", err)
	}
}

// TestReleaseTreatsAMissingLockIDAsForeign: the id is the only evidence a
// lock is ours, and a published lock without one used to be releasable by
// anyone.
func TestReleaseTreatsAMissingLockIDAsForeign(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	ctx := context.Background()
	const ref = "refs/heads/main"

	client := registrytest.Client(t, ts)
	if _, err := client.AcquireRefLock(ctx, ref, time.Minute); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := reg.SetManifestAnnotation(oci.LockTag(ref), oci.AnnotationLockID, ""); err != nil {
		t.Fatalf("SetManifestAnnotation: %v", err)
	}
	before := reg.RawManifest(t, oci.LockTag(ref))

	if err := client.ReleaseRefLock(ctx, ref); err == nil {
		t.Error("a lock with no id was released")
	}
	if !bytes.Equal(reg.RawManifest(t, oci.LockTag(ref)), before) {
		t.Error("the id-less lock was overwritten")
	}
}

// TestLockOnARefThatFillsTheTagLimit: a ref whose encoding uses all 128 bytes
// could not be locked, because the lock tag prefixes the encoding and the
// registry rejects a tag longer than the limit. The push takes the lock
// first, so the ref could not be pushed at all.
func TestLockOnARefThatFillsTheTagLimit(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	ctx := context.Background()

	ref := "refs/heads/" + strings.Repeat("x", 128)
	if n := len(oci.EncodeRefTag(ref)); n != 128 {
		t.Fatalf("test setup: ref tag is %d bytes, want 128", n)
	}

	client := registrytest.Client(t, ts)
	if _, err := client.AcquireRefLock(ctx, ref, time.Minute); err != nil {
		t.Fatalf("AcquireRefLock on a 128-byte ref tag: %v", err)
	}
	locked, _, err := client.IsLocked(ctx, ref)
	if err != nil || !locked {
		t.Fatalf("IsLocked = %v, %v; want locked", locked, err)
	}
	if err := client.ReleaseRefLock(ctx, ref); err != nil {
		t.Fatalf("ReleaseRefLock: %v", err)
	}
}

// TestLFSLockUpdateNoticesAConcurrentWriter mirrors the _refs test: the index
// lock is advisory, so another writer can land between this client's read of
// the lock list and its write. The digest re-check before the write has to
// notice, and the update has to be redone against what they left.
func TestLFSLockUpdateNoticesAConcurrentWriter(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)
	ctx := context.Background()
	client := registrytest.Client(t, ts)

	first, err := client.AcquireLFSLock(ctx, "a.bin", "alice")
	if err != nil {
		t.Fatalf("first AcquireLFSLock: %v", err)
	}

	// The competing writer: a list holding the first lock and one of its
	// own, written directly at the moment this client re-resolves _lfs_locks
	// before its write.
	intruder := lfs.LFSLock{
		ID:       lfs.GenerateLockID("z.bin", "zed", time.Now().UTC()),
		Path:     "z.bin",
		Owner:    lfs.LFSOwner{Name: "zed"},
		LockedAt: time.Now().UTC(),
	}
	var fired atomic.Bool
	reg.Observe(func(method, path string) {
		if method != "HEAD" || !strings.HasSuffix(path, "/manifests/"+lfs.TagLFSLocks) {
			return
		}
		if !fired.CompareAndSwap(false, true) {
			return
		}
		list, err := json.Marshal(lfs.LFSLockList{Locks: []lfs.LFSLock{*first, intruder}})
		if err != nil {
			panic(err)
		}
		digest := reg.PutBlob(list)
		manifest, err := json.Marshal(ocispec.Manifest{
			MediaType: ocispec.MediaTypeImageManifest,
			Config: ocispec.Descriptor{
				MediaType: oci.MediaTypeGitConfig,
				Digest:    opencontainers.Digest(digest),
				Size:      int64(len(list)),
			},
			Layers:      []ocispec.Descriptor{ocispec.DescriptorEmptyJSON},
			Annotations: map[string]string{oci.AnnotationLFSLocksCount: "2"},
		})
		if err != nil {
			panic(err)
		}
		reg.PutManifest(lfs.TagLFSLocks, manifest)
	})

	if _, err := client.AcquireLFSLock(ctx, "y.bin", "yves"); err != nil {
		t.Fatalf("AcquireLFSLock under contention: %v", err)
	}
	if !fired.Load() {
		t.Fatal("test setup: the concurrent write never happened")
	}
	reg.Observe(nil)

	locks, err := client.FetchLFSLocks(ctx)
	if err != nil {
		t.Fatalf("FetchLFSLocks: %v", err)
	}
	paths := map[string]bool{}
	for _, l := range locks {
		paths[l.Path] = true
	}
	for _, want := range []string{"a.bin", "z.bin", "y.bin"} {
		if !paths[want] {
			t.Errorf("%s is missing from the lock list %v; the concurrent writer was clobbered", want, paths)
		}
	}
}
