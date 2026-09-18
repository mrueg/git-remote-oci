package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"mime"
	"sort"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// runFsck checks that every published ref can actually be fetched.
//
// A registry validates nothing. It accepts any blob and any manifest whose
// blobs exist, and has no idea a packfile is a packfile, so the correctness of
// a repository rests entirely on whoever wrote it having been right. There is
// no server-side reachability check to fall back on, which is exactly why the
// pack-bases contract is a hard error at fetch time rather than a warning.
//
// This walks the same graph a fetch walks and reports what a fetch would hit,
// without downloading packfiles or touching the local repository. It cannot
// prove the objects inside a packfile are complete - only a real fetch does
// that - but it does catch the failure that matters: a manifest naming a base
// the registry no longer serves, which is a repository nobody can clone.
func runFsck(ctx context.Context, env Env) error {
	fs := flag.NewFlagSet("fsck", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		diag(env.Stderr, `usage: git-remote-oci fsck <oci-url>

Checks that every ref published in a repository is fetchable.

For each ref it follows io.git-remote-oci.pack-bases the way a fetch does, and
reports any manifest that is missing, malformed, or names a base the registry
does not serve. Nothing is downloaded and no local repository is needed.

It also compares the _index mirror against _refs. The two are written together
and stand in for each other, so a disagreement means a past write failed
part-way and generic OCI tooling is being shown a stale ref list.

Exits non-zero if any ref is unfetchable or the mirror has drifted.
`)
	}
	if err := fs.Parse(env.Args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return fmt.Errorf("fsck takes exactly one oci:// URL")
	}

	client, err := clientFor(env, fs.Arg(0))
	if err != nil {
		return err
	}

	refs, err := client.FetchRichRefIndex(ctx)
	if err != nil {
		if oci.IsNotFound(err) {
			return printf(env.Stdout, "the repository has no refs\n")
		}
		return fmt.Errorf("failed to read the ref index: %w", err)
	}
	if len(refs) == 0 {
		return printf(env.Stdout, "the repository has no refs\n")
	}

	refNames := make([]string, 0, len(refs))
	for name := range refs {
		refNames = append(refNames, name)
	}
	sort.Strings(refNames)

	// Shared across refs: repositories overwhelmingly share pack bases between
	// branches, and re-walking them per ref turns a linear check quadratic.
	checked := make(map[string]error)
	broken := 0

	// The published pack chain (§6.1) is advisory, but a reader that has it
	// fetches what it names, so an entry pointing at a manifest the registry
	// no longer serves breaks a clone as surely as a bad annotation does.
	chain, _ := client.FetchPackChain(ctx)

	for _, name := range refNames {
		entry := refs[name]
		if entry.SHA == "" {
			diag(env.Stderr, "%s: no commit id recorded\n", name)
			broken++
			continue
		}
		// Start from the ref manifest, not from a commit id. For an annotated
		// tag the index records the tag object, and no manifest is tagged with
		// that - a fetch reaches it through the ref tag, so this must too.
		if err := checkRef(ctx, client, name, entry.SHA, chain, checked); err != nil {
			diag(env.Stderr, "%s: %v\n", name, err)
			broken++
			continue
		}
		if err := printf(env.Stdout, "%s ok\n", name); err != nil {
			return err
		}
	}

	head, headErr := client.FetchHead(ctx)
	switch {
	case headErr != nil:
		diag(env.Stderr, "could not read the recorded HEAD: %v\n", headErr)
	case head == "":
		if err := printf(env.Stdout, "HEAD: not recorded; readers will guess\n"); err != nil {
			return err
		}
	default:
		if _, live := refs[head]; !live {
			diag(env.Stderr, "HEAD points at %s, which is not a published ref\n", head)
			broken++
		} else if err := printf(env.Stdout, "HEAD -> %s\n", head); err != nil {
			return err
		}
	}

	// _index mirrors _refs and stands in for it when _refs cannot be read, so a
	// stale mirror is a way to be served an outdated ref list without being
	// told. Nothing else notices: every normal read prefers _refs and never
	// compares the two.
	drifted := false
	if drift := indexMirrorDrift(ctx, client, refs); len(drift) > 0 {
		for _, line := range drift {
			diag(env.Stderr, "_index mirror: %s\n", line)
		}
		diag(env.Stderr, "_index mirror: run any push to rewrite it\n")
		drifted = true
	} else if err := printf(env.Stdout, "_index mirror matches _refs\n"); err != nil {
		return err
	}

	// Reported separately, because they are different problems with different
	// answers: an unfetchable ref is data loss, a drifted mirror is a stale
	// view that the next push repairs.
	var problems []string
	if broken > 0 {
		problems = append(problems, fmt.Sprintf("%d of %d refs are not fetchable", broken, len(refNames)))
	}
	if drifted {
		problems = append(problems, "the _index mirror has drifted from _refs")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return printf(env.Stdout, "all %d refs are fetchable\n", len(refNames))
}

// checkRef verifies one ref the way a fetch resolves it: through the ref
// manifest, then down its declared pack bases, and along the published pack
// chain from its commit.
//
// sha is the commit the index records for the ref, which is what the chain is
// keyed by; for an annotated tag that is the commit the tag resolves to.
func checkRef(ctx context.Context, client *oci.Client, refName, sha string, chain map[string][]string, checked map[string]error) error {
	desc, err := client.ResolveRefManifest(ctx, refName)
	if err != nil {
		return fmt.Errorf("no ref manifest on the registry: %w", err)
	}
	manifest, err := client.FetchManifest(ctx, desc.Digest.String())
	if err != nil {
		return fmt.Errorf("ref manifest could not be read: %w", err)
	}

	// A manifest with no packfile has nothing to fetch. A snapshot layer
	// carries a packfile media type too, and is not the packfile: it is the
	// tip alone, for `--depth 1`, so a manifest holding only one of those is
	// exactly as unclonable as one holding nothing.
	if !hasPackfileLayer(manifest) {
		return fmt.Errorf("ref manifest has no packfile layer")
	}

	bases, err := oci.ParsePackBases(manifest.Annotations)
	if err != nil {
		return err
	}
	for _, base := range bases {
		if err := walkPackBases(ctx, client, base, checked, nil); err != nil {
			return fmt.Errorf("packed against %s, which is not fetchable: %w", short(base), err)
		}
	}

	return walkPackChain(ctx, client, sha, chain, checked, map[string]bool{})
}

// walkPackChain follows the published chain from sha, checking that every
// manifest it names is one the registry serves and is itself fetchable.
//
// A reader with the chain asks for all of it in one wave, so this is the set
// a clone of the ref actually requests. It is walked separately from the
// annotations because the two can disagree: the chain is rewritten by whoever
// writes `_refs` last, and a compaction that pruned manifests before
// republishing it left entries naming manifests that were already gone.
func walkPackChain(ctx context.Context, client *oci.Client, sha string, chain map[string][]string, checked map[string]error, visited map[string]bool) error {
	if visited[sha] {
		return nil
	}
	visited[sha] = true
	for _, base := range chain[sha] {
		if err := walkPackBases(ctx, client, base, checked, nil); err != nil {
			return fmt.Errorf("the pack chain says %s was packed against %s, which is not fetchable: %w",
				short(sha), short(base), err)
		}
		if err := walkPackChain(ctx, client, base, chain, checked, visited); err != nil {
			return err
		}
	}
	return nil
}

// hasPackfileLayer reports whether a manifest carries a packfile layer that is
// not a tip snapshot.
func hasPackfileLayer(manifest *ocispec.Manifest) bool {
	for _, layer := range manifest.Layers {
		if layer.Annotations[oci.AnnotationSnapshot] == "true" {
			continue
		}
		mediaType := layer.MediaType
		if parsed, _, err := mime.ParseMediaType(mediaType); err == nil {
			mediaType = parsed
		}
		switch mediaType {
		case oci.MediaTypeGitPackfile, oci.MediaTypeGitPackfileGzip, oci.MediaTypeGitPackfileZstd:
			return true
		}
	}
	return false
}

// walkPackBases follows a commit's declared pack bases, as a fetch would.
//
// checked memoises results across refs. path carries the chain in progress so a
// cycle - which registry content could describe, since nothing validates it -
// is reported instead of recursing forever.
func walkPackBases(ctx context.Context, client *oci.Client, sha string, checked map[string]error, path []string) error {
	if result, seen := checked[sha]; seen {
		return result
	}
	for _, ancestor := range path {
		if ancestor == sha {
			return fmt.Errorf("pack bases form a cycle through %s", short(sha))
		}
	}

	manifest, err := client.FetchManifest(ctx, sha)
	if err != nil {
		result := fmt.Errorf("commit %s has no manifest on the registry: %w", short(sha), err)
		checked[sha] = result
		return result
	}

	bases, err := oci.ParsePackBases(manifest.Annotations)
	if err != nil {
		result := fmt.Errorf("commit %s: %w", short(sha), err)
		checked[sha] = result
		return result
	}

	for _, base := range bases {
		if err := walkPackBases(ctx, client, base, checked, append(path, sha)); err != nil {
			result := fmt.Errorf("commit %s was packed against %s, which is not fetchable: %w", short(sha), short(base), err)
			checked[sha] = result
			return result
		}
	}

	checked[sha] = nil
	return nil
}

// short abbreviates a commit id for a message.
func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// indexMirrorDrift compares the _index image index against _refs and describes
// every disagreement.
//
// The two are written together and carry the same information (FORMAT.md §7),
// so they agree unless a write failed part-way. That is survivable while _refs
// is readable -- readers prefer it -- and is exactly what gets served if _refs
// ever is not.
//
// An absent _index is reported, not ignored: it is the same fallback gone
// missing, and a repository that has one is not the same as one that does not.
func indexMirrorDrift(ctx context.Context, client *oci.Client, refs map[string]oci.RefEntry) []string {
	mirror, err := client.FetchOCIImageIndexRefs(ctx, "")
	if err != nil {
		if oci.IsNotFound(err) {
			return []string{"absent; generic OCI tooling cannot discover this repository's refs"}
		}
		return []string{fmt.Sprintf("could not be read: %v", err)}
	}

	var drift []string
	for name, entry := range refs {
		mirrored, present := mirror[name]
		switch {
		case !present:
			drift = append(drift, fmt.Sprintf("%s is missing", name))
		case mirrored.SHA != entry.SHA:
			drift = append(drift, fmt.Sprintf("%s says %s, _refs says %s",
				name, short(mirrored.SHA), short(entry.SHA)))
		}
	}
	for name := range mirror {
		if _, live := refs[name]; !live {
			drift = append(drift, fmt.Sprintf("%s is listed but no longer published", name))
		}
	}
	sort.Strings(drift)
	return drift
}
