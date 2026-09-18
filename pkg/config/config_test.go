package config_test

import (
	"os/exec"
	"testing"
	"time"

	"github.com/mrueg/git-remote-oci/pkg/config"
)

// repoWith creates a git repository holding the given config entries and makes
// it the working directory for the test.
//
// Load shells out to `git config --list`, so this exercises the real parser
// rather than a hand-built map — including how git lowercases names and how it
// reports a value containing a newline.
func repoWith(t *testing.T, entries map[string]string) {
	t.Helper()

	// Global and system scope are excluded so the test cannot be swayed by the
	// developer's own ociremote.* settings, the same way GIT_DIR must not be
	// inherited by the dispatch tests.
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", ".")
	for k, v := range entries {
		run("config", k, v)
	}
	t.Chdir(dir)
}

func TestDefaultsWhenNothingIsConfigured(t *testing.T) {
	repoWith(t, nil)
	c := config.Load("origin")

	if got := c.Int(config.KeyConcurrency, 12); got != 12 {
		t.Errorf("Int = %d, want the default 12", got)
	}
	if got := c.Duration(config.KeyPushLockTTL, time.Minute); got != time.Minute {
		t.Errorf("Duration = %v, want the default", got)
	}
	if got := c.String(config.KeyCompression, "none"); got != "none" {
		t.Errorf("String = %q, want the default", got)
	}
}

// TestOutsideARepositoryFallsBackToDefaults: the subcommands can be run
// anywhere, and a tunable that cannot be read is not a reason to fail.
func TestOutsideARepositoryFallsBackToDefaults(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Chdir(t.TempDir())
	c := config.Load("")
	if got := c.Int(config.KeyConcurrency, 12); got != 12 {
		t.Errorf("Int = %d, want the default 12", got)
	}
}

// TestNilConfigIsUsable guards the path where a caller never loaded one.
func TestNilConfigIsUsable(t *testing.T) {
	var c *config.Config
	if got := c.Int(config.KeyConcurrency, 7); got != 7 {
		t.Errorf("Int on nil = %d, want 7", got)
	}
	if got := c.Duration(config.KeyPushLockTTL, time.Minute); got != time.Minute {
		t.Errorf("Duration on nil = %v, want the default", got)
	}
}

func TestRepositoryScope(t *testing.T) {
	repoWith(t, map[string]string{
		"ociremote.concurrency": "3",
		"ociremote.pushlockttl": "90s",
		"ociremote.compression": "zstd",
	})
	c := config.Load("origin")

	if got := c.Int(config.KeyConcurrency, 12); got != 3 {
		t.Errorf("concurrency = %d, want 3", got)
	}
	if got := c.Duration(config.KeyPushLockTTL, time.Minute); got != 90*time.Second {
		t.Errorf("pushlockttl = %v, want 90s", got)
	}
	if got := c.String(config.KeyCompression, "none"); got != "zstd" {
		t.Errorf("compression = %q, want zstd", got)
	}
}

// TestPerRemoteScopeWins is the reason NewHelper is given the remote name.
// Pushing to a local registry and to a hosted one from the same clone are
// different situations and want different numbers.
func TestPerRemoteScopeWins(t *testing.T) {
	repoWith(t, map[string]string{
		"ociremote.concurrency":          "3",
		"remote.slowhost.ociconcurrency": "1",
	})

	if got := config.Load("slowhost").Int(config.KeyConcurrency, 12); got != 1 {
		t.Errorf("slowhost concurrency = %d, want the per-remote 1", got)
	}
	if got := config.Load("origin").Int(config.KeyConcurrency, 12); got != 3 {
		t.Errorf("origin concurrency = %d, want the repository-wide 3", got)
	}
	if got := config.Load("").Int(config.KeyConcurrency, 12); got != 3 {
		t.Errorf("unnamed concurrency = %d, want the repository-wide 3", got)
	}
}

// TestKeyNamesAreCaseInsensitive: git lowercases the section and variable of
// every name, so the natural camelCase spelling has to reach the same entry.
//
// The subsection -- the remote's name -- is not lowercased, by git or here, so
// the remote is asked for under the name it was configured with. This test
// used to load "origin" and expect the "Origin" entry, which passed only
// because the whole name was being folded; see TestRemoteNamesAreCaseSensitive.
func TestKeyNamesAreCaseInsensitive(t *testing.T) {
	repoWith(t, map[string]string{
		"ociRemote.blobConcurrency":    "5",
		"remote.Origin.ociPushLockTTL": "2m",
	})
	c := config.Load("Origin")

	if got := c.Int(config.KeyBlobConcurrency, 64); got != 5 {
		t.Errorf("blobconcurrency = %d, want 5", got)
	}
	if got := c.Duration(config.KeyPushLockTTL, time.Minute); got != 2*time.Minute {
		t.Errorf("pushlockttl = %v, want 2m", got)
	}
}

// TestRemoteNamesAreCaseSensitive: `remote.Foo` and `remote.foo` are two
// remotes to git, and `git config --list` reports the subsection as written.
// Lowercasing the whole name folded them together, so a remote with a capital
// in its name read settings meant for another.
func TestRemoteNamesAreCaseSensitive(t *testing.T) {
	repoWith(t, map[string]string{
		"remote.Foo.ociConcurrency": "2",
		"remote.foo.ociConcurrency": "9",
	})

	if got := config.Load("Foo").Int(config.KeyConcurrency, 12); got != 2 {
		t.Errorf("Foo concurrency = %d, want 2", got)
	}
	if got := config.Load("foo").Int(config.KeyConcurrency, 12); got != 9 {
		t.Errorf("foo concurrency = %d, want 9", got)
	}
	// A remote configured only under one spelling is not found under the
	// other -- that is a different remote as far as git is concerned.
	repoWith(t, map[string]string{"remote.Bar.ociConcurrency": "4"})
	if got := config.Load("bar").Int(config.KeyConcurrency, 12); got != 12 {
		t.Errorf("bar concurrency = %d, want the default: remote.Bar is a different remote", got)
	}
	if got := config.Load("Bar").Int(config.KeyConcurrency, 12); got != 4 {
		t.Errorf("Bar concurrency = %d, want 4", got)
	}
}

// TestCountAcceptsZero is the difference between Count and Int, and the reason
// Count exists: `ociremote.compactAfter 0` is documented as switching automatic
// compaction off, and Int read it as "use the default", which switched it on.
func TestCountAcceptsZero(t *testing.T) {
	repoWith(t, map[string]string{"ociremote.compactafter": "0"})
	c := config.Load("origin")

	if got := c.Count(config.KeyCompactAfter, 50); got != 0 {
		t.Errorf("Count(compactafter=0) = %d, want 0", got)
	}
	// And Int still refuses it, so nothing that relies on Int rejecting zero
	// has changed underneath.
	if got := c.Int(config.KeyCompactAfter, 50); got != 50 {
		t.Errorf("Int(compactafter=0) = %d, want the default 50", got)
	}
}

// TestCountFallsBackLikeInt: negative and non-numeric values are typos, and a
// typo in a threshold must not stop a push.
func TestCountFallsBackLikeInt(t *testing.T) {
	repoWith(t, map[string]string{"ociremote.compactafter": "-3"})
	if got := config.Load("").Count(config.KeyCompactAfter, 50); got != 50 {
		t.Errorf("Count(-3) = %d, want the default", got)
	}
	repoWith(t, map[string]string{"ociremote.compactafter": "lots"})
	if got := config.Load("").Count(config.KeyCompactAfter, 50); got != 50 {
		t.Errorf("Count(lots) = %d, want the default", got)
	}
	repoWith(t, map[string]string{"ociremote.compactafter": "7"})
	if got := config.Load("").Count(config.KeyCompactAfter, 50); got != 7 {
		t.Errorf("Count(7) = %d, want 7", got)
	}
	var nilConfig *config.Config
	if got := nilConfig.Count(config.KeyCompactAfter, 50); got != 50 {
		t.Errorf("Count on nil = %d, want 50", got)
	}
}

// TestBadValuesFallBackRatherThanFail: a typo in a tunable must not stop a
// push. The cost of the default is a slower transfer; the cost of failing is a
// user unable to work.
func TestBadValuesFallBackRatherThanFail(t *testing.T) {
	repoWith(t, map[string]string{
		"ociremote.concurrency":     "not-a-number",
		"ociremote.blobconcurrency": "-4",
		"ociremote.pushlockttl":     "next tuesday",
		"ociremote.indexlockttl":    "0",
	})
	c := config.Load("")

	if got := c.Int(config.KeyConcurrency, 12); got != 12 {
		t.Errorf("non-numeric concurrency = %d, want the default", got)
	}
	if got := c.Int(config.KeyBlobConcurrency, 64); got != 64 {
		t.Errorf("negative blobconcurrency = %d, want the default", got)
	}
	if got := c.Duration(config.KeyPushLockTTL, time.Minute); got != time.Minute {
		t.Errorf("unparseable ttl = %v, want the default", got)
	}
	if got := c.Duration(config.KeyIndexLockTTL, time.Minute); got != time.Minute {
		t.Errorf("zero ttl = %v, want the default", got)
	}
}

// TestBareIntegerDurationIsSeconds: a git config value is more likely to be
// written as a plain number than as a Go duration.
func TestBareIntegerDurationIsSeconds(t *testing.T) {
	repoWith(t, map[string]string{"ociremote.indexlockttl": "45"})
	if got := config.Load("").Duration(config.KeyIndexLockTTL, time.Minute); got != 45*time.Second {
		t.Errorf("bare integer = %v, want 45s", got)
	}
}

// TestBoolAcceptsGitsSpellings covers the accessor behind the two keys that
// decide whether a feature runs at all — ociremote.protocolV2 and
// ociremote.shallowSnapshot. Everything else here is a tunable, where a
// misread value costs a slower transfer; these two change which protocol is
// spoken and what a push publishes, so "on" quietly reading as off is a
// different class of wrong.
//
// The spellings are git's own, and they are set through `git config` rather
// than a hand-built map so that what git actually stores is what gets parsed.
func TestBoolAcceptsGitsSpellings(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"true", true},
		{"yes", true},
		{"on", true},
		{"1", true},
		{"false", false},
		{"no", false},
		{"off", false},
		{"0", false},
		// git lowercases the key but not the value, so the parser has to.
		{"TRUE", true},
		{"Off", false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			repoWith(t, map[string]string{"ociremote.protocolv2": tc.value})
			// Asked for with the opposite default, so a value that failed to
			// parse would be indistinguishable from one that was read.
			if got := config.Load("origin").Bool(config.KeyProtocolV2, !tc.want); got != tc.want {
				t.Errorf("Bool(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

// TestBoolFallsBackRatherThanFailing: an unparseable value is a typo, and a
// typo in a tunable should not stop someone pushing. The default stands.
func TestBoolFallsBackRatherThanFailing(t *testing.T) {
	for _, value := range []string{"maybe", "2", "", "tru"} {
		t.Run("value="+value, func(t *testing.T) {
			repoWith(t, map[string]string{"ociremote.protocolv2": value})
			c := config.Load("origin")
			if !c.Bool(config.KeyProtocolV2, true) {
				t.Errorf("Bool(%q) with default true = false", value)
			}
			if c.Bool(config.KeyProtocolV2, false) {
				t.Errorf("Bool(%q) with default false = true", value)
			}
		})
	}
}

// TestBoolIsUnsetWhenAbsent pins the case that matters most: both keys are off
// by default, and a repository that has never heard of them must stay that way.
func TestBoolIsUnsetWhenAbsent(t *testing.T) {
	repoWith(t, nil)
	c := config.Load("origin")
	if c.Bool(config.KeyProtocolV2, false) {
		t.Error("protocolV2 reads as enabled in a repository that never set it")
	}
	if c.Bool(config.KeyShallowSnapshot, false) {
		t.Error("shallowSnapshot reads as enabled in a repository that never set it")
	}
}

// TestBoolPerRemoteOverridesRepositoryWide: pushing to a local registry and a
// hosted one from the same clone are different situations, which is the whole
// point of the per-remote form.
func TestBoolPerRemoteOverridesRepositoryWide(t *testing.T) {
	repoWith(t, map[string]string{
		"ociremote.protocolv2":       "false",
		"remote.fancy.ociprotocolv2": "true",
	})
	if config.Load("origin").Bool(config.KeyProtocolV2, false) {
		t.Error("the repository-wide false was not honoured for an unnamed remote")
	}
	if !config.Load("fancy").Bool(config.KeyProtocolV2, false) {
		t.Error("the per-remote true did not override the repository-wide false")
	}
}
