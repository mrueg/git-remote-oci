package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	opencontainers "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/errgroup"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote/errcode"
)

// The _refs index and its _index mirror: reading, merging and publishing the
// set of refs a repository advertises.

const (
	// refsIndexLockRef is the pseudo-ref whose lock serialises updates to the
	// _refs index, which every push read-modify-writes.
	refsIndexLockRef  = "_refs_index_lock"
	refsIndexLockWait = 45 * time.Second
	// refsIndexMaxAttempts bounds the optimistic-concurrency retry in
	// PushRichRefIndex. Contention is expected to be rare; a client that loses
	// three times running is better off reporting it than spinning.
	refsIndexMaxAttempts = 3
)

// indexAnnotations builds the annotation set for a ref index manifest.
func indexAnnotations(head string) map[string]string {
	a := map[string]string{
		// Declares the layout of this repository. A reader that does not
		// implement this version refuses the repository; see
		// checkFormatVersion.
		AnnotationFormatVersion: FormatVersion,
	}
	if head != "" {
		a[AnnotationGitHead] = head
	}
	return a
}

// currentHead reads the HEAD recorded on the published ref index.
//
// A repository with no index yet, or none recorded, reports "" with no error:
// that is the normal state of something nobody has pushed a branch to.
func (c *Client) currentHead(ctx context.Context) (string, error) {
	c.manifestCache.Delete(TagRefIndex)
	manifest, err := c.FetchManifest(ctx, TagRefIndex)
	if err != nil {
		if IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return manifest.Annotations[AnnotationGitHead], nil
}

// FetchHead returns the ref the remote's HEAD points at, or "" if none is
// recorded.
func (c *Client) FetchHead(ctx context.Context) (string, error) {
	head, err := c.currentHead(ctx)
	if err == nil && head != "" {
		return head, nil
	}
	if err != nil && !IsNotFound(err) {
		return "", err
	}
	// _index stands in when _refs is missing. A repository with neither simply
	// records no HEAD, which is not an error; anything else is reported, so a
	// caller can tell "none recorded" from "could not find out".
	idx, idxErr := c.FetchOCIImageIndex(ctx, TagOCIIndex)
	if idxErr != nil {
		if IsNotFound(idxErr) {
			return "", nil
		}
		return "", idxErr
	}
	return idx.Annotations[AnnotationGitHead], nil
}

// ListRefs lists all git references published in the OCI registry.
// Uses fast O(1) _refs index lookup if present, with fallback to tag enumeration.
func (c *Client) ListRefs(ctx context.Context) (map[string]string, error) {
	// 1. Try fast O(1) lookup using repository _refs index
	indexRefs, err := c.FetchRefIndex(ctx)
	if err == nil {
		return indexRefs, nil
	}
	if !IsNotFound(err) {
		// Only "there is no index" is repaired around. This used to fall
		// through on every error, so a repository in a format this build
		// refuses was quietly listed from its tags instead -- the exact read
		// the version check exists to stop -- and an unreachable registry
		// looked like an empty one.
		return nil, err
	}

	// 2. Fall back to enumerating tags. This is a repair path for a repository
	//    whose index is missing: it costs a round trip per tag and cannot see
	//    refs whose tags were truncated, so it is never the fast path.
	return c.enumerateTagRefs(ctx, false)
}

// EnumerateTagRefs directly enumerates all Git ref tags from registry tag listings.
//
// Unlike the fallback inside ListRefs it skips a tag whose manifest cannot be
// read rather than failing: it is the repair path a caller reaches for when
// the registry is already known to be in a state the index does not describe.
func (c *Client) EnumerateTagRefs(ctx context.Context) (map[string]string, error) {
	return c.enumerateTagRefs(ctx, true)
}

// enumerateTagRefs walks the tag list and collects the refs the manifests
// under it publish. lenient decides whether a manifest that cannot be read is
// skipped or fails the enumeration.
//
// Whatever is collected goes through sanitiseRefIndex, exactly as an entry
// read from _refs would. A ref name arriving as a manifest annotation is no
// more trustworthy than one arriving in the index blob, and it reaches the
// same place: the helper's `list` output on stdout, where a newline in a
// name is another line of protocol.
func (c *Client) enumerateTagRefs(ctx context.Context, lenient bool) (map[string]string, error) {
	found := make(map[string]RefEntry)
	err := c.Repo.Tags(ctx, "", func(tags []string) error {
		for _, tag := range tags {
			if tag == TagRefIndex || tag == TagOCIIndex {
				continue
			}
			// Skip commit-id tags up front: they are ref-agnostic commit
			// manifests containing no Git ref annotations. Skipping them
			// avoids a manifest fetch round-trip for every historical commit.
			if isObjectID(tag) {
				continue
			}

			c.manifestCache.Delete(tag)
			manifest, err := c.FetchManifest(ctx, tag)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				// Not an image manifest, or gone between the listing and the
				// fetch: neither is a ref, in either mode.
				if lenient || errors.Is(err, ErrNotAnImageManifest) || errors.Is(err, ErrManifestNotFound) {
					continue
				}
				return fmt.Errorf("failed to fetch manifest for tag %s: %w", tag, err)
			}

			if refName, entry, ok := tagRefEntry(manifest); ok {
				found[refName] = entry
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return map[string]string{}, nil
		}
		var errResp *errcode.ErrorResponse
		if errors.As(err, &errResp) && errResp.StatusCode == http.StatusNotFound {
			return map[string]string{}, nil
		}
		return nil, c.explainAuth(fmt.Errorf("failed to list tags from registry: %w", err))
	}

	refs := make(map[string]string, len(found))
	for refName, entry := range c.sanitiseRefIndex(found) {
		refs[refName] = entry.SHA
	}
	return refs, nil
}

// tagRefEntry reads the ref a tag's manifest publishes. ok is false for
// anything that is not a live ref manifest; validation of what it returns is
// sanitiseRefIndex's job, so that the tag path and the index path cannot
// disagree about what a ref name may contain.
func tagRefEntry(manifest *ocispec.Manifest) (refName string, entry RefEntry, ok bool) {
	// A tombstone means the ref was deleted on a registry that would not
	// remove the manifest. Enumerating it would resurrect the ref.
	if manifest.Annotations[AnnotationGitDeleted] == "true" {
		return "", RefEntry{}, false
	}
	// Anything without a packfile layer is not a ref: a lock, an LFS lock
	// list, or an ordinary container image sharing the repository.
	if !hasGitPackfileLayer(manifest) {
		return "", RefEntry{}, false
	}
	refName = manifest.Annotations[AnnotationGitRef]
	sha := manifest.Annotations[ocispec.AnnotationRevision]
	if refName == "" || sha == "" {
		return "", RefEntry{}, false
	}
	return refName, RefEntry{SHA: sha}, true
}

// RefEntry represents cached reference metadata inside the _refs JSON index payload.
type RefEntry struct {
	SHA        string `json:"sha"`
	Author     string `json:"author,omitempty"`
	Timestamp  int64  `json:"timestamp,omitempty"`
	Message    string `json:"message,omitempty"`
	TagSig     string `json:"tag_sig,omitempty"`
	Tagger     string `json:"tagger,omitempty"`
	TagMessage string `json:"tag_message,omitempty"`
	TagObject  string `json:"tag_object,omitempty"`
}

// FetchRichRefIndex retrieves the rich ref mapping from the _refs index tag,
// with automatic fallback to the _index OCI Image Index manifest.
func (c *Client) FetchRichRefIndex(ctx context.Context) (map[string]RefEntry, error) {
	c.manifestCache.Delete(TagRefIndex)
	manifest, err := c.FetchManifest(ctx, TagRefIndex)
	if err != nil {
		if !IsNotFound(err) {
			// Only an absent _refs is stood in for. Falling back on any
			// failure, as this used to, turned a momentary 5xx on _refs into a
			// read of _index -- which is a mirror that a failed push can leave
			// stale, and which then became the base of the caller's
			// read-modify-write.
			return nil, fmt.Errorf("failed to fetch _refs index manifest: %w", err)
		}
		// _index carries the same ref set and is written alongside _refs, so it
		// stands in when _refs is missing. It is not a legacy path: both are
		// current, and FetchOCIImageIndexRefs applies the same version check.
		ociRefs, ociErr := c.FetchOCIImageIndexRefs(ctx, TagOCIIndex)
		switch {
		case ociErr == nil && len(ociRefs) > 0:
			return ociRefs, nil
		case ociErr != nil && !IsNotFound(ociErr):
			// A mirror that exists but cannot be read -- a refused format
			// version, say -- is reported as such, not as a missing index.
			return nil, fmt.Errorf("_refs is missing and the _index mirror could not be read: %w", ociErr)
		}
		return nil, fmt.Errorf("failed to fetch _refs index manifest: %w", err)
	}

	// The _refs index is the first thing every operation reads, which makes it
	// the one place worth checking the format version. Doing it here means an
	// unreadable repository is refused before anything acts on its contents.
	if err := checkFormatVersion(manifest.Annotations[AnnotationFormatVersion]); err != nil {
		return nil, err
	}

	var indexDesc *ocispec.Descriptor
	for i := range manifest.Layers {
		if manifest.Layers[i].MediaType == MediaTypeGitIndex {
			indexDesc = &manifest.Layers[i]
			break
		}
	}
	if indexDesc == nil {
		return nil, fmt.Errorf("no %s layer in the _refs manifest", MediaTypeGitIndex)
	}

	data, err := c.fetchBlobBytes(ctx, *indexDesc, maxMetadataBytes, "the _refs index blob")
	if err != nil {
		return nil, fmt.Errorf("failed to fetch _refs index blob: %w", err)
	}

	var richRefs map[string]RefEntry
	if err := json.Unmarshal(data, &richRefs); err != nil {
		return nil, fmt.Errorf("failed to unmarshal _refs index: %w", err)
	}

	return c.sanitiseRefIndex(richRefs), nil
}

// FetchRefIndex retrieves the ref mapping (ref -> SHA) from the _refs index tag.
func (c *Client) FetchRefIndex(ctx context.Context) (map[string]string, error) {
	richRefs, err := c.FetchRichRefIndex(ctx)
	if err != nil {
		return nil, err
	}

	refs := make(map[string]string, len(richRefs))
	for refName, entry := range richRefs {
		refs[refName] = entry.SHA
	}
	return refs, nil
}

// PushOCIImageIndex publishes an OCI Image Index (application/vnd.oci.image.index.v1+json)
// grouping multiple commit branch heads and repository references under a single manifest index tag.
func (c *Client) PushOCIImageIndex(ctx context.Context, tag string, refs map[string]RefEntry, head string) error {
	if tag == "" {
		tag = TagOCIIndex
	}

	// One HEAD per ref, and a repository can have thousands. They are
	// independent, so they go out in parallel, bounded so a wide repository
	// does not open a connection per ref.
	names := make([]string, 0, len(refs))
	for refName, entry := range refs {
		if entry.SHA != "" {
			names = append(names, refName)
		}
	}
	sort.Strings(names)
	resolved := make([]ocispec.Descriptor, len(names))
	present := make([]bool, len(names))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(c.concurrency())
	for i, refName := range names {
		g.Go(func() error {
			desc, ok, err := c.resolveIndexEntry(gctx, refName, refs[refName].SHA)
			if err != nil {
				return err
			}
			resolved[i], present[i] = desc, ok
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	manifests := make([]ocispec.Descriptor, 0, len(names))
	for i, refName := range names {
		if !present[i] {
			continue
		}
		desc, entry := resolved[i], refs[refName]

		// Carry the resolved media type rather than assuming an OCI manifest:
		// a registry that stored the ref as a Docker manifest would otherwise be
		// misdescribed here, and clients follow the index's word for it.
		childMediaType := desc.MediaType
		if childMediaType == "" {
			childMediaType = ocispec.MediaTypeImageManifest
		}

		manifestDesc := ocispec.Descriptor{
			MediaType: childMediaType,
			Digest:    desc.Digest,
			Size:      desc.Size,
			// Without a platform, an index entry cannot be selected by
			// `docker pull` or anything else that matches on one. Git data is
			// platform-agnostic, and unknown/unknown is the convention for
			// exactly that, matching the config blob written for each commit.
			Platform: &ocispec.Platform{
				Architecture: "unknown",
				OS:           "unknown",
			},
			Annotations: map[string]string{
				// The tag the ref's manifest is actually under, which is not
				// always EncodeRefTag: a ref whose encoding looks like a
				// commit id is published under a "ref-" prefix (§3.2), and
				// naming the unprefixed form here pointed at the wrong tag.
				ocispec.AnnotationRefName:  RefManifestTag(refName),
				AnnotationGitRef:           refName,
				ocispec.AnnotationRevision: entry.SHA,
			},
		}

		if entry.TagSig != "" {
			manifestDesc.Annotations[AnnotationGitTagSig] = entry.TagSig
		}
		if entry.Tagger != "" {
			manifestDesc.Annotations[AnnotationGitTagger] = entry.Tagger
		}
		if entry.TagMessage != "" {
			manifestDesc.Annotations[AnnotationGitTagMessage] = entry.TagMessage
		}
		if entry.TagObject != "" {
			manifestDesc.Annotations[AnnotationGitTagObj] = entry.TagObject
		}
		if entry.Author != "" {
			manifestDesc.Annotations[ocispec.AnnotationAuthors] = entry.Author
		}
		if entry.Timestamp > 0 {
			manifestDesc.Annotations[ocispec.AnnotationCreated] = time.Unix(entry.Timestamp, 0).UTC().Format(time.RFC3339)
		}
		if entry.Message != "" {
			msgLines := strings.Split(strings.TrimSpace(entry.Message), "\n")
			if len(msgLines) > 0 && msgLines[0] != "" {
				manifestDesc.Annotations[ocispec.AnnotationDescription] = msgLines[0]
			}
		}
		manifestDesc.Annotations[ocispec.AnnotationTitle] = refName
		manifestDesc.Annotations[ocispec.AnnotationVendor] = "git-remote-oci"

		manifests = append(manifests, manifestDesc)
	}

	// Already in ref-name order: names was sorted before resolution.
	annotations := map[string]string{
		AnnotationGitType: "repository-index",
		// _index stands in for _refs when _refs is missing, so it has to
		// declare the layout too. Without this the fallback was a way into
		// a repository this build cannot read.
		AnnotationFormatVersion: FormatVersion,
	}
	// Omitted when empty, as on _refs (indexAnnotations): "absent" is how §6.2
	// spells "none recorded", and an empty string is not the same statement.
	if head != "" {
		annotations[AnnotationGitHead] = head
	}
	indexObj := ocispec.Index{
		Versioned:   specs.Versioned{SchemaVersion: 2},
		MediaType:   ocispec.MediaTypeImageIndex,
		Manifests:   manifests,
		Annotations: annotations,
	}

	indexBytes, err := json.Marshal(indexObj)
	if err != nil {
		return fmt.Errorf("failed to marshal OCI Image Index: %w", err)
	}

	indexDigest := opencontainers.FromBytes(indexBytes)
	indexDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageIndex,
		Digest:    indexDigest,
		Size:      int64(len(indexBytes)),
	}

	err = c.Repo.PushReference(ctx, indexDesc, bytes.NewReader(indexBytes), tag)
	if err == nil {
		// By digest only: the tag is mutable and is never served from the
		// cache, see FetchOCIImageIndex.
		c.manifestCache.Store(indexDigest.String(), &indexObj)
	}
	return err
}

// resolveIndexEntry finds the manifest an _index entry for refName should point
// at. ok is false when the ref has to be left out of the mirror.
//
// The ref manifest is what the entry names. The commit manifest stands in only
// when the ref manifest is *absent* -- a ref recorded in _refs whose tag was
// never written -- and not on any other failure: a ref whose manifest could not
// be resolved because the registry was momentarily unavailable would otherwise
// be mirrored as pointing at the ref-agnostic commit manifest, which carries no
// ref annotations. That is reported and the ref skipped; fsck finds the drift.
//
// A cancelled context is the exception and is returned: skipping every ref
// with a warning each would publish an empty mirror.
func (c *Client) resolveIndexEntry(ctx context.Context, refName, sha string) (ocispec.Descriptor, bool, error) {
	desc, err := c.ResolveRefManifest(ctx, refName)
	if err == nil {
		return desc, true, nil
	}
	if ctx.Err() != nil {
		return ocispec.Descriptor{}, false, ctx.Err()
	}
	if !IsNotFound(err) {
		c.warnf("leaving %s out of the _index mirror: its ref manifest could not be resolved: %v", refName, err)
		return ocispec.Descriptor{}, false, nil
	}

	desc, err = c.Repo.Resolve(ctx, sha)
	if err == nil {
		return desc, true, nil
	}
	if ctx.Err() != nil {
		return ocispec.Descriptor{}, false, ctx.Err()
	}
	if !IsNotFound(err) {
		c.warnf("leaving %s out of the _index mirror: neither its ref manifest nor commit %s could be resolved: %v", refName, sha, err)
	}
	// Neither exists. An entry with an empty digest is one no client can
	// follow, so the ref is left out.
	return ocispec.Descriptor{}, false, nil
}

// FetchOCIImageIndex fetches an OCI Image Index manifest from the registry by tag or digest.
//
// The _index tag is never served from the cache, for the same reason
// FetchManifest bypasses it for _refs: the tag moves on every push, and a
// reader that stands in for _refs with a cached _index is reading the
// repository as it was when this client last looked.
func (c *Client) FetchOCIImageIndex(ctx context.Context, tagOrDigest string) (*ocispec.Index, error) {
	if tagOrDigest == "" {
		tagOrDigest = TagOCIIndex
	}

	if tagOrDigest != TagOCIIndex {
		if cached, ok := c.manifestCache.Load(tagOrDigest); ok {
			if idx, ok := cached.(*ocispec.Index); ok {
				return idx, nil
			}
		}
	}

	desc, data, err := c.fetchManifestBytes(ctx, tagOrDigest, "the OCI image index")
	if err != nil {
		return nil, err
	}

	var index ocispec.Index
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("failed to unmarshal OCI Image Index JSON: %w", err)
	}

	if tagOrDigest != TagOCIIndex {
		c.manifestCache.Store(tagOrDigest, &index)
	}
	if digestStr := desc.Digest.String(); digestStr != "" {
		c.manifestCache.Store(digestStr, &index)
	}
	return &index, nil
}

// FetchOCIImageIndexRefs retrieves the ref mapping (ref -> RefEntry) from an OCI Image Index tag.
func (c *Client) FetchOCIImageIndexRefs(ctx context.Context, tagOrDigest string) (map[string]RefEntry, error) {
	index, err := c.FetchOCIImageIndex(ctx, tagOrDigest)
	if err != nil {
		return nil, err
	}

	// Same check as on _refs. This path exists because _index stands in when
	// _refs is missing, and an unchecked stand-in is just an unchecked way in.
	if err := checkFormatVersion(index.Annotations[AnnotationFormatVersion]); err != nil {
		return nil, err
	}

	refs := make(map[string]RefEntry, len(index.Manifests))
	for _, m := range index.Manifests {
		// Every entry this build writes carries the full ref name. There used
		// to be a fallback that guessed refs/tags/ from a leading "v" and
		// refs/heads/ otherwise, which was wrong for a branch called v2 or any
		// tag not starting with v.
		// A tombstone here means a deletion that reached _index. Enumerating
		// tags skips these already; this path did not, and it is the one that
		// runs when _refs cannot be read -- so it was the fallback that could
		// resurrect a deleted ref. It does not close the wider hole, which is
		// an _index left stale by a failed write and therefore listing the ref
		// as it was before the deletion; `fsck` reports that drift.
		if m.Annotations[AnnotationGitDeleted] == "true" {
			continue
		}

		gitRef := m.Annotations[AnnotationGitRef]

		sha := m.Annotations[ocispec.AnnotationRevision]
		if gitRef != "" && sha != "" {
			entry := RefEntry{
				SHA:        sha,
				Tagger:     m.Annotations[AnnotationGitTagger],
				TagMessage: m.Annotations[AnnotationGitTagMessage],
				TagSig:     m.Annotations[AnnotationGitTagSig],
				TagObject:  m.Annotations[AnnotationGitTagObj],
			}
			refs[gitRef] = entry
		}
	}
	// The same sanitising as on _refs, for the same reason the version check
	// above is duplicated here: this stands in for _refs, so it is a second
	// parser of the same facts and must not be the lenient one.
	return c.sanitiseRefIndex(refs), nil
}

// PushRichRefIndex publishes the ref index, preserving whatever HEAD is already
// recorded. See PushRichRefIndexWithHead for the contract on refs and deleted.
func (c *Client) PushRichRefIndex(ctx context.Context, refs map[string]RefEntry, deleted map[string]bool) error {
	return c.PushRichRefIndexWithHead(ctx, refs, deleted, "")
}

// SetHead moves the repository's recorded default branch.
//
// PushRichRefIndexWithHead deliberately will not: first writer wins there,
// because nothing in the remote-helper protocol tells a helper what a remote's
// default *should* be, and silently retargeting it on every push would be worse
// than leaving it alone. The consequence is that the first branch anyone pushed
// is the default forever, which is a decision someone has to be able to revisit
// — deliberately, by saying so. This is where they say it.
//
// The ref must be one the repository publishes. A HEAD naming something that is
// not there is the state the deletion path already goes out of its way to
// avoid, and there is no reason to let this command create it.
//
// It returns the ref that was recorded before, or "" if none was.
func (c *Client) SetHead(ctx context.Context, ref string) (previous string, err error) {
	if ref == "" {
		return "", fmt.Errorf("no ref given")
	}
	// HEAD is a symbolic ref to a branch (FORMAT.md §6.2). A reader that finds
	// it naming a tag has no branch to check out, so the tag is refused here
	// rather than recorded and discovered at the next clone.
	if !strings.HasPrefix(ref, "refs/heads/") {
		return "", fmt.Errorf("%s is not a branch: HEAD must name a ref under refs/heads/", ref)
	}

	// Same optimistic-concurrency shape as an index update, and for the same
	// reason: a registry has no compare-and-swap, so the digest check before
	// the write is what stops a concurrent push losing this change or vice
	// versa.
	var lastConflict error
	for attempt := 1; attempt <= refsIndexMaxAttempts; attempt++ {
		baseline, baseErr := c.refIndexDigest(ctx)
		if baseErr != nil {
			return "", fmt.Errorf("failed to read the _refs index state: %w", baseErr)
		}

		// No refs added or deleted: this republishes what is there, with a
		// different HEAD annotation.
		remoteRefs, mergeErr := c.mergeRemoteRefs(ctx, nil, nil)
		if mergeErr != nil {
			return "", mergeErr
		}
		if len(remoteRefs) == 0 {
			return "", fmt.Errorf("this repository publishes no refs")
		}
		if _, ok := remoteRefs[ref]; !ok {
			return "", fmt.Errorf("%s is not a ref this repository publishes", ref)
		}

		previous, err = c.currentHead(ctx)
		if err != nil {
			return "", fmt.Errorf("failed to read the recorded HEAD: %w", err)
		}

		conflict, commitErr := c.commitRefIndex(ctx, baseline, remoteRefs, ref)
		if commitErr != nil {
			return "", commitErr
		}
		if conflict != nil {
			lastConflict = conflict
			continue
		}
		return previous, nil
	}
	return "", fmt.Errorf("gave up setting HEAD after %d attempts: %w", refsIndexMaxAttempts, lastConflict)
}

// PushRichRefIndexWithHead publishes the ref index and, when the repository has
// no HEAD recorded yet, adopts headHint.
//
// refs MUST contain only the refs this writer changed, and deleted only the
// refs it removed. The published index is the base: every entry in refs
// overrides the published value for that ref, and every name in deleted is
// dropped from it, while everything else is carried over untouched. Passing
// the writer's whole view of the remote -- refs it merely fetched earlier --
// is how a concurrent writer's update to one of those refs gets reverted: the
// stale entry wins the override, and nothing reports it. deleted is needed as
// well as absence because the tag-enumeration repair path would otherwise
// re-add a ref whose index entry was removed but whose OCI tag deletion the
// registry refused, resurrecting it on the very next push.
//
// First writer wins for HEAD: a later push does not move it, because nothing
// in the remote-helper protocol tells the helper what the remote's default
// branch should be, and silently retargeting it on every push would be worse
// than leaving it alone.
func (c *Client) PushRichRefIndexWithHead(ctx context.Context, refs map[string]RefEntry, deleted map[string]bool, headHint string) error {
	// Read-modify-write under optimistic concurrency control.
	//
	// The digest check is what protects the data, not the lock. A registry
	// offers no compare-and-swap, so lock acquisition is itself check-then-write
	// and two clients can both believe they hold it. Comparing the index digest
	// from before the merge against the digest immediately before the write
	// catches the case that actually loses data — another client updating _refs
	// while this one was busy merging — and retries against fresh state instead
	// of overwriting them.
	//
	// Because the digest check is the real guard, the merge is computed
	// *outside* the lock and the lock covers only the re-check and the write.
	// The merge is the expensive half: it re-reads the index with its own
	// retries and enumerates every tag in the repository, which on a wide
	// repository over a slow link took long enough to routinely outlast the
	// lock's TTL — and a lock that expires mid-update is worse than no lock,
	// because the next client acquires it legitimately and the two interleave
	// exactly the update the lock exists to serialise. Anything that changes
	// while the merge runs unlocked is caught by the re-check under the lock.
	//
	// Retrying converges: each attempt re-reads whatever is on the registry and
	// layers this push's refs on top, so concurrent updates to *different* refs
	// all survive. Concurrent updates to the *same* ref are still last-writer-
	// wins, but they are no longer silent.
	var lastConflict error
	for attempt := 1; attempt <= refsIndexMaxAttempts; attempt++ {
		baseline, baseErr := c.refIndexDigest(ctx)
		if baseErr != nil {
			return fmt.Errorf("failed to read the _refs index state: %w", baseErr)
		}

		remoteRefs, mergeErr := c.mergeRemoteRefs(ctx, refs, deleted)
		if mergeErr != nil {
			return mergeErr
		}

		head, headErr := c.currentHead(ctx)
		if headErr != nil {
			return fmt.Errorf("failed to read the recorded HEAD: %w", headErr)
		}
		if head == "" {
			head = headHint
		}
		if head != "" {
			if _, stillThere := remoteRefs[head]; !stillThere {
				// The recorded HEAD was deleted. Drop it rather than advertise
				// a ref that is no longer there.
				head = ""
			}
		}

		conflict, err := c.commitRefIndex(ctx, baseline, remoteRefs, head)
		if err != nil {
			return err
		}
		if conflict != nil {
			lastConflict = conflict
			continue
		}
		return nil
	}
	return fmt.Errorf("gave up updating the _refs index after %d attempts: %w", refsIndexMaxAttempts, lastConflict)
}

// commitRefIndex takes the index lock, verifies nothing moved since baseline,
// and writes.
//
// A non-nil first return is a lost race, not a failure: the caller re-merges
// against the newer state and tries again.
func (c *Client) commitRefIndex(ctx context.Context, baseline string, remoteRefs map[string]RefEntry, head string) (conflict error, err error) {
	if lockErr := c.acquireRefsIndexLock(ctx); lockErr != nil {
		return nil, lockErr
	}
	defer func() {
		// A lock that could not be given back stalls every other writer until
		// the TTL runs out, which is worth more than a warning: the caller is
		// told even when the write itself went through.
		if releaseErr := c.releaseRefsIndexLock(ctx); releaseErr != nil {
			err = errors.Join(err, releaseErr)
		}
	}()

	// Re-check under the lock. Anything that changed since baseline means the
	// merge was computed from stale state.
	current, curErr := c.refIndexDigest(ctx)
	if curErr != nil {
		return nil, fmt.Errorf("failed to re-read the _refs index state: %w", curErr)
	}
	if current != baseline {
		return fmt.Errorf("the _refs index changed from %s to %s while this push was merging",
			shortDigest(baseline), shortDigest(current)), nil
	}

	if pushErr := c.pushRichRefIndexDirect(ctx, remoteRefs, head); pushErr != nil {
		return nil, pushErr
	}
	return nil, nil
}

// acquireRefsIndexLock takes the lock serialising _refs updates.
func (c *Client) acquireRefsIndexLock(ctx context.Context) error {
	if _, err := c.AcquireRefLockWithRetry(ctx, refsIndexLockRef, c.RefsIndexLockTTL, refsIndexLockWait); err != nil {
		return fmt.Errorf("failed to acquire _refs index lock: %w", err)
	}
	return nil
}

// releaseRefsIndexLock releases it, on a context of its own so that a cancelled
// push still gives the lock back rather than leaving the ref stalled until the
// TTL runs out.
func (c *Client) releaseRefsIndexLock(ctx context.Context) error {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := c.ReleaseRefLock(releaseCtx, refsIndexLockRef); err != nil {
		return fmt.Errorf("failed to release the _refs index lock: %w", err)
	}
	return nil
}

// mergeRemoteRefsAttempts bounds how often the published index is re-read
// before a merge gives up. Reads of _refs are already retried at the transport
// for transient statuses; this covers the write-then-read race of a registry
// that briefly serves an inconsistent view of a tag it has just moved.
const mergeRemoteRefsAttempts = 5

// mergeRemoteRefs reads the current published refs and layers this push on top.
//
// The published index is the base and refs is the set of overrides: each entry
// replaces the published value for that ref, unconditionally, and each name in
// deleted is removed. There is deliberately no comparison against what is
// published -- the caller has just written the ref manifest and is the
// authority on where the ref now points -- which is exactly why refs must hold
// only the refs this writer changed (see PushRichRefIndexWithHead).
//
// A base that cannot be read is an error, not an empty map. Proceeding with an
// empty base, as this used to after its retries ran out, republished the index
// as just this push's refs: every ref whose tag was truncated (§3.1) and cannot
// be recovered from tag enumeration was gone, and every other ref lost its
// author, timestamp and tag metadata.
func (c *Client) mergeRemoteRefs(ctx context.Context, refs map[string]RefEntry, deleted map[string]bool) (map[string]RefEntry, error) {
	var (
		remoteRefs map[string]RefEntry
		lastErr    error
	)
	for attempt := 0; attempt < mergeRemoteRefsAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("failed to read the published _refs index: %w", errors.Join(ctx.Err(), lastErr))
			case <-time.After(time.Duration(50*attempt) * time.Millisecond):
			}
		}
		c.manifestCache.Delete(TagRefIndex)
		published, fetchErr := c.FetchRichRefIndex(ctx)
		if fetchErr == nil {
			remoteRefs = published
			break
		}
		if IsNotFound(fetchErr) {
			// No index yet: the normal state of a repository nobody has pushed
			// to, and the one case where an empty base is the truth.
			lastErr = nil
			break
		}
		lastErr = fetchErr
	}
	if lastErr != nil {
		return nil, fmt.Errorf("failed to read the published _refs index after %d attempts: %w", mergeRemoteRefsAttempts, lastErr)
	}

	if remoteRefs == nil {
		// Without an index, the tags are the only record of what is
		// published. This is the repair path for a repository whose index was
		// lost; it cannot see truncated tags, but it recovers everything else.
		remoteRefs = make(map[string]RefEntry, len(refs))
		listed, listErr := c.enumerateTagRefs(ctx, false)
		if listErr != nil {
			return nil, fmt.Errorf("failed to enumerate the published refs: %w", listErr)
		}
		for rName, rSHA := range listed {
			remoteRefs[rName] = RefEntry{SHA: rSHA}
		}
	}

	for k, v := range refs {
		remoteRefs[k] = v
	}
	for name := range deleted {
		delete(remoteRefs, name)
	}
	return remoteRefs, nil
}

// refIndexDigest returns the digest of the published _refs manifest, or "" when
// the repository has no index yet.
//
// "" is a real state, not an error: it is what a fresh repository looks like,
// and two pushes racing to create the first index must be able to tell that
// apart from one of them having already won.
func (c *Client) refIndexDigest(ctx context.Context) (string, error) {
	desc, err := c.Repo.Resolve(ctx, TagRefIndex)
	if err != nil {
		if IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return desc.Digest.String(), nil
}

// shortDigest abbreviates a digest for an error message.
func shortDigest(d string) string {
	if d == "" {
		return "absent"
	}
	if i := strings.IndexByte(d, ':'); i >= 0 && len(d) > i+13 {
		return d[:i+13]
	}
	return d
}

// pushRichRefIndexDirect writes the _refs manifest for refs, and then the
// _index mirror.
//
// One attempt. Transient failures are retried at the transport, and a lost
// optimistic-concurrency race is retried by the caller against fresh state;
// this used to carry a third loop of five attempts between the two, which
// re-sent a 4xx the registry was never going to accept and slept through the
// caller's context while doing it.
func (c *Client) pushRichRefIndexDirect(ctx context.Context, refs map[string]RefEntry, head string) error {
	c.manifestCache.Delete(TagRefIndex)

	indexBytes, err := json.Marshal(refs)
	if err != nil {
		return fmt.Errorf("failed to marshal ref index: %w", err)
	}

	indexDigest := opencontainers.FromBytes(indexBytes)
	indexDesc := ocispec.Descriptor{
		MediaType: MediaTypeGitIndex,
		Digest:    indexDigest,
		Size:      int64(len(indexBytes)),
	}

	if err := c.Repo.Push(ctx, indexDesc, bytes.NewReader(indexBytes)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return fmt.Errorf("failed to push ref index blob: %w", err)
	}

	configObj := ocispec.Image{
		Platform: ocispec.Platform{
			Architecture: "unknown",
			OS:           "unknown",
		},
	}
	configBytes, err := json.Marshal(configObj)
	if err != nil {
		return fmt.Errorf("failed to marshal config blob: %w", err)
	}

	configDigest := opencontainers.FromBytes(configBytes)
	configDesc := ocispec.Descriptor{
		MediaType: MediaTypeGitConfig,
		Digest:    configDigest,
		Size:      int64(len(configBytes)),
	}
	if err := c.pushBlobOnce(ctx, configDesc, configBytes); err != nil {
		return fmt.Errorf("failed to push ref index config blob: %w", err)
	}

	// The pack-base graph rides on this manifest because every operation
	// reads it already; publishing the chain anywhere else would cost the
	// round trip it exists to remove.
	layers := []ocispec.Descriptor{indexDesc}
	if chainDesc, ok := c.packChainLayer(ctx); ok {
		layers = append(layers, chainDesc)
	}

	manifest := ocispec.Manifest{
		Versioned:   specs.Versioned{SchemaVersion: 2},
		MediaType:   ocispec.MediaTypeImageManifest,
		Config:      configDesc,
		Layers:      layers,
		Annotations: indexAnnotations(head),
	}

	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("failed to marshal index manifest: %w", err)
	}

	manifestDigest := opencontainers.FromBytes(manifestBytes)
	manifestDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    manifestDigest,
		Size:      int64(len(manifestBytes)),
	}

	if err := c.Repo.PushReference(ctx, manifestDesc, bytes.NewReader(manifestBytes), TagRefIndex); err != nil {
		return err
	}
	c.manifestCache.Store(TagRefIndex, &manifest)
	c.manifestCache.Store(manifestDigest.String(), &manifest)

	// _index mirrors _refs and stands in for it when _refs cannot be read
	// (§7), so an _index left behind is not a cosmetic problem: a reader that
	// falls back to it gets an outdated ref list, which can resurrect a
	// deleted ref or serve an old commit id, and nothing says so.
	//
	// It cannot be made to fail the operation -- _refs has already landed and
	// the refs are live, so reporting failure now would be a lie in the other
	// direction. What it can do is stop being silent. `git-remote-oci fsck`
	// re-checks the two against each other, which is what turns "somebody's
	// push once failed here" into something findable later.
	if idxErr := c.pushOCIImageIndexWithRetry(ctx, refs, head); idxErr != nil {
		c.warnf("the refs are published, but the _index mirror could not be updated: %v", idxErr)
		c.warnf("generic OCI tooling will show a stale ref list until the next push; `git-remote-oci fsck` reports the drift")
	}
	return nil
}

// pushOCIImageIndexWithRetry writes the _index mirror, retrying transient
// failures.
//
// It used to be one attempt whose error was discarded. One attempt is optimistic
// for a write that follows a successful one -- whatever made it fail is often
// momentary, and the cost of trying again is small next to leaving the mirror
// describing a repository that has moved on.
func (c *Client) pushOCIImageIndexWithRetry(ctx context.Context, refs map[string]RefEntry, head string) error {
	var lastErr error
	for attempt := 0; attempt < refsIndexMaxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(50*attempt) * time.Millisecond):
			}
		}
		if err := c.PushOCIImageIndex(ctx, TagOCIIndex, refs, head); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return lastErr
}

// sanitiseRefIndex drops entries a reader must not act on.
//
// The `_refs` index is the first thing every operation reads, and everything in
// it came out of a blob the registry served. Its ref names reach stdout, which
// is the wire protocol; its object ids become tags on the next request. Both
// were being taken on trust here while the same values arriving as a
// `pack-bases` annotation or in the pack chain were checked — the entry point
// was the one door left open.
//
// Dropping rather than refusing the whole index: one malformed entry should not
// make an otherwise good repository unreadable, and an entry that cannot be a
// ref could never have been fetched anyway. It is reported, because a ref
// silently disappearing is its own kind of confusing.
func (c *Client) sanitiseRefIndex(refs map[string]RefEntry) map[string]RefEntry {
	clean := make(map[string]RefEntry, len(refs))
	for name, entry := range refs {
		switch {
		case !validRefName(name):
			c.warnf("ignoring a published ref whose name is not a valid ref name (%q)", name)
		case entry.SHA != "" && !isObjectID(entry.SHA):
			c.warnf("ignoring %s: %q is not an object id", name, entry.SHA)
		case entry.TagObject != "" && !isObjectID(entry.TagObject):
			c.warnf("ignoring %s: tag object %q is not an object id", name, entry.TagObject)
		default:
			clean[name] = entry
		}
	}
	return clean
}
