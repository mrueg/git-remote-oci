package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"sort"
	"text/tabwriter"
)

// Git LFS file locking, exposed as subcommands.
//
// This is not `git lfs lock`, and cannot be. Locking in Git LFS is an HTTP API
// - POST /locks, GET /locks, POST /locks/:id/unlock - that git-lfs calls on an
// LFS server discovered from the remote URL. An `oci://` remote has no such
// endpoint, and a remote helper is not an HTTP server: git speaks to it over a
// pipe, only for fetch and push, and never for locking. Serving the API would
// mean running a daemon alongside git, which is a different program.
//
// What is achievable, and what these do, is expose the same locks over the same
// registry storage so a team can coordinate on unmergeable binaries. The
// records live in `_lfs_locks` and interoperate with anything else using this
// tool - they are simply driven by hand rather than by `git lfs`.

// runLFSLock takes a lock on a path.
func runLFSLock(ctx context.Context, env Env) error {
	fs := flag.NewFlagSet("lfs-lock", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	owner := fs.String("owner", "", "lock owner to record (default: $USER@$HOSTNAME)")
	fs.Usage = func() {
		diag(env.Stderr, `usage: git-remote-oci lfs-lock [flags] <oci-url> <path>

Takes a Git LFS file lock on <path>.

This is not `+"`git lfs lock`"+`: locking in Git LFS is an HTTP API served by an LFS
server, and an oci:// remote has none. These subcommands expose the same locks
over the registry so a team can still coordinate on files that cannot be merged.

Locking is advisory. Nothing prevents a push to a locked path; the lock records
an intent that other people can read.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(env.Args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return fmt.Errorf("lfs-lock takes an oci:// URL and a path")
	}

	client, err := clientFor(env, fs.Arg(0))
	if err != nil {
		return err
	}
	path := fs.Arg(1)

	lock, err := client.AcquireLFSLock(ctx, path, *owner)
	if lock == nil && err != nil {
		return fmt.Errorf("failed to lock %s: %w", path, err)
	}
	if printErr := printf(env.Stdout, "locked %s (id %s, owner %s)\n", lock.Path, lock.ID, lock.Owner.Name); printErr != nil {
		// The lock is published either way; say so, and say whatever else
		// went wrong after it, rather than letting one failure hide the other.
		return errors.Join(printErr, err)
	}
	if err != nil {
		// The lock was published; what failed afterwards was giving back the
		// index lock that protected the list. Say both, because the second
		// part leaves other lock operations waiting until it expires.
		return fmt.Errorf("locked %s, but: %w", path, err)
	}
	return nil
}

// runLFSLocks lists the locks currently held.
func runLFSLocks(ctx context.Context, env Env) error {
	fs := flag.NewFlagSet("lfs-locks", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		diag(env.Stderr, `usage: git-remote-oci lfs-locks <oci-url>

Lists the Git LFS file locks held in a repository.
`)
	}
	if err := fs.Parse(env.Args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return fmt.Errorf("lfs-locks takes exactly one oci:// URL")
	}

	client, err := clientFor(env, fs.Arg(0))
	if err != nil {
		return err
	}

	locks, err := client.FetchLFSLocks(ctx)
	if err != nil {
		return fmt.Errorf("failed to read the locks: %w", err)
	}
	if len(locks) == 0 {
		return printf(env.Stdout, "no locks held\n")
	}
	sort.Slice(locks, func(i, j int) bool { return locks[i].Path < locks[j].Path })

	w := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	if err := printf(w, "PATH\tOWNER\tLOCKED AT\tID\n"); err != nil {
		return err
	}
	for _, l := range locks {
		if err := printf(w, "%s\t%s\t%s\t%s\n", l.Path, l.Owner.Name, l.LockedAt.Format("2006-01-02 15:04:05Z07:00"), l.ID); err != nil {
			return err
		}
	}
	return w.Flush()
}

// runLFSUnlock releases a lock.
func runLFSUnlock(ctx context.Context, env Env) error {
	fs := flag.NewFlagSet("lfs-unlock", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	force := fs.Bool("force", false, "release a lock held by someone else")
	owner := fs.String("owner", "", "owner to release as (default: $USER@$HOSTNAME)")
	fs.Usage = func() {
		diag(env.Stderr, `usage: git-remote-oci lfs-unlock [flags] <oci-url> <path-or-id>

Releases a Git LFS file lock, named by the locked path or by its lock id.

A path is resolved to a lock id first and the resolution is reported, because
the two namespaces can overlap and releasing the wrong lock silently would be
worse than failing.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(env.Args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return fmt.Errorf("lfs-unlock takes an oci:// URL and a path or lock id")
	}

	client, err := clientFor(env, fs.Arg(0))
	if err != nil {
		return err
	}
	target := fs.Arg(1)

	// Prefer an exact path match, and say so, rather than letting a path and an
	// id be conflated.
	lockID := target
	if byPath, err := client.FindLFSLockByPath(ctx, target); err != nil {
		return fmt.Errorf("failed to look up %s: %w", target, err)
	} else if byPath != nil {
		lockID = byPath.ID
		diag(env.Stderr, "git-remote-oci: %s is locked as %s\n", target, lockID)
	}

	released, err := client.ReleaseLFSLock(ctx, lockID, *force, *owner)
	if released == nil && err != nil {
		return fmt.Errorf("failed to unlock %s: %w", target, err)
	}
	if released == nil {
		return printf(env.Stdout, "no lock matched %s\n", target)
	}
	if printErr := printf(env.Stdout, "unlocked %s (id %s)\n", released.Path, released.ID); printErr != nil {
		return errors.Join(printErr, err)
	}
	if err != nil {
		// Same as lfs-lock: the release itself landed; the index lock did not
		// come back.
		return fmt.Errorf("unlocked %s, but: %w", target, err)
	}
	return nil
}
