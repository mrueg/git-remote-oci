// Package cli dispatches between the git remote-helper protocol and the
// maintenance subcommands.
//
// The dispatch has to coexist with how git invokes a remote helper, which is
// always exactly two arguments: `git-remote-oci <remote> <url>`. A subcommand
// that also takes a single URL — gc, fsck, lfs-locks — is then indistinguishable
// from a remote whose name happens to be "gc", by argv alone.
//
// Git resolves the ambiguity for us: it exports GIT_DIR into the helper's
// environment, and a hand-run subcommand does not normally have it set. So a
// reserved name arriving with two arguments and GIT_DIR set is a colliding
// remote, and is refused by name. Refusing matters more than it looks: without
// it, `git fetch gc` runs a real garbage collection against the registry, and
// prints its result onto stdout, which under git is the wire protocol.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mrueg/git-remote-oci/pkg/config"
	"github.com/mrueg/git-remote-oci/pkg/helper"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// forceSubcommandEnv overrides the collision check, for the rare caller that
// runs a subcommand by hand with GIT_DIR exported.
const forceSubcommandEnv = "GIT_REMOTE_OCI_SUBCOMMAND"

// Env is everything Run needs from the process.
type Env struct {
	Args    []string
	Version string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer

	// Getenv reads the process environment. Nil means os.Getenv.
	Getenv func(string) string
}

func (e Env) getenv(key string) string {
	if e.Getenv != nil {
		return e.Getenv(key)
	}
	return os.Getenv(key)
}

// subcommand is one operation invocable directly rather than through git.
type subcommand struct {
	name    string
	aliases []string
	args    string
	summary string
	run     func(context.Context, Env) error
}

// subcommands is the single declaration of what exists. Dispatch, the reserved
// name set and the usage text are all derived from it, so a new subcommand
// cannot be added without also reserving its name — which used to be a comment
// asking the reader to remember.
//
// It is built in init rather than as a composite literal because "help" runs
// usage, and usage reads this table; the compiler sees that as an
// initialisation cycle.
var subcommands []subcommand

// byName resolves a name or alias to its subcommand.
var byName map[string]*subcommand

func init() {
	subcommands = []subcommand{
		{
			name:    "gc",
			args:    "[flags] <oci-url>",
			summary: "compact a repository stored in a registry",
			run:     runGC,
		},
		{
			name:    "fsck",
			args:    "<oci-url>",
			summary: "check a published repository is fetchable",
			run:     runFsck,
		},
		{
			name:    "break-lock",
			args:    "[flags] <oci-url> <ref>",
			summary: "break a stranded advisory ref lock",
			run:     runBreakLock,
		},
		{
			name:    "lfs-lock",
			args:    "[flags] <oci-url> <path>",
			summary: "take a Git LFS file lock",
			run:     runLFSLock,
		},
		{
			name:    "lfs-locks",
			args:    "<oci-url>",
			summary: "list Git LFS file locks",
			run:     runLFSLocks,
		},
		{
			name:    "lfs-unlock",
			args:    "[flags] <oci-url> <path-or-id>",
			summary: "release a Git LFS file lock",
			run:     runLFSUnlock,
		},
		{
			name:    "set-head",
			args:    "<oci-url> [ref]",
			summary: "show or set the default branch a clone checks out",
			run:     runSetHead,
		},
		{
			name:    "version",
			aliases: []string{"--version", "-v"},
			summary: "print the version",
			run:     runVersion,
		},
		{
			name:    "help",
			aliases: []string{"--help", "-h"},
			summary: "print this message",
			run:     runHelp,
		},
	}

	byName = make(map[string]*subcommand, len(subcommands)*2)
	for i := range subcommands {
		s := &subcommands[i]
		byName[s.name] = s
		for _, a := range s.aliases {
			byName[a] = s
		}
	}
}

// UsageSummary returns a subcommand's argument summary as the top-level help
// advertises it, so a test can hold it against the subcommand's own usage line.
func UsageSummary(name string) (string, bool) {
	s, ok := byName[name]
	if !ok {
		return "", false
	}
	return s.args, true
}

// ReservedNames lists the names a git remote cannot use, in declaration order.
// Aliases beginning with "-" are omitted: git will not accept them as a remote
// name in the first place.
func ReservedNames() []string {
	names := make([]string, 0, len(subcommands))
	for _, s := range subcommands {
		names = append(names, s.name)
	}
	return names
}

// Run dispatches a single invocation.
func Run(ctx context.Context, env Env) error {
	if len(env.Args) == 0 {
		diag(env.Stderr, "%s", usageText())
		return fmt.Errorf("no arguments given")
	}

	name := env.Args[0]
	sub := byName[name]

	if sub != nil {
		if err := checkRemoteNameCollision(env, name); err != nil {
			return err
		}
		err := sub.run(ctx, env)
		if errors.Is(err, flag.ErrHelp) {
			// `<subcommand> -h` asked for the usage and got it; the flag
			// package reports that as an error so that parsing stops. It is
			// not a failure, and exiting non-zero with "flag: help requested"
			// after the usage text read as one.
			return nil
		}
		return err
	}

	// Git's remote-helper invocation: exactly <remote> <url>.
	if len(env.Args) == 2 {
		h, err := helper.NewHelper(env.Args[0], env.Args[1], env.Stdin, env.Stdout)
		if err != nil {
			return err
		}
		return h.Run(ctx)
	}

	diag(env.Stderr, "%s", usageText())
	return fmt.Errorf("unrecognised arguments: %s", strings.Join(env.Args, " "))
}

// checkRemoteNameCollision refuses an invocation that is more likely to be git
// calling the helper for a badly named remote than a user running a subcommand.
//
// The signal is GIT_DIR, which git exports into a remote helper's environment
// and which a shell normally does not have set. It is a heuristic in one
// direction only: a false positive refuses a subcommand with an explanation and
// an override, while a false negative would let `git fetch gc` repack the
// remote. The asymmetry decides which way to lean.
func checkRemoteNameCollision(env Env, name string) error {
	if len(env.Args) != 2 || env.getenv("GIT_DIR") == "" {
		return nil
	}
	if env.getenv(forceSubcommandEnv) != "" {
		return nil
	}
	return fmt.Errorf(
		"refusing to run the %q subcommand: git invokes a remote helper as "+
			"`git-remote-oci <remote> <url>`, so this looks like a git remote named %q, "+
			"which collides with a subcommand name.\n"+
			"  If it is a remote, rename it:  git remote rename %s <new-name>\n"+
			"  If you meant the subcommand:   %s=1 git-remote-oci %s <args>",
		name, name, name, forceSubcommandEnv, name)
}

// noArgs rejects trailing arguments for the subcommands that take none.
//
// Every subcommand with a flag set checks its argument count; version and help
// used to ignore whatever followed them, so `git-remote-oci version some-typo`
// reported success and the typo went unmentioned.
func noArgs(env Env, name string) error {
	if len(env.Args) > 1 {
		return fmt.Errorf("%s takes no arguments, got %s",
			name, strings.Join(env.Args[1:], " "))
	}
	return nil
}

func runVersion(_ context.Context, env Env) error {
	if err := noArgs(env, "version"); err != nil {
		return err
	}
	return printf(env.Stdout, "git-remote-oci %s\n", env.Version)
}

func runHelp(_ context.Context, env Env) error {
	if err := noArgs(env, "help"); err != nil {
		return err
	}
	return printf(env.Stdout, "%s", usageText())
}

// printf writes a subcommand's result to its stdout.
//
// The write error is the subcommand's error: a result that did not reach the
// caller is a failed command, however well the registry operation went, and a
// non-zero exit is the only way to say so.
func printf(w io.Writer, format string, a ...any) error {
	_, err := fmt.Fprintf(w, format, a...)
	return err
}

// diag writes a diagnostic to a subcommand's stderr.
//
// The write error is dropped on purpose. A diagnostic accompanies an error
// that is about to be returned, or a usage request, and a stderr that cannot
// be written to leaves nothing better to do than carry on to that return.
func diag(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}

// clientFor builds a registry client for an oci:// URL. The scheme and
// plain-HTTP rules live in pkg/oci so that the helper and the subcommands
// cannot disagree about them.
func clientFor(env Env, rawURL string) (*oci.Client, error) {
	client, err := oci.NewClientForURL(rawURL, env.getenv)
	if err != nil {
		return nil, fmt.Errorf("failed to initialise the OCI client: %w", err)
	}
	// A subcommand is given a URL, not a remote name, so only the
	// repository-wide scope applies.
	client.ApplyConfig(config.Load(""))
	return client, nil
}

// usageText renders the top-level help. It is a value rather than a writer so
// that the one caller for whom it is the result (help) and the ones for whom
// it is a diagnostic can each treat the write accordingly.
func usageText() string {
	var b strings.Builder
	b.WriteString(`git-remote-oci - store git repositories in OCI registries

Git invokes this automatically for oci:// remotes; you do not normally run it
by hand:

    git clone oci://registry.example.com/org/repo

Subcommands:

`)

	width := 0
	for _, s := range subcommands {
		if n := len(s.name) + len(s.args) + 1; n > width {
			width = n
		}
	}
	for _, s := range subcommands {
		invocation := s.name
		if s.args != "" {
			invocation += " " + s.args
		}
		fmt.Fprintf(&b, "    %-*s  %s\n", width, invocation, s.summary)
	}

	fmt.Fprintf(&b, `
Because git invokes the helper as "git-remote-oci <remote> <url>", a git remote
cannot be named any of:

    %s

Run "git-remote-oci <subcommand> -h" for a subcommand's flags.
`, strings.Join(ReservedNames(), ", "))
	return b.String()
}
