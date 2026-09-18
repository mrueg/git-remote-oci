package oci

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mrueg/git-remote-oci/pkg/lfs"
)

// ErrDeletionUnsupported reports that the registry refuses manifest deletion
// outright, as opposed to this particular delete having failed.
//
// GHCR, ECR and Docker Hub all restrict it to varying degrees.
var ErrDeletionUnsupported = errors.New("registry does not support manifest deletion")

// TagClass describes what a tag in the repository is for.
type TagClass int

const (
	// TagClassMetadata is one of the fixed index tags (_refs, _index, ...).
	TagClassMetadata TagClass = iota
	// TagClassLock is a ref lock (_lock_<encoded ref>, see LockTag).
	TagClassLock
	// TagClassCommit is a ref-agnostic commit manifest, tagged with the commit id.
	TagClassCommit
	// TagClassRef is a ref manifest.
	TagClassRef
)

func (c TagClass) String() string {
	switch c {
	case TagClassMetadata:
		return "metadata"
	case TagClassLock:
		return "lock"
	case TagClassCommit:
		return "commit"
	case TagClassRef:
		return "ref"
	}
	return "unknown"
}

// ClassifyTag reports what a tag is for, so garbage collection can decide
// whether it is still needed.
func ClassifyTag(tag string) TagClass {
	switch {
	case tag == TagRefIndex || tag == TagOCIIndex || tag == lfs.TagLFSLocks:
		return TagClassMetadata
	case strings.HasPrefix(tag, LockTagPrefix):
		return TagClassLock
	case isObjectID(tag):
		return TagClassCommit
	default:
		return TagClassRef
	}
}

// ListAllTags returns every tag in the repository.
func (c *Client) ListAllTags(ctx context.Context) ([]string, error) {
	var tags []string
	err := c.Repo.Tags(ctx, "", func(page []string) error {
		tags = append(tags, page...)
		return nil
	})
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, c.explainAuth(fmt.Errorf("failed to enumerate tags: %w", err))
	}
	return tags, nil
}

// DeleteTag removes a single tag from the registry.
//
// A missing tag is not an error: garbage collection is idempotent, and a
// concurrent collector may have removed it already. A registry that refuses
// deletion outright returns ErrDeletionUnsupported, which callers that can make
// progress without deleting should treat as a skip.
func (c *Client) DeleteTag(ctx context.Context, tag string) error {
	c.InvalidateManifestCache(tag)
	desc, err := c.Repo.Resolve(ctx, tag)
	if err != nil {
		if IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to resolve tag %s: %w", tag, err)
	}
	if err := c.Repo.Delete(ctx, desc); err != nil {
		if IsNotFound(err) {
			return nil
		}
		if isDeletionUnsupported(err) {
			// The registry does not allow manifest deletion at all. That is a
			// property of the registry, not a failure of this call, and the
			// caller may well want to carry on: for compaction the valuable
			// half is consolidating the packfiles, and reclaiming tags is
			// cleanup.
			return fmt.Errorf("%w: %s", ErrDeletionUnsupported, tag)
		}
		return fmt.Errorf("failed to delete tag %s: %w", tag, err)
	}
	c.InvalidateManifestCache(tag)
	return nil
}

// IsLockReclaimable reports whether a lock tag is safe to remove: either it has
// been released, or it has expired.
//
// The tag is read directly. The text after the prefix is the encoded ref, not the
// ref name, so it cannot be fed back through IsLocked.
func (c *Client) IsLockReclaimable(ctx context.Context, lockTag string) (bool, error) {
	locked, _, err := c.lockStateByTag(ctx, lockTag)
	if err != nil {
		// lockStateByTag fails closed, and so does this: an unreadable lock is
		// left alone rather than reclaimed out from under whoever holds it.
		return false, err
	}
	return !locked, nil
}
