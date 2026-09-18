// Package gc compacts a repository stored in an OCI registry.
//
// Every push writes its own packfile artifact and its own commit-SHA tag, and
// nothing ever removes them. A repository pushed to over a long period
// therefore accumulates one packfile and one tag per push: cloning it means
// fetching every one of those packfiles and running git index-pack once per
// pack, and the registry's tag list grows without bound.
//
// Compaction rewrites each ref as a single self-contained packfile and then
// removes the intermediate commit manifests that are no longer reachable, plus
// any lock tags that have been released or have expired.
//
// The consolidation has to happen before the pruning. Fetch walks a commit's
// parents and asks the registry for each one by id, so deleting intermediate
// commit tags while the packfiles are still per-push deltas would leave a
// repository that cannot be cloned.
package gc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/mrueg/git-remote-oci/pkg/git"
	"github.com/mrueg/git-remote-oci/pkg/oci"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Options controls a collection run.
type Options struct {
	// DryRun reports what would change without modifying the registry.
	DryRun bool
	// Logf receives progress messages. Required.
	Logf func(format string, a ...any)
}

// Result summarises what a run did, or would do.
type Result struct {
	RefsConsolidated int
	CommitTagsPruned int
	LockTagsPruned   int
	// TagsUnprunable counts tags left behind because the registry refuses
	// manifest deletion. Compaction still succeeds; nothing is reclaimed.
	TagsUnprunable int
	TagsBefore     int
	TagsAfter      int
}

// Run compacts the repository behind client, using repo as the source of git
// objects.
//
// It must be run from a clone that already contains every commit the remote
// refs point at; the consolidated packfiles are built locally.
func Run(ctx context.Context, client *oci.Client, repo *git.Repository, opts Options) (*Result, error) {
	if opts.Logf == nil {
		return nil, fmt.Errorf("gc: Options.Logf is required")
	}

	refs, err := client.FetchRichRefIndex(ctx)
	if err != nil && !oci.IsNotFound(err) {
		return nil, fmt.Errorf("failed to read the ref index: %w", err)
	}
	if len(refs) == 0 {
		opts.Logf("nothing to do: the repository has no refs\n")
		return &Result{}, nil
	}

	before, err := client.ListAllTags(ctx)
	if err != nil {
		return nil, err
	}
	result := &Result{TagsBefore: len(before)}

	// Every commit a ref points at must be reachable from some object store, or
	// the consolidated packfile would silently omit history. Anything the local
	// repository does not have is fetched from the registry into a scratch
	// store instead of refusing the run: the objects are in the registry by
	// definition, and requiring a full clone made the job most worth scheduling
	// the job that could not be scheduled.
	//
	// All of them are checked up front, so the run either proceeds fully or
	// stops before it has half-rewritten the repository.
	source := repo
	if missing, why := missingLocally(repo, refs); len(missing) > 0 {
		sort.Strings(missing)
		if opts.DryRun {
			opts.Logf("would fetch %d ref(s) from the registry to repack: %v (%s)\n", len(missing), missing, why)
		} else {
			opts.Logf("%d ref(s) are not in the local repository (%v; %s); fetching them to repack\n",
				len(missing), missing, why)

			staged, hydrateErr := hydrate(ctx, client, refs, opts.Logf)
			if hydrateErr != nil {
				return nil, fmt.Errorf("failed to fetch the history to repack: %w", hydrateErr)
			}
			defer staged.Close()

			// Everything, not just the missing refs. Mixing two object stores
			// would mean deciding per ref which one to pack from, and the
			// scratch store now holds every ref's history anyway.
			source = staged.repo
		}
	}

	// Consolidation makes every edge in the published pack-base chain obsolete:
	// the manifests they name are the ones step 2 prunes. Merging the new
	// chain into the old one would carry that wreckage forward for the life of
	// the repository, so the old chain is dropped and rebuilt from what this
	// run publishes.
	//
	// A run that skips some refs therefore publishes a chain describing only
	// the ones it repacked. That is allowed and costs only speed: FORMAT.md
	// §6.1 requires a reader to follow each manifest's own pack-bases anyway,
	// so a ref missing from the chain is walked the slow way rather than
	// mis-resolved.
	if !opts.DryRun {
		client.ResetPackChain()
	}

	// 1. Consolidation. Each ref is rewritten as one packfile containing its
	//    whole history, so no ref depends on an intermediate commit manifest.
	refNames := make([]string, 0, len(refs))
	for refName := range refs {
		refNames = append(refNames, refName)
	}
	sort.Strings(refNames)

	// Consolidation republishes a ref manifest from the snapshot read above, so
	// a ref that another client advances while this runs would be rewound to
	// the older tip -- and then the pruning below would delete the commit tag
	// that push had just published. Both are silent: the pushing client was
	// told `ok` before any of it happened.
	//
	// So each ref is taken under the same lock a push takes, and its tip is
	// re-read once held. A ref that moved, or that is locked by a push in
	// flight, is left exactly as it is. Skipping is always safe -- an
	// unconsolidated ref is a ref that costs more to clone, not a broken one --
	// and the next run picks it up.
	keep := make(map[string]bool, len(refs))
	var concurrent []string

	for _, refName := range refNames {
		entry := refs[refName]
		if entry.SHA == "" {
			continue
		}
		keep[entry.SHA] = true

		if opts.DryRun {
			opts.Logf("would repack %s (%s) into a single packfile\n", refName, short(entry.SHA))
			result.RefsConsolidated++
			continue
		}

		// No retry: gc is the background job here, and a push waiting on a
		// lock held by a repack is exactly the delay this should not cause.
		if _, lockErr := client.AcquireRefLock(ctx, refName, consolidationLockTTL); lockErr != nil {
			opts.Logf("leaving %s alone: %v\n", refName, lockErr)
			concurrent = append(concurrent, refName)
			continue
		}

		if current, moved := refTipMoved(ctx, client, refName, entry.SHA); moved {
			opts.Logf("leaving %s alone: it moved to %s while this run was working\n",
				refName, short(current))
			concurrent = append(concurrent, refName)
			releaseRefLock(ctx, client, refName, opts)
			continue
		}

		err := consolidateRef(ctx, client, source, refName, entry)
		releaseRefLock(ctx, client, refName, opts)
		if err != nil {
			return nil, fmt.Errorf("failed to repack %s: %w", refName, err)
		}
		opts.Logf("repacked %s (%s)\n", refName, short(entry.SHA))
		result.RefsConsolidated++
	}

	// The ref set as it stands now, which is what decides whether pruning is
	// safe. Comparing against the opening snapshot is how a push that landed
	// mid-run is noticed at all.
	latest := refs
	if !opts.DryRun {
		if fresh, err := client.FetchRichRefIndex(ctx); err == nil {
			latest = fresh
		} else if !oci.IsNotFound(err) {
			// Unable to confirm the current state, so nothing may be deleted on
			// the strength of the old one.
			concurrent = append(concurrent, "(the ref index could not be re-read)")
		}
	}
	if changed := refsChangedSince(refs, latest); len(changed) > 0 {
		concurrent = append(concurrent, changed...)
	}

	// 2. Republish the indexes so they describe the consolidated repository.
	//
	// Before anything is deleted, not after. `_refs` carries the pack chain
	// (§6.1), and the chain still published at this point names every
	// intermediate manifest the pruning below removes. A run that died between
	// deleting them and rewriting the chain -- a crash, a failed write -- left
	// a chain pointing at manifests that no longer existed, and a reader
	// following it hit a missing base, which the format makes fatal. Written
	// now, the chain says each consolidated ref stands alone, which is true
	// from here on whatever happens next; the worst a failure after this
	// leaves behind is a commit tag nobody needs.
	//
	// No refs of its own. gc moves no ref, so it has nothing to say about
	// where any of them point; the merge inside this call takes the live
	// index as it stands. Handing it a snapshot instead -- even one re-read a
	// moment ago -- was how a push landing between that read and this write
	// got rewound: the caller's entries win the merge unconditionally.
	if !opts.DryRun {
		if err := client.PushRichRefIndex(ctx, nil, nil); err != nil {
			return nil, fmt.Errorf("failed to republish the ref index: %w", err)
		}
	}

	// 3. Pruning, and only if nothing moved underneath the consolidation.
	//
	// A commit tag is a pack base. Deleting one that a push published while
	// this ran strands the ref that names it, and the check above cannot be
	// made airtight against a registry with no compare-and-swap -- so the rule
	// is to prune from a view that was still current at the end of the run, and
	// otherwise to leave it for the next one. Deferring costs a repository that
	// stays large for another push; getting it wrong costs a ref that cannot be
	// fetched.
	prune := before
	if len(concurrent) > 0 {
		sort.Strings(concurrent)
		opts.Logf("not pruning this run: %v changed while it was working\n", concurrent)
		prune = nil
	}
	for _, tag := range prune {
		switch oci.ClassifyTag(tag) {
		case oci.TagClassCommit:
			if keep[tag] {
				continue
			}
			if opts.DryRun {
				opts.Logf("would prune commit manifest %s\n", short(tag))
				result.CommitTagsPruned++
				continue
			}
			if err := client.DeleteTag(ctx, tag); err != nil {
				if errors.Is(err, oci.ErrDeletionUnsupported) {
					// Nothing to reclaim here, but the consolidation above is
					// the part that makes clones cheaper, and it has already
					// happened. Refusing the whole run would throw that away.
					result.TagsUnprunable++
					continue
				}
				return nil, fmt.Errorf("failed to prune commit manifest %s: %w", short(tag), err)
			}
			result.CommitTagsPruned++

		case oci.TagClassLock:
			reclaimable, err := client.IsLockReclaimable(ctx, tag)
			if err != nil {
				opts.Logf("leaving %s alone: %v\n", tag, err)
				continue
			}
			if !reclaimable {
				opts.Logf("leaving %s alone: it is still held\n", tag)
				continue
			}
			if opts.DryRun {
				opts.Logf("would prune released lock %s\n", tag)
				result.LockTagsPruned++
				continue
			}
			if err := client.DeleteTag(ctx, tag); err != nil {
				if errors.Is(err, oci.ErrDeletionUnsupported) {
					result.TagsUnprunable++
					continue
				}
				return nil, fmt.Errorf("failed to prune lock %s: %w", tag, err)
			}
			result.LockTagsPruned++

		case oci.TagClassMetadata, oci.TagClassRef:
			// Index tags and live ref manifests are what the repository is.
		}
	}

	if result.TagsUnprunable > 0 {
		opts.Logf("left %d tag(s) in place: this registry does not allow manifest deletion, "+
			"so the packfiles were consolidated but nothing was reclaimed\n", result.TagsUnprunable)
	}

	if opts.DryRun {
		result.TagsAfter = result.TagsBefore - result.CommitTagsPruned - result.LockTagsPruned
		return result, nil
	}

	after, err := client.ListAllTags(ctx)
	if err != nil {
		return nil, err
	}
	result.TagsAfter = len(after)
	return result, nil
}

// consolidateRef republishes one ref as a single packfile covering its entire
// history, with no delta base exclusions.
func consolidateRef(ctx context.Context, client *oci.Client, repo *git.Repository, refName string, entry oci.RefEntry) error {
	wantHash := plumbing.NewHash(entry.SHA)
	if entry.TagObject != "" {
		// An annotated tag is published under its tag object, so pack from
		// there or the tag object itself would be left behind.
		wantHash = plumbing.NewHash(entry.TagObject)
	}

	refTag := oci.RefManifestTag(refName)
	if refTag == "" {
		return fmt.Errorf("ref %q cannot be represented as an OCI tag", refName)
	}

	// Republish the object index alongside it. A consolidated packfile is the
	// whole history, which is the case where a partial clone's lazy fetch most
	// needs to know whether this ref is the one worth downloading — dropping
	// the index here would leave every compacted repository paying the cost
	// compaction was run to avoid. Failing to build one is not a failed
	// consolidation; a missing index reads as "unknown" and falls back.
	//
	// Listed before the packfile goroutine starts: both walk the same go-git
	// storer, and that is not safe to do from two goroutines at once. The
	// list is handed to the packer as well, so its pure-Go fallback does not
	// walk the whole history a second time.
	var extraLayers []ocispec.Descriptor
	objects, listErr := repo.RevList(wantHash, nil)
	if listErr == nil {
		if desc, pushErr := client.PushPackIndex(ctx, packIndexEntries(repo.PackedObjectsOf(objects))); pushErr == nil && desc.Digest != "" {
			extraLayers = append(extraLayers, desc)
		}
	}

	// An annotated tag's metadata travels on the ref manifest (FORMAT.md §5),
	// and a consolidation replaces that manifest. The index entry is where the
	// same facts were recorded at push time, so they are carried across from
	// there; a rewrite that dropped them would turn every annotated tag in
	// the repository into a lightweight one on the next clone.
	var tagAnnotations map[string]string
	if entry.TagObject != "" {
		tagAnnotations = map[string]string{
			oci.AnnotationGitTagObj:     entry.TagObject,
			oci.AnnotationGitTagger:     entry.Tagger,
			oci.AnnotationGitTagMessage: entry.TagMessage,
			oci.AnnotationGitTagSig:     entry.TagSig,
		}
	}

	pr, pw := io.Pipe()
	go func() {
		// No haveHashes: the point is a self-contained pack. A nil list --
		// the walk above failed -- lets the fallback compute its own.
		_ = pw.CloseWithError(repo.CreatePackfileFromListTo(pw, wantHash, nil, objects))
	}()

	// No parents and no pack bases: a consolidated packfile carries the whole
	// history, so it depends on nothing. Declaring PackBasesNone is what makes
	// the pruning below safe - a fetcher of this ref will never be sent looking
	// for a commit manifest this run is about to delete.
	err := client.PushCommitStream(ctx, oci.CommitPush{
		CommitSHA:      entry.SHA,
		RefName:        refName,
		RefTag:         refTag,
		TagAnnotations: tagAnnotations,
		ExtraLayers:    extraLayers,
		// This commit is almost certainly already published; replacing its
		// packfile with a self-contained one is the point of the run.
		Rewrite: true,
	}, pr, 0)
	if err != nil {
		_ = pr.CloseWithError(err)
		return err
	}
	return nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// consolidationLockTTL bounds how long a repack may hold one ref's lock.
//
// The same order as a push's default, and for the same reason: it has to cover
// building a packfile over the repository's whole history and uploading it,
// which on a large history over a slow link is minutes. A lock that expires
// mid-repack is worse than none, because a push then acquires it legitimately
// and the two interleave exactly the update the lock exists to serialise.
const consolidationLockTTL = 10 * time.Minute

// refTipMoved reports the ref's current tip and whether it differs from what
// this run set out to repack.
//
// Read past the manifest cache: this process may well have published that ref
// itself moments ago -- automatic compaction runs inside the push that crossed
// the threshold -- and a cached answer would confirm what it already believed.
//
// An unreadable ref counts as moved. The question being asked is "is it still
// safe to overwrite this", and the only safe answer to "cannot tell" is no.
func refTipMoved(ctx context.Context, client *oci.Client, refName, expected string) (string, bool) {
	tag := oci.RefManifestTag(refName)
	if tag == "" {
		return "", true
	}
	client.InvalidateManifestCache(tag)

	manifest, err := client.FetchManifest(ctx, tag)
	if err != nil || manifest == nil {
		return "", true
	}
	current := manifest.Annotations[ocispec.AnnotationRevision]
	return current, current != expected
}

// releaseRefLock gives a ref's lock back, on a context of its own so a
// cancelled run does not leave the ref stalled until the TTL runs out.
func releaseRefLock(ctx context.Context, client *oci.Client, refName string, opts Options) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := client.ReleaseRefLock(releaseCtx, refName); err != nil {
		opts.Logf("warning: failed to release the lock on %s: %v\n", refName, err)
	}
}

// refsChangedSince names the refs that differ between two readings of the
// index: moved, added or removed.
//
// Any of the three means the snapshot the consolidation worked from is no
// longer the repository, and that pruning decisions taken from it are not
// safe to act on.
func refsChangedSince(before, after map[string]oci.RefEntry) []string {
	var changed []string
	for name, entry := range before {
		current, present := after[name]
		if !present {
			changed = append(changed, name+" (deleted)")
			continue
		}
		if current.SHA != entry.SHA {
			changed = append(changed, name)
		}
	}
	for name := range after {
		if _, present := before[name]; !present {
			changed = append(changed, name+" (new)")
		}
	}
	return changed
}

// packIndexEntries adapts the git package's view of a packfile's contents to
// the registry package's. The two describe the same thing and neither should
// import the other's types.
func packIndexEntries(objects []git.PackedObject) []oci.PackIndexEntry {
	entries := make([]oci.PackIndexEntry, 0, len(objects))
	for _, o := range objects {
		entries = append(entries, oci.PackIndexEntry{OID: o.OID, Size: o.Size})
	}
	return entries
}
