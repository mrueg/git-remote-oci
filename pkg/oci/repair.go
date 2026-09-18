package oci

import (
	"context"
	"errors"
	"fmt"
)

// Rebuilding _refs from the ref tags.
//
// The ref tags are authoritative. A push writes the ref manifest first and
// _refs second, and a deletion removes or tombstones the tag before it drops
// the index entry, so after any crash between those two steps the tag is the
// truth and the index is the thing that lags: a tag ahead of its entry means
// the entry is stale, a tag missing or tombstoned under a live entry means the
// deletion never finished, and a tag with no entry at all means the push never
// reached the index. Every one of those is invisible to a reader, because a
// reader lists refs from _refs and never looks at the tags.
//
// Ref names come from each manifest's io.git-remote-oci.ref annotation, never
// from decoding the tag, which is what lets a truncated `_h_` tag (§3.1) be
// recovered at all.

// RefIndexRepair is the difference between what the ref tags publish and what
// the _refs index lists, as the changes that would make the index agree.
type RefIndexRepair struct {
	// Add holds refs whose tag is live but which the index does not list.
	Add map[string]RefEntry
	// Update holds refs whose tag names a different commit than the index
	// does, with the entry rebuilt from the tag.
	Update map[string]RefEntry
	// Remove holds the entries the index lists for refs whose tag is gone or
	// tombstoned, as the index currently has them.
	Remove map[string]RefEntry
	// IndexAbsent is true when the repository has no _refs tag at all, so
	// readers are being served the _index mirror or a tag enumeration.
	IndexAbsent bool
}

// Empty reports whether the index already agrees with the tags.
func (r RefIndexRepair) Empty() bool {
	return len(r.Add) == 0 && len(r.Update) == 0 && len(r.Remove) == 0
}

// Summary describes the repair in one line.
func (r RefIndexRepair) Summary() string {
	return fmt.Sprintf("%d added, %d updated, %d removed", len(r.Add), len(r.Update), len(r.Remove))
}

// PlanRefIndexRepair reports how the _refs index disagrees with the ref tags,
// without changing anything.
//
// A repository in a format this build does not implement is refused, as every
// other read refuses it: a repair that rewrote such an index would publish a
// version-1 index over a layout it cannot read.
func (c *Client) PlanRefIndexRepair(ctx context.Context) (RefIndexRepair, error) {
	return c.planRefIndexRepair(ctx)
}

// RepairRefIndex rewrites _refs so that it agrees with the ref tags, and
// returns what it changed.
//
// The write goes through the same read-modify-write as a push: under the
// index lock, guarded by the digest compare-and-swap, republishing the
// recorded HEAD, the pack chain and every entry the repair does not touch
// exactly as they were. The plan is recomputed on every attempt against the
// index and tags as they then are, so a push that lands while the repair is
// running is layered under it rather than reverted by a plan made against
// state that is no longer true.
//
// Nothing is written when there is nothing to repair and the index exists.
func (c *Client) RepairRefIndex(ctx context.Context) (RefIndexRepair, error) {
	plan, err := c.planRefIndexRepair(ctx)
	if err != nil {
		return RefIndexRepair{}, err
	}
	if plan.Empty() && !plan.IndexAbsent {
		return plan, nil
	}

	var applied RefIndexRepair
	err = c.updateRefIndex(ctx, "", func(ctx context.Context, _ map[string]RefEntry) (map[string]RefEntry, map[string]bool, error) {
		current, planErr := c.planRefIndexRepair(ctx)
		if planErr != nil {
			return nil, nil, planErr
		}
		applied = current
		refs := make(map[string]RefEntry, len(current.Add)+len(current.Update))
		for name, entry := range current.Add {
			refs[name] = entry
		}
		for name, entry := range current.Update {
			refs[name] = entry
		}
		deleted := make(map[string]bool, len(current.Remove))
		for name := range current.Remove {
			deleted[name] = true
		}
		return refs, deleted, nil
	})
	if err != nil {
		return RefIndexRepair{}, fmt.Errorf("failed to rewrite the _refs index: %w", err)
	}
	return applied, nil
}

// planRefIndexRepair reads _refs and the ref tags and diffs them.
func (c *Client) planRefIndexRepair(ctx context.Context) (RefIndexRepair, error) {
	indexed, err := c.fetchRefsIndexOnly(ctx)
	var plan RefIndexRepair
	switch {
	case err == nil:
	case IsNotFound(err):
		plan.IndexAbsent = true
		indexed = map[string]RefEntry{}
	case errors.Is(err, ErrUnsupportedFormat):
		return RefIndexRepair{}, err
	default:
		return RefIndexRepair{}, fmt.Errorf("failed to read the _refs index: %w", err)
	}

	published, err := c.enumerateRefManifests(ctx)
	if err != nil {
		return RefIndexRepair{}, err
	}

	plan.Add = map[string]RefEntry{}
	plan.Update = map[string]RefEntry{}
	plan.Remove = map[string]RefEntry{}
	for name, fromTag := range published {
		entry, listed := indexed[name]
		switch {
		case !listed:
			plan.Add[name] = fromTag
		case entry.SHA != fromTag.SHA:
			plan.Update[name] = fromTag
		}
	}
	for name, entry := range indexed {
		if _, live := published[name]; !live {
			plan.Remove[name] = entry
		}
	}
	return plan, nil
}

// enumerateRefManifests reads every live ref manifest in the repository and
// returns the entries their annotations describe.
//
// Unlike the listing fallback it is strict: a ref manifest that exists but
// cannot be read fails the enumeration, because a repair that skipped it
// would go on to drop that ref from the index. A tag that is not an image
// manifest, or is gone between the listing and the fetch, is not a ref and is
// passed over, as the listing does.
func (c *Client) enumerateRefManifests(ctx context.Context) (map[string]RefEntry, error) {
	found := make(map[string]RefEntry)
	err := c.Repo.Tags(ctx, "", func(tags []string) error {
		for _, tag := range tags {
			if ClassifyTag(tag) != TagClassRef {
				continue
			}
			c.manifestCache.Delete(tag)
			manifest, err := c.FetchManifest(ctx, tag)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if errors.Is(err, ErrNotAnImageManifest) || errors.Is(err, ErrManifestNotFound) {
					continue
				}
				return fmt.Errorf("failed to read the manifest tagged %s: %w", tag, err)
			}
			if refName, entry, ok := tagRefEntry(manifest); ok {
				found[refName] = entry
			}
		}
		return nil
	})
	if err != nil {
		if IsNotFound(err) {
			// A repository with no tags at all publishes no refs.
			return map[string]RefEntry{}, nil
		}
		return nil, c.explainAuth(fmt.Errorf("failed to list the repository's tags: %w", err))
	}
	return c.sanitiseRefIndex(found), nil
}
