package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/mrueg/git-remote-oci/pkg/lfs"
	opencontainers "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// lfsLocksLockRef is the pseudo-ref whose ref lock serialises updates to the
// _lfs_locks manifest. Acquiring and releasing an LFS lock is a
// read-modify-write of one blob, so concurrent callers must take turns.
const lfsLocksLockRef = "_lfs_locks_index_lock"

// lfsLocksIndexWait bounds how long a client waits for the LFS lock index.
// How long it may hold it is Client.LFSLocksIndexTTL.
const lfsLocksIndexWait = 45 * time.Second

// AnnotationLFSLocksCount is set on the _lfs_locks manifest to the number of
// locks its config blob holds, so a tool can see that without fetching the
// blob (FORMAT.md §9.1).
const AnnotationLFSLocksCount = "org.git.lfs.locks.count"

// lfsLocksMaxAttempts bounds the optimistic-concurrency retry on the lock
// list, for the same reason refsIndexMaxAttempts bounds it on _refs: losing
// three times running is worth reporting rather than spinning on.
const lfsLocksMaxAttempts = 3

// withLFSLocksIndexLock takes the ref lock that serialises updates to the
// _lfs_locks manifest and returns a function that releases it.
//
// The release reports failure rather than warning: a lock that could not be
// given back stalls every other LFS lock operation until its TTL runs out,
// and the caller is the one place that can say so alongside its own result.
func (c *Client) withLFSLocksIndexLock(ctx context.Context) (func() error, error) {
	if _, err := c.AcquireRefLockWithRetry(ctx, lfsLocksLockRef, c.LFSLocksIndexTTL, lfsLocksIndexWait); err != nil {
		return nil, fmt.Errorf("failed to acquire the LFS lock index: %w", err)
	}
	return func() error {
		// Use a context detached from ctx: if the caller's context has already
		// been cancelled, the release still needs to reach the registry, or the
		// index stays locked until its TTL expires.
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := c.ReleaseRefLock(releaseCtx, lfsLocksLockRef); err != nil {
			return fmt.Errorf("failed to release the LFS lock index: %w", err)
		}
		return nil
	}, nil
}

// defaultLockOwner derives a lock owner identity from the environment.
func defaultLockOwner() string {
	user := os.Getenv("USER")
	if user == "" {
		user = "git-user"
	}
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "localhost"
	}
	return fmt.Sprintf("%s@%s", user, hostname)
}

// FetchLFSLocks fetches the list of active Git LFS file locks from the OCI registry.
//
// A missing _lfs_locks manifest means "no locks yet" and yields an empty list.
// Every other failure is returned: callers use this list as the base of a
// read-modify-write, so treating an unreadable list as empty would republish a
// lock list that silently drops everyone else's locks.
func (c *Client) FetchLFSLocks(ctx context.Context) ([]lfs.LFSLock, error) {
	locks, _, err := c.fetchLFSLockState(ctx)
	return locks, err
}

// fetchLFSLockState is FetchLFSLocks together with the digest of the manifest
// the list was read from, which is what a read-modify-write compares against
// before it writes. The digest is "" when there is no manifest yet, and that
// is a real state rather than an error: two clients racing to create the
// first lock list must be able to tell it apart from one having already won.
func (c *Client) fetchLFSLockState(ctx context.Context) ([]lfs.LFSLock, string, error) {
	desc, data, err := c.fetchManifestBytes(ctx, lfs.TagLFSLocks, "the LFS lock manifest")
	if err != nil {
		if IsNotFound(err) {
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("failed to fetch LFS lock manifest: %w", err)
	}

	var manifest ocispec.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, "", fmt.Errorf("failed to parse LFS lock manifest: %w", err)
	}

	configData, err := c.fetchBlobBytes(ctx, manifest.Config, maxMetadataBytes, "the LFS lock list")
	if err != nil {
		if IsNotFound(err) {
			return nil, desc.Digest.String(), nil
		}
		return nil, "", fmt.Errorf("failed to fetch LFS lock list blob: %w", err)
	}

	var lockList lfs.LFSLockList
	if err := json.Unmarshal(configData, &lockList); err != nil {
		return nil, "", fmt.Errorf("failed to parse LFS lock list: %w", err)
	}

	return lockList.Locks, desc.Digest.String(), nil
}

// FindLFSLockByPath returns the lock held on path, or nil if there is none.
//
// Separate from releasing so a caller that only has a path resolves it
// explicitly and can report which lock it is about to release.
func (c *Client) FindLFSLockByPath(ctx context.Context, path string) (*lfs.LFSLock, error) {
	locks, err := c.FetchLFSLocks(ctx)
	if err != nil {
		return nil, err
	}
	for i := range locks {
		if locks[i].Path == path {
			return &locks[i], nil
		}
	}
	return nil, nil
}

// updateLFSLocks is the read-modify-write every change to the lock list goes
// through. mutate is given the published list and returns the list to
// publish; a mutate that returns an error stops the update without writing.
//
// The write is guarded twice, as _refs updates are. The index lock keeps
// well-behaved clients from interleaving at all; but lock acquisition on a
// registry is itself check-then-write, so two clients can both believe they
// hold it. What actually protects the list is comparing the manifest digest
// the base was read from against the digest immediately before the write: a
// change in between means another writer landed, and the merge is redone
// against what they left rather than written over it. The old shape -- read,
// modify, write, under the lock alone -- lost one of two concurrent lock
// acquisitions whenever the lock failed to serialise them.
func (c *Client) updateLFSLocks(ctx context.Context, mutate func(existing []lfs.LFSLock) ([]lfs.LFSLock, error)) (err error) {
	release, err := c.withLFSLocksIndexLock(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if releaseErr := release(); releaseErr != nil {
			err = errors.Join(err, releaseErr)
		}
	}()

	var lastConflict error
	for attempt := 1; attempt <= lfsLocksMaxAttempts; attempt++ {
		existing, baseline, fetchErr := c.fetchLFSLockState(ctx)
		if fetchErr != nil {
			return fmt.Errorf("failed to fetch existing LFS locks: %w", fetchErr)
		}

		updated, mutateErr := mutate(existing)
		if mutateErr != nil {
			return mutateErr
		}

		lockListJSON, marshalErr := json.Marshal(lfs.LFSLockList{Locks: updated})
		if marshalErr != nil {
			return fmt.Errorf("failed to marshal LFS lock list: %w", marshalErr)
		}
		configDesc := ocispec.Descriptor{
			MediaType: MediaTypeGitConfig,
			Digest:    opencontainers.FromBytes(lockListJSON),
			Size:      int64(len(lockListJSON)),
		}
		if pushErr := c.pushBlobOnce(ctx, configDesc, lockListJSON); pushErr != nil {
			return fmt.Errorf("failed to push LFS lock config blob: %w", pushErr)
		}
		if pushErr := c.pushEmptyBlob(ctx); pushErr != nil {
			return pushErr
		}

		manifest := ocispec.Manifest{
			Versioned: specs.Versioned{SchemaVersion: 2},
			MediaType: ocispec.MediaTypeImageManifest,
			Config:    configDesc,
			Layers:    emptyLayers(),
			Annotations: map[string]string{
				AnnotationLFSLocksCount: fmt.Sprintf("%d", len(updated)),
			},
		}
		manifestData, marshalErr := json.Marshal(manifest)
		if marshalErr != nil {
			return fmt.Errorf("failed to marshal LFS lock manifest: %w", marshalErr)
		}
		manifestDesc := ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageManifest,
			Digest:    opencontainers.FromBytes(manifestData),
			Size:      int64(len(manifestData)),
		}

		// Re-check immediately before the write. Anything that moved since
		// the base was read means the list above was computed from stale
		// state and must not be written.
		current, resolveErr := c.lfsLocksDigest(ctx)
		if resolveErr != nil {
			return fmt.Errorf("failed to re-read the LFS lock list state: %w", resolveErr)
		}
		if current != baseline {
			lastConflict = fmt.Errorf("the LFS lock list changed from %s to %s while this update was being prepared",
				shortDigest(baseline), shortDigest(current))
			continue
		}

		if pushErr := c.Repo.PushReference(ctx, manifestDesc, bytes.NewReader(manifestData), lfs.TagLFSLocks); pushErr != nil {
			return fmt.Errorf("failed to push LFS lock manifest %s: %w", lfs.TagLFSLocks, pushErr)
		}
		return nil
	}
	return fmt.Errorf("gave up updating the LFS lock list after %d attempts: %w", lfsLocksMaxAttempts, lastConflict)
}

// lfsLocksDigest returns the digest of the published _lfs_locks manifest, or
// "" when there is none yet.
func (c *Client) lfsLocksDigest(ctx context.Context) (string, error) {
	desc, err := c.Repo.Resolve(ctx, lfs.TagLFSLocks)
	if err != nil {
		if IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return desc.Digest.String(), nil
}

// AcquireLFSLock acquires a Git LFS lock on path for ownerName.
//
// A returned lock with a non-nil error means the lock was recorded but the
// index lock protecting the list could not be given back afterwards; the
// caller should report both.
func (c *Client) AcquireLFSLock(ctx context.Context, path string, ownerName string) (*lfs.LFSLock, error) {
	if path == "" {
		return nil, errors.New("file path cannot be empty")
	}
	if ownerName == "" {
		ownerName = defaultLockOwner()
	}

	var newLock *lfs.LFSLock
	err := c.updateLFSLocks(ctx, func(existing []lfs.LFSLock) ([]lfs.LFSLock, error) {
		// Reset on every attempt: a retry that then finds the path taken
		// must not hand back the lock the earlier attempt never published.
		newLock = nil
		for _, lock := range existing {
			if lock.Path == path {
				return nil, fmt.Errorf("path %q is already locked by %s (lock ID: %s)", path, lock.Owner.Name, lock.ID)
			}
		}

		now := time.Now().UTC()
		created := lfs.LFSLock{
			ID:       lfs.GenerateLockID(path, ownerName, now),
			Path:     path,
			Owner:    lfs.LFSOwner{Name: ownerName},
			LockedAt: now,
		}
		newLock = &created

		// Copy rather than append in place: existing may share a backing
		// array with a caller's slice.
		updated := make([]lfs.LFSLock, 0, len(existing)+1)
		updated = append(updated, existing...)
		return append(updated, created), nil
	})
	if err != nil && newLock == nil {
		return nil, err
	}
	return newLock, err
}

// ReleaseLFSLock releases a Git LFS lock by ID or path.
//
// Unless force is set, ownerName must match the lock holder. An empty ownerName
// is resolved from the environment rather than skipping the check, so an
// unnamed caller cannot unlock someone else's file by omission.
func (c *Client) ReleaseLFSLock(ctx context.Context, lockID string, force bool, ownerName string) (*lfs.LFSLock, error) {
	if !force && ownerName == "" {
		ownerName = defaultLockOwner()
	}

	var targetLock *lfs.LFSLock
	err := c.updateLFSLocks(ctx, func(existing []lfs.LFSLock) ([]lfs.LFSLock, error) {
		targetLock = nil
		var target *lfs.LFSLock
		remaining := make([]lfs.LFSLock, 0, len(existing))
		for _, lock := range existing {
			// Match on the id only. Accepting a path here as well meant a
			// path that happened to equal another lock's id released the
			// wrong record, and there was no way for a caller to say which
			// it meant. Callers that hold a path resolve it to an id first;
			// see FindLFSLockByPath.
			if lock.ID == lockID {
				l := lock
				target = &l
			} else {
				remaining = append(remaining, lock)
			}
		}

		if target == nil {
			return nil, fmt.Errorf("lock ID %q not found", lockID)
		}
		if !force && target.Owner.Name != ownerName {
			return nil, fmt.Errorf("lock %s is owned by %s, cannot unlock without force", lockID, target.Owner.Name)
		}
		targetLock = target
		return remaining, nil
	})
	if err != nil && targetLock == nil {
		return nil, err
	}
	return targetLock, err
}
