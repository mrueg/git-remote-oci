package test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGHCRE2E drives a full push, clone and delete against a real hosted
// registry.
//
// Everything else runs against the CNCF registry:3 reference implementation,
// which is permissive: it accepts manifests without validating them, allows
// manifest deletion, and needs no credentials. A hosted registry does none of
// those things, so the behaviour that matters most for anyone actually using
// this - the token exchange, manifest schema validation, and what happens when
// deletion is refused - has no coverage at all.
//
// Skipped unless E2E_GHCR_REPO names a repository to use, so `go test ./...`
// stays offline and hermetic by default.
func TestGHCRE2E(t *testing.T) {
	repo := os.Getenv("E2E_GHCR_REPO")
	if repo == "" {
		t.Skip("set E2E_GHCR_REPO (e.g. ghcr.io/owner/name/e2e) to run the hosted-registry E2E test")
	}
	if os.Getenv("OCI_PASSWORD") == "" && os.Getenv("OCI_BEARER_TOKEN") == "" && os.Getenv("OCI_TOKEN") == "" {
		t.Fatal("E2E_GHCR_REPO is set but no credentials are; expected OCI_USERNAME/OCI_PASSWORD or a token")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}

	// Each run works on its own refs so concurrent or repeated runs cannot
	// collide. The registry path is shared, which is deliberate: a fresh path
	// per run would leave a new package behind every time.
	suffix := os.Getenv("E2E_GHCR_REF_SUFFIX")
	if suffix == "" {
		suffix = "local"
	}
	branch := "ci-" + suffix
	remoteURL := "oci://" + strings.TrimPrefix(repo, "oci://")
	t.Logf("using %s, branch %s", remoteURL, branch)

	// Plain HTTP must stay off: this is a real registry over TLS, and silently
	// downgrading would make the test pass for the wrong reason.
	t.Setenv("OCI_INSECURE", "")

	binDir := t.TempDir()
	binary := filepath.Join(binDir, "git-remote-oci")
	if out, err := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "github.com/mrueg/git-remote-oci").CombinedOutput(); err != nil {
		t.Fatalf("failed to build git-remote-oci: %v\n%s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	env := []string{
		"GIT_AUTHOR_NAME=E2E", "GIT_AUTHOR_EMAIL=e2e@example.com",
		"GIT_COMMITTER_NAME=E2E", "GIT_COMMITTER_EMAIL=e2e@example.com",
		"GIT_TERMINAL_PROMPT=0",
	}
	run := func(t *testing.T, dir string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	// Always try to remove the refs this run published, even on failure, so a
	// broken run does not leave the shared path accumulating branches.
	t.Cleanup(func() {
		src := t.TempDir()
		cmd := exec.CommandContext(context.Background(), "git", "init", "-q", "-b", "main", ".")
		cmd.Dir, cmd.Env = src, append(os.Environ(), env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("cleanup: git init failed: %v\n%s", err, out)
			return
		}
		for _, ref := range []string{branch, "cleanup-" + suffix} {
			cmd := exec.CommandContext(context.Background(), "git", "push", remoteURL, ":refs/heads/"+ref)
			cmd.Dir, cmd.Env = src, append(os.Environ(), env...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Logf("cleanup: could not delete %s: %v\n%s", ref, err, out)
			}
		}
	})

	srcDir := t.TempDir()
	run(t, srcDir, "init", "-q", "-b", branch, ".")

	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(srcDir, name), []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	// 1. First push: a self-contained packfile, and the first thing that
	//    exercises the token exchange for real.
	write("README.md", "# hosted registry e2e\n")
	run(t, srcDir, "add", "README.md")
	run(t, srcDir, "commit", "-q", "-m", "initial commit")
	run(t, srcDir, "push", remoteURL, branch+":refs/heads/"+branch)

	// 2. A push carrying two commits, which is the shape that produced
	//    unclonable repositories before pack bases were recorded.
	write("SECOND.md", "second\n")
	run(t, srcDir, "add", "SECOND.md")
	run(t, srcDir, "commit", "-q", "-m", "second commit")
	write("THIRD.md", "third\n")
	run(t, srcDir, "add", "THIRD.md")
	run(t, srcDir, "commit", "-q", "-m", "third commit")
	run(t, srcDir, "push", remoteURL, branch+":refs/heads/"+branch)

	// 3. Clone it back and check the object graph is closed. Reading a couple
	//    of files would not catch a pack whose bases never arrived.
	cloneParent := t.TempDir()
	run(t, cloneParent, "clone", "--branch", branch, remoteURL, "clone")
	cloneDir := filepath.Join(cloneParent, "clone")
	run(t, cloneDir, "fsck", "--connectivity-only", "--no-dangling")
	for _, name := range []string{"README.md", "SECOND.md", "THIRD.md"} {
		if _, err := os.ReadFile(filepath.Join(cloneDir, name)); err != nil {
			t.Errorf("clone from %s is missing %s: %v", repo, name, err)
		}
	}

	// 4. An annotated tag, which publishes a manifest carrying tag metadata.
	tagName := "v0.0.0-" + suffix
	run(t, srcDir, "tag", "-a", tagName, "-m", "e2e tag")
	run(t, srcDir, "push", remoteURL, "refs/tags/"+tagName)

	// 5. Deleting a ref. GHCR does not expose manifest deletion the way
	//    the distribution registries do, so this is the one place the tombstone fallback is
	//    exercised against a registry that actually refuses.
	//
	//    The assertion is on the contract, not the mechanism: however the ref
	//    goes away, it must stop being listed and must not come back.
	run(t, srcDir, "push", remoteURL, ":refs/tags/"+tagName)

	remoteRefs := run(t, srcDir, "ls-remote", remoteURL)
	if strings.Contains(remoteRefs, "refs/tags/"+tagName) {
		t.Errorf("deleted tag %s is still advertised:\n%s", tagName, remoteRefs)
	}
	if !strings.Contains(remoteRefs, "refs/heads/"+branch) {
		t.Errorf("branch %s disappeared after deleting an unrelated tag:\n%s", branch, remoteRefs)
	}

	// 6. Pushing again after a deletion must not resurrect it, which is what a
	//    surviving tag rediscovered by enumeration would do.
	write("FOURTH.md", "fourth\n")
	run(t, srcDir, "add", "FOURTH.md")
	run(t, srcDir, "commit", "-q", "-m", "fourth commit")
	run(t, srcDir, "push", remoteURL, branch+":refs/heads/"+branch)

	if after := run(t, srcDir, "ls-remote", remoteURL); strings.Contains(after, "refs/tags/"+tagName) {
		t.Errorf("deleted tag %s came back after a later push:\n%s", tagName, after)
	}
}

// TestGHCRRejectsBadCredentials checks that a hosted registry's refusal is
// reported usefully.
//
// The local registry runs unauthenticated in the other tests, so nothing there
// exercises a real 401, and the message a user sees when their token is wrong
// or expired is exactly the thing worth getting right.
func TestGHCRRejectsBadCredentials(t *testing.T) {
	repo := os.Getenv("E2E_GHCR_REPO")
	if repo == "" {
		t.Skip("set E2E_GHCR_REPO to run the hosted-registry E2E test")
	}

	binDir := t.TempDir()
	binary := filepath.Join(binDir, "git-remote-oci")
	if out, err := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "github.com/mrueg/git-remote-oci").CombinedOutput(); err != nil {
		t.Fatalf("failed to build git-remote-oci: %v\n%s", err, out)
	}

	srcDir := t.TempDir()
	base := []string{
		"GIT_AUTHOR_NAME=E2E", "GIT_AUTHOR_EMAIL=e2e@example.com",
		"GIT_COMMITTER_NAME=E2E", "GIT_COMMITTER_EMAIL=e2e@example.com",
		"GIT_TERMINAL_PROMPT=0",
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"OCI_USERNAME=e2e-wrong-user",
		"OCI_PASSWORD=e2e-definitely-not-a-valid-token",
		"OCI_BEARER_TOKEN=",
		"OCI_TOKEN=",
		"DOCKER_CONFIG=" + t.TempDir(),
	}

	cmd := exec.CommandContext(t.Context(), "git", "ls-remote", "oci://"+strings.TrimPrefix(repo, "oci://"))
	cmd.Dir = srcDir
	cmd.Env = append(os.Environ(), base...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("ls-remote succeeded with invalid credentials:\n%s", out)
	}

	// The resolution order is internal, so a bare 401 leaves the user guessing
	// which of several credential sources was actually used.
	if !strings.Contains(string(out), "OCI_USERNAME") {
		t.Errorf("the failure should name the credential source that was used, got:\n%s", out)
	}
	fmt.Fprintf(os.Stderr, "rejected as expected: %s\n", strings.TrimSpace(string(out)))
}
