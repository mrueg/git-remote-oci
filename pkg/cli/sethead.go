package cli

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// runSetHead points a published repository's HEAD at a different branch.
//
// A repository's default branch is adopted from the first branch ever pushed to
// it and never moves afterwards, because a push cannot say what the default
// should be: nothing in the remote-helper protocol carries that, so the only
// safe reading of "push refs/heads/topic" is "publish this branch", not "make
// it the default". First writer wins is the right rule for a push and the wrong
// rule for a repository — it leaves whoever pushed first having decided
// permanently, and renaming master to main impossible.
//
// So it is a command instead. The same decision, made deliberately, by someone
// who means it.
func runSetHead(ctx context.Context, env Env) error {
	fs := flag.NewFlagSet("set-head", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		diag(env.Stderr, `usage: git-remote-oci set-head <oci-url> [ref]

Sets the default branch a fresh clone checks out.

The ref is a full name, e.g. refs/heads/main, and must be one the repository
already publishes; "main" and "heads/main" are accepted and expanded. Pass no
ref to print the current one.

A repository adopts its default from the first branch pushed to it, because a
push carries no way to say what the default should be. This is how to change it
afterwards.
`)
	}
	if err := fs.Parse(env.Args[1:]); err != nil {
		return err
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fs.Usage()
		return fmt.Errorf("set-head takes an oci:// URL and, to change it, a ref name")
	}

	client, err := clientFor(env, fs.Arg(0))
	if err != nil {
		return err
	}

	// With no ref this reports rather than changes. Asking what the default is
	// has been unanswerable from the command line too, and a user about to
	// change something should be able to see what it is first.
	if fs.NArg() == 1 {
		current, err := client.FetchHead(ctx)
		if err != nil {
			return fmt.Errorf("failed to read the recorded HEAD: %w", err)
		}
		if current == "" {
			return printf(env.Stdout, "no default branch is recorded\n")
		}
		return printf(env.Stdout, "%s\n", current)
	}

	refs, err := client.FetchRichRefIndex(ctx)
	if err != nil {
		return fmt.Errorf("failed to list the published refs: %w", err)
	}
	ref, err := resolveHeadRef(fs.Arg(1), refs)
	if err != nil {
		return err
	}

	previous, err := client.SetHead(ctx, ref)
	if err != nil {
		return err
	}
	switch previous {
	case ref:
		return printf(env.Stdout, "%s was already the default branch\n", ref)
	case "":
		return printf(env.Stdout, "default branch set to %s\n", ref)
	default:
		return printf(env.Stdout, "default branch moved from %s to %s\n", previous, ref)
	}
}

// resolveHeadRef expands a shorthand to the full ref name it names.
//
// A user types "main"; the index holds "refs/heads/main". Accepting only the
// full form would be defensible and annoying, and guessing without checking
// would let a typo be recorded as the default. So each candidate is tried
// against what the repository actually publishes, and an unmatched name is
// reported with the branches that were available — the answer to "then what
// should I have typed" is the useful part of that error.
func resolveHeadRef(name string, refs map[string]oci.RefEntry) (string, error) {
	candidates := []string{name}
	if !strings.HasPrefix(name, "refs/") {
		candidates = append(candidates,
			"refs/"+name,
			"refs/heads/"+name,
		)
	}
	for _, candidate := range candidates {
		if _, ok := refs[candidate]; !ok {
			continue
		}
		// HEAD is a symbolic ref to a branch. Pointing it at a tag would
		// produce a clone with a detached head and no branch to commit on,
		// which is not what anyone setting a default branch is asking for.
		if !strings.HasPrefix(candidate, "refs/heads/") {
			return "", fmt.Errorf("%s is not a branch; HEAD has to name one", candidate)
		}
		return candidate, nil
	}
	return "", fmt.Errorf("%s is not a branch this repository publishes (it has: %s)",
		name, branchNames(refs))
}

// branchNames lists the branches a repository publishes, for an error message.
// "Then what should I have typed" is the useful half of a name-not-found error.
func branchNames(refs map[string]oci.RefEntry) string {
	var branches []string
	for name := range refs {
		if strings.HasPrefix(name, "refs/heads/") {
			branches = append(branches, strings.TrimPrefix(name, "refs/heads/"))
		}
	}
	sort.Strings(branches)
	if len(branches) == 0 {
		return "none"
	}
	return strings.Join(branches, ", ")
}
