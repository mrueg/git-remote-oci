package oci_test

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

func TestRefLocking(t *testing.T) {
	mock := registrytest.New()
	server := mock.Serve(t)

	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("Failed to parse registry server URL: %v", err)
	}

	repoURL := u.Host + "/test-repo"
	ctx := context.Background()

	client, err := oci.NewClient(repoURL, true)
	if err != nil {
		t.Fatalf("Failed to create OCI client: %v", err)
	}

	refName := "refs/heads/main"

	// 1. Initially unlocked
	locked, info, err := client.IsLocked(ctx, refName)
	if err != nil {
		t.Fatalf("IsLocked failed: %v", err)
	}
	if locked {
		t.Errorf("Expected initial state to be unlocked, got locked by %v", info)
	}

	// 2. Acquire lock
	lockInfo, err := client.AcquireRefLock(ctx, refName, 10*time.Minute)
	if err != nil {
		t.Fatalf("AcquireRefLock failed: %v", err)
	}
	if lockInfo == nil || lockInfo.Ref != refName {
		t.Fatalf("Unexpected lockInfo: %+v", lockInfo)
	}

	// 3. Verify state is locked
	locked, info, err = client.IsLocked(ctx, refName)
	if err != nil {
		t.Fatalf("IsLocked failed after acquiring: %v", err)
	}
	if !locked || info == nil {
		t.Fatalf("Expected locked status after acquiring lock")
	}

	// 4. Acquiring lock again should fail due to active lock
	_, err = client.AcquireRefLock(ctx, refName, 10*time.Minute)
	if err == nil {
		t.Errorf("Expected error when acquiring an already locked ref, got nil")
	}

	// 5. Release lock
	if err := client.ReleaseRefLock(ctx, refName); err != nil {
		t.Fatalf("ReleaseRefLock failed: %v", err)
	}

	// 6. Verify state is unlocked again
	locked, _, err = client.IsLocked(ctx, refName)
	if err != nil {
		t.Fatalf("IsLocked failed after release: %v", err)
	}
	if locked {
		t.Errorf("Expected unlocked state after release, got locked")
	}
}

// TestSecondInProcessAcquireIsRefused pins the ownership bookkeeping.
//
// heldLocks records one lock id per ref. Two goroutines in this process both
// acquiring the same ref would leave the second's id there, and the first's
// release would then pass the ownership check and release the second's lock.
// The registry grants one holder per ref, so the second acquisition was never
// legitimate; it is refused rather than allowed to overwrite.
func TestSecondInProcessAcquireIsRefused(t *testing.T) {
	mock := registrytest.New()
	server := mock.Serve(t)

	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	client, err := oci.NewClient(u.Host+"/test-repo", true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx := context.Background()

	if _, err := client.AcquireRefLock(ctx, "refs/heads/main", time.Minute); err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	_, err = client.AcquireRefLock(ctx, "refs/heads/main", time.Minute)
	if err == nil {
		t.Fatal("a second in-process acquire of the same ref was allowed")
	}
	if !strings.Contains(err.Error(), "already holds") {
		t.Errorf("unexpected error: %v", err)
	}

	// A different ref is unaffected: the reservation is per ref, not global.
	if _, err := client.AcquireRefLock(ctx, "refs/heads/other", time.Minute); err != nil {
		t.Errorf("acquiring a different ref should still work: %v", err)
	}

	// The reservation must not outlive the lock.
	if err := client.ReleaseRefLock(ctx, "refs/heads/main"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := client.AcquireRefLock(ctx, "refs/heads/main", time.Minute); err != nil {
		t.Errorf("re-acquiring after release failed: %v", err)
	}
}

// TestFailedAcquireDoesNotWedgeTheRef: a failed attempt must give the
// reservation back, or one transient registry error would make the ref
// permanently unlockable by this process.
func TestFailedAcquireDoesNotWedgeTheRef(t *testing.T) {
	mock := registrytest.New()
	server := mock.Serve(t)

	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	client, err := oci.NewClient(u.Host+"/test-repo", true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx := context.Background()

	// Someone else holds it, so this acquisition fails.
	other, err := oci.NewClient(u.Host+"/test-repo", true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := other.AcquireRefLock(ctx, "refs/heads/main", time.Minute); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	if _, err := client.AcquireRefLock(ctx, "refs/heads/main", time.Minute); err == nil {
		t.Fatal("acquiring a lock held by another client should fail")
	}

	// Once it is free, this client must be able to take it.
	if err := other.ReleaseRefLock(ctx, "refs/heads/main"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := client.AcquireRefLock(ctx, "refs/heads/main", time.Minute); err != nil {
		t.Errorf("the failed attempt wedged the ref: %v", err)
	}
}
