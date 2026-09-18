package helper

import (
	"context"
	"fmt"
	"strings"

	"github.com/mrueg/git-remote-oci/pkg/git"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// The v2 `object-info` command: how big is this object, without sending it.
//
// A partial clone is the caller. `--filter=blob:limit=1m` has to decide per
// blob whether it wants the thing, and asking costs a size rather than a
// download; `git cat-file --batch-check` against a promisor remote goes the
// same way. Without it a client that only wanted to know has to fetch to find
// out, which for the filter is precisely backwards.
//
// Sizes are published. Each packfile's index (FORMAT.md §4.4) records the size
// of every object in it, so the usual answer costs one index fetch per ref
// searched — a few kilobytes — and no packfile at all. A repository written
// before sizes existed carries an index without them; there the size is found
// by fetching the packfile that holds it, which is the same work a fetch would
// do minus sending the result, and never worse than the fetch the client would
// otherwise have issued.

// v2ObjectInfo serves `command=object-info`.
//
// Request arguments are an attribute list — only `size` is defined, and it is
// the only one advertised — followed by `oid <id>` lines. The response repeats
// the attribute line and then gives one `<oid> <size>` line per request, in the
// order asked.
func (h *Helper) v2ObjectInfo(ctx context.Context, w *pktWriter, req v2Request) error {
	var oids []string
	wantSize := false
	for _, arg := range req.args {
		switch {
		case arg == "size":
			wantSize = true
		case strings.HasPrefix(arg, "oid "):
			oid := strings.TrimSpace(strings.TrimPrefix(arg, "oid "))
			if oid != "" {
				oids = append(oids, oid)
			}
		}
	}

	// Nothing else is advertised, so anything else is git asking for something
	// it was never offered. Answering with sizes it did not request would be a
	// response it cannot parse.
	if !wantSize {
		// sendV2Error terminates the response itself; ending it again put a
		// second flush and response-end on the wire, which git reads as the
		// start of a response to a request it has not made.
		return sendV2Error(w, "protocol v2: object-info supports only the size attribute")
	}
	if len(oids) == 0 {
		if err := w.WriteLine("size"); err != nil {
			return err
		}
		return endResponse(w)
	}

	sizes, err := h.objectSizes(ctx, oids)
	if err != nil {
		// An ERR packet rather than a dropped connection: git reports the
		// former as the remote's own message and the latter as "the remote end
		// hung up unexpectedly", which names neither the cause nor the side.
		return sendV2Error(w, fmt.Sprintf("protocol v2: %v", err))
	}

	if err := w.WriteLine("size"); err != nil {
		return err
	}
	for _, oid := range oids {
		if err := w.WriteLine("%s %d", oid, sizes[oid]); err != nil {
			return err
		}
	}
	return endResponse(w)
}

// objectSizes resolves the size of every requested object, fetching whatever it
// takes to be able to.
//
// The local store is tried first and often answers: an object-info request
// during a partial clone frequently asks about things a previous fetch already
// brought down. Only what is left drives a staging pass, which is the same
// search a lazy fetch performs — ref by ref, skipping any whose published pack
// index rules it out.
func (h *Helper) objectSizes(ctx context.Context, oids []string) (map[string]int64, error) {
	if err := h.ensureGitRepo(); err != nil {
		return nil, err
	}

	// `size` is a fixed cost per object, so there is no progress worth
	// narrating and no filter to apply: the client asked about these
	// specifically, and skipping one would leave a line out of the response.
	st, cleanup, err := h.newStagingArea("", false)
	if cleanup != nil {
		// Deferred before the error check: a staging directory can exist by
		// the time the error is reported, and the cleanup returned alongside
		// is the only handle on it.
		defer cleanup()
	}
	if err != nil {
		return nil, err
	}

	store, err := git.OpenObjectStore(st.dir)
	if err != nil {
		return nil, fmt.Errorf("could not read the staged object store: %w", err)
	}
	sizes, missing := git.ObjectSizes(store, oids)
	if len(missing) == 0 {
		return sizes, nil
	}

	// The published indexes, before any packfile. This is the path that makes
	// the command worth having: it reads tens of kilobytes and transfers no
	// history.
	if found := h.sizesFromPackIndexes(ctx, missing); len(found) > 0 {
		remaining := missing[:0]
		for _, oid := range missing {
			if size, ok := found[oid]; ok {
				sizes[oid] = size
				continue
			}
			remaining = append(remaining, oid)
		}
		missing = remaining
		if len(missing) == 0 {
			return sizes, nil
		}
	}

	h.logVerbose("git-remote-oci: [verbose] object-info: %d of %d objects are not local and have no published size; searching\n",
		len(missing), len(oids))
	if err := h.stageUntilFound(ctx, missing, st, errWantNotServed); err != nil {
		return nil, err
	}

	// Re-open: the store cached the pack list it found when it was opened, and
	// staging has added to it since.
	staged, err := git.OpenObjectStore(st.dir)
	if err != nil {
		return nil, fmt.Errorf("could not re-read the staged object store: %w", err)
	}
	found, stillMissing := git.ObjectSizes(staged, missing)
	if len(stillMissing) > 0 {
		return nil, fmt.Errorf("object-info: %s is not something this remote can serve", stillMissing[0])
	}
	for oid, size := range found {
		sizes[oid] = size
	}
	return sizes, nil
}

// sizesFromPackIndexes answers from the published indexes alone.
//
// It walks refs in the same order a lazy fetch searches them, and for each one
// reads the index of every packfile in its graph. An index that records sizes
// (FORMAT.md §4.4) answers outright; one that does not is skipped without being
// fetched at all, because the manifest says which kind it is.
//
// Whatever is not found here is not an error — it is a repository written
// before sizes existed, or an object on no ref this searched. The caller falls
// back to staging.
func (h *Helper) sizesFromPackIndexes(ctx context.Context, oids []string) map[string]int64 {
	defer h.timer.phase("read published sizes")()

	refs := mustRefs(ctx, h)
	if len(refs) == 0 {
		return nil
	}

	found := make(map[string]int64, len(oids))
	wanted := make(map[string]bool, len(oids))
	for _, oid := range oids {
		wanted[oid] = true
	}

	for _, name := range h.refsByLikelihood(ctx, refs) {
		if len(wanted) == 0 {
			break
		}
		entry := refs[name]
		if entry.SHA == "" {
			continue
		}
		graph, err := h.resolvePackGraph(ctx, []fetchSpec{{sha: entry.SHA, ref: name}}, false)
		if err != nil {
			continue
		}
		for _, manifest := range graph.manifests {
			if len(wanted) == 0 {
				break
			}
			// Checked from the manifest, so a v1 index is never downloaded to
			// discover it has no sizes in it. That check is the whole reason
			// sizes got their own media type rather than a wider v1.
			if !oci.PackIndexRecordsSizes(manifest) {
				continue
			}
			index, ok := h.ociClient.FetchPackIndex(ctx, manifest)
			if !ok {
				continue
			}
			for oid := range wanted {
				if size, has := oci.PackIndexSize(index, oid); has {
					found[oid] = size
					delete(wanted, oid)
				}
			}
		}
	}

	if len(found) > 0 {
		h.logVerbose("git-remote-oci: [verbose] object-info: %d size(s) answered from published indexes\n", len(found))
	}
	return found
}
