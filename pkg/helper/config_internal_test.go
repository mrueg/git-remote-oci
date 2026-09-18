package helper

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// configuredRepo creates a repository holding the given git config entries and
// makes it the working directory.
func configuredRepo(t *testing.T, entries map[string]string) {
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

// TestNewHelperAppliesDefaults pins what the tunables were before they became
// tunable, so a change to a default is a deliberate edit rather than a drift.
func TestNewHelperAppliesDefaults(t *testing.T) {
	configuredRepo(t, nil)

	h, err := NewHelper("origin", "oci://localhost:1/x/y", strings.NewReader(""), &strings.Builder{})
	if err != nil {
		t.Fatalf("NewHelper: %v", err)
	}

	if h.transferWorkers != defaultTransferWorkers {
		t.Errorf("transferWorkers = %d, want %d", h.transferWorkers, defaultTransferWorkers)
	}
	if h.blobWorkers != defaultBlobWorkers {
		t.Errorf("blobWorkers = %d, want %d", h.blobWorkers, defaultBlobWorkers)
	}
	if h.pushLockTTL != defaultPushLockTTL {
		t.Errorf("pushLockTTL = %v, want %v", h.pushLockTTL, defaultPushLockTTL)
	}
	if h.ociClient.RefsIndexLockTTL != oci.DefaultRefsIndexLockTTL {
		t.Errorf("RefsIndexLockTTL = %v, want %v",
			h.ociClient.RefsIndexLockTTL, oci.DefaultRefsIndexLockTTL)
	}
	// Both of these decide whether a feature is served at all, so an accidental
	// flip is not a slower transfer but a different protocol.
	if h.shallowSnapshot != defaultShallowSnapshot {
		t.Errorf("shallowSnapshot = %v, want %v", h.shallowSnapshot, defaultShallowSnapshot)
	}
	if h.protocolV2 != defaultProtocolV2 {
		t.Errorf("protocolV2 = %v, want %v", h.protocolV2, defaultProtocolV2)
	}
}

// TestNewHelperReadsProtocolV2 covers the key that turns on the wire-protocol-v2
// server. The README spells it `ociremote.protocolV2`, and git lowercases the
// variable before this code ever sees it, so the camel-case spelling users are
// told to type has to reach the same key as the lower-case constant.
func TestNewHelperReadsProtocolV2(t *testing.T) {
	for _, spelling := range []string{"ociremote.protocolv2", "ociremote.protocolV2"} {
		t.Run(spelling, func(t *testing.T) {
			configuredRepo(t, map[string]string{spelling: "true"})

			h, err := NewHelper("origin", "oci://localhost:1/x/y", strings.NewReader(""), &strings.Builder{})
			if err != nil {
				t.Fatalf("NewHelper: %v", err)
			}
			if !h.protocolV2 {
				t.Errorf("%s did not enable protocol v2", spelling)
			}
		})
	}
}

// TestNewHelperUsesTheRemoteName is why NewHelper takes a remote name at all.
// It was an unused parameter carrying a "reserved for future use" comment;
// per-remote configuration is that use.
func TestNewHelperUsesTheRemoteName(t *testing.T) {
	configuredRepo(t, map[string]string{
		"ociremote.concurrency":           "8",
		"ociremote.pushlockttl":           "4m",
		"remote.slowhost.ociconcurrency":  "2",
		"remote.slowhost.ocipushlockttl":  "30m",
		"remote.slowhost.ociindexlockttl": "20m",
	})

	slow, err := NewHelper("slowhost", "oci://localhost:1/x/y", strings.NewReader(""), &strings.Builder{})
	if err != nil {
		t.Fatalf("NewHelper(slowhost): %v", err)
	}
	if slow.transferWorkers != 2 {
		t.Errorf("slowhost transferWorkers = %d, want the per-remote 2", slow.transferWorkers)
	}
	if slow.pushLockTTL != 30*time.Minute {
		t.Errorf("slowhost pushLockTTL = %v, want the per-remote 30m", slow.pushLockTTL)
	}
	if slow.ociClient.RefsIndexLockTTL != 20*time.Minute {
		t.Errorf("slowhost RefsIndexLockTTL = %v, want the per-remote 20m",
			slow.ociClient.RefsIndexLockTTL)
	}

	// A different remote in the same repository falls back to the repository
	// scope, not to slowhost's values.
	other, err := NewHelper("origin", "oci://localhost:1/x/y", strings.NewReader(""), &strings.Builder{})
	if err != nil {
		t.Fatalf("NewHelper(origin): %v", err)
	}
	if other.transferWorkers != 8 {
		t.Errorf("origin transferWorkers = %d, want the repository-wide 8", other.transferWorkers)
	}
	if other.pushLockTTL != 4*time.Minute {
		t.Errorf("origin pushLockTTL = %v, want the repository-wide 4m", other.pushLockTTL)
	}
	if other.ociClient.RefsIndexLockTTL != oci.DefaultRefsIndexLockTTL {
		t.Errorf("origin RefsIndexLockTTL = %v, want the default",
			other.ociClient.RefsIndexLockTTL)
	}
}

// TestEnvironmentBeatsConfigForCompression: OCI_COMPRESSION predates the config
// keys and a one-off override must not require editing a config file.
func TestEnvironmentBeatsConfigForCompression(t *testing.T) {
	configuredRepo(t, map[string]string{"ociremote.compression": "zstd"})

	h, err := NewHelper("origin", "oci://localhost:1/x/y", strings.NewReader(""), &strings.Builder{})
	if err != nil {
		t.Fatalf("NewHelper: %v", err)
	}
	if h.ociClient.Compression != "zstd" {
		t.Fatalf("config was not applied: Compression = %q", h.ociClient.Compression)
	}

	t.Setenv("OCI_COMPRESSION", "gzip")
	h, err = NewHelper("origin", "oci://localhost:1/x/y", strings.NewReader(""), &strings.Builder{})
	if err != nil {
		t.Fatalf("NewHelper: %v", err)
	}
	if h.ociClient.Compression != "gzip" {
		t.Errorf("Compression = %q, want the environment to win with gzip", h.ociClient.Compression)
	}
}

// TestPushSpecDst covers the ref name that goes into a failure report.
//
// git reads `error <dst> <reason>` to decide which ref failed, so getting this
// wrong does not lose the error — it attributes it to the wrong ref, or to
// something git cannot match against anything it asked for, and the push
// appears to fail for a ref the user never mentioned.
func TestPushSpecDst(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec string
		want string
	}{
		{"the ordinary form", "refs/heads/main:refs/heads/main", "refs/heads/main"},
		{"source and destination differ", "HEAD:refs/heads/main", "refs/heads/main"},
		// A leading "+" is the force marker and is not part of the ref.
		{"a forced push", "+refs/heads/main:refs/heads/main", "refs/heads/main"},
		// Deleting a ref is an empty source, which must not be mistaken for an
		// empty destination.
		{"a deletion", ":refs/heads/gone", "refs/heads/gone"},
		{"a forced deletion", "+:refs/heads/gone", "refs/heads/gone"},
		// No colon at all: the whole thing is the ref it refers to.
		{"no destination given", "refs/heads/main", "refs/heads/main"},
		// An empty destination is not a ref name, so reporting the spec back
		// verbatim beats reporting nothing at all.
		{"an empty destination", "refs/heads/main:", "refs/heads/main:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pushSpecDst(tc.spec); got != tc.want {
				t.Errorf("pushSpecDst(%q) = %q, want %q", tc.spec, got, tc.want)
			}
		})
	}
}
