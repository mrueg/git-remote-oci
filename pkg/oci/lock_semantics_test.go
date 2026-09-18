package oci_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	opencontainers "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// lockManifestRegistry serves a single lock manifest with the given
// annotations, as another client would have left it, and nothing else.
func lockManifestRegistry(t *testing.T, tag string, annotations map[string]string) (*registrytest.Registry, *httptest.Server) {
	t.Helper()

	manifest := ocispec.Manifest{
		Versioned:   specs.Versioned{SchemaVersion: 2},
		MediaType:   ocispec.MediaTypeImageManifest,
		Config:      ocispec.Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: opencontainers.FromBytes([]byte("{}")), Size: 2},
		Annotations: annotations,
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}

	reg := registrytest.New()
	ts := reg.Serve(t)
	reg.PutManifest(tag, body)
	return reg, ts
}

// readOnly makes the registry refuse every write, so a test that expects an
// operation to write nothing fails on the write itself rather than needing to
// know which write to look for.
func readOnly(reg *registrytest.Registry) {
	reg.Intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			return false
		}
		http.Error(w, "this registry is read-only", http.StatusInternalServerError)
		return true
	})
}

// TestIsLockedHonoursExpiry is the regression test for locks that never expire.
// The TTL was parsed but never compared against the clock, so a client that
// crashed mid-push wedged the ref permanently.
func TestIsLockedHonoursExpiry(t *testing.T) {
	tests := []struct {
		name       string
		expiresAt  time.Time
		wantLocked bool
	}{
		{"unexpired lock is held", time.Now().UTC().Add(5 * time.Minute), true},
		{"expired lock is not held", time.Now().UTC().Add(-1 * time.Minute), false},
		{"lock expiring right now is not held", time.Now().UTC().Add(-time.Millisecond), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ts := lockManifestRegistry(t, oci.LockTag("refs/heads/main"), map[string]string{
				oci.AnnotationLockRef:       "refs/heads/main",
				oci.AnnotationLockOwner:     "someone@elsewhere",
				oci.AnnotationLockExpiresAt: tt.expiresAt.Format(time.RFC3339),
			})

			locked, _, err := registrytest.Client(t, ts).IsLocked(context.Background(), "refs/heads/main")
			if err != nil {
				t.Fatalf("IsLocked: %v", err)
			}
			if locked != tt.wantLocked {
				t.Errorf("IsLocked = %v, want %v", locked, tt.wantLocked)
			}
		})
	}
}

// TestIsLockedFailsClosed verifies that an unreachable or erroring registry is
// reported as an error rather than as "not locked". Failing open would let
// concurrent pushers walk straight past the check.
func TestIsLockedFailsClosed(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprintf("status %d", status), func(t *testing.T) {
			reg := registrytest.New()
			ts := reg.Serve(t)
			reg.Intercept(func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path == "/v2/" {
					return false
				}
				w.WriteHeader(status)
				return true
			})

			locked, _, err := registrytest.Client(t, ts).IsLocked(context.Background(), "refs/heads/main")
			if err == nil {
				t.Fatalf("IsLocked returned locked=%v and no error for a %d registry", locked, status)
			}
			if locked {
				t.Error("IsLocked should not claim a lock is held when it could not tell")
			}
		})
	}
}

// TestIsLockedTreatsMissingManifestAsUnlocked keeps the common case working: no
// lock manifest simply means the ref is not locked.
func TestIsLockedTreatsMissingManifestAsUnlocked(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	locked, info, err := registrytest.Client(t, ts).IsLocked(context.Background(), "refs/heads/main")
	if err != nil {
		t.Fatalf("IsLocked: %v", err)
	}
	if locked || info != nil {
		t.Errorf("expected an absent lock manifest to read as unlocked, got locked=%v info=%+v", locked, info)
	}
}

// TestReleaseRefLockRefusesForeignLock verifies that a client cannot release a
// lock it does not hold, while BreakRefLock deliberately can.
func TestReleaseRefLockRefusesForeignLock(t *testing.T) {
	_, ts := lockManifestRegistry(t, oci.LockTag("refs/heads/main"), map[string]string{
		oci.AnnotationLockRef:       "refs/heads/main",
		oci.AnnotationLockOwner:     "someone@elsewhere",
		oci.AnnotationLockID:        "1234-someone@elsewhere",
		oci.AnnotationLockExpiresAt: time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339),
	})

	client := registrytest.Client(t, ts)
	err := client.ReleaseRefLock(context.Background(), "refs/heads/main")
	if err == nil {
		t.Fatal("ReleaseRefLock released a lock held by another client")
	}
	if !strings.Contains(err.Error(), "does not hold it") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestReleaseRefLockOnUnlockedRefIsNoOp: releasing something already released
// or expired should not be an error, and should not write anything either.
//
// The registry here is read-only, so a release that tried to write a tombstone
// would fail on the write. It used to: the expired lock read as unlocked, the
// ownership check passed, and the tombstone went out regardless -- over
// whatever lock another client had taken in the meantime.
func TestReleaseRefLockOnUnlockedRefIsNoOp(t *testing.T) {
	reg, ts := lockManifestRegistry(t, oci.LockTag("refs/heads/main"), map[string]string{
		oci.AnnotationLockRef:       "refs/heads/main",
		oci.AnnotationLockOwner:     "someone@elsewhere",
		oci.AnnotationLockExpiresAt: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
	})
	readOnly(reg)

	if err := registrytest.Client(t, ts).ReleaseRefLock(context.Background(), "refs/heads/main"); err != nil {
		t.Fatalf("releasing an expired lock should be a no-op, got: %v", err)
	}
}

// TestAcquireRefLockWithRetryRespectsContext verifies the wait loop is
// cancellable and does not spin on a flat interval.
func TestAcquireRefLockWithRetryRespectsContext(t *testing.T) {
	_, ts := lockManifestRegistry(t, oci.LockTag("refs/heads/main"), map[string]string{
		oci.AnnotationLockRef:       "refs/heads/main",
		oci.AnnotationLockOwner:     "someone@elsewhere",
		oci.AnnotationLockExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := registrytest.Client(t, ts).AcquireRefLockWithRetry(ctx, "refs/heads/main", time.Minute, time.Hour)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("AcquireRefLockWithRetry should not have acquired a held lock")
	}
	if elapsed > 5*time.Second {
		t.Errorf("cancellation took %s; the wait loop is not honouring the context", elapsed)
	}
}

// TestAcquireRefLockReportsALostRace pins the read-back after the write.
//
// Acquisition is check-then-write against a registry with no compare-and-swap,
// so two clients can both find the ref free and both write their lock; the
// second write wins and the first client, told nothing, would push under a
// lock it does not hold. The read-back is the only thing that catches it. Here
// another owner's lock lands on the tag the moment this client's write is
// served -- between the write and the confirmation -- and the acquisition
// must report the loss rather than success.
func TestAcquireRefLockReportsALostRace(t *testing.T) {
	const ref = "refs/heads/main"
	tag := oci.LockTag(ref)

	reg := registrytest.New()
	ts := reg.Serve(t)

	theirs, err := json.Marshal(ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    ocispec.Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: opencontainers.FromBytes([]byte("{}")), Size: 2},
		Annotations: map[string]string{
			oci.AnnotationLockRef:       ref,
			oci.AnnotationLockOwner:     "someone@elsewhere",
			oci.AnnotationLockID:        "1234-someone@elsewhere",
			oci.AnnotationLockExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
		},
	})
	if err != nil {
		t.Fatalf("marshal the competing lock: %v", err)
	}

	// The registry takes this client's write and then, before it can be read
	// back, the other client's write replaces it: the interleaving a real
	// registry can produce and a fake has to be made to. The write is
	// acknowledged as a registry would acknowledge it, digest and all.
	var lost atomic.Bool
	reg.Intercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPut || !strings.HasSuffix(r.URL.Path, "/manifests/"+tag) {
			return false
		}
		mine, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return true
		}
		reg.PutManifest(tag, theirs)
		lost.Store(true)
		w.Header().Set("Docker-Content-Digest", registrytest.Digest(mine))
		w.WriteHeader(http.StatusCreated)
		return true
	})

	client := registrytest.Client(t, ts)
	info, err := client.AcquireRefLock(context.Background(), ref, time.Minute)
	if !lost.Load() {
		t.Fatal("test setup: the competing write never happened, so nothing was raced")
	}
	if err == nil {
		t.Fatalf("AcquireRefLock reported success with someone else's lock on the tag: %+v", info)
	}
	if !errors.Is(err, oci.ErrRefLocked) {
		t.Errorf("a lost race is contention and should wrap ErrRefLocked, got: %v", err)
	}
	if !strings.Contains(err.Error(), "lost race") {
		t.Errorf("the error should say the race was lost, got: %v", err)
	}

	// The loser must not believe it holds anything: a release now would be a
	// release of the other client's lock, and a re-acquire must be allowed to
	// try again rather than be refused as a double acquisition.
	if releaseErr := client.ReleaseRefLock(context.Background(), ref); releaseErr == nil {
		t.Error("the loser released a lock it never acquired")
	}
	reg.Intercept(nil)
	if _, err := client.AcquireRefLock(context.Background(), ref, time.Minute); !errors.Is(err, oci.ErrRefLocked) {
		t.Errorf("a second attempt while the winner still holds the lock should report contention, got: %v", err)
	}
}
