package oci_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mrueg/git-remote-oci/pkg/oci"
	opencontainers "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// lockManifestServer serves a single lock-<ref> manifest with the given
// annotations, and 404s everything else.
func lockManifestServer(t *testing.T, tag string, annotations map[string]string) *httptest.Server {
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

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/manifests/"+tag):
			w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
			w.Header().Set("Docker-Content-Digest", opencontainers.FromBytes(body).String())
			_, _ = w.Write(body)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func clientFor(t *testing.T, ts *httptest.Server) *oci.Client {
	t.Helper()
	c, err := oci.NewClient(strings.TrimPrefix(ts.URL, "http://")+"/test-repo", true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
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
			ts := lockManifestServer(t, oci.LockTag("refs/heads/main"), map[string]string{
				oci.AnnotationLockRef:       "refs/heads/main",
				oci.AnnotationLockOwner:     "someone@elsewhere",
				oci.AnnotationLockExpiresAt: tt.expiresAt.Format(time.RFC3339),
			})
			defer ts.Close()

			locked, _, err := clientFor(t, ts).IsLocked(context.Background(), "refs/heads/main")
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
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v2/" {
					w.WriteHeader(http.StatusOK)
					return
				}
				w.WriteHeader(status)
			}))
			defer ts.Close()

			locked, _, err := clientFor(t, ts).IsLocked(context.Background(), "refs/heads/main")
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
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	locked, info, err := clientFor(t, ts).IsLocked(context.Background(), "refs/heads/main")
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
	ts := lockManifestServer(t, oci.LockTag("refs/heads/main"), map[string]string{
		oci.AnnotationLockRef:       "refs/heads/main",
		oci.AnnotationLockOwner:     "someone@elsewhere",
		oci.AnnotationLockID:        "1234-someone@elsewhere",
		oci.AnnotationLockExpiresAt: time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339),
	})
	defer ts.Close()

	client := clientFor(t, ts)
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
// The server here is read-only, so a release that tried to write a tombstone
// would fail on the write. It used to: the expired lock read as unlocked, the
// ownership check passed, and the tombstone went out regardless -- over
// whatever lock another client had taken in the meantime.
func TestReleaseRefLockOnUnlockedRefIsNoOp(t *testing.T) {
	ts := lockManifestServer(t, oci.LockTag("refs/heads/main"), map[string]string{
		oci.AnnotationLockRef:       "refs/heads/main",
		oci.AnnotationLockOwner:     "someone@elsewhere",
		oci.AnnotationLockExpiresAt: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
	})
	defer ts.Close()

	if err := clientFor(t, ts).ReleaseRefLock(context.Background(), "refs/heads/main"); err != nil {
		t.Fatalf("releasing an expired lock should be a no-op, got: %v", err)
	}
}

// TestAcquireRefLockWithRetryRespectsContext verifies the wait loop is
// cancellable and does not spin on a flat interval.
func TestAcquireRefLockWithRetryRespectsContext(t *testing.T) {
	ts := lockManifestServer(t, oci.LockTag("refs/heads/main"), map[string]string{
		oci.AnnotationLockRef:       "refs/heads/main",
		oci.AnnotationLockOwner:     "someone@elsewhere",
		oci.AnnotationLockExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
	})
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := clientFor(t, ts).AcquireRefLockWithRetry(ctx, "refs/heads/main", time.Minute, time.Hour)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("AcquireRefLockWithRetry should not have acquired a held lock")
	}
	if elapsed > 5*time.Second {
		t.Errorf("cancellation took %s; the wait loop is not honouring the context", elapsed)
	}
}
