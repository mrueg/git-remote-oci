package oci

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	opencontainers "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// The shape of the pack-base graph, published in one place.
//
// Every push writes a thin packfile cut against the previous push's tip, so
// `io.git-remote-oci.pack-bases` (§4.2) forms a chain: push N names push N-1,
// which names push N-2. A reader that learns the chain by reading those
// annotations learns exactly one link per request, and the links are strictly
// sequential — there is nothing to parallelise, because the reader does not
// know what to ask for next until the current answer arrives.
//
// That makes clone latency linear in the number of pushes since the last gc. A
// repository with five hundred pushes costs five hundred sequential round trips
// before a single byte of packfile moves, which on a 100ms link is most of a
// minute of doing nothing. The 50000-manifest ceiling in the fetch path, and
// the "run gc" advice in the error when it is hit, are both symptoms of this.
//
// So the chain is published as a whole, on the `_refs` manifest that every
// operation already reads. A reader that has it knows the full set of manifests
// it needs up front and fetches them in one parallel wave.

// MediaTypePackChain is the layer holding the whole pack-base graph.
const MediaTypePackChain = "application/vnd.git.repository.packchain.v1+json"

// FetchPackChain reads the published pack-base graph: commit id to the commits
// its packfile was cut against.
//
// ok is false when the repository publishes no chain, which every repository
// written before this layer existed does. The chain is an accelerator and
// nothing depends on it being present.
//
// The result is cached for the life of the client. It is read on the fetch path
// and again when a push republishes it, and re-reading it would spend the round
// trip this exists to save. Only an answer is cached -- a chain, or the
// knowledge that there is none. A read that failed is not an answer, and
// remembering it as "no chain" is what made a push after a momentary registry
// error republish the chain with every edge but its own missing.
func (c *Client) FetchPackChain(ctx context.Context) (map[string][]string, bool) {
	chain, err := c.packChainCached(ctx)
	if err != nil {
		return map[string][]string{}, false
	}
	return chain, len(chain) > 0
}

// packChainCached is FetchPackChain with the failure kept apart from the
// absence, for the one caller that must treat them differently.
func (c *Client) packChainCached(ctx context.Context) (map[string][]string, error) {
	if cached, ok := c.packChain.Load().(map[string][]string); ok {
		return cached, nil
	}

	chain, err := c.fetchPackChainUncached(ctx)
	if err != nil {
		return nil, err
	}
	c.packChain.Store(chain)
	return chain, nil
}

// fetchPackChainUncached reads the published chain. An empty map with no error
// means the repository publishes none; an error means it could not be read,
// which says nothing about whether it exists.
func (c *Client) fetchPackChainUncached(ctx context.Context) (map[string][]string, error) {
	manifest, err := c.FetchManifest(ctx, TagRefIndex)
	if err != nil {
		if IsNotFound(err) {
			return map[string][]string{}, nil
		}
		return nil, fmt.Errorf("failed to read the _refs manifest for the pack chain: %w", err)
	}

	desc, found := packChainLayerOf(manifest)
	if !found {
		return map[string][]string{}, nil
	}

	data, err := c.fetchBlobBytes(ctx, desc, maxMetadataBytes, "the pack chain")
	if err != nil {
		if IsNotFound(err) {
			// A manifest naming a blob the registry no longer has. That is a
			// damaged repository rather than an unreachable one, and there is
			// no chain to be had from it.
			return map[string][]string{}, nil
		}
		return nil, fmt.Errorf("failed to read the pack chain: %w", err)
	}

	return decodePackChain(data), nil
}

// decodePackChain parses a chain blob, and yields an empty chain for one that
// cannot be parsed.
//
// That is deliberately not an error. Every use of the chain is an optimisation
// over reading the annotations, so a chain that is unreadable costs round
// trips and nothing else, the same as a chain that was never published.
func decodePackChain(data []byte) map[string][]string {
	var chain map[string][]string
	if err := json.Unmarshal(data, &chain); err != nil {
		return map[string][]string{}
	}
	return sanitisePackChain(chain)
}

// packChainLayerOf finds the chain layer on a _refs manifest.
func packChainLayerOf(manifest *ocispec.Manifest) (ocispec.Descriptor, bool) {
	for i := range manifest.Layers {
		if manifest.Layers[i].MediaType == MediaTypePackChain {
			return manifest.Layers[i], true
		}
	}
	return ocispec.Descriptor{}, false
}

// sanitisePackChain drops anything that is not a pair of object ids.
//
// Everything in here came out of a blob the registry served, and every id in it
// becomes a tag on the next request. ParsePackBases validates the annotation
// form for exactly that reason -- "these become tag names on the next request,
// so they are validated here rather than at the point of use" -- and the chain
// is the same data arriving by a different route, so it gets the same
// treatment at the same boundary.
//
// Dropping rather than rejecting: an entry that cannot be trusted is an entry
// the reader has no shortcut for, which puts it back on the annotation walk.
// That is the same outcome as a chain that never mentioned it.
func sanitisePackChain(chain map[string][]string) map[string][]string {
	clean := make(map[string][]string, len(chain))
	for sha, bases := range chain {
		if !isObjectID(sha) {
			continue
		}
		checked := make([]string, 0, len(bases))
		bad := false
		for _, base := range bases {
			if !isObjectID(base) {
				bad = true
				break
			}
			checked = append(checked, base)
		}
		if bad {
			// A partially readable edge list is worse than none: it would look
			// like a complete answer with a base missing, which is the one
			// thing the chain must never do.
			continue
		}
		clean[sha] = checked
	}
	return clean
}

// recordPackChain notes that commitSHA's packfile was cut against bases, so the
// next `_refs` push can publish it.
//
// An empty bases list is recorded, not skipped: "this packfile stands alone" is
// where a reader stops walking, and leaving it out would be indistinguishable
// from a commit the chain says nothing about.
func (c *Client) recordPackChain(commitSHA string, bases []string) {
	if commitSHA == "" {
		return
	}
	recorded := make([]string, 0, len(bases))
	recorded = append(recorded, bases...)
	c.packChainEdges.Store(commitSHA, recorded)
}

// ResetPackChain discards the published chain, so the next `_refs` push writes
// only the edges recorded since.
//
// This is for gc. Consolidation rewrites every ref as a self-contained packfile
// and prunes the intermediate commit manifests, so every edge in the old chain
// names a manifest that is about to stop existing. Merging with it would carry
// that wreckage forward for the life of the repository, which is the opposite
// of what the run was for.
func (c *Client) ResetPackChain() {
	c.packChainReset.Store(true)
	c.packChain.Store(map[string][]string{})
	// Edges this client recorded earlier go too. A client that pushed and then
	// ran gc in the same process would otherwise carry its own pushes' edges
	// past the consolidation that superseded them -- rare in production, where
	// gc is its own command, and exactly what a test doing both would hit.
	c.packChainEdges.Range(func(k, _ any) bool {
		c.packChainEdges.Delete(k)
		return true
	})
}

// packChainLayer builds the chain blob to attach to the `_refs` manifest, and
// returns ok=false when there is nothing to publish.
func (c *Client) packChainLayer(ctx context.Context) (ocispec.Descriptor, bool) {
	merged := map[string][]string{}
	if !c.packChainReset.Load() {
		published, err := c.packChainCached(ctx)
		if err != nil {
			// The published chain could not be read, so it cannot be merged
			// with, and publishing this client's edges alone would replace
			// the whole graph with a fragment of it. Carrying the previous
			// layer forward unchanged loses only this push's edges, which a
			// reader recovers from the annotations; losing the rest would
			// cost every clone the round trips the chain exists to save.
			c.warnf("the published pack chain could not be read and is left as it was; "+
				"the edges from this push are not in it: %v", err)
			return c.publishedPackChainLayer(ctx)
		}
		for sha, bases := range published {
			merged[sha] = bases
		}
	}
	c.packChainEdges.Range(func(k, v any) bool {
		sha, _ := k.(string)
		bases, _ := v.([]string)
		merged[sha] = bases
		return true
	})
	// Publishing only what a reader would accept, so a malformed entry is a
	// bug caught here rather than a shortcut silently lost at every clone.
	merged = sanitisePackChain(merged)
	if len(merged) == 0 {
		return ocispec.Descriptor{}, false
	}

	// Sorted, so the same graph produces the same digest and a re-push of an
	// unchanged repository does not upload a new blob. Go's map iteration would
	// otherwise make every push look like a change.
	for _, bases := range merged {
		sort.Strings(bases)
	}
	// encoding/json sorts map keys, so an unchanged graph marshals to the same
	// bytes and the same digest, and the registry skips the upload.
	data, err := json.Marshal(merged)
	if err != nil {
		return ocispec.Descriptor{}, false
	}

	desc := ocispec.Descriptor{
		MediaType: MediaTypePackChain,
		Digest:    opencontainers.FromBytes(data),
		Size:      int64(len(data)),
	}
	if err := c.pushBlobOnce(ctx, desc, data); err != nil {
		// The chain is an accelerator. A push that could not publish it is
		// still a correct push, and failing here would trade a working push
		// for a faster clone.
		return ocispec.Descriptor{}, false
	}
	// What was just published is what a reader would now get, so the cache can
	// be brought forward rather than invalidated.
	c.packChain.Store(merged)
	return desc, true
}

// publishedPackChainLayer returns the chain layer descriptor currently on the
// _refs manifest, for a push that has to carry it forward without having been
// able to read the chain itself. ok is false when there is none, or when the
// manifest cannot be read either -- in which case the new _refs goes out
// without a chain, and the next push that can read the registry starts one.
func (c *Client) publishedPackChainLayer(ctx context.Context) (ocispec.Descriptor, bool) {
	manifest, err := c.FetchManifest(ctx, TagRefIndex)
	if err != nil {
		return ocispec.Descriptor{}, false
	}
	return packChainLayerOf(manifest)
}
