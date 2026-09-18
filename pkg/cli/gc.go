package cli

import (
	"context"
	"flag"
	"fmt"

	"github.com/mrueg/git-remote-oci/pkg/gc"
	"github.com/mrueg/git-remote-oci/pkg/git"
)

func runGC(ctx context.Context, env Env) error {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	dryRun := fs.Bool("dry-run", false, "report what would change without modifying the registry")
	fs.Usage = func() {
		diag(env.Stderr, `usage: git-remote-oci gc [flags] <oci-url>

Compacts a repository stored in an OCI registry.

Every push writes its own packfile and its own commit-SHA tag, and nothing
removes them, so a long-lived repository accumulates one of each per push.
This rewrites every ref as a single self-contained packfile and then prunes the
commit manifests that are no longer needed, along with released or expired
locks.

Objects it cannot find locally are fetched from the registry, so this can run
anywhere -- including outside a git repository, which is what makes it usable
as a scheduled job next to the registry. Running it from a clone that already
holds the history just avoids the download.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(env.Args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return fmt.Errorf("gc takes exactly one oci:// URL")
	}

	client, err := clientFor(env, fs.Arg(0))
	if err != nil {
		return err
	}

	// Outside a repository is fine: gc fetches what it needs. Opening one when
	// there is one is still worth doing, because objects already on disk do not
	// have to come back down the wire.
	repo, err := git.OpenRepository()
	if err != nil {
		repo = nil
	}

	logf := func(format string, a ...any) { diag(env.Stderr, format, a...) }
	result, err := gc.Run(ctx, client, repo, gc.Options{DryRun: *dryRun, Logf: logf})
	if err != nil {
		return err
	}

	verb := "removed"
	if *dryRun {
		verb = "would remove"
	}
	return printf(env.Stdout,
		"repacked %d refs; %s %d commit manifests and %d lock tags (%d tags -> %d)\n",
		result.RefsConsolidated, verb, result.CommitTagsPruned, result.LockTagsPruned,
		result.TagsBefore, result.TagsAfter)
}
