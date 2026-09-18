package test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSHA256RepositoryE2E pushes and clones a repository whose objects are
// SHA-256.
//
// Everything else in the suite uses SHA-1, so nothing caught that object ids
// were validated as exactly 40 hex characters in a dozen places - as tag names,
// as pack-base entries, as revision annotations. A SHA-256 push failed at the
// first of them.
//
// It also covers the protocol side, which is what makes git willing to talk to
// such a repository at all: the object-format capability, and the
// ":object-format sha256" keyword that list emits when git asks for it.
func TestSHA256RepositoryE2E(t *testing.T) {
	if err := exec.CommandContext(t.Context(), "docker", "info").Run(); err != nil {
		t.Skip("Skipping SHA-256 E2E test: Docker is not running")
	}

	binDir := t.TempDir()
	binaryPath := filepath.Join(binDir, "git-remote-oci")
	if out, err := exec.CommandContext(t.Context(), "go", "build", "-o", binaryPath, "github.com/mrueg/git-remote-oci").CombinedOutput(); err != nil {
		t.Fatalf("failed to build git-remote-oci: %v\n%s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OCI_INSECURE", "1")

	name := fmt.Sprintf("git-remote-oci-sha256-%d", time.Now().UnixNano())
	out, err := exec.CommandContext(t.Context(), "docker", registryRunArgs(name)...).CombinedOutput()
	if err != nil {
		t.Fatalf("failed to start registry: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	id := strings.TrimSpace(lines[len(lines)-1])
	t.Cleanup(func() { _ = exec.CommandContext(context.Background(), "docker", "rm", "-f", id).Run() })

	portOut, err := exec.CommandContext(t.Context(), "docker", "port", id, "5000").CombinedOutput()
	if err != nil {
		t.Fatalf("failed to read the container port: %v\n%s", err, portOut)
	}
	_, port, err := net.SplitHostPort(strings.TrimSpace(strings.Split(string(portOut), "\n")[0]))
	if err != nil {
		t.Fatalf("failed to parse the port from %q: %v", portOut, err)
	}
	readyReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("http://localhost:%s/v2/", port), nil)
	if err != nil {
		t.Fatalf("failed to build readiness request: %v", err)
	}
	for range 50 {
		resp, err := http.DefaultClient.Do(readyReq)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	env := []string{
		"GIT_AUTHOR_NAME=E2E", "GIT_AUTHOR_EMAIL=e2e@example.com",
		"GIT_COMMITTER_NAME=E2E", "GIT_COMMITTER_EMAIL=e2e@example.com",
	}
	run := func(t *testing.T, dir string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir, cmd.Env = dir, append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	remote := "oci://localhost:" + port + "/sha256/repo"

	src := t.TempDir()
	run(t, src, "init", "-q", "--object-format=sha256", "-b", "main", ".")
	if got := strings.TrimSpace(run(t, src, "rev-parse", "--show-object-format")); got != "sha256" {
		t.Fatalf("fixture is not a SHA-256 repository: %s", got)
	}
	for _, f := range []struct{ name, msg string }{{"a.txt", "one"}, {"b.txt", "two"}} {
		if err := os.WriteFile(filepath.Join(src, f.name), []byte(f.msg+"\n"), 0644); err != nil {
			t.Fatalf("write: %v", err)
		}
		run(t, src, "add", f.name)
		run(t, src, "commit", "-q", "-m", f.msg)
	}
	tip := strings.TrimSpace(run(t, src, "rev-parse", "HEAD"))
	if len(tip) != 64 {
		t.Fatalf("expected a 64-character object id, got %q", tip)
	}

	run(t, src, "push", remote, "main:refs/heads/main")

	// A second push, so the pack-base path is exercised with 64-character ids
	// rather than only the self-contained first push.
	if err := os.WriteFile(filepath.Join(src, "c.txt"), []byte("three\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	run(t, src, "add", "c.txt")
	run(t, src, "commit", "-q", "-m", "three")
	run(t, src, "push", remote, "main:refs/heads/main")

	parent := t.TempDir()
	run(t, parent, "clone", remote, "clone")
	clone := filepath.Join(parent, "clone")

	if got := strings.TrimSpace(run(t, clone, "rev-parse", "--show-object-format")); got != "sha256" {
		t.Errorf("clone is %s, want sha256", got)
	}
	run(t, clone, "fsck", "--connectivity-only", "--no-dangling")
	for _, f := range []string{"a.txt", "b.txt", "c.txt"} {
		if _, err := os.ReadFile(filepath.Join(clone, f)); err != nil {
			t.Errorf("clone is missing %s: %v", f, err)
		}
	}

	// fsck must be able to walk a SHA-256 repository too: it validates ids
	// before using them as tag names.
	cmd := exec.CommandContext(t.Context(), binaryPath, "fsck", remote)
	cmd.Env = append(os.Environ(), "OCI_INSECURE=1")
	if fsckOut, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("fsck rejected a healthy SHA-256 repository: %v\n%s", err, fsckOut)
	}
}
