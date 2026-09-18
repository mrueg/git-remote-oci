package cli_test

import (
	"strings"
	"testing"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
)

// The dispatch tests cover which subcommand runs. These cover what the
// subcommands then do: until now nothing invoked fsck, break-lock or the lfs-*
// bodies at all, only their argument checks.

// emptyRegistry is the oci:// URL of a registry hosting a repository that
// exists but holds nothing: no tags, no manifests.
func emptyRegistry(t *testing.T) string {
	t.Helper()
	return registrytest.URL(registrytest.New().Serve(t))
}

func TestFsckOnAnEmptyRepository(t *testing.T) {
	url := emptyRegistry(t)

	stdout, stderr, err := runCLI(t, "fsck", url)
	if err != nil {
		t.Fatalf("fsck on an empty repository should not fail: %v\nstderr: %s", err, stderr)
	}
	if stdout == "" {
		t.Error("fsck reported nothing at all")
	}
}

// TestBreakLockOnAnUnlockedRefIsANoOp: reporting "not locked" is the correct
// answer, not an error. A user reaching for break-lock is already having a bad
// day and should not be told off for guessing wrong about which ref is stuck.
func TestBreakLockOnAnUnlockedRefIsANoOp(t *testing.T) {
	url := emptyRegistry(t)

	stdout, stderr, err := runCLI(t, "break-lock", url, "refs/heads/main")
	if err != nil {
		t.Fatalf("break-lock on an unlocked ref: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "not locked") {
		t.Errorf("expected it to say the ref is not locked, got: %q", stdout)
	}
}

func TestLFSLocksOnAnEmptyRepository(t *testing.T) {
	url := emptyRegistry(t)

	stdout, stderr, err := runCLI(t, "lfs-locks", url)
	if err != nil {
		t.Fatalf("lfs-locks on an empty repository: %v\nstderr: %s", err, stderr)
	}
	if stdout == "" {
		t.Error("lfs-locks reported nothing at all")
	}
}

// TestSubcommandsReportAnUnreachableRegistry: every subcommand takes a URL, and
// a registry that is not there has to produce a legible failure rather than a
// panic or a success.
func TestSubcommandsReportAnUnreachableRegistry(t *testing.T) {
	const dead = "oci://127.0.0.1:1/repo"

	for _, args := range [][]string{
		{"fsck", dead},
		{"break-lock", dead, "refs/heads/main"},
		{"lfs-locks", dead},
		{"lfs-lock", dead, "art/hero.psd"},
		{"lfs-unlock", dead, "art/hero.psd"},
	} {
		t.Run(args[0], func(t *testing.T) {
			_, _, err := runCLI(t, args...)
			if err == nil {
				t.Fatalf("%v against an unreachable registry reported success", args)
			}
			if strings.Contains(err.Error(), "panic") {
				t.Errorf("unhelpful failure: %v", err)
			}
		})
	}
}

// TestGCOutsideAGitRepositorySucceeds: gc is meant to run as a scheduled job
// next to the registry, where there is no clone. Objects it cannot find
// locally are fetched from the registry, so outside a repository it must
// complete and report what it did -- not fail somewhere deep in go-git, and
// not fail politely either.
//
// This used to assert only inside `if err != nil`, so a run that succeeded
// passed without checking anything.
func TestGCOutsideAGitRepositorySucceeds(t *testing.T) {
	t.Run("empty registry", func(t *testing.T) {
		url := emptyRegistry(t)
		t.Setenv("GIT_DIR", "")
		t.Chdir(t.TempDir())

		stdout, stderr, err := runCLI(t, "gc", url)
		if err != nil {
			t.Fatalf("gc outside a repository against an empty registry: %v\nstderr: %s", err, stderr)
		}
		if !strings.Contains(stdout, "repacked 0 refs") {
			t.Errorf("gc should report that nothing was repacked, got stdout: %q", stdout)
		}
		if !strings.Contains(stderr, "no refs") {
			t.Errorf("gc should explain there was nothing to do, got stderr: %q", stderr)
		}
	})

	t.Run("populated registry", func(t *testing.T) {
		reg := registrytest.New()
		ts := reg.Serve(t)
		registrytest.SeedRepository(t, registrytest.Client(t, ts), 3)
		tagsBefore := len(reg.Tags())

		// Seeding left GIT_DIR pointing at the source clone; gc must not have
		// it. Anything it needs comes back down from the registry.
		t.Setenv("GIT_DIR", "")
		t.Chdir(t.TempDir())

		stdout, stderr, err := runCLI(t, "gc", registrytest.URL(ts))
		if err != nil {
			t.Fatalf("gc outside a repository against a populated registry: %v\nstderr: %s", err, stderr)
		}
		if !strings.Contains(stdout, "repacked 1 refs") {
			t.Errorf("gc should report the one ref it repacked, got stdout: %q", stdout)
		}
		if !strings.Contains(stderr, "fetching them to repack") {
			t.Errorf("gc should say it fetched the history it did not have, got stderr: %q", stderr)
		}
		if after := len(reg.Tags()); after >= tagsBefore {
			t.Errorf("gc did not prune anything: %d tags before, %d after", tagsBefore, after)
		}
	})
}
