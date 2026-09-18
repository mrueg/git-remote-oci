package gc

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mrueg/git-remote-oci/pkg/git"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// Compaction from the registry alone.
//
// Consolidation builds each ref's replacement packfile out of git objects, and
// those objects used to have to be sitting in a local clone: gc refused unless
// every ref tip was present. That made the one job most worth automating the
// one job that could not be — compaction is a maintenance task that wants to run
// on a schedule next to the registry, and instead it needed a machine holding a
// full clone of every repository it was to look after.
//
// The objects are in the registry, which is the whole point of the registry. So
// when they are not local, they are fetched into a scratch repository and the
// consolidated packs are built from that. The scratch store is thrown away
// afterwards; nothing is written to any repository the user has.

// hydrated is a scratch repository holding objects pulled from the registry.
type hydrated struct {
	repo *git.Repository
	dir  string
}

// Close removes the scratch store.
func (h *hydrated) Close() {
	if h == nil || h.dir == "" {
		return
	}
	_ = os.RemoveAll(h.dir)
}

// hydrate builds a scratch repository containing every object the named refs
// need, by fetching their packfiles from the registry.
//
// It needs room for as much history as it fetches. That goes in the system
// temporary directory, so $TMPDIR is how to put it somewhere with space when
// the default is a small tmpfs — the same consideration the push path has, for
// the same reason.
func hydrate(ctx context.Context, client *oci.Client, refs map[string]oci.RefEntry, logf func(string, ...any)) (*hydrated, error) {
	dir, err := os.MkdirTemp("", "git-remote-oci-gc-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create a scratch object store: %w", err)
	}
	h := &hydrated{dir: dir}

	if _, err := gogit.PlainInit(dir, true); err != nil {
		h.Close()
		return nil, fmt.Errorf("failed to initialise the scratch object store: %w", err)
	}
	// Only for the subprocess helpers, which is all the import path uses. The
	// store is re-opened once everything is in it, because go-git caches the
	// pack list it found when it opened and would not see the imports.
	writer, err := git.OpenRepositoryAt(dir)
	if err != nil {
		h.Close()
		return nil, err
	}

	order, err := manifestOrder(ctx, client, refs)
	if err != nil {
		h.Close()
		return nil, err
	}

	logf("fetching %d packfile(s) from the registry to repack from\n", len(order))
	for _, manifest := range order {
		stream, streamErr := client.FetchPackfileStream(ctx, manifest.manifest)
		if streamErr != nil {
			h.Close()
			return nil, fmt.Errorf("failed to fetch the packfile for %s: %w", short(manifest.sha), streamErr)
		}
		_, importErr := writer.ImportPackfile(stream)
		_ = stream.Close()
		if importErr != nil {
			h.Close()
			return nil, fmt.Errorf("failed to import the packfile for %s: %w", short(manifest.sha), importErr)
		}
	}

	reader, err := git.OpenRepositoryAt(dir)
	if err != nil {
		h.Close()
		return nil, err
	}
	h.repo = reader
	return h, nil
}

// stagedManifest is a commit manifest and the commit it belongs to.
type stagedManifest struct {
	sha      string
	manifest *ocispec.Manifest
}

// manifestOrder resolves the pack-base graph for the given tips and returns the
// manifests in an order safe to import: a packfile is thin, and `index-pack
// --fix-thin` can only complete it once the objects it deltas against are
// already in the store, so bases come before the packs cut against them.
//
// That is a topological order, and it is produced as one: a depth-first walk
// that lists a manifest only after everything it was packed against. The
// breadth-first levels this replaced were not -- a tip that is also another
// tip's base sat in the first level with it, and the order within a level was
// whatever the map iteration gave -- so a merge whose branch was also pushed
// as a ref imported the merge first, half the time, and index-pack refused it.
func manifestOrder(ctx context.Context, client *oci.Client, refs map[string]oci.RefEntry) ([]stagedManifest, error) {
	chain, _ := client.FetchPackChain(ctx)

	// A tip is addressed by its commit manifest, which carries the same
	// layers as the ref manifest (§5) and is what every base points at. An
	// annotated tag is the exception: its packfile holds the tag object, and
	// only the ref manifest describes that one -- nothing is tagged with the
	// tag object's id, so asking for it by that id finds nothing. Those start
	// from the ref manifest, the way a fetch does.
	type tip struct{ key, chainKey string }
	var tips []tip
	seenTip := map[string]bool{}
	for refName, entry := range refs {
		if entry.SHA == "" {
			continue
		}
		start := tip{key: entry.SHA, chainKey: entry.SHA}
		if entry.TagObject != "" {
			start.key = refKeyPrefix + refName
		}
		if seenTip[start.key] {
			continue
		}
		seenTip[start.key] = true
		tips = append(tips, start)
	}
	// Sorted so the same repository hydrates the same way twice, which is worth
	// having when an import fails and the run has to be understood.
	sort.Slice(tips, func(i, j int) bool { return tips[i].key < tips[j].key })

	const (
		unvisited = iota
		visiting
		done
	)
	state := map[string]int{}
	var order []stagedManifest

	var visit func(key, chainKey string, path []string) error
	visit = func(key, chainKey string, path []string) error {
		switch state[key] {
		case done:
			return nil
		case visiting:
			// Registry content is untrusted and nothing validates the graph,
			// so a cycle is something a reader has to survive rather than
			// recurse into.
			return fmt.Errorf("pack bases form a cycle: %s -> %s",
				strings.Join(shorten(path), " -> "), short(key))
		}
		state[key] = visiting

		manifest, err := fetchStagedManifest(ctx, client, key)
		if err != nil {
			return err
		}
		bases, err := oci.ParsePackBases(manifest.Annotations)
		if err != nil {
			return fmt.Errorf("%s: %w", short(key), err)
		}
		// The published chain (§6.1) is only a shortcut for discovering the
		// graph in fewer round trips; the annotation above is what decides
		// what has to be imported, so anything the chain adds beyond it is
		// extra history rather than a correction.
		for _, base := range append(bases, chain[chainKey]...) {
			if err := visit(base, base, append(path, key)); err != nil {
				return err
			}
		}

		state[key] = done
		order = append(order, stagedManifest{sha: key, manifest: manifest})
		return nil
	}

	for _, start := range tips {
		if err := visit(start.key, start.chainKey, nil); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// refKeyPrefix marks a manifest addressed by ref name rather than commit id in
// the hydration walk.
const refKeyPrefix = "ref:"

// fetchStagedManifest reads the manifest a hydration key names: a ref manifest
// for a ref-prefixed key, the commit manifest otherwise.
func fetchStagedManifest(ctx context.Context, client *oci.Client, key string) (*ocispec.Manifest, error) {
	refName, isRef := strings.CutPrefix(key, refKeyPrefix)
	if !isRef {
		manifest, err := client.FetchManifest(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch the manifest for %s: %w", short(key), err)
		}
		return manifest, nil
	}
	desc, err := client.ResolveRefManifest(ctx, refName)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve the ref manifest for %s: %w", refName, err)
	}
	manifest, err := client.FetchManifest(ctx, desc.Digest.String())
	if err != nil {
		return nil, fmt.Errorf("failed to fetch the ref manifest for %s: %w", refName, err)
	}
	return manifest, nil
}

// shorten abbreviates each id in a path for a message.
func shorten(path []string) []string {
	out := make([]string, 0, len(path))
	for _, sha := range path {
		out = append(out, short(sha))
	}
	return out
}

// missingLocally reports which of the given refs the repository cannot repack
// from its own objects, and why.
//
// Having the tip commit is not enough. A shallow clone has every tip and none
// of the history behind the boundary, and `git pack-objects` stops at that
// boundary without a word -- so a consolidated pack built from one is a
// truncated pack that looks complete, and the pruning that follows deletes the
// only copies of what it left out. A partial clone is the same hazard with a
// different shape: any object may be missing until it is asked for. Neither
// can be trusted to hold a ref's whole history, so from either every ref is
// treated as absent and the history comes from the registry, which has it.
func missingLocally(repo *git.Repository, refs map[string]oci.RefEntry) (missing []string, why string) {
	everything := func() []string {
		names := make([]string, 0, len(refs))
		for name, entry := range refs {
			if entry.SHA != "" {
				names = append(names, name)
			}
		}
		return names
	}

	switch {
	case repo == nil:
		return everything(), "no local repository"
	case repo.IsShallow():
		return everything(), "the local repository is a shallow clone, whose history is truncated"
	case repo.IsPartial():
		return everything(), "the local repository is a partial clone, which may be missing objects"
	}

	for name, entry := range refs {
		if entry.SHA == "" {
			continue
		}
		if _, err := repo.GetCommitInfo(plumbing.NewHash(entry.SHA)); err != nil {
			missing = append(missing, name)
		}
	}
	return missing, "their tips are not in the local repository"
}
