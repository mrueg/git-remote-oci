package cli

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// runBreakLock breaks a stranded advisory ref lock.
//
// oci.BreakRefLock has existed since ref locking was added and had no caller,
// so the documented escape hatch for a lock left behind by a client that died
// mid-push was unreachable from anything a user can run. Locks do expire on
// their TTL, but that is ten minutes of a blocked ref with no way to say "I
// know, it is mine, let me through".
//
// It is named "break-lock" rather than "unlock" because it takes a lock away
// from whoever holds it, which is not what "lfs-unlock" — releasing your own —
// does. One verb for both would have made them read as a pair.
func runBreakLock(ctx context.Context, env Env) error {
	fs := flag.NewFlagSet("break-lock", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	force := fs.Bool("force", false, "release the lock even though this client does not hold it")
	fs.Usage = func() {
		diag(env.Stderr, `usage: git-remote-oci break-lock [flags] <oci-url> <ref>

Releases an advisory lock on a ref.

A push takes a lock on the ref it is updating and releases it when it finishes.
A client that dies mid-push leaves one behind, blocking that ref until the lock
expires on its own. This releases it immediately.

The ref is a full name, e.g. refs/heads/main. Locking is advisory in any case:
a registry offers no compare-and-swap, so the lock narrows the window for
concurrent pushes to clobber each other without closing it.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(env.Args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return fmt.Errorf("break-lock takes an oci:// URL and a ref name")
	}

	client, err := clientFor(env, fs.Arg(0))
	if err != nil {
		return err
	}
	refName := fs.Arg(1)

	held, info, err := client.IsLocked(ctx, refName)
	if err != nil {
		return fmt.Errorf("failed to read the lock on %s: %w", refName, err)
	}
	if !held {
		return printf(env.Stdout, "%s is not locked\n", refName)
	}

	owner := "an unknown owner"
	if info != nil && info.Owner != "" {
		owner = info.Owner
	}

	// Releasing someone else's lock is the whole point of this command, but it
	// is also how two clients end up writing at once, so it has to be asked for.
	if !*force {
		return fmt.Errorf("%s is locked by %s until %s; pass --force to break it",
			refName, owner, lockExpiry(info))
	}

	if err := client.BreakRefLock(ctx, refName); err != nil {
		return fmt.Errorf("failed to break the lock on %s: %w", refName, err)
	}
	return printf(env.Stdout, "broke the lock on %s, previously held by %s\n", refName, owner)
}

// lockExpiry renders a lock's expiry for a message.
func lockExpiry(info *oci.LockInfo) string {
	if info == nil {
		return "an unknown time"
	}
	return info.ExpiresAt.Format(time.RFC3339)
}
