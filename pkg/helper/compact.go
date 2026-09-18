package helper

import (
	"context"

	"github.com/mrueg/git-remote-oci/pkg/gc"
)

// Compaction that happens by itself.
//
// Every push publishes a commit manifest and a commit tag, and neither can be
// removed: the tag is a pack base later pushes were cut against, so the history
// has to be repacked to stand alone before anything can go. Left alone the
// count only grows, and a clone pays it -- one manifest per push to fetch, one
// packfile per push to index.
//
// Publishing the pack chain took the round trips out of that graph, but the
// graph is still there, and `gc` is still what removes it. As a command it ran
// when somebody remembered to run it, which for the repository that most needs
// it -- the busy one nobody is minding -- is never. So a push checks, and every
// so often one push pays to clean up after all the others.
//
// It is deliberately the *pusher* that does this. It already holds the objects,
// so consolidation costs it no download, and it is the process that caused the
// growth.

// maybeCompact repacks the repository when it has accumulated enough published
// commits to be worth it.
//
// Called after the push has already reported its result to git, and every
// failure here is a warning. A repository that could not be compacted is
// untidy; a push that failed because tidying failed is broken, and the user
// asked for the push.
func (h *Helper) maybeCompact(ctx context.Context) {
	if h.compactAfter <= 0 || h.dryRun {
		return
	}

	// The published chain has one entry per commit manifest, which is one per
	// push since the last compaction -- exactly the count this threshold is
	// about. It is already cached from the push that just wrote it, so asking
	// costs nothing.
	chain, ok := h.ociClient.FetchPackChain(ctx)
	if !ok || len(chain) <= h.compactAfter {
		return
	}

	// A clone that does not hold the whole history cannot be the source of a
	// consolidated packfile. gc would notice and fetch everything from the
	// registry instead, which is correct and is also a download of the entire
	// repository tacked onto somebody's push -- the opposite of the "already
	// holds the objects, costs no download" reasoning that put this on the
	// pusher in the first place. Left to a full clone, or to `gc` run by hand.
	if h.gitRepo != nil {
		var why string
		switch {
		case h.gitRepo.IsShallow():
			why = "a shallow clone"
		case h.gitRepo.IsPartial():
			why = "a partial clone"
		}
		if why != "" {
			h.logInfo("git-remote-oci: %d published commits is over the %d this remote compacts at, "+
				"but this is %s and does not hold the whole history; not compacting from here\n",
				len(chain), h.compactAfter, why)
			h.logInfo("git-remote-oci: (run `git-remote-oci gc` to compact from the registry's own copy)\n")
			return
		}
	}

	h.logInfo("git-remote-oci: %d published commits is over the %d this remote compacts at; repacking\n",
		len(chain), h.compactAfter)
	h.logInfo("git-remote-oci: (set ociremote.compactAfter to change or disable this)\n")

	defer h.timer.phase("compact")()

	result, err := gc.Run(ctx, h.ociClient, h.gitRepo, gc.Options{
		// Progress goes to stderr like the rest of the helper's narration.
		Logf: func(format string, a ...any) { h.logVerbose("git-remote-oci: [verbose] gc: "+format, a...) },
	})
	if err != nil {
		h.logWarn("git-remote-oci: warning: could not compact the repository: %v\n", err)
		h.logWarn("git-remote-oci: warning: the push succeeded; run `git-remote-oci gc` to retry\n")
		return
	}

	h.logInfo("git-remote-oci: repacked %d ref(s); %d tag(s) removed, %d remain\n",
		result.RefsConsolidated, result.TagsBefore-result.TagsAfter, result.TagsAfter)
}
