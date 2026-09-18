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

	"golang.org/x/crypto/bcrypt"
)

// TestAuthenticatedRegistryE2E drives a full push and clone against a registry
// that requires credentials.
//
// Every other end-to-end test runs against an open registry, so the entire
// authentication path - the challenge, the token exchange, and the credential
// resolution order - was only ever exercised by unit tests against a fake. That
// is the path anyone using a hosted registry depends on.
// authenticatedRegistryArgs builds the `docker run` arguments for a registry
// that demands the credentials in authDir/htpasswd.
//
// The distribution family takes REGISTRY_AUTH_* from the environment. zot takes
// a JSON config file, so one is written beside the htpasswd and the container
// is pointed at it — the image is distroless with `serve /etc/zot/config.json`
// as its command, and overriding that command is how a different path is given.
//
// Anything else gets the distribution form. If that does not take effect the
// registry comes up unauthenticated, which the test detects and reports rather
// than passing a check nothing enforced.
func authenticatedRegistryArgs(t *testing.T, containerName, authDir string) []string {
	t.Helper()

	args := []string{
		"run", "-d", "--name", containerName,
		"-p", "0:5000",
		"-v", authDir + ":/auth",
	}

	if registryAuthStyle() == authStyleZot {
		config := `{
  "distSpecVersion": "1.1.0",
  "storage": { "rootDirectory": "/tmp/zot" },
  "http": {
    "address": "0.0.0.0",
    "port": "5000",
    "auth": { "htpasswd": { "path": "/auth/htpasswd" } }
  },
  "log": { "level": "warn" }
}`
		// Readable by the container, which does not run as this test's user.
		if err := os.WriteFile(filepath.Join(authDir, "config.json"), []byte(config), 0644); err != nil {
			t.Fatalf("write zot config: %v", err)
		}
		args = append(args, extraRegistryArgs()...)
		return append(args, registryImage(), "serve", "/auth/config.json")
	}

	args = append(args,
		"-e", "REGISTRY_AUTH=htpasswd",
		"-e", "REGISTRY_AUTH_HTPASSWD_REALM=Registry Realm",
		"-e", "REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd",
		"-e", "REGISTRY_STORAGE_DELETE_ENABLED=true",
	)
	args = append(args, extraRegistryArgs()...)
	return append(args, registryImage())
}

func TestAuthenticatedRegistryE2E(t *testing.T) {
	if err := exec.CommandContext(t.Context(), "docker", "info").Run(); err != nil {
		t.Skip("Skipping authenticated registry E2E test: Docker is not running")
	}

	const (
		user = "e2e-user"
		pass = "e2e-password"
	)

	binDir := t.TempDir()
	binaryPath := filepath.Join(binDir, "git-remote-oci")
	if out, err := exec.CommandContext(t.Context(), "go", "build", "-o", binaryPath, "github.com/mrueg/git-remote-oci").CombinedOutput(); err != nil {
		t.Fatalf("Failed to build git-remote-oci: %v\n%s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OCI_INSECURE", "1")

	// The distribution registries read an htpasswd file and accept only
	// bcrypt entries.
	hash, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	authDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(authDir, "htpasswd"), []byte(user+":"+string(hash)+"\n"), 0644); err != nil {
		t.Fatalf("write htpasswd: %v", err)
	}
	// The container runs as a different user, so the file has to be readable
	// from outside the test's own account.
	if err := os.Chmod(authDir, 0755); err != nil {
		t.Fatalf("chmod auth dir: %v", err)
	}

	containerName := fmt.Sprintf("git-remote-oci-auth-e2e-%d", time.Now().UnixNano())
	runOut, err := exec.CommandContext(t.Context(), "docker", authenticatedRegistryArgs(t, containerName, authDir)...).CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to start authenticated registry: %v\n%s", err, runOut)
	}
	lines := strings.Split(strings.TrimSpace(string(runOut)), "\n")
	containerID := strings.TrimSpace(lines[len(lines)-1])
	t.Cleanup(func() { _ = exec.CommandContext(context.Background(), "docker", "rm", "-f", containerID).Run() })

	portOut, err := exec.CommandContext(t.Context(), "docker", "port", containerID, "5000").CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to get container port: %v\n%s", err, portOut)
	}
	_, portStr, err := net.SplitHostPort(strings.TrimSpace(strings.Split(string(portOut), "\n")[0]))
	if err != nil {
		t.Fatalf("Failed to parse port from %q: %v", portOut, err)
	}

	// An authenticated registry answers /v2/ with 401 until credentials are
	// supplied, so readiness is "responding", not "responding 200".
	readyReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("http://localhost:%s/v2/", portStr), nil)
	if err != nil {
		t.Fatalf("Failed to build readiness request: %v", err)
	}
	ready, challenged := false, false
	for range 100 {
		resp, err := http.DefaultClient.Do(readyReq)
		if err == nil {
			code := resp.StatusCode
			_ = resp.Body.Close()
			if code == http.StatusUnauthorized || code == http.StatusOK {
				ready = true
				challenged = code == http.StatusUnauthorized
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("Authenticated registry at localhost:%s did not become ready", portStr)
	}

	// No challenge means the registry ignored the htpasswd configuration above.
	// REGISTRY_AUTH_* is how the distribution family is configured and not how
	// everyone is — zot, for one, takes a JSON config file — so for a registry
	// this suite was merely pointed at, that is a statement about the registry
	// rather than a failure. For the one it is developed against it is a
	// failure, and silently skipping would retire the test without saying so.
	if !challenged {
		if registryIsDefault() {
			t.Fatalf("%s served /v2/ without a challenge despite being started with htpasswd configuration; "+
				"the authentication path is no longer being tested", registryImage())
		}
		t.Skipf("%s does not take its authentication configuration from REGISTRY_AUTH_*; "+
			"everything else in this suite still runs against it", registryImage())
	}

	remoteURL := fmt.Sprintf("oci://localhost:%s/test-org/private-repo", portStr)

	runGit := func(dir string, env []string, args ...string) (string, error) {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	srcDir := t.TempDir()
	base := []string{
		"GIT_AUTHOR_NAME=E2E", "GIT_AUTHOR_EMAIL=e2e@example.com",
		"GIT_COMMITTER_NAME=E2E", "GIT_COMMITTER_EMAIL=e2e@example.com",
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", "."},
		{"remote", "add", "origin", remoteURL},
	} {
		if out, err := runGit(srcDir, base, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(srcDir, "SECRET.md"), []byte("private content\n"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	for _, args := range [][]string{{"add", "SECRET.md"}, {"commit", "-q", "-m", "private commit"}} {
		if out, err := runGit(srcDir, base, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// 1. Without credentials the push must fail, and say what to do about it.
	noCreds := append(append([]string{}, base...), "OCI_USERNAME=", "OCI_PASSWORD=", "DOCKER_CONFIG="+t.TempDir())
	out, err := runGit(srcDir, noCreds, "push", "origin", "main")
	if err == nil {
		t.Fatal("push to an authenticated registry succeeded without credentials")
	}
	if !strings.Contains(out, "anonymously") {
		t.Errorf("unauthenticated push should say the request was anonymous, got:\n%s", out)
	}

	// 2. With the wrong password it must fail naming the source.
	badCreds := append(append([]string{}, base...), "OCI_USERNAME="+user, "OCI_PASSWORD=wrong", "DOCKER_CONFIG="+t.TempDir())
	out, err = runGit(srcDir, badCreds, "push", "origin", "main")
	if err == nil {
		t.Fatal("push succeeded with the wrong password")
	}
	if !strings.Contains(out, "OCI_USERNAME") {
		t.Errorf("failed push should name the credential source, got:\n%s", out)
	}

	// 3. With the right credentials, push and clone must both work.
	goodCreds := append(append([]string{}, base...), "OCI_USERNAME="+user, "OCI_PASSWORD="+pass, "DOCKER_CONFIG="+t.TempDir())
	if out, err := runGit(srcDir, goodCreds, "push", "origin", "main"); err != nil {
		t.Fatalf("authenticated push failed: %v\n%s", err, out)
	}

	cloneParent := t.TempDir()
	if out, err := runGit(cloneParent, goodCreds, "clone", remoteURL, "cloned"); err != nil {
		t.Fatalf("authenticated clone failed: %v\n%s", err, out)
	}
	clonedDir := filepath.Join(cloneParent, "cloned")
	if out, err := runGit(clonedDir, goodCreds, "fsck", "--connectivity-only", "--no-dangling"); err != nil {
		t.Fatalf("cloned repository is not connected: %v\n%s", err, out)
	}
	content, err := os.ReadFile(filepath.Join(clonedDir, "SECRET.md"))
	if err != nil || !strings.Contains(string(content), "private content") {
		t.Errorf("cloned SECRET.md is wrong: %v, %q", err, content)
	}
}
