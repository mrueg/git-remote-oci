package oci_test

import (
	"context"
	"net/url"
	"testing"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

func TestLFSLockingAPI(t *testing.T) {
	mock := registrytest.New()
	server := mock.Serve(t)

	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("Failed to parse registry server URL: %v", err)
	}

	client, err := oci.NewClient(u.Host+"/test-repo", true)
	if err != nil {
		t.Fatalf("Failed to create OCI client: %v", err)
	}

	ctx := context.Background()

	// 1. Initially no LFS locks
	locks, err := client.FetchLFSLocks(ctx)
	if err != nil {
		t.Fatalf("FetchLFSLocks failed: %v", err)
	}
	if len(locks) != 0 {
		t.Errorf("Expected 0 initial LFS locks, got %d", len(locks))
	}

	// 2. Acquire LFS lock for "art/hero.psd"
	lock1, err := client.AcquireLFSLock(ctx, "art/hero.psd", "artist1")
	if err != nil {
		t.Fatalf("AcquireLFSLock failed: %v", err)
	}
	if lock1.Path != "art/hero.psd" || lock1.Owner.Name != "artist1" || lock1.ID == "" {
		t.Fatalf("Unexpected lock record: %+v", lock1)
	}

	// 3. Acquire another LFS lock for "textures/skin.png"
	lock2, err := client.AcquireLFSLock(ctx, "textures/skin.png", "artist2")
	if err != nil {
		t.Fatalf("AcquireLFSLock 2 failed: %v", err)
	}
	if lock2.Path != "textures/skin.png" {
		t.Fatalf("Unexpected lock 2 record: %+v", lock2)
	}

	// 4. Verify FetchLFSLocks returns 2 active locks
	locks, err = client.FetchLFSLocks(ctx)
	if err != nil {
		t.Fatalf("FetchLFSLocks after acquiring failed: %v", err)
	}
	if len(locks) != 2 {
		t.Fatalf("Expected 2 active LFS locks, got %d", len(locks))
	}

	// 5. Acquiring lock on already locked path should fail
	_, err = client.AcquireLFSLock(ctx, "art/hero.psd", "artist3")
	if err == nil {
		t.Errorf("Expected error acquiring already locked path, got nil")
	}

	// 6. Release lock1
	released, err := client.ReleaseLFSLock(ctx, lock1.ID, false, "artist1")
	if err != nil {
		t.Fatalf("ReleaseLFSLock failed: %v", err)
	}
	if released.ID != lock1.ID {
		t.Errorf("Expected released lock ID %s, got %s", lock1.ID, released.ID)
	}

	// 7. Verify only 1 lock remains
	locks, err = client.FetchLFSLocks(ctx)
	if err != nil {
		t.Fatalf("FetchLFSLocks after release failed: %v", err)
	}
	if len(locks) != 1 || locks[0].Path != "textures/skin.png" {
		t.Fatalf("Expected 1 remaining lock for textures/skin.png, got: %+v", locks)
	}
}
