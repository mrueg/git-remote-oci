package test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrueg/git-remote-oci/pkg/oci"
)

func TestRealContainerRegistryE2E(t *testing.T) {
	// 1. Check if Docker CLI & Daemon are available
	if err := exec.CommandContext(t.Context(), "docker", "info").Run(); err != nil {
		t.Skip("Skipping real container E2E test: Docker is not running")
	}

	// 2. Build git-remote-oci binary
	tempBinDir := t.TempDir()

	binaryPath := filepath.Join(tempBinDir, "git-remote-oci")
	buildCmd := exec.CommandContext(t.Context(), "go", "build", "-o", binaryPath, "github.com/mrueg/git-remote-oci")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("Failed to build git-remote-oci binary: %v\nOutput: %s", err, string(out))
	}

	// Prepend tempBinDir to PATH environment variable
	oldPath := os.Getenv("PATH")
	newPath := tempBinDir + string(os.PathListSeparator) + oldPath
	t.Setenv("PATH", newPath)
	t.Setenv("OCI_INSECURE", "1")

	// 3. Start a real registry container on a random free port. Which image is
	// E2E_REGISTRY_IMAGE; see registry_image_test.go.
	containerName := fmt.Sprintf("git-remote-oci-e2e-%d", time.Now().UnixNano())
	t.Logf("Starting Docker container %s (%s)...", containerName, registryImage())

	runCmd := exec.CommandContext(t.Context(), "docker", registryRunArgs(containerName)...)
	runOut, err := runCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to start %s container: %v\nOutput: %s", registryImage(), err, string(runOut))
	}
	lines := strings.Split(strings.TrimSpace(string(runOut)), "\n")
	containerID := strings.TrimSpace(lines[len(lines)-1])

	// Ensure container is stopped & removed on test completion
	t.Cleanup(func() {
		t.Logf("Cleaning up Docker container %s...", containerID)
		_ = exec.CommandContext(context.Background(), "docker", "rm", "-f", containerID).Run()
	})

	// Get host port mapped to container port 5000
	portCmd := exec.CommandContext(t.Context(), "docker", "port", containerID, "5000")
	portOut, err := portCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to get container port: %v\nOutput: %s", err, string(portOut))
	}

	// Output format is e.g. "0.0.0.0:49153\n::1:49153\n"
	portLine := strings.Split(string(portOut), "\n")[0]
	_, portStr, err := net.SplitHostPort(strings.TrimSpace(portLine))
	if err != nil {
		t.Fatalf("Failed to parse mapped container port from %q: %v", portLine, err)
	}

	registryURL := fmt.Sprintf("localhost:%s/test-org/test-repo", portStr)
	t.Logf("Container Registry running at %s", registryURL)

	// Poll registry until responsive (up to 5s)
	readyReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("http://localhost:%s/v2/", portStr), nil)
	if err != nil {
		t.Fatalf("Failed to build readiness request: %v", err)
	}
	ready := false
	for i := 0; i < 50; i++ {
		resp, err := http.DefaultClient.Do(readyReq)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("Registry container at localhost:%s did not respond with 200 OK within 5s", portStr)
	}

	// 4. Create a source Git repository using real git CLI
	srcDir := t.TempDir()

	runGit := func(dir string, args ...string) string {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = os.Environ()
		var outBuf, errBuf bytes.Buffer
		cmd.Stdout = &outBuf
		cmd.Stderr = &errBuf
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %s failed in %s: %v\nStderr: %s\nStdout: %s",
				strings.Join(args, " "), dir, err, errBuf.String(), outBuf.String())
		}
		return outBuf.String()
	}

	runGitAllowError := func(dir string, args ...string) (string, string, error) {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = os.Environ()
		var outBuf, errBuf bytes.Buffer
		cmd.Stdout = &outBuf
		cmd.Stderr = &errBuf
		err := cmd.Run()
		return outBuf.String(), errBuf.String(), err
	}

	// Initialise git repo
	runGit(srcDir, "init", "-b", "main")
	runGit(srcDir, "config", "user.name", "E2E Tester")
	runGit(srcDir, "config", "user.email", "e2e@example.com")

	// Create initial file & commit
	readmePath := filepath.Join(srcDir, "README.md")
	if err := os.WriteFile(readmePath, []byte("# E2E Test Repo\n"), 0644); err != nil {
		t.Fatalf("Failed to write file: %v", err)
	}
	runGit(srcDir, "add", "README.md")
	runGit(srcDir, "commit", "-m", "Initial E2E commit")
	runGit(srcDir, "tag", "v1.0.0")

	// 5. Add OCI remote and push using real `git push` command!
	ociRemoteURL := "oci://" + registryURL
	runGit(srcDir, "remote", "add", "origin", ociRemoteURL)

	t.Logf("Executing real 'git push' to %s...", ociRemoteURL)
	runGit(srcDir, "push", "origin", "main", "--tags")

	// 6. Add second commit and push incrementally
	featurePath := filepath.Join(srcDir, "FEATURE.md")
	if err := os.WriteFile(featurePath, []byte("Incremental feature\n"), 0644); err != nil {
		t.Fatalf("Failed to write feature file: %v", err)
	}
	runGit(srcDir, "add", "FEATURE.md")
	runGit(srcDir, "commit", "-m", "Second incremental commit")
	runGit(srcDir, "tag", "v1.1.0")

	t.Logf("Executing incremental 'git push'...")
	runGit(srcDir, "push", "origin", "main", "--tags")

	// 7. Clone repository from OCI registry in a clean directory using real `git clone`!
	cloneParentDir := t.TempDir()

	t.Logf("Executing real 'git clone %s'...", ociRemoteURL)
	clonedRepoDir := filepath.Join(cloneParentDir, "cloned-repo")
	runGit(cloneParentDir, "clone", ociRemoteURL, "cloned-repo")

	// 8. Verify cloned repository content and git history
	readmeContent, err := os.ReadFile(filepath.Join(clonedRepoDir, "README.md"))
	if err != nil || !strings.Contains(string(readmeContent), "# E2E Test Repo") {
		t.Errorf("Cloned README.md content invalid: %v, content: %s", err, string(readmeContent))
	}

	featureContent, err := os.ReadFile(filepath.Join(clonedRepoDir, "FEATURE.md"))
	if err != nil || !strings.Contains(string(featureContent), "Incremental feature") {
		t.Errorf("Cloned FEATURE.md content invalid: %v, content: %s", err, string(featureContent))
	}

	// Verify tags in cloned repo
	tagsOutput := runGit(clonedRepoDir, "tag", "-l")
	if !strings.Contains(tagsOutput, "v1.0.0") || !strings.Contains(tagsOutput, "v1.1.0") {
		t.Errorf("Cloned repository missing expected tags v1.0.0 / v1.1.0, got:\n%s", tagsOutput)
	}

	// Verify commit log history
	logOutput := runGit(clonedRepoDir, "log", "--oneline")
	if !strings.Contains(logOutput, "Second incremental commit") || !strings.Contains(logOutput, "Initial E2E commit") {
		t.Errorf("Cloned repository log missing commits, got:\n%s", logOutput)
	}

	// Reading a couple of files only proves the tip commit arrived. Ask git
	// whether the object graph is actually closed.
	runGit(clonedRepoDir, "fsck", "--connectivity-only", "--no-dangling")

	// 8b. A push carrying more than one commit.
	//
	// Every push above added exactly one commit, which makes each tip's parent
	// the previous tip and hides an entire class of failure: with two commits
	// in one push the tip's parent is never published on its own, so a fetcher
	// that walks parents stops before reaching the commit the packfile was
	// really cut against and silently drops its objects.
	t.Log("Testing a push that carries several commits...")
	for _, f := range []struct{ name, content, message string }{
		{"MULTI_A.md", "multi a\n", "Multi-commit push, first"},
		{"MULTI_B.md", "multi b\n", "Multi-commit push, second"},
	} {
		if err := os.WriteFile(filepath.Join(srcDir, f.name), []byte(f.content), 0644); err != nil {
			t.Fatalf("Failed to write %s: %v", f.name, err)
		}
		runGit(srcDir, "add", f.name)
		runGit(srcDir, "commit", "-m", f.message)
	}
	runGit(srcDir, "push", "origin", "main")

	multiDir := filepath.Join(cloneParentDir, "cloned-multi")
	runGit(cloneParentDir, "clone", ociRemoteURL, "cloned-multi")
	runGit(multiDir, "fsck", "--connectivity-only", "--no-dangling")
	for _, name := range []string{"README.md", "FEATURE.md", "MULTI_A.md", "MULTI_B.md"} {
		if _, err := os.ReadFile(filepath.Join(multiDir, name)); err != nil {
			t.Errorf("Clone after a multi-commit push is missing %s: %v", name, err)
		}
	}

	// 8c. Force push after rewriting history.
	//
	// The replaced tip is not an ancestor of the new one, so it must not be
	// used to exclude objects from the packfile.
	t.Log("Testing force push after an amend...")
	runGit(srcDir, "commit", "--amend", "-m", "Multi-commit push, second (reworded)")
	runGit(srcDir, "push", "--force", "origin", "main")

	forceDir := filepath.Join(cloneParentDir, "cloned-force")
	runGit(cloneParentDir, "clone", ociRemoteURL, "cloned-force")
	runGit(forceDir, "fsck", "--connectivity-only", "--no-dangling")
	if _, err := os.ReadFile(filepath.Join(forceDir, "MULTI_A.md")); err != nil {
		t.Errorf("Clone after a force push is missing MULTI_A.md: %v", err)
	}

	// 8d. A branch that is not a descendant of main, cloned on its own.
	//
	// Its packfile must not be cut against main just because main is also on
	// the remote: a single-branch clone never fetches main and would be left
	// without the shared objects.
	t.Log("Testing a single-branch clone of a sibling branch...")
	rootCommit := runGit(srcDir, "rev-list", "--max-parents=0", "HEAD")
	runGit(srcDir, "checkout", "-b", "sibling", strings.TrimSpace(rootCommit))
	if err := os.WriteFile(filepath.Join(srcDir, "SIBLING.md"), []byte("sibling\n"), 0644); err != nil {
		t.Fatalf("Failed to write SIBLING.md: %v", err)
	}
	runGit(srcDir, "add", "SIBLING.md")
	runGit(srcDir, "commit", "-m", "Sibling branch commit")
	runGit(srcDir, "push", "--atomic", "origin", "sibling")
	runGit(srcDir, "checkout", "main")

	siblingDir := filepath.Join(cloneParentDir, "cloned-sibling")
	runGit(cloneParentDir, "clone", "--single-branch", "--branch", "sibling", ociRemoteURL, "cloned-sibling")
	runGit(siblingDir, "fsck", "--connectivity-only", "--no-dangling")
	for _, name := range []string{"README.md", "SIBLING.md"} {
		if _, err := os.ReadFile(filepath.Join(siblingDir, name)); err != nil {
			t.Errorf("Single-branch clone of sibling is missing %s: %v", name, err)
		}
	}

	// 9. E2E Verification for Distributed Ref Locking (locks/<ref>.lock)
	t.Log("Testing E2E Distributed Ref Locking...")
	ctx := context.Background()
	ociClient, err := oci.NewClient(registryURL, true)
	if err != nil {
		t.Fatalf("Failed to create OCI client for ref lock test: %v", err)
	}

	// Acquire lock on refs/heads/main
	lockInfo, err := ociClient.AcquireRefLock(ctx, "refs/heads/main", 10*time.Minute)
	if err != nil {
		t.Fatalf("Failed to acquire ref lock for refs/heads/main: %v", err)
	}
	t.Logf("Acquired ref lock for refs/heads/main by %s", lockInfo.Owner)

	// Verify that pushing to refs/heads/main is rejected while locked
	lockTestPath := filepath.Join(srcDir, "LOCK_TEST.md")
	_ = os.WriteFile(lockTestPath, []byte("Lock test payload\n"), 0644)
	runGit(srcDir, "add", "LOCK_TEST.md")
	runGit(srcDir, "commit", "-m", "Commit while locked")

	_, stderrLocked, lockPushErr := runGitAllowError(srcDir, "push", "origin", "main")
	if lockPushErr == nil {
		t.Errorf("Push to locked branch should have failed, but succeeded!")
	}
	if !strings.Contains(stderrLocked, "reference is locked") {
		t.Errorf("Expected 'reference is locked' in stderr, got:\n%s", stderrLocked)
	}

	// Release ref lock and verify push succeeds
	if err := ociClient.ReleaseRefLock(ctx, "refs/heads/main"); err != nil {
		t.Fatalf("Failed to release ref lock: %v", err)
	}
	t.Log("Released ref lock for refs/heads/main")

	runGit(srcDir, "push", "origin", "main")
	t.Log("Push succeeded after releasing ref lock!")

	// 10. E2E Verification for Git LFS File Locking API (_lfs_locks)
	t.Log("Testing E2E Git LFS File Locking API...")
	lfsLock1, err := ociClient.AcquireLFSLock(ctx, "assets/hero.psd", "designer1")
	if err != nil {
		t.Fatalf("AcquireLFSLock failed: %v", err)
	}
	if lfsLock1.Path != "assets/hero.psd" || lfsLock1.Owner.Name != "designer1" {
		t.Fatalf("Unexpected LFS lock: %+v", lfsLock1)
	}

	activeLocks, err := ociClient.FetchLFSLocks(ctx)
	if err != nil || len(activeLocks) != 1 || activeLocks[0].Path != "assets/hero.psd" {
		t.Fatalf("FetchLFSLocks failed or unexpected locks list: %v, locks: %+v", err, activeLocks)
	}

	_, err = ociClient.AcquireLFSLock(ctx, "assets/hero.psd", "designer2")
	if err == nil {
		t.Errorf("Expected duplicate LFS lock acquisition to fail, but succeeded!")
	}

	_, err = ociClient.ReleaseLFSLock(ctx, lfsLock1.ID, false, "designer1")
	if err != nil {
		t.Fatalf("ReleaseLFSLock failed: %v", err)
	}

	activeLocksAfter, err := ociClient.FetchLFSLocks(ctx)
	if err != nil || len(activeLocksAfter) != 0 {
		t.Fatalf("Expected 0 active LFS locks after release, got %d", len(activeLocksAfter))
	}
	t.Log("Git LFS File Locking API verification passed!")

	// 11. E2E Verification for option followtags (git push --follow-tags)
	t.Log("Testing E2E option followtags...")
	runGit(srcDir, "checkout", "-b", "unreachable-branch")
	unreachableFile := filepath.Join(srcDir, "UNREACHABLE.md")
	_ = os.WriteFile(unreachableFile, []byte("Unreachable commit\n"), 0644)
	runGit(srcDir, "add", "UNREACHABLE.md")
	runGit(srcDir, "commit", "-m", "Unreachable commit")
	runGit(srcDir, "tag", "v3.0.0-unreachable")

	runGit(srcDir, "checkout", "main")
	reachableFile := filepath.Join(srcDir, "REACHABLE.md")
	_ = os.WriteFile(reachableFile, []byte("Reachable commit\n"), 0644)
	runGit(srcDir, "add", "REACHABLE.md")
	runGit(srcDir, "commit", "-m", "Reachable commit for followtags")
	runGit(srcDir, "tag", "v2.0.0-reachable")

	t.Logf("Executing push with option followtags=true...")
	outFollow := runGit(srcDir, "push", "-o", "followtags=true", "origin", "main")
	t.Logf("push --follow-tags output:\n%s", outFollow)

	refsAfterFollow, _ := ociClient.ListRefs(ctx)
	t.Logf("Remote refs in registry after followtags push: %+v", refsAfterFollow)

	cloneFollowDir := filepath.Join(cloneParentDir, "cloned-followtags")
	runGit(cloneParentDir, "clone", ociRemoteURL, "cloned-followtags")

	followTagsOutput := runGit(cloneFollowDir, "tag", "-l")
	if !strings.Contains(followTagsOutput, "v2.0.0-reachable") {
		t.Errorf("Reachable tag v2.0.0-reachable missing from follow-tags push clone, got:\n%s", followTagsOutput)
	}
	if strings.Contains(followTagsOutput, "v3.0.0-unreachable") {
		t.Errorf("Unreachable tag v3.0.0-unreachable should NOT have been pushed by --follow-tags, got:\n%s", followTagsOutput)
	}
	t.Log("option followtags verification passed!")

	// 12. E2E Verification for option pushcert (GPG / SSH Signed Push Attestation)
	t.Log("Testing E2E option pushcert annotations...")
	mainManifest, err := ociClient.FetchManifest(ctx, "main")
	if err != nil {
		t.Fatalf("FetchManifest failed for main: %v", err)
	}
	if mainManifest == nil || mainManifest.Annotations == nil {
		t.Fatalf("Main manifest annotations are empty!")
	}
	// Verify revision annotation matches pushed commit
	rev := mainManifest.Annotations["org.opencontainers.image.revision"]
	if rev == "" {
		t.Errorf("Revision annotation missing from main manifest: %+v", mainManifest.Annotations)
	}
	t.Logf("Main manifest verified with revision annotation %s", rev)

	// 13. Test remote ref deletion via real git push
	t.Logf("Testing remote branch and tag deletion via real 'git push'...")
	runGit(srcDir, "checkout", "-b", "feature-delete")
	runGit(srcDir, "push", "origin", "feature-delete")

	runGit(srcDir, "push", "origin", ":feature-delete")
	runGit(srcDir, "push", "origin", ":refs/tags/v1.0.0")

	cloneDelDir := filepath.Join(cloneParentDir, "cloned-after-delete")
	runGit(cloneParentDir, "clone", ociRemoteURL, "cloned-after-delete")
	delTagsOutput := runGit(cloneDelDir, "tag", "-l")
	if strings.Contains(delTagsOutput, "v1.0.0") {
		t.Errorf("Deleted tag v1.0.0 still present in clone after remote deletion: %s", delTagsOutput)
	}

	// 14. E2E Verification for OCI Image Index (vnd.oci.image.index.v1+json)
	t.Log("Testing E2E OCI Image Index (_index tag)...")
	ociIndexObj, err := ociClient.FetchOCIImageIndex(ctx, "_index")
	if err != nil {
		t.Fatalf("FetchOCIImageIndex failed for _index: %v", err)
	}
	if ociIndexObj == nil || ociIndexObj.MediaType != "application/vnd.oci.image.index.v1+json" {
		t.Fatalf("Invalid OCI Image Index mediaType: %+v", ociIndexObj)
	}
	if len(ociIndexObj.Manifests) == 0 {
		t.Fatalf("OCI Image Index manifests array is empty!")
	}
	indexRefs, err := ociClient.FetchOCIImageIndexRefs(ctx, "_index")
	if err != nil {
		t.Fatalf("FetchOCIImageIndexRefs failed: %v", err)
	}
	if _, ok := indexRefs["refs/heads/main"]; !ok {
		t.Errorf("refs/heads/main missing from OCI Image Index refs: %+v", indexRefs)
	}

	// Verify OpenContainers Image Spec v1.1 Annotations in _index manifest descriptors
	firstDesc := ociIndexObj.Manifests[0]
	if firstDesc.Annotations["org.opencontainers.image.vendor"] != "git-remote-oci" {
		t.Errorf("Expected OCI vendor annotation 'git-remote-oci', got %q", firstDesc.Annotations["org.opencontainers.image.vendor"])
	}
	if firstDesc.Annotations["org.opencontainers.image.title"] == "" {
		t.Errorf("Expected non-empty OCI title annotation in index descriptor: %+v", firstDesc.Annotations)
	}
	t.Logf("OCI Image Index verified with %d descriptors and %d refs!", len(ociIndexObj.Manifests), len(indexRefs))

	// 15. E2E Verification for Annotated Tag Object Preservation (git tag -a)
	t.Log("Testing E2E Annotated Tag Object Preservation (git tag -a)...")
	runGit(srcDir, "tag", "-a", "-m", "v3.0 annotated release", "v3.0-annotated")
	stdout, stderr, pushErr := runGitAllowError(srcDir, "push", "origin", "refs/tags/v3.0-annotated")
	t.Logf("push stdout: %s\npush stderr: %s\npushErr: %v", stdout, stderr, pushErr)

	freshClient, err := oci.NewClient(registryURL, true)
	if err != nil {
		t.Fatalf("Failed to create fresh OCI client: %v", err)
	}
	indexRefsAnnotated, err := freshClient.FetchOCIImageIndexRefs(ctx, "_index")
	if err != nil {
		t.Fatalf("FetchOCIImageIndexRefs failed after pushing annotated tag: %v", err)
	}
	annotatedEntry, ok := indexRefsAnnotated["refs/tags/v3.0-annotated"]
	if !ok {
		t.Fatalf("refs/tags/v3.0-annotated missing from OCI Image Index refs: %+v", indexRefsAnnotated)
	}
	if annotatedEntry.TagMessage != "v3.0 annotated release" {
		t.Errorf("Expected TagMessage %q, got %q", "v3.0 annotated release", annotatedEntry.TagMessage)
	}

	cloneTagDir := filepath.Join(cloneParentDir, "cloned-annotated-tag")
	runGit(cloneParentDir, "clone", ociRemoteURL, "cloned-annotated-tag")
	tagListMsg := runGit(cloneTagDir, "tag", "-l", "-n")
	if !strings.Contains(tagListMsg, "v3.0 annotated release") {
		t.Errorf("Cloned repository tag message missing annotated tag message: %s", tagListMsg)
	}
	t.Log("Annotated Tag Object Preservation verified successfully!")

	// 16. E2E Verification for Shallow Clone (git clone --depth 1)
	t.Log("Testing E2E Shallow Clone (git clone --depth 1)...")
	cloneShallowDir := filepath.Join(cloneParentDir, "cloned-shallow")
	runGit(cloneParentDir, "clone", "--depth", "1", ociRemoteURL, "cloned-shallow")
	shallowCountOut := runGit(cloneShallowDir, "rev-list", "--count", "HEAD")
	if countStr := strings.TrimSpace(shallowCountOut); countStr != "1" {
		t.Errorf("Expected shallow clone --depth 1 to contain exactly 1 commit, got %s", countStr)
	}
	t.Log("E2E Shallow Clone verified successfully!")

	// 17. gc compacts the repository, and the result is still clonable.
	//
	// This is the interaction worth testing rather than gc in isolation:
	// commit-id tags are load-bearing, because they are the pack bases later
	// pushes were cut against. gc prunes them, and is only safe because it
	// rewrites every ref as a self-contained packfile first. If that ordering
	// or the resulting pack-bases annotation were wrong, clones would break
	// only *after* a gc - which is exactly the failure nobody would catch.
	t.Log("Testing E2E gc compaction...")

	tagsBefore := listRegistryTags(t, portStr)

	gcCmd := exec.CommandContext(t.Context(), binaryPath, "gc", ociRemoteURL)
	gcCmd.Dir = srcDir
	gcCmd.Env = append(os.Environ(), "OCI_INSECURE=1")
	if gcOut, gcErr := gcCmd.CombinedOutput(); gcErr != nil {
		t.Fatalf("gc failed: %v\n%s", gcErr, gcOut)
	}

	tagsAfter := listRegistryTags(t, portStr)
	if len(tagsAfter) >= len(tagsBefore) {
		t.Errorf("gc did not reduce the tag count: %d before, %d after", len(tagsBefore), len(tagsAfter))
	}
	t.Logf("gc: %d tags before, %d after", len(tagsBefore), len(tagsAfter))

	gcCloneDir := filepath.Join(cloneParentDir, "cloned-after-gc")
	runGit(cloneParentDir, "clone", ociRemoteURL, "cloned-after-gc")
	runGit(gcCloneDir, "fsck", "--connectivity-only", "--no-dangling")
	for _, name := range []string{"README.md", "FEATURE.md", "MULTI_A.md", "MULTI_B.md"} {
		if _, err := os.ReadFile(filepath.Join(gcCloneDir, name)); err != nil {
			t.Errorf("clone after gc is missing %s: %v", name, err)
		}
	}
	t.Log("E2E gc compaction verified successfully!")

	// 18. fsck and break-lock against the real registry.
	t.Log("Testing E2E fsck and break-lock...")

	runBin := func(args ...string) (string, error) {
		cmd := exec.CommandContext(t.Context(), binaryPath, args...)
		cmd.Dir = srcDir
		// GIT_REMOTE_OCI_SUBCOMMAND says these are deliberate subcommand runs.
		// A single-URL subcommand has the same argv as git invoking the helper
		// for a remote of that name, and the binary tells them apart by GIT_DIR,
		// which os.Environ() may or may not carry depending on how the tests
		// were started.
		cmd.Env = append(os.Environ(), "OCI_INSECURE=1", "GIT_REMOTE_OCI_SUBCOMMAND=1")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	fsckOut, fsckErr := runBin("fsck", ociRemoteURL)
	if fsckErr != nil {
		t.Errorf("fsck reported a healthy repository as broken: %v\n%s", fsckErr, fsckOut)
	}
	if !strings.Contains(fsckOut, "fetchable") {
		t.Errorf("fsck output does not report fetchability:\n%s", fsckOut)
	}

	// break-lock on an unlocked ref is a no-op, not an error.
	if out, err := runBin("break-lock", ociRemoteURL, "refs/heads/main"); err != nil {
		t.Errorf("break-lock of an unlocked ref failed: %v\n%s", err, out)
	} else if !strings.Contains(out, "not locked") {
		t.Errorf("break-lock should say the ref is not locked:\n%s", out)
	}

	// With a lock held, it must refuse without --force and succeed with it.
	if _, err := ociClient.AcquireRefLock(ctx, "refs/heads/main", 10*time.Minute); err != nil {
		t.Fatalf("failed to take a lock for the break-lock test: %v", err)
	}
	if out, err := runBin("break-lock", ociRemoteURL, "refs/heads/main"); err == nil {
		t.Errorf("break-lock should refuse someone else's lock without --force:\n%s", out)
	}
	if out, err := runBin("break-lock", "--force", ociRemoteURL, "refs/heads/main"); err != nil {
		t.Errorf("break-lock --force failed: %v\n%s", err, out)
	}
	if held, _, err := ociClient.IsLocked(ctx, "refs/heads/main"); err != nil {
		t.Errorf("IsLocked after break-lock: %v", err)
	} else if held {
		t.Error("the ref is still locked after break-lock --force")
	}
	t.Log("E2E fsck and break-lock verified successfully!")

	// 19. Git LFS objects, end to end against the real registry.
	//
	// Every other LFS assertion in this suite exercises the *locking* API
	// through the client directly. Nothing pushed or fetched an actual LFS
	// object through git, so the whole transfer path was covered by mocks
	// alone — which is how a push could silently publish a ref whose LFS
	// layers had never been uploaded.
	t.Log("Testing E2E Git LFS object transfer...")
	verifyLFSRoundTrip(t, runGit, srcDir, cloneParentDir, ociRemoteURL)

	// 20. --atomic against the real registry.
	//
	// The suite pushed --atomic once, in the happy case. The point of the flag
	// is what happens when one ref in the batch fails.
	t.Log("Testing E2E atomic push rollback...")
	verifyAtomicRollback(t, runGit, runGitAllowError, srcDir, ociRemoteURL)

	// 21. Wire protocol v2 against the real registry.
	//
	// Everything v2 does is covered against an in-process mock, which answers
	// instantly and from memory. What it cannot tell us is whether a packfile
	// streamed out of real registry blobs, through a staging area, into a
	// sideband channel, arrives as something git accepts — the parts most
	// likely to differ are exactly the ones a mock stands in for.
	t.Log("Testing E2E protocol v2 clone, fetch and partial clone...")
	verifyProtocolV2(t, runGit, cloneParentDir, ociRemoteURL)

	// Chunked upload is protocol against a real server, which is exactly the
	// thing a mock cannot vouch for: whether this registry accepts an
	// inclusive Content-Range, hands back a usable Location, and reassembles
	// the chunks into the blob the digest promises.
	t.Log("Testing E2E chunked, resumable blob upload...")
	verifyChunkedUpload(t, runGit, srcDir, cloneParentDir, ociRemoteURL)

	t.Log("All Comprehensive Real Container E2E Tests completed successfully!")
}

// verifyChunkedUpload pushes with the chunk size turned down far enough that an
// ordinary commit is split across several requests.
//
// The default is 32 MiB, so nothing in this suite would otherwise reach the
// chunked path — the fixtures are kilobytes. Lowering it is the only way to
// exercise against a real registry the part that a fake cannot answer for: the
// exact Content-Range spelling, the Location handling, and whether the closing
// PUT accepts the digest of what was reassembled.
//
// A registry that cannot do chunked uploads falls back and still passes; what
// this catches is a registry that accepts the chunks and assembles them wrong,
// which fails the push rather than corrupting anything.
func verifyChunkedUpload(t *testing.T, runGit func(string, ...string) string, srcDir, cloneParentDir, ociRemoteURL string) {
	t.Helper()

	runGit(srcDir, "checkout", "-q", "-b", "chunked")
	// Comfortably more than a few chunks at 64 KiB, and incompressible, so the
	// packfile cannot shrink back under the threshold.
	payload := make([]byte, 1<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "chunked.bin"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(srcDir, "add", "chunked.bin")
	runGit(srcDir, "commit", "-q", "-m", "a blob worth chunking")

	chunked := []string{"-c", "ociremote.chunkSize=64k"}
	runGit(srcDir, append(append([]string{}, chunked...), "push", ociRemoteURL, "chunked")...)

	// The bytes have to survive the round trip exactly. A misassembled blob
	// would normally be caught by the registry's own digest check on the
	// closing PUT, but a registry that does not verify would hand back a
	// packfile that fails to index — so read it back and compare.
	dst := filepath.Join(cloneParentDir, "chunked-clone")
	runGit(cloneParentDir, "clone", "--branch", "chunked", ociRemoteURL, "chunked-clone")
	runGit(dst, "fsck")

	got, err := os.ReadFile(filepath.Join(dst, "chunked.bin"))
	if err != nil {
		t.Fatalf("the chunked blob did not survive the round trip: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("the cloned blob is %d bytes, want %d: the chunks were reassembled wrong",
			len(got), len(payload))
	}
}

// verifyProtocolV2 clones and fetches over the protocol-v2 path, and checks the
// two things it exists to make possible: a filtered clone, and the lazy fetch
// that has to answer for the objects the filter left out.
func verifyProtocolV2(t *testing.T, runGit func(string, ...string) string, cloneParentDir, ociRemoteURL string) {
	t.Helper()

	v2 := []string{"-c", "protocol.version=2", "-c", "ociremote.protocolV2=true"}
	clone := func(dir string, extra ...string) {
		args := append(append([]string{}, v2...), "clone")
		args = append(args, extra...)
		runGit(cloneParentDir, append(args, ociRemoteURL, dir)...)
	}

	// A full clone, checked the way git checks: fsck, and the history it holds.
	full := filepath.Join(cloneParentDir, "v2-full")
	clone("v2-full")
	runGit(full, "fsck")
	if log := runGit(full, "log", "--format=%s", "--all"); !strings.Contains(log, "Initial E2E commit") {
		t.Errorf("protocol v2 clone is missing history: %q", log)
	}
	// An annotated tag must arrive as a tag object, not flattened to the commit
	// it peels to. ls-refs is the only place that distinction can be drawn —
	// the simple `list` output has no peel form — so this is the assertion that
	// the advertisement got it right. (v1.0.0 is gone by now: the deletion step
	// above removed it.)
	if tags := runGit(full, "tag", "--list"); !strings.Contains(tags, "v3.0-annotated") {
		t.Errorf("protocol v2 clone is missing tags: %q", tags)
	}
	if kind := strings.TrimSpace(runGit(full, "cat-file", "-t", "v3.0-annotated")); kind != "tag" {
		t.Errorf("annotated tag arrived as %q, want a tag object", kind)
	}

	// An incremental fetch over the same path must be a no-op that succeeds
	// rather than one that re-reports work.
	runGit(full, append(append([]string{}, v2...), "fetch", "origin")...)
	runGit(full, "fsck")

	// A shallow clone: the depth is applied when the pack is built here, so
	// what arrives has to be both truncated and complete at the boundary.
	shallow := filepath.Join(cloneParentDir, "v2-shallow")
	clone("v2-shallow", "--depth", "1")
	runGit(shallow, "fsck")
	if out := runGit(shallow, "rev-parse", "--is-shallow-repository"); !strings.Contains(out, "true") {
		t.Errorf("--depth 1 over protocol v2 did not produce a shallow repository: %q", out)
	}

	// A partial clone, then a checkout that forces the lazy fetch of every
	// blob the filter omitted. This is the case that cannot work at all
	// without v2, and the one where a wrong answer is a loop rather than an
	// error.
	partial := filepath.Join(cloneParentDir, "v2-partial")
	clone("v2-partial", "--filter=blob:none", "--no-checkout")
	runGit(partial, append(append([]string{}, v2...), "checkout", "-f", "main")...)
	runGit(partial, "fsck")
	if _, err := os.Stat(filepath.Join(partial, "README.md")); err != nil {
		t.Errorf("the lazy fetch did not materialise the working tree: %v", err)
	}
}

// verifyLFSRoundTrip pushes a commit carrying an LFS pointer and clones it back.
//
// git-lfs is not involved and is not required: the helper scans the pushed
// range for pointer files itself and uploads the objects out of the local LFS
// store, which is the state a smudge would have left behind.
func verifyLFSRoundTrip(t *testing.T, runGit func(string, ...string) string, srcDir, cloneParentDir, ociRemoteURL string) {
	t.Helper()

	// Earlier steps leave the working tree on whichever branch they last
	// touched, so say which branch this is about rather than inheriting it.
	runGit(srcDir, "checkout", "-q", "main")

	payload := []byte("large binary payload that must survive a round trip through the registry")
	sum := sha256.Sum256(payload)
	oid := hex.EncodeToString(sum[:])

	// The object, where git-lfs would have put it.
	objPath := filepath.Join(srcDir, ".git", "lfs", "objects", oid[:2], oid[2:4], oid)
	if err := os.MkdirAll(filepath.Dir(objPath), 0o755); err != nil {
		t.Fatalf("mkdir for the LFS object: %v", err)
	}
	if err := os.WriteFile(objPath, payload, 0o644); err != nil {
		t.Fatalf("write the LFS object: %v", err)
	}

	// The pointer, which is what is actually committed.
	pointer := fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n",
		oid, len(payload))
	if err := os.WriteFile(filepath.Join(srcDir, "asset.bin"), []byte(pointer), 0o644); err != nil {
		t.Fatalf("write the LFS pointer: %v", err)
	}
	runGit(srcDir, "add", "asset.bin")
	runGit(srcDir, "commit", "-m", "add an LFS-tracked asset")
	runGit(srcDir, "push", "origin", "main")

	cloneDir := filepath.Join(cloneParentDir, "cloned-lfs")
	runGit(cloneParentDir, "clone", "-b", "main", ociRemoteURL, "cloned-lfs")
	runGit(cloneDir, "fsck", "--connectivity-only", "--no-dangling")

	// The pointer must arrive as a pointer...
	got, err := os.ReadFile(filepath.Join(cloneDir, "asset.bin"))
	if err != nil {
		t.Fatalf("the pointer file is missing from the clone: %v", err)
	}
	if !strings.Contains(string(got), oid) {
		t.Errorf("the checked-out pointer does not name the object:\n%s", got)
	}

	// ...and the object it names must have been restored alongside it, or the
	// clone holds a pointer to nothing.
	restored := filepath.Join(cloneDir, ".git", "lfs", "objects", oid[:2], oid[2:4], oid)
	data, err := os.ReadFile(restored)
	if err != nil {
		t.Fatalf("the LFS object was not restored into the clone: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Errorf("the restored LFS object does not match what was pushed (%d vs %d bytes)",
			len(data), len(payload))
	}
	t.Log("E2E Git LFS object transfer verified successfully!")
}

// verifyAtomicRollback pushes two refs atomically where one must be rejected,
// and checks the other did not move.
//
// A non-fast-forward is used as the failure rather than fault injection,
// because it is a rejection the registry and the helper agree on without any
// help from the test.
func verifyAtomicRollback(t *testing.T,
	runGit func(string, ...string) string,
	runGitAllowError func(string, ...string) (string, string, error),
	srcDir, ociRemoteURL string,
) {
	t.Helper()

	// Two branches, both published.
	runGit(srcDir, "checkout", "-q", "-b", "atomic-a", "main")
	runGit(srcDir, "commit", "-q", "--allow-empty", "-m", "a1")
	runGit(srcDir, "checkout", "-q", "-b", "atomic-b", "main")
	runGit(srcDir, "commit", "-q", "--allow-empty", "-m", "b1")
	runGit(srcDir, "push", "origin", "atomic-a", "atomic-b")

	beforeA := strings.TrimSpace(runGit(srcDir, "rev-parse", "atomic-a"))

	// Advance a, and rewind b so pushing it is a non-fast-forward.
	runGit(srcDir, "checkout", "-q", "atomic-a")
	runGit(srcDir, "commit", "-q", "--allow-empty", "-m", "a2")
	afterA := strings.TrimSpace(runGit(srcDir, "rev-parse", "atomic-a"))
	runGit(srcDir, "checkout", "-q", "atomic-b")
	runGit(srcDir, "reset", "-q", "--hard", "HEAD~1")

	if beforeA == afterA {
		t.Fatal("the test did not actually advance atomic-a")
	}

	_, stderr, err := runGitAllowError(srcDir, "push", "--atomic", "origin", "atomic-a", "atomic-b")
	if err == nil {
		t.Fatalf("an atomic push containing a non-fast-forward succeeded:\n%s", stderr)
	}

	// The whole point: a's update must not have landed.
	remoteA := strings.TrimSpace(runGit(srcDir, "ls-remote", ociRemoteURL, "refs/heads/atomic-a"))
	if remoteA == "" {
		t.Fatal("atomic-a disappeared from the remote")
	}
	if strings.HasPrefix(remoteA, afterA) {
		t.Errorf("atomic-a advanced to %s even though the batch failed; the rollback did not happen", afterA)
	}
	if !strings.HasPrefix(remoteA, beforeA) {
		t.Errorf("atomic-a is at %q, expected it still at %s", remoteA, beforeA)
	}

	runGit(srcDir, "checkout", "-q", "main")
	t.Log("E2E atomic push rollback verified successfully!")
}

// listRegistryTags reads the repository's tag list straight from the registry.
func listRegistryTags(t *testing.T, port string) []string {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("http://localhost:%s/v2/test-org/test-repo/tags/list", port), nil)
	if err != nil {
		t.Fatalf("failed to build the tag list request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("failed to list tags: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var payload struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("failed to decode the tag list: %v", err)
	}
	return payload.Tags
}
