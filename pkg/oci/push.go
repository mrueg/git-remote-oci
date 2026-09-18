package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"

	"github.com/mrueg/git-remote-oci/pkg/lfs"
	opencontainers "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote/errcode"
)

// Publishing commits and refs: commit manifests, ref manifests, tombstones and
// ref deletion.

// CommitManifestExists reports whether the registry serves a manifest for
// commitSHA.
//
// The error is returned rather than folded into a false so callers can tell
// "the registry does not have it" from "we could not find out". A push that
// cannot find out must not go on to cut a packfile against that commit.
func (c *Client) CommitManifestExists(ctx context.Context, commitSHA string) (bool, error) {
	if !isObjectID(commitSHA) {
		return false, fmt.Errorf("invalid commit SHA %q: must be a hex object id of 40 (SHA-1) or 64 (SHA-256) characters", commitSHA)
	}
	if _, cached := c.manifestCache.Load(commitSHA); cached {
		return true, nil
	}
	if _, err := c.Repo.Resolve(ctx, commitSHA); err != nil {
		if IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check for commit manifest %s: %w", commitSHA, err)
	}
	return true, nil
}

// IsCommitManifestCached returns true if the commit manifest for the given SHA is already cached.
func (c *Client) IsCommitManifestCached(commitSHA string) bool {
	if cached, ok := c.manifestCache.Load(commitSHA); ok && cached != nil {
		return true
	}
	return false
}

// ResolveRefManifest resolves the manifest descriptor for refName.
func (c *Client) ResolveRefManifest(ctx context.Context, refName string) (ocispec.Descriptor, error) {
	tag := RefManifestTag(refName)
	if tag == "" {
		return ocispec.Descriptor{}, fmt.Errorf("ref %q has no representable tag", refName)
	}
	return c.Repo.Resolve(ctx, tag)
}

// IsRefFullyPushed reports whether both the ref-agnostic commit manifest for
// commitSHA and the ref manifest for refName at that commit have already been
// pushed by this client. Callers use it to skip redundant work on multi-ref
// pushes.
//
// Both halves must be present: a pushed commit manifest alone does not mean
// the ref tag exists, and skipping on that basis leaves the ref discoverable
// only through the _refs index. And "pushed" means pushed by this client, not
// merely seen: the manifest cache also holds whatever a fetch or a tag
// enumeration read, and answering from it let a push that followed a list skip
// publishing the ref.
func (c *Client) IsRefFullyPushed(commitSHA, refName string) bool {
	if _, pushed := c.pushedCommits.Load(commitSHA); !pushed {
		return false
	}
	if refName == "" {
		return true
	}
	targetTag := RefManifestTag(refName)
	if targetTag == "" {
		return false
	}
	tip, ok := c.pushedRefs.Load(targetTag)
	return ok && tip == commitSHA
}

// isDeletionUnsupported reports whether err means the registry will not delete
// manifests at all, as opposed to this particular delete having failed.
//
// The distinction decides whether to fall back to a tombstone or to report the
// deletion as failed: falling back on a transient error would leave a tombstone
// over a ref that could have been deleted properly.
func isDeletionUnsupported(err error) bool {
	if err == nil {
		return false
	}
	var errResp *errcode.ErrorResponse
	if errors.As(err, &errResp) {
		switch errResp.StatusCode {
		case http.StatusMethodNotAllowed, http.StatusForbidden, http.StatusNotImplemented:
			return true
		}
	}
	var codeErr errcode.Error
	if errors.As(err, &codeErr) {
		switch codeErr.Code {
		case errcode.ErrorCodeUnsupported, errcode.ErrorCodeDenied:
			return true
		}
	}
	return false
}

// pushRefTombstone overwrites a ref tag with a manifest that marks the ref as
// deleted.
//
// It carries no packfile layer and is annotated as deleted, so neither the
// layer check nor the annotation check in EnumerateTagRefs will treat it as a
// live ref.
func (c *Client) pushRefTombstone(ctx context.Context, refName, targetTag string) error {
	if err := c.pushEmptyBlob(ctx); err != nil {
		return err
	}

	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    ocispec.DescriptorEmptyJSON,
		Layers:    emptyLayers(),
		Annotations: map[string]string{
			AnnotationGitDeleted:            "true",
			AnnotationGitRef:                refName,
			ocispec.AnnotationTitle:         refName + " (deleted)",
			ocispec.AnnotationVendor:        "git-remote-oci",
			ocispec.AnnotationDocumentation: "https://github.com/mrueg/git-remote-oci",
		},
	}

	data, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("failed to marshal the tombstone manifest for %s: %w", refName, err)
	}
	desc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    opencontainers.FromBytes(data),
		Size:      int64(len(data)),
	}
	if err := c.Repo.PushReference(ctx, desc, bytes.NewReader(data), targetTag); err != nil {
		return fmt.Errorf("failed to push the tombstone for %s: %w", targetTag, err)
	}
	c.forgetRefTag(targetTag)
	return nil
}

// forgetRefTag drops everything this client remembers about a ref tag it has
// moved away from a ref manifest, so a later push of the ref is not skipped as
// already done.
func (c *Client) forgetRefTag(tag string) {
	c.manifestCache.Delete(tag)
	c.refTagDigests.Delete(tag)
	c.pushedRefs.Delete(tag)
}

// RefTagSnapshot records what a ref tag pointed at, so a failed batch can put
// it back.
//
// Exists reports whether the tag was present at all; a ref created by the batch
// has nothing to restore and must be removed instead.
type RefTagSnapshot struct {
	RefName string
	Desc    ocispec.Descriptor
	Exists  bool
}

// SnapshotRefTag captures the current target of a ref's tag.
func (c *Client) SnapshotRefTag(ctx context.Context, refName string) (RefTagSnapshot, error) {
	snap := RefTagSnapshot{RefName: refName}
	desc, err := c.ResolveRefManifest(ctx, refName)
	if err != nil {
		if IsNotFound(err) {
			return snap, nil
		}
		return snap, fmt.Errorf("failed to read the current target of %s: %w", refName, err)
	}
	snap.Desc, snap.Exists = desc, true
	return snap, nil
}

// RestoreRefTag puts a ref tag back where the snapshot found it.
//
// Restoring is re-tagging an existing manifest, which is an ordinary tag write
// and works on every registry. Removing a tag the batch created needs deletion,
// which some registries refuse; that failure is reported so the caller can say
// so rather than implying the rollback was complete.
func (c *Client) RestoreRefTag(ctx context.Context, snap RefTagSnapshot) error {
	tag := RefManifestTag(snap.RefName)
	if tag == "" {
		return fmt.Errorf("ref %q cannot be represented as an OCI tag", snap.RefName)
	}
	c.forgetRefTag(tag)

	if !snap.Exists {
		desc, err := c.Repo.Resolve(ctx, tag)
		if err != nil {
			if IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("failed to resolve %s for removal: %w", tag, err)
		}
		if err := c.Repo.Delete(ctx, desc); err != nil {
			return fmt.Errorf("failed to remove %s, which this push created: %w", tag, err)
		}
		return nil
	}

	if err := c.Repo.Tag(ctx, snap.Desc, tag); err != nil {
		return fmt.Errorf("failed to restore %s to %s: %w", tag, snap.Desc.Digest, err)
	}
	return nil
}

// DeleteRef deletes a reference tag from the OCI registry and updates the _refs index.
func (c *Client) DeleteRef(ctx context.Context, refName string) (err error) {
	if lockErr := c.acquireRefsIndexLock(ctx); lockErr != nil {
		return fmt.Errorf("failed to acquire _refs index lock for deletion: %w", lockErr)
	}
	defer func() {
		if releaseErr := c.releaseRefsIndexLock(ctx); releaseErr != nil {
			err = errors.Join(err, releaseErr)
		}
	}()

	c.ClearManifestCache()

	richRefs, err := c.FetchRichRefIndex(ctx)
	if err != nil {
		if !IsNotFound(err) {
			// An index that exists but cannot be read is not one to rebuild
			// from the tags: the rebuild cannot see truncated tags or any of
			// the metadata, and would publish that loss as the new index.
			return fmt.Errorf("failed to read the _refs index prior to deletion: %w", err)
		}
		refs, listErr := c.ListRefs(ctx)
		if listErr != nil {
			return fmt.Errorf("failed to list refs prior to deletion: %w", listErr)
		}
		richRefs = make(map[string]RefEntry, len(refs))
		for k, v := range refs {
			richRefs[k] = RefEntry{SHA: v}
		}
	}

	delete(richRefs, refName)

	// One tag per ref, so there is exactly one thing to remove.
	for _, targetTag := range []string{RefManifestTag(refName)} {
		if targetTag == "" {
			continue
		}
		c.ClearManifestCache()
		c.forgetRefTag(targetTag)
		desc, err := c.Repo.Resolve(ctx, targetTag)
		switch {
		case err == nil:
			delErr := c.Repo.Delete(ctx, desc)
			switch {
			case delErr == nil:
			case isDeletionUnsupported(delErr):
				// Several hosted registries refuse manifest deletion outright.
				// Leaving the tag as it is would let the tag-enumeration
				// fallback rediscover it and resurrect the ref on the next
				// push, so overwrite it with a tombstone instead: the tag
				// survives, but it no longer describes a ref.
				if tombErr := c.pushRefTombstone(ctx, refName, targetTag); tombErr != nil {
					return fmt.Errorf("registry refuses manifest deletion (%s) and the tombstone for %s could not be written: %w", delErr.Error(), targetTag, tombErr)
				}
			default:
				// Any other failure leaves a live tag behind, which would be
				// rediscovered and resurrect the ref. Report it rather than
				// claiming a deletion that will not stay done.
				return fmt.Errorf("failed to delete tag %s from the registry: %w", targetTag, delErr)
			}
		case IsNotFound(err):
			// Already gone; nothing to delete.
		default:
			return fmt.Errorf("failed to resolve tag %s for deletion: %w", targetTag, err)
		}
	}

	c.ClearManifestCache()

	// Carry HEAD across the deletion. Writing the index without it silently
	// erased the recorded default branch on the first `git push origin :ref`.
	head, headErr := c.currentHead(ctx)
	if headErr != nil {
		return fmt.Errorf("failed to read the recorded HEAD before deleting %s: %w", refName, headErr)
	}
	if _, stillThere := richRefs[head]; head != "" && !stillThere {
		head = ""
	}
	return c.pushRichRefIndexDirect(ctx, richRefs, head)
}

// CommitPush describes one commit publication.
//
// It is a struct rather than a parameter list because the call has ten-odd
// inputs, most of them strings, and a positional call was already impossible to
// read at the call site.
type CommitPush struct {
	// CommitSHA is the commit the manifest is published for.
	CommitSHA string
	// RefName is the full git ref name, e.g. "refs/heads/main". Required
	// whenever WriteRefTag is set.
	RefName string
	// WriteRefTag also publishes a ref manifest under the tag derived from
	// RefName (see RefManifestTag). False publishes only the ref-agnostic
	// commit manifest.
	WriteRefTag bool
	// Parents is the comma-separated list of the commit's git parents. It is
	// metadata; see PackBases for what fetch actually follows.
	Parents string
	// PackBases are the commits the packfile was cut against. Empty means the
	// packfile is self-contained and is recorded as PackBasesNone.
	PackBases []string
	// TagAnnotations are merged into the ref manifest's annotations.
	TagAnnotations map[string]string
	// ExtraLayers are appended after the packfile layer, e.g. LFS blobs.
	ExtraLayers []ocispec.Descriptor
	// Rewrite republishes a commit this client may already have pushed, with
	// different content.
	//
	// The skip-if-already-pushed caches below are keyed on the commit id and
	// the ref name, not on what is being published, which is right for the
	// case they exist for — one push touching the same commit from two
	// branches — and wrong for gc, whose whole job is to replace a commit's
	// packfile with a self-contained one. Without this, a consolidation run by
	// a client that had already pushed that ref silently did nothing.
	Rewrite bool
}

// validate rejects a push that could publish an unfetchable manifest.
func (p *CommitPush) validate() error {
	if !isObjectID(p.CommitSHA) {
		return fmt.Errorf("invalid commit SHA %q: must be a hex object id of 40 (SHA-1) or 64 (SHA-256) characters", p.CommitSHA)
	}
	for _, base := range p.PackBases {
		if !isObjectID(base) {
			return fmt.Errorf("invalid pack base %q for commit %s: must be a hex object id of 40 (SHA-1) or 64 (SHA-256) characters", base, p.CommitSHA)
		}
		if base == p.CommitSHA {
			return fmt.Errorf("commit %s cannot be its own pack base", p.CommitSHA)
		}
	}
	return nil
}

// PushCommitStream pushes a packfile layer stream, config blob, and manifest tagged with p.CommitSHA and,
// when p.WriteRefTag is set, a ref manifest under RefManifestTag(p.RefName).
// If that tag sanitises to a 40-character hex string (e.g. branch named after a SHA or matching p.CommitSHA), the ref manifest
// is tagged with "ref-<sanitisedTag>" so ListRefs can discover it and commitSHA tags remain ref-agnostic.
// If packfileSize > 0, the stream is validated to contain exactly packfileSize bytes.
// If packfileSize <= 0, the stream is read until EOF.
func (c *Client) PushCommitStream(
	ctx context.Context,
	p CommitPush,
	packfileReader io.Reader,
	packfileSize int64,
) error {
	if err := p.validate(); err != nil {
		return err
	}

	reader := io.Reader(packfileReader)
	if packfileSize > 0 {
		limit := packfileSize
		if limit < math.MaxInt64 {
			limit++
		}
		reader = io.LimitReader(packfileReader, limit)
	}

	// rootfs.diff_ids must name the *uncompressed* layer, so it is digested on
	// the way in rather than reusing the layer digest. Those coincide only when
	// OCI_COMPRESSION is unset or "none", which is the default and is why the
	// discrepancy went unnoticed under gzip and zstd.
	//
	// Teeing keeps this streaming: the uncompressed bytes are never held, only
	// hashed as they pass through into the compressor.
	diffDigester := opencontainers.SHA256.Digester()
	reader = io.TeeReader(reader, diffDigester.Hash())

	compMode := c.Compression
	mediaType, err := compressedMediaType(compMode)
	if err != nil {
		return err
	}

	// Stage the packfile on disk rather than in memory. The registry needs its
	// digest and length before the upload can begin, and a packfile is as large
	// as the history it carries.
	blob, rawSize, err := spoolBlob(mediaType, "packfile", func(w io.Writer) (int64, error) {
		cw, _, cErr := CompressStream(w, compMode)
		if cErr != nil {
			return 0, fmt.Errorf("failed to create compression stream writer: %w", cErr)
		}
		n, copyErr := io.Copy(cw, reader)
		if closeErr := cw.Close(); closeErr != nil && copyErr == nil {
			copyErr = fmt.Errorf("compression writer close failed: %w", closeErr)
		}
		if copyErr != nil {
			return 0, fmt.Errorf("failed to write packfile stream: %w", copyErr)
		}
		return n, nil
	})
	if err != nil {
		return err
	}
	defer func() { _ = blob.Close() }()

	// Validate that the raw uncompressed stream length matches packfileSize exactly when packfileSize > 0.
	if packfileSize > 0 {
		if rawSize < packfileSize {
			return fmt.Errorf("packfile size mismatch: expected %d bytes, got %d bytes", packfileSize, rawSize)
		}
		if rawSize > packfileSize {
			return fmt.Errorf("packfile size mismatch: stream exceeds expected size of %d bytes", packfileSize)
		}
	}

	return c.pushCommitArtifacts(ctx, p, blob.desc, diffDigester.Digest(), blob.Reader())
}

// pushCommitArtifacts pushes the packfile layer blob, config blob, commit manifest, and optional ref manifest.
func (c *Client) pushCommitArtifacts(
	ctx context.Context,
	p CommitPush,
	packfileDesc ocispec.Descriptor,
	packfileDiffID opencontainers.Digest,
	packfileReader io.Reader,
) error {
	commitSHA := p.CommitSHA
	refName := p.RefName
	// 0. Fast check: skip only when *both* the ref-agnostic commit manifest and
	// the ref manifest have already been pushed in this process (e.g. a
	// multi-branch push touching the same commit).
	//
	// IsRefFullyPushed takes the ref *name*: it derives the tag itself, the
	// same way the ref manifest below is tagged.
	if !p.Rewrite && c.IsRefFullyPushed(commitSHA, refName) {
		return nil
	}

	// 1. Fast HEAD check: if packfile layer blob already exists on OCI registry, skip blob upload!
	exists, err := c.Repo.Blobs().Exists(ctx, packfileDesc)
	if err != nil || !exists {
		if pushErr := c.pushPackfileBlob(ctx, packfileDesc, packfileReader); pushErr != nil {
			return c.explainAuth(fmt.Errorf("failed to push packfile layer blob stream: %w", pushErr))
		}
	}

	// 2. Push Minimal OCI Image Config Blob for broad registry compatibility.
	// Use "unknown" platform since Git packfiles are platform-agnostic artifacts.
	// RootFS.DiffIDs names the uncompressed layer, which equals the layer digest
	// only when the packfile is stored raw.
	diffID := packfileDiffID
	if diffID == "" {
		diffID = packfileDesc.Digest
	}
	configObj := ocispec.Image{
		Platform: ocispec.Platform{
			Architecture: "unknown",
			OS:           "unknown",
		},
		RootFS: ocispec.RootFS{
			Type:    "layers",
			DiffIDs: []opencontainers.Digest{diffID},
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

	err = c.pushBlobOnce(ctx, configDesc, configBytes)
	if err != nil {
		return fmt.Errorf("failed to push config blob: %w", err)
	}

	// 3a. Create Ref-Agnostic Commit SHA Manifest (contains revision & parents only)
	commitAnnotations := map[string]string{
		ocispec.AnnotationRevision:      commitSHA,
		ocispec.AnnotationTitle:         commitSHA,
		ocispec.AnnotationVendor:        "git-remote-oci",
		ocispec.AnnotationDocumentation: "https://github.com/mrueg/git-remote-oci",
		// Mandatory, including for a self-contained packfile, which says
		// PackBasesNone rather than nothing at all.
		AnnotationGitPackBases: FormatPackBases(p.PackBases),
	}
	if p.Parents != "" {
		commitAnnotations[AnnotationGitParents] = p.Parents
	}
	// The same edge the annotation above records, kept so the _refs push can
	// publish the graph as a whole. Recorded here rather than at the call sites
	// because this is the one place a commit manifest is written, and a chain
	// that disagrees with the annotations is worse than no chain.
	c.recordPackChain(commitSHA, p.PackBases)

	manifestLayers := append([]ocispec.Descriptor{packfileDesc}, p.ExtraLayers...)
	commitManifest := ocispec.Manifest{
		Versioned:   specs.Versioned{SchemaVersion: 2},
		MediaType:   ocispec.MediaTypeImageManifest,
		Config:      configDesc,
		Layers:      manifestLayers,
		Annotations: commitAnnotations,
	}

	commitManifestData, err := json.Marshal(commitManifest)
	if err != nil {
		return fmt.Errorf("failed to marshal commit manifest: %w", err)
	}

	commitManifestDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    opencontainers.FromBytes(commitManifestData),
		Size:      int64(len(commitManifestData)),
	}

	// Push the ref-agnostic manifest tagged with commitSHA unless this client
	// already has. Having *fetched* it does not count -- the cache holds
	// those too, and a manifest read from the registry says nothing about
	// whether the packfile this push carries is the one it describes.
	if _, pushed := c.pushedCommits.Load(commitSHA); p.Rewrite || !pushed {
		err = c.Repo.PushReference(ctx, commitManifestDesc, bytes.NewReader(commitManifestData), commitSHA)
		if err != nil {
			return fmt.Errorf("failed to push commit SHA tag %s: %w", commitSHA, err)
		}
		c.pushedCommits.Store(commitSHA, true)
		c.manifestCache.Store(commitSHA, &commitManifest)
		c.manifestCache.Store(commitManifestDesc.Digest.String(), &commitManifest)
	}

	// 3b. If a ref tag is wanted, push a separate manifest for it containing AnnotationGitRef.
	// To keep the commitSHA tag immutable and prevent collision/overwriting when sanitisedTag == commitSHA,
	// publish the ref-annotated manifest under a "ref-" prefixed tag if a collision occurs.
	if p.WriteRefTag {
		if refName == "" {
			return errors.New("refName cannot be empty when a ref tag is requested")
		}
		// The ref name is what has to map injectively onto a tag.
		targetTag := RefManifestTag(refName)
		if targetTag == "" {
			return fmt.Errorf("ref %q cannot be represented as an OCI tag", refName)
		}

		refAnnotations := map[string]string{
			ocispec.AnnotationRevision:      commitSHA,
			AnnotationGitRef:                refName,
			ocispec.AnnotationTitle:         refName,
			ocispec.AnnotationVendor:        "git-remote-oci",
			ocispec.AnnotationDocumentation: "https://github.com/mrueg/git-remote-oci",
			// Recorded here as well as on the commit manifest: a fetch that
			// resolves a ref goes straight to this manifest and never reads the
			// commit-tagged one.
			AnnotationGitPackBases: FormatPackBases(p.PackBases),
		}
		if p.Parents != "" {
			refAnnotations[AnnotationGitParents] = p.Parents
		}
		for k, v := range p.TagAnnotations {
			if v != "" {
				refAnnotations[k] = v
			}
		}

		refManifest := ocispec.Manifest{
			Versioned:   specs.Versioned{SchemaVersion: 2},
			MediaType:   ocispec.MediaTypeImageManifest,
			Config:      configDesc,
			Layers:      manifestLayers,
			Annotations: refAnnotations,
		}

		refManifestData, err := json.Marshal(refManifest)
		if err != nil {
			return fmt.Errorf("failed to marshal ref manifest: %w", err)
		}

		refManifestDesc := ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageManifest,
			Digest:    opencontainers.FromBytes(refManifestData),
			Size:      int64(len(refManifestData)),
		}

		// Skip the push only when this client already published *this exact
		// manifest* under the tag. The guard used to be "is the tag in the
		// manifest cache at all", which is a question about the tag rather
		// than about its content: once a ref manifest had been pushed, the
		// same client pushing a later commit to the same ref silently did not
		// move the tag. A fresh process per push hid it; anything reusing a
		// client for two pushes of one ref did not.
		digest := refManifestDesc.Digest.String()
		if published, ok := c.refTagDigests.Load(targetTag); p.Rewrite || !ok || published != digest {
			err = c.Repo.PushReference(ctx, refManifestDesc, bytes.NewReader(refManifestData), targetTag)
			if err != nil {
				return fmt.Errorf("failed to push ref tag %s: %w", targetTag, err)
			}
			c.refTagDigests.Store(targetTag, digest)
			c.pushedRefs.Store(targetTag, commitSHA)
			c.manifestCache.Store(targetTag, &refManifest)
			c.manifestCache.Store(digest, &refManifest)
		}
	}

	// The _refs index is deliberately not touched here. It is a
	// read-modify-write shared by every ref in a push, and it is written once
	// per push by the caller through PushRichRefIndexWithHead, under its lock
	// and its optimistic-concurrency check, with only the refs that changed.
	return nil
}

// PushLFSLayer uploads a Git LFS object as a layer.
//
// An LFS object id *is* the SHA-256 of its content, so the descriptor is built
// from the id and the pointer's recorded size rather than by reading the object
// to measure it. The content then streams to the registry through a verifying
// reader.
//
// That verification is the point. The previous implementation digested whatever
// it happened to read and published the blob under *that* digest, so a local
// object that did not match the id it is filed under was uploaded anyway, under
// a digest disagreeing with its own OID annotation. Now the push fails.
//
// It also avoids holding the object in memory, though measurement showed that
// is not what dominates peak usage on a push - go-git's packfile encoder is.
func (c *Client) PushLFSLayer(ctx context.Context, oid string, r io.Reader, size int64) (ocispec.Descriptor, error) {
	cleanOID, err := lfs.ValidateOID(oid)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	if size < 0 {
		return ocispec.Descriptor{}, fmt.Errorf("LFS blob %s has a negative size %d", cleanOID, size)
	}

	desc := ocispec.Descriptor{
		MediaType: lfs.MediaTypeGitLFSBlob,
		Digest:    opencontainers.NewDigestFromEncoded(opencontainers.SHA256, cleanOID),
		Size:      size,
		Annotations: map[string]string{
			lfs.AnnotationLFSOID:  cleanOID,
			lfs.AnnotationLFSSize: strconv.FormatInt(size, 10),
		},
	}

	// VerifyReader fails the read if the content does not hash to desc.Digest
	// or is not exactly desc.Size bytes, so a corrupt local object cannot be
	// published under a name that misdescribes it.
	vr := content.NewVerifyReader(r, desc)
	if err := c.Repo.Blobs().Push(ctx, desc, vr); err != nil {
		if errors.Is(err, errdef.ErrAlreadyExists) {
			return desc, nil
		}
		return desc, fmt.Errorf("failed to push LFS blob layer to OCI registry: %w", err)
	}
	if err := vr.Verify(); err != nil {
		return desc, fmt.Errorf("LFS object %s does not match its object id: %w", cleanOID, err)
	}

	return desc, nil
}
