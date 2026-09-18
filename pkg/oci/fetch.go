package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
)

// Reading manifests and blobs back from the registry, verified against what
// the registry claims they are.

// FetchManifest fetches an OCI image manifest by tag or digest, utilising the manifest cache if available.
func (c *Client) FetchManifest(ctx context.Context, tagOrDigest string) (*ocispec.Manifest, error) {
	if tagOrDigest != "" && tagOrDigest != TagRefIndex && tagOrDigest != TagOCIIndex {
		if cached, ok := c.manifestCache.Load(tagOrDigest); ok {
			if manifest, isManifest := cached.(*ocispec.Manifest); isManifest {
				return manifest, nil
			}
		}
	}

	desc, data, err := c.fetchManifestBytes(ctx, tagOrDigest, "a manifest")
	if err != nil {
		return nil, err
	}

	// Validate media type descriptor up front (parsing media type to strip parameters like "; charset=utf-8")
	mediaType, _, err := mime.ParseMediaType(desc.MediaType)
	if err != nil {
		mediaType = strings.TrimSpace(strings.Split(desc.MediaType, ";")[0])
	}
	if mediaType != ocispec.MediaTypeImageManifest && mediaType != "application/vnd.docker.distribution.manifest.v2+json" {
		return nil, fmt.Errorf("%w: reference %s has media type %s", ErrNotAnImageManifest, tagOrDigest, desc.MediaType)
	}

	var manifest ocispec.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("%w: failed to unmarshal manifest for %s: %w", ErrNotAnImageManifest, tagOrDigest, err)
	}

	if tagOrDigest != "" {
		c.manifestCache.Store(tagOrDigest, &manifest)
	}
	if digestStr := desc.Digest.String(); digestStr != "" {
		c.manifestCache.Store(digestStr, &manifest)
	}

	return &manifest, nil
}

// FetchPackfileStream returns a ReadCloser stream of the packfile layer content from an OCI manifest.
func (c *Client) FetchPackfileStream(ctx context.Context, manifest *ocispec.Manifest) (io.ReadCloser, error) {
	var packfileDesc *ocispec.Descriptor
	for i := range manifest.Layers {
		if isSnapshotLayer(manifest.Layers[i]) {
			continue
		}
		if isPackfileMediaType(baseMediaType(manifest.Layers[i].MediaType)) {
			packfileDesc = &manifest.Layers[i]
			break
		}
	}
	if packfileDesc == nil {
		return nil, fmt.Errorf("no valid git packfile layer found in manifest")
	}

	rc, err := c.fetchBlob(ctx, *packfileDesc)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch packfile layer blob: %w", err)
	}
	return DecompressStream(rc, packfileDesc.MediaType)
}

// fetchBlob opens a blob for streaming, verified against its descriptor.
//
// The registry client checks the digest *header* against the descriptor and
// nothing else; the body it hands back is whatever the server sent. That is
// the wrong place to trust a registry: a body that does not hash to its
// digest -- a corrupted object store, a truncated response, a proxy serving
// something else under the same URL -- would otherwise reach `git index-pack`
// as a packfile that claims to be one this repository published. The reader
// returned here fails, instead of reporting EOF, when the bytes it delivered
// do not match the digest or the size the descriptor declared.
func (c *Client) fetchBlob(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	if desc.Size < 0 {
		return nil, fmt.Errorf("blob %s declares a negative size %d", desc.Digest, desc.Size)
	}
	rc, err := c.Repo.Fetch(ctx, desc)
	if err != nil {
		return nil, err
	}
	return &verifiedReadCloser{vr: content.NewVerifyReader(rc, desc), closer: rc}, nil
}

// verifiedReadCloser turns oras's VerifyReader, which has to be asked to
// verify, into a stream that verifies itself at the end.
//
// Consumers of a packfile stream read to EOF and never see the descriptor, so
// the check has to happen where they would otherwise see io.EOF. A mismatch
// is returned in its place, and Close does not re-report it: the consumer has
// already been told.
type verifiedReadCloser struct {
	vr     *content.VerifyReader
	closer io.Closer
}

func (v *verifiedReadCloser) Read(p []byte) (int, error) {
	n, err := v.vr.Read(p)
	if errors.Is(err, io.EOF) {
		if verifyErr := v.vr.Verify(); verifyErr != nil {
			return n, fmt.Errorf("blob content does not match its descriptor: %w", verifyErr)
		}
	}
	return n, err
}

func (v *verifiedReadCloser) Close() error {
	return v.closer.Close()
}

// fetchBlobBytes reads a whole blob into memory, verified and bounded.
//
// what names the blob for error messages. limit is the most this caller is
// willing to hold regardless of what the descriptor says; a descriptor
// declaring more than that, or declaring no size at all, is refused rather
// than read to find out. Zero is not "unknown" here: every descriptor this
// format writes carries a size, and a blob of no bytes cannot be any of the
// documents this is used for.
func (c *Client) fetchBlobBytes(ctx context.Context, desc ocispec.Descriptor, limit int64, what string) ([]byte, error) {
	if desc.Size <= 0 {
		return nil, fmt.Errorf("%s declares no size", what)
	}
	if desc.Size > limit {
		return nil, fmt.Errorf("%s declares %d bytes, above the %d-byte limit this reads", what, desc.Size, limit)
	}
	if err := desc.Digest.Validate(); err != nil {
		return nil, fmt.Errorf("%s has an unusable digest %q: %w", what, desc.Digest, err)
	}

	rc, err := c.Repo.Fetch(ctx, desc)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	data, err := readMetadataBlob(rc, desc.Size, what)
	if err != nil {
		return nil, err
	}
	if err := verifyBytes(data, desc); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return data, nil
}

// fetchManifestBytes reads a manifest by tag or digest, verified against the
// digest the registry reports for it.
//
// A manifest fetched by digest is content-addressed and the registry client
// checks only the response *header* names that digest; one fetched by tag is
// whatever the registry chose to send. Either way, what is acted on here is
// the body, so the body is what is checked: an index that does not hash to
// what the registry says it is, is not the index the registry says it is.
func (c *Client) fetchManifestBytes(ctx context.Context, tagOrDigest, what string) (ocispec.Descriptor, []byte, error) {
	desc, rc, err := c.Repo.FetchReference(ctx, tagOrDigest)
	if err != nil {
		if IsNotFound(err) {
			return ocispec.Descriptor{}, nil, fmt.Errorf("%w: %s: %w", ErrManifestNotFound, tagOrDigest, err)
		}
		return ocispec.Descriptor{}, nil, c.explainAuth(fmt.Errorf("failed to fetch reference %s: %w", tagOrDigest, err))
	}
	defer func() { _ = rc.Close() }()

	data, err := readMetadataBlob(rc, desc.Size, what)
	if err != nil {
		return ocispec.Descriptor{}, nil, fmt.Errorf("failed to read %s: %w", what, err)
	}
	if err := verifyBytes(data, desc); err != nil {
		return ocispec.Descriptor{}, nil, fmt.Errorf("%s %s: %w", what, tagOrDigest, err)
	}
	return desc, data, nil
}

// verifyBytes checks a whole buffer against its descriptor.
//
// An empty digest is accepted as "nothing was claimed": FetchReference can
// hand back a descriptor without one when the registry sends neither a
// digest header nor a content length, and there is then nothing to check
// against. A digest in an algorithm this build cannot compute is refused, not
// skipped -- it claims something and the claim cannot be tested.
func verifyBytes(data []byte, desc ocispec.Descriptor) error {
	if desc.Digest == "" {
		return nil
	}
	if err := desc.Digest.Validate(); err != nil {
		return fmt.Errorf("unusable digest %q: %w", desc.Digest, err)
	}
	if !desc.Digest.Algorithm().Available() {
		return fmt.Errorf("digest algorithm %s is not available", desc.Digest.Algorithm())
	}
	if desc.Size > 0 && int64(len(data)) != desc.Size {
		return fmt.Errorf("content is %d bytes, the descriptor declares %d", len(data), desc.Size)
	}
	if got := desc.Digest.Algorithm().FromBytes(data); got != desc.Digest {
		return fmt.Errorf("content hashes to %s, the descriptor declares %s", got, desc.Digest)
	}
	return nil
}

// FetchLFSLayer fetches an LFS binary layer stream from the OCI registry by
// descriptor. The stream fails at the end, rather than reporting EOF, if the
// bytes do not match the descriptor; see fetchBlob.
func (c *Client) FetchLFSLayer(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	return c.fetchBlob(ctx, desc)
}
