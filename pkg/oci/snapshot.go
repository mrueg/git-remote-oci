package oci

import (
	"context"
	"fmt"
	"io"
	"mime"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// AnnotationSnapshot marks a layer as a self-contained snapshot of a ref tip.
//
// A shallow clone needs the boundary commit's *complete* tree, and the stored
// packfiles are incremental: each one carries only what the previous ones did
// not, so reconstructing the tip means fetching the whole chain. Compacting
// with gc collapses that chain to one packfile but not to fewer bytes — the
// consolidated pack still contains the entire history, so `--depth 1` costs
// what a full clone costs.
//
// The snapshot is the missing artifact: a packfile holding exactly the objects
// reachable from the tip commit, with no ancestry at all. A depth-1 clone
// fetches it and nothing else.
//
// It duplicates the tip's tree, which is the price. Publishing it is optional
// and controlled by the ociremote.shallowSnapshot config key.
const AnnotationSnapshot = "io.git-remote-oci.snapshot"

// PushSnapshotLayer uploads a self-contained tip packfile and returns its
// descriptor, ready to be attached to a manifest as an extra layer.
func (c *Client) PushSnapshotLayer(ctx context.Context, commitSHA string, r io.Reader, size int64) (ocispec.Descriptor, error) {
	mediaType, err := compressedMediaType(c.Compression)
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	blob, _, err := spoolBlob(mediaType, "snapshot", func(w io.Writer) (int64, error) {
		cw, _, cErr := CompressStream(w, c.Compression)
		if cErr != nil {
			return 0, fmt.Errorf("failed to create the snapshot compression stream: %w", cErr)
		}
		n, copyErr := io.Copy(cw, r)
		if closeErr := cw.Close(); closeErr != nil && copyErr == nil {
			copyErr = closeErr
		}
		return n, copyErr
	})
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("failed to stage the snapshot packfile: %w", err)
	}
	defer func() { _ = blob.Close() }()

	desc := blob.desc
	desc.Annotations = map[string]string{
		AnnotationSnapshot:         "true",
		ocispec.AnnotationRevision: commitSHA,
	}

	// Already present is the normal case for an unchanged tip.
	exists, existsErr := c.Repo.Blobs().Exists(ctx, desc)
	if existsErr != nil || !exists {
		if err := c.Repo.Push(ctx, desc, blob.Reader()); err != nil {
			return ocispec.Descriptor{}, c.explainAuth(fmt.Errorf("failed to push the snapshot layer: %w", err))
		}
	}
	return desc, nil
}

// SnapshotLayer returns the manifest's snapshot layer, if it has one.
func SnapshotLayer(manifest *ocispec.Manifest) (ocispec.Descriptor, bool) {
	if manifest == nil {
		return ocispec.Descriptor{}, false
	}
	for i := range manifest.Layers {
		if isSnapshotLayer(manifest.Layers[i]) {
			return manifest.Layers[i], true
		}
	}
	return ocispec.Descriptor{}, false
}

// FetchSnapshotStream reads a snapshot layer as an uncompressed packfile. The
// stream fails at the end, rather than reporting EOF, if the bytes do not match
// the descriptor; see fetchBlob.
func (c *Client) FetchSnapshotStream(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	rc, err := c.fetchBlob(ctx, desc)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch the snapshot layer: %w", err)
	}
	return DecompressStream(rc, desc.MediaType)
}

// isSnapshotLayer reports whether a layer is a tip snapshot rather than the
// ref's incremental packfile.
//
// A snapshot carries a packfile media type, so anything looking for "the
// packfile" has to exclude it explicitly rather than relying on the snapshot
// being appended after it.
func isSnapshotLayer(desc ocispec.Descriptor) bool {
	return desc.Annotations[AnnotationSnapshot] == "true"
}

// baseMediaType strips any parameters from a layer's media type.
func baseMediaType(raw string) string {
	mt, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return strings.TrimSpace(strings.Split(raw, ";")[0])
	}
	return mt
}
