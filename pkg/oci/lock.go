package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	opencontainers "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	// AnnotationLockRef is the OCI annotation storing the locked git ref name.
	AnnotationLockRef = "org.git-remote-oci.lock.ref"
	// AnnotationLockOwner is the OCI annotation storing the lock owner information.
	AnnotationLockOwner = "org.git-remote-oci.lock.owner"
	// AnnotationLockExpiresAt is the OCI annotation storing the RFC3339 expiration timestamp.
	AnnotationLockExpiresAt = "org.git-remote-oci.lock.expires_at"
	// AnnotationLockID is the OCI annotation storing the unique id of the lock
	// holder, used to check ownership on release.
	AnnotationLockID = "org.git-remote-oci.lock.id"

	// DefaultLockTTL is the default duration for reference locks (5 minutes).
	DefaultLockTTL = 5 * time.Minute
)

// ErrRefLocked reports that a ref lock is currently held by someone else, as
// distinct from being unable to find out.
//
// Callers need the difference: a held lock is another client working normally
// and is worth reporting as such, while a failed check leaves the state unknown
// and must not be presented as contention.
var ErrRefLocked = errors.New("reference is locked")

// LockInfo represents distributed reference lock metadata.
type LockInfo struct {
	Ref       string    `json:"ref"`
	Owner     string    `json:"owner"`
	LockID    string    `json:"lock_id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// LockTagPrefix is the reserved namespace ref locks live in.
//
// It begins with `_`, which §2 of FORMAT.md reserves and which the ref-name
// encoding provably never produces: a leading underscore in a branch name is
// escaped to `__`. That is the whole reason for the prefix.
//
// It used to be "lock-", which is not reserved and which a branch can be named.
// `refs/heads/lock-main` encodes to the tag `lock-main`, and so did the lock on
// `refs/heads/main` -- so pushing `main` overwrote that branch's ref manifest
// with a lock manifest, and gc, classifying the tag by its prefix, would prune
// the branch as a released lock. Two ways to lose a branch for having named it
// something ordinary.
const LockTagPrefix = "_lock_"

// LockTag returns the OCI tag name for a given ref lock.
//
// It uses the same encoding as ref manifests -- injective, except that the
// truncated form for over-long refs is collision-resistant rather than
// guaranteed distinct (§3.1) -- so two different refs do not end up sharing one
// lock, under a prefix no ref can reach.
//
// The encoding is given a budget of maxTagLength less the prefix, so that the
// whole tag fits the OCI limit (FORMAT.md §9). EncodeRefTag alone may use all
// 128 bytes, and prefixing that produced a tag the registry rejected: the lock
// could not be taken, so the ref could not be pushed at all. A ref whose
// encoding fits the smaller budget gets exactly the tag it always did; one that
// does not is truncated by the §3.1 scheme at the smaller budget, which keeps
// distinct refs on distinct locks.
func LockTag(refName string) string {
	encoded := encodeRefTagWithin(refName, maxTagLength-len(LockTagPrefix))
	if encoded == "" {
		encoded = "default"
	}
	return LockTagPrefix + encoded
}

// ownerReleased is the sentinel owner written by ReleaseRefLock. A released
// lock manifest is left in place rather than deleted, because several registries
// restrict or disable manifest deletion.
const ownerReleased = "released"

// IsLocked reports whether refName currently holds an active, unexpired lock.
//
// It fails *closed*: any error other than "no lock manifest exists" is returned
// to the caller rather than being reported as "not locked". Reporting an
// unreachable registry as unlocked would let concurrent pushers straight past
// the check the lock exists to enforce.
func (c *Client) IsLocked(ctx context.Context, refName string) (bool, *LockInfo, error) {
	return c.lockStateByTag(ctx, LockTag(refName))
}

// lockStateByTag reads a lock manifest by its tag. Callers that already hold a
// tag (garbage collection enumerates them) must use this rather than
// round-tripping through IsLocked, because the text after the prefix is the
// *encoded* ref, and re-encoding it would name a different tag.
func (c *Client) lockStateByTag(ctx context.Context, tag string) (bool, *LockInfo, error) {
	_, data, err := c.fetchManifestBytes(ctx, tag, "a lock manifest")
	if err != nil {
		if IsNotFound(err) {
			return false, nil, nil
		}
		return false, nil, fmt.Errorf("failed to read lock %s: %w", tag, err)
	}

	var manifest ocispec.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return false, nil, fmt.Errorf("failed to parse lock manifest %s: %w", tag, err)
	}

	if manifest.Annotations[AnnotationLockOwner] == ownerReleased {
		return false, nil, nil
	}

	expStr := manifest.Annotations[AnnotationLockExpiresAt]
	if expStr == "" {
		// A lock with no expiry cannot be reasoned about or reclaimed; treat
		// it as malformed rather than as an eternal lock.
		return false, nil, fmt.Errorf("lock manifest %s has no %s annotation", tag, AnnotationLockExpiresAt)
	}
	expiresAt, err := time.Parse(time.RFC3339, expStr)
	if err != nil {
		return false, nil, fmt.Errorf("lock manifest %s has an unparseable expiry %q: %w", tag, expStr, err)
	}

	// An expired lock is not a lock. Without this the TTL is decorative and a
	// client that crashed mid-push wedges the ref permanently.
	if !time.Now().UTC().Before(expiresAt) {
		return false, nil, nil
	}

	info := &LockInfo{
		Ref:       manifest.Annotations[AnnotationLockRef],
		Owner:     manifest.Annotations[AnnotationLockOwner],
		LockID:    manifest.Annotations[AnnotationLockID],
		ExpiresAt: expiresAt,
	}
	return true, info, nil
}

// lockReserved marks a ref whose acquisition is in flight but has not yet
// published a lock id. It is distinct from any real id, which is a timestamp
// and an owner.
const lockReserved = "\x00reserved"

// AcquireRefLock acquires an exclusive lock on refName for the specified duration (or DefaultLockTTL).
//
// Only one goroutine in this process may hold a given ref's lock at a time, and
// a second attempt is refused rather than queued. The registry grants exactly
// one holder per ref, so two in-process acquisitions of the same ref cannot
// both be legitimate — and heldLocks records one lock id per ref, so the second
// acquisition would overwrite the first's id and the first's release would then
// happily release the second's lock. Refusing here is what makes the ownership
// check below meaningful.
func (c *Client) AcquireRefLock(ctx context.Context, refName string, ttl time.Duration) (*LockInfo, error) {
	if ttl <= 0 {
		ttl = DefaultLockTTL
	}

	// Reserve in-process ownership before touching the registry, and give it
	// back on every failure path so a failed attempt does not wedge the ref.
	if _, busy := c.heldLocks.LoadOrStore(refName, lockReserved); busy {
		return nil, fmt.Errorf("this client already holds a lock on %s", refName)
	}
	acquired := false
	defer func() {
		if !acquired {
			c.heldLocks.Delete(refName)
		}
	}()

	locked, lockInfo, err := c.IsLocked(ctx, refName)
	if err != nil {
		return nil, fmt.Errorf("failed to check lock status for %s: %w", refName, err)
	}
	if locked && lockInfo != nil {
		return nil, fmt.Errorf("%w: ref %s is held by %s until %s", ErrRefLocked, refName, lockInfo.Owner, lockInfo.ExpiresAt.Format(time.RFC3339))
	}

	owner := defaultLockOwner()

	now := time.Now().UTC()
	expiresAt := now.Add(ttl)
	lockID := fmt.Sprintf("%d-%s", now.UnixNano(), owner)

	lockInfo = &LockInfo{
		Ref:       refName,
		Owner:     owner,
		LockID:    lockID,
		CreatedAt: now,
		ExpiresAt: expiresAt,
	}

	lockJSON, err := json.Marshal(lockInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal lock info: %w", err)
	}

	configDesc := ocispec.Descriptor{
		MediaType: MediaTypeGitConfig,
		Digest:    opencontainers.FromBytes(lockJSON),
		Size:      int64(len(lockJSON)),
	}
	// Push the config blob before the manifest that references it: a manifest
	// pointing at a missing blob is rejected by registries that validate.
	if err := c.pushBlobOnce(ctx, configDesc, lockJSON); err != nil {
		return nil, fmt.Errorf("failed to push lock config blob: %w", err)
	}
	if err := c.pushEmptyBlob(ctx); err != nil {
		return nil, err
	}

	lockManifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    emptyLayers(),
		Annotations: map[string]string{
			AnnotationLockRef:       refName,
			AnnotationLockOwner:     owner,
			AnnotationLockExpiresAt: expiresAt.Format(time.RFC3339),
			AnnotationLockID:        lockID,
		},
	}

	manifestData, err := json.Marshal(lockManifest)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal lock manifest: %w", err)
	}

	manifestDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    opencontainers.FromBytes(manifestData),
		Size:      int64(len(manifestData)),
	}

	tag := LockTag(refName)
	if err := c.Repo.PushReference(ctx, manifestDesc, bytes.NewReader(manifestData), tag); err != nil {
		return nil, fmt.Errorf("failed to push lock reference %s: %w", tag, err)
	}

	// Read the lock back. Acquisition is check-then-write rather than an atomic
	// compare-and-swap, so a concurrent acquirer may have overwritten the tag
	// between our check and our write. Confirming that the published lock is
	// still ours narrows that window; it does not eliminate it. See the
	// Limitations section of the README.
	published, publishedInfo, err := c.IsLocked(ctx, refName)
	if err != nil {
		return nil, fmt.Errorf("failed to confirm lock on %s: %w", refName, err)
	}
	if !published || publishedInfo == nil || publishedInfo.LockID != lockID {
		// Wraps ErrRefLocked: losing the race is contention, which is exactly
		// what AcquireRefLockWithRetry should wait out.
		return nil, fmt.Errorf("%w: lost race acquiring lock on %s, it is now held by another client", ErrRefLocked, refName)
	}

	c.heldLocks.Store(refName, lockID)
	acquired = true
	// Deliberately not cached: a lock is read fresh every time, both by
	// lockStateByTag and through the transport's no-cache marking, so a cached
	// copy would only ever be stale.
	return lockInfo, nil
}

// ReleaseRefLock releases a ref lock previously acquired by this client.
//
// Errors are returned rather than swallowed: a release that silently fails
// leaves the ref locked until its TTL expires, and callers deserve the chance to
// report or retry that.
func (c *Client) ReleaseRefLock(ctx context.Context, refName string) error {
	return c.releaseRefLock(ctx, refName, false)
}

// BreakRefLock forcibly releases a ref lock regardless of who holds it. It is
// the escape hatch for a lock stranded by a crashed client whose TTL has not
// yet elapsed.
func (c *Client) BreakRefLock(ctx context.Context, refName string) error {
	return c.releaseRefLock(ctx, refName, true)
}

func (c *Client) releaseRefLock(ctx context.Context, refName string, force bool) error {
	tag := LockTag(refName)

	if !force {
		live, err := c.verifyLockOwnership(ctx, refName)
		if err != nil {
			return err
		}
		if !live {
			// Nothing of ours is published: the lock is absent, was released
			// already, or expired. Writing the tombstone anyway used to be the
			// bug here — this client's lock had expired, another client had
			// legitimately taken the ref, and the tombstone overwrote *their*
			// lock. With nothing to release there is nothing to write; only
			// the in-process bookkeeping needs clearing.
			c.heldLocks.Delete(refName)
			c.InvalidateManifestCache(tag)
			return nil
		}
	}

	now := time.Now().UTC().Add(-1 * time.Minute)
	lockInfo := &LockInfo{
		Ref:       refName,
		Owner:     ownerReleased,
		LockID:    ownerReleased,
		CreatedAt: now,
		ExpiresAt: now,
	}
	lockJSON, err := json.Marshal(lockInfo)
	if err != nil {
		return fmt.Errorf("failed to marshal released lock info: %w", err)
	}

	configDesc := ocispec.Descriptor{
		MediaType: MediaTypeGitConfig,
		Digest:    opencontainers.FromBytes(lockJSON),
		Size:      int64(len(lockJSON)),
	}
	if err := c.pushBlobOnce(ctx, configDesc, lockJSON); err != nil {
		return fmt.Errorf("failed to push released lock config blob: %w", err)
	}
	if err := c.pushEmptyBlob(ctx); err != nil {
		return err
	}

	lockManifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    emptyLayers(),
		Annotations: map[string]string{
			AnnotationLockRef:       refName,
			AnnotationLockOwner:     ownerReleased,
			AnnotationLockExpiresAt: now.Format(time.RFC3339),
		},
	}
	manifestData, err := json.Marshal(lockManifest)
	if err != nil {
		return fmt.Errorf("failed to marshal released lock manifest: %w", err)
	}
	manifestDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    opencontainers.FromBytes(manifestData),
		Size:      int64(len(manifestData)),
	}

	if err := c.Repo.PushReference(ctx, manifestDesc, bytes.NewReader(manifestData), tag); err != nil {
		return fmt.Errorf("failed to release lock %s: %w", tag, err)
	}
	c.heldLocks.Delete(refName)
	c.InvalidateManifestCache(tag)
	return nil
}

// verifyLockOwnership reports whether the lock currently published for refName
// is a live one that this client holds.
//
// live is false, with no error, when there is no live lock at all — absent,
// released or expired — which a release treats as nothing to do. An error means
// there *is* a live lock and it is not ours, or the state could not be read.
func (c *Client) verifyLockOwnership(ctx context.Context, refName string) (live bool, err error) {
	locked, info, err := c.IsLocked(ctx, refName)
	if err != nil {
		return false, fmt.Errorf("failed to verify lock ownership for %s: %w", refName, err)
	}
	if !locked || info == nil {
		return false, nil
	}

	held, ok := c.heldLocks.Load(refName)
	if !ok || held == lockReserved {
		// lockReserved means an acquisition is still in flight, so no id has
		// been published for this client yet.
		return false, fmt.Errorf("refusing to release lock on %s held by %s: this client does not hold it", refName, info.Owner)
	}
	// A published lock with no id cannot be shown to be ours, so it is treated
	// as someone else's: the id is the only evidence there is, and "no
	// evidence" used to pass, which let any client release any id-less lock.
	if heldID, _ := held.(string); info.LockID == "" || heldID != info.LockID {
		// The ref moved on without us — our lock expired and another client
		// took it. Forgetting our stale id is what lets this client acquire
		// the ref again later: AcquireRefLock refuses while an entry exists.
		c.heldLocks.Delete(refName)
		return false, fmt.Errorf("refusing to release lock on %s: it was taken over by %s", refName, info.Owner)
	}
	return true, nil
}

// AcquireRefLockWithRetry attempts to acquire a lock on refName, retrying until
// maxWait elapses or ctx is cancelled.
//
// Each attempt costs a manifest read plus a blob and manifest write, so the
// backoff grows rather than spinning: a flat 50ms poll over a 45s budget was
// close to a thousand round trips.
func (c *Client) AcquireRefLockWithRetry(ctx context.Context, refName string, ttl time.Duration, maxWait time.Duration) (*LockInfo, error) {
	const (
		initialBackoff = 50 * time.Millisecond
		maxBackoff     = 2 * time.Second
	)

	deadline := time.Now().Add(maxWait)
	backoff := initialBackoff

	for {
		lock, err := c.AcquireRefLock(ctx, refName, ttl)
		if err == nil {
			return lock, nil
		}
		lastErr := err

		// Only contention is worth waiting out. This used to retry on any
		// error, so an unreachable registry, a rejected credential or a
		// programming error all spent the full budget backing off before
		// reporting something that was never going to change — 45 seconds to
		// be told the connection was refused.
		if !errors.Is(err, ErrRefLocked) {
			return nil, err
		}

		if ctx.Err() != nil {
			return nil, fmt.Errorf("cancelled while waiting for lock on %s: %w", refName, ctx.Err())
		}
		if !time.Now().Add(backoff).Before(deadline) {
			return nil, fmt.Errorf("timed out after %s waiting for lock on %s: %w", maxWait, refName, lastErr)
		}

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("cancelled while waiting for lock on %s: %w", refName, ctx.Err())
		case <-timer.C:
		}

		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}
