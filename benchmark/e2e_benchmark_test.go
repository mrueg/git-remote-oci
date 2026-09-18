package benchmark

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// benchmarkResult holds quantitative metrics for each benchmark run
type benchmarkResult struct {
	category       string
	operation      string
	target         string
	duration       time.Duration
	itemsCount     int
	throughput     float64 // items/sec or MB/sec
	throughputUnit string
	extraInfo      string
}

var (
	allResults []benchmarkResult
	resultsMu  sync.Mutex
)

func recordResult(res benchmarkResult) {
	resultsMu.Lock()
	defer resultsMu.Unlock()
	allResults = append(allResults, res)
}

func printUnifiedBenchmarkReport(t *testing.T) {
	t.Helper()
	resultsMu.Lock()
	defer resultsMu.Unlock()

	if len(allResults) == 0 {
		return
	}

	var reportBuf bytes.Buffer
	reportBuf.WriteString("\n")
	reportBuf.WriteString("========================================================================================================================\n")
	reportBuf.WriteString("                                  E2E PERFORMANCE BENCHMARK SUITE REPORT                                  \n")
	reportBuf.WriteString("========================================================================================================================\n")
	fmt.Fprintf(&reportBuf, "%-18s | %-20s | %-32s | %-10s | %-16s | %s\n", "Category", "Operation", "Target / Protocol", "Duration", "Throughput", "Details")
	reportBuf.WriteString("------------------------------------------------------------------------------------------------------------------------\n")

	for _, res := range allResults {
		tpStr := fmt.Sprintf("%.2f %s", res.throughput, res.throughputUnit)
		fmt.Fprintf(&reportBuf, "%-18s | %-20s | %-32s | %-10s | %-16s | %s\n",
			res.category, res.operation, res.target, fmt.Sprintf("%.2fs", res.duration.Seconds()), tpStr, res.extraInfo)
	}
	reportBuf.WriteString("========================================================================================================================\n")

	reportStr := reportBuf.String()
	t.Log(reportStr)
	fmt.Print(reportStr)

	// Output Markdown summary for GitHub Actions step summary if running in CI
	if summaryFile := os.Getenv("GITHUB_STEP_SUMMARY"); summaryFile != "" {
		var mdBuf bytes.Buffer
		mdBuf.WriteString("## 🚀 Git Remote OCI Performance Benchmark Results\n\n")
		mdBuf.WriteString("| Category | Operation | Target / Protocol | Duration | Throughput | Details |\n")
		mdBuf.WriteString("| :--- | :--- | :--- | :--- | :--- | :--- |\n")
		for _, res := range allResults {
			fmt.Fprintf(&mdBuf, "| **%s** | %s | `%s` | %.2fs | **%.2f %s** | %s |\n",
				res.category, res.operation, res.target, res.duration.Seconds(), res.throughput, res.throughputUnit, res.extraInfo)
		}
		mdBuf.WriteString("\n")
		_ = os.WriteFile(summaryFile, mdBuf.Bytes(), 0644)
	}
}

// findRepoRoot locates the root of git-remote-oci repository
func findRepoRoot(tb testing.TB) string {
	tb.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		tb.Fatalf("Failed to get working dir: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			tb.Fatalf("Could not find repository root (go.mod)")
		}
		dir = parent
	}
}

// buildGitRemoteOCI builds the git-remote-oci binary into a temp dir and adds it to PATH.
func buildGitRemoteOCI(tb testing.TB) {
	tb.Helper()

	repoRoot := findRepoRoot(tb)
	tempBinDir := tb.TempDir()

	binaryPath := filepath.Join(tempBinDir, "git-remote-oci")
	buildCmd := exec.CommandContext(tb.Context(), "go", "build", "-o", binaryPath, "github.com/mrueg/git-remote-oci")
	buildCmd.Dir = repoRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		tb.Fatalf("Failed to build git-remote-oci binary: %v\nOutput: %s", err, string(out))
	}

	oldPath := os.Getenv("PATH")
	newPath := tempBinDir + string(os.PathListSeparator) + oldPath
	tb.Setenv("PATH", newPath)
	tb.Setenv("OCI_INSECURE", "1")
}

// benchmarkRegistryImage is the registry the benchmark runs against. It is not
// configurable the way the E2E suite's is: a benchmark comparing numbers across
// runs has to hold the registry fixed, or the figures are not comparable.
const benchmarkRegistryImage = "registry:3"

// startRegistryContainer starts a Docker registry container on a random host
// port. The image matches the E2E suite's default.
func startRegistryContainer(tb testing.TB) (string, func()) {
	tb.Helper()

	if err := exec.CommandContext(tb.Context(), "docker", "info").Run(); err != nil {
		tb.Skip("Skipping OCI benchmark: Docker is not available")
	}

	containerName := fmt.Sprintf("git-remote-oci-bench-%d", time.Now().UnixNano())
	// Match the E2E suite: manifest deletion is off by default.
	runCmd := exec.CommandContext(tb.Context(), "docker", "run", "-d", "--name", containerName,
		"-p", "0:5000",
		"-e", "REGISTRY_STORAGE_DELETE_ENABLED=true",
		benchmarkRegistryImage)
	runOut, err := runCmd.CombinedOutput()
	if err != nil {
		tb.Fatalf("Failed to start %s container: %v\nOutput: %s", benchmarkRegistryImage, err, string(runOut))
	}
	lines := strings.Split(strings.TrimSpace(string(runOut)), "\n")
	containerID := strings.TrimSpace(lines[len(lines)-1])

	cleanup := func() {
		_ = exec.CommandContext(context.Background(), "docker", "rm", "-f", containerID).Run()
	}

	portCmd := exec.CommandContext(tb.Context(), "docker", "port", containerID, "5000")
	portOut, err := portCmd.CombinedOutput()
	if err != nil {
		cleanup()
		tb.Fatalf("Failed to get container port: %v\nOutput: %s", err, string(portOut))
	}

	portLine := strings.Split(string(portOut), "\n")[0]
	_, portStr, err := net.SplitHostPort(strings.TrimSpace(portLine))
	if err != nil {
		cleanup()
		tb.Fatalf("Failed to parse mapped container port from %q: %v", portLine, err)
	}

	readyReq, err := http.NewRequestWithContext(tb.Context(), http.MethodGet, fmt.Sprintf("http://localhost:%s/v2/", portStr), nil)
	if err != nil {
		cleanup()
		tb.Fatalf("Failed to build readiness request: %v", err)
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
		cleanup()
		tb.Fatalf("Registry container at localhost:%s did not respond within 5s", portStr)
	}

	registryURL := fmt.Sprintf("localhost:%s/bench-org/bench-repo", portStr)
	return registryURL, cleanup
}

// startGitHTTPServer starts a local HTTP server serving Git repositories in parentDir via git-http-backend.
func startGitHTTPServer(tb testing.TB, parentDir string) (string, func()) {
	tb.Helper()

	backendPath := "/usr/lib/git-core/git-http-backend"
	if _, err := os.Stat(backendPath); err != nil {
		if p, err := exec.LookPath("git-http-backend"); err == nil {
			backendPath = p
		}
	}

	cgiHandler := &cgi.Handler{
		Path: backendPath,
		Dir:  parentDir,
		Env: []string{
			"GIT_PROJECT_ROOT=" + parentDir,
			"GIT_HTTP_EXPORT_ALL=1",
		},
	}

	server := httptest.NewServer(cgiHandler)
	return server.URL, server.Close
}

// generateLinearRepo generates a Git repository containing the specified number of linear commits.
func generateLinearRepo(tb testing.TB, dir string, numCommits int) string {
	tb.Helper()

	runGit := func(args ...string) string {
		cmd := exec.CommandContext(tb.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = os.Environ()
		var outBuf, errBuf bytes.Buffer
		cmd.Stdout = &outBuf
		cmd.Stderr = &errBuf
		if err := cmd.Run(); err != nil {
			tb.Fatalf("git %s failed: %v\nStderr: %s", strings.Join(args, " "), err, errBuf.String())
		}
		return strings.TrimSpace(outBuf.String())
	}

	runGit("init", "-b", "main")
	runGit("config", "user.name", "Benchmark Tester")
	runGit("config", "user.email", "benchmark@example.com")

	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Benchmark Repo\n"), 0644); err != nil {
		tb.Fatalf("Failed to write README: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644); err != nil {
		tb.Fatalf("Failed to write main.go: %v", err)
	}

	runGit("add", ".")
	treeSHA := runGit("write-tree")

	var parentSHA string
	for i := 1; i <= numCommits; i++ {
		args := []string{"commit-tree", treeSHA}
		if parentSHA != "" {
			args = append(args, "-p", parentSHA)
		}
		args = append(args, "-m", fmt.Sprintf("Commit %d", i))

		cmd := exec.CommandContext(tb.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = os.Environ()
		var outBuf, errBuf bytes.Buffer
		cmd.Stdout = &outBuf
		cmd.Stderr = &errBuf
		if err := cmd.Run(); err != nil {
			tb.Fatalf("git commit-tree failed at commit %d: %v", i, err)
		}
		parentSHA = strings.TrimSpace(outBuf.String())
	}

	runGit("update-ref", "refs/heads/main", parentSHA)
	return parentSHA
}

// -----------------------------------------------------------------------------
// Benchmark 1: Linear 20k Commits (OCI Remote Helper vs Git HTTP Server)
// -----------------------------------------------------------------------------
func TestBenchmarkLinear20k(t *testing.T) {
	buildGitRemoteOCI(t)

	registryURL, cleanupDocker := startRegistryContainer(t)
	defer cleanupDocker()

	numCommits := 20000
	if envCommits := os.Getenv("BENCHMARK_COMMITS"); envCommits != "" {
		if n, err := strconv.Atoi(envCommits); err == nil && n > 0 {
			numCommits = n
		}
	}

	t.Logf("Generating linear benchmark repo with %d commits...", numCommits)
	srcDir := t.TempDir()

	headSHA := generateLinearRepo(t, srcDir, numCommits)
	t.Logf("Generated %d commits. Head SHA: %s", numCommits, headSHA)

	// 1. OCI Push
	ociPushStart := time.Now()
	cmdPushOCI := exec.CommandContext(t.Context(), "git", "push", "oci://"+registryURL, "main")
	cmdPushOCI.Dir = srcDir
	if out, err := cmdPushOCI.CombinedOutput(); err != nil {
		t.Fatalf("OCI Push failed: %v\nOutput: %s", err, string(out))
	}
	ociPushDur := time.Since(ociPushStart)
	recordResult(benchmarkResult{
		category:       "Linear 20k",
		operation:      "Push",
		target:         "git-remote-oci (Registry)",
		duration:       ociPushDur,
		itemsCount:     numCommits,
		throughput:     float64(numCommits) / ociPushDur.Seconds(),
		throughputUnit: "commits/s",
		extraInfo:      fmt.Sprintf("%d commits", numCommits),
	})

	// 2. OCI Clone
	cloneOCIDir := t.TempDir()

	ociCloneStart := time.Now()
	cmdCloneOCI := exec.CommandContext(t.Context(), "git", "clone", "oci://"+registryURL, cloneOCIDir)
	if out, err := cmdCloneOCI.CombinedOutput(); err != nil {
		t.Fatalf("OCI Clone failed: %v\nOutput: %s", err, string(out))
	}
	ociCloneDur := time.Since(ociCloneStart)
	recordResult(benchmarkResult{
		category:       "Linear 20k",
		operation:      "Clone",
		target:         "git-remote-oci (Registry)",
		duration:       ociCloneDur,
		itemsCount:     numCommits,
		throughput:     float64(numCommits) / ociCloneDur.Seconds(),
		throughputUnit: "commits/s",
		extraInfo:      fmt.Sprintf("%d commits", numCommits),
	})

	// 3. Git HTTP Push & Clone comparison
	gitServerDir := t.TempDir()

	bareRepoDir := filepath.Join(gitServerDir, "linear.git")
	cmdBare := exec.CommandContext(t.Context(), "git", "init", "--bare", bareRepoDir)
	if out, err := cmdBare.CombinedOutput(); err != nil {
		t.Fatalf("Failed to init bare repo: %v\nOutput: %s", err, string(out))
	}
	_ = exec.CommandContext(t.Context(), "git", "--git-dir="+bareRepoDir, "config", "http.receivepack", "true").Run()
	_ = exec.CommandContext(t.Context(), "git", "--git-dir="+bareRepoDir, "config", "http.postBuffer", "524288000").Run()
	_ = exec.CommandContext(t.Context(), "git", "--git-dir="+bareRepoDir, "config", "http.maxRequestBuffer", "524288000").Run()

	serverURL, cleanupServer := startGitHTTPServer(t, gitServerDir)
	defer cleanupServer()

	httpTargetURL := serverURL + "/linear.git"

	httpPushStart := time.Now()
	cmdPushHTTP := exec.CommandContext(t.Context(), "git", "-c", "http.postBuffer=524288000", "push", httpTargetURL, "main")
	cmdPushHTTP.Dir = srcDir
	if out, err := cmdPushHTTP.CombinedOutput(); err != nil {
		t.Fatalf("Git HTTP Push failed: %v\nOutput: %s", err, string(out))
	}
	httpPushDur := time.Since(httpPushStart)
	recordResult(benchmarkResult{
		category:       "Linear 20k",
		operation:      "Push",
		target:         "git-http-backend (HTTP)",
		duration:       httpPushDur,
		itemsCount:     numCommits,
		throughput:     float64(numCommits) / httpPushDur.Seconds(),
		throughputUnit: "commits/s",
		extraInfo:      fmt.Sprintf("%d commits", numCommits),
	})

	cloneHTTPDir := t.TempDir()

	httpCloneStart := time.Now()
	cmdCloneHTTP := exec.CommandContext(t.Context(), "git", "clone", httpTargetURL, cloneHTTPDir)
	if out, err := cmdCloneHTTP.CombinedOutput(); err != nil {
		t.Fatalf("Git HTTP Clone failed: %v\nOutput: %s", err, string(out))
	}
	httpCloneDur := time.Since(httpCloneStart)
	recordResult(benchmarkResult{
		category:       "Linear 20k",
		operation:      "Clone",
		target:         "git-http-backend (HTTP)",
		duration:       httpCloneDur,
		itemsCount:     numCommits,
		throughput:     float64(numCommits) / httpCloneDur.Seconds(),
		throughputUnit: "commits/s",
		extraInfo:      fmt.Sprintf("%d commits", numCommits),
	})

	printUnifiedBenchmarkReport(t)
}

// -----------------------------------------------------------------------------
// Benchmark 2: Incremental Push Overhead (Single Commit Delta)
// -----------------------------------------------------------------------------
func TestBenchmarkIncremental1(t *testing.T) {
	buildGitRemoteOCI(t)

	registryURL, cleanupDocker := startRegistryContainer(t)
	defer cleanupDocker()

	srcDir := t.TempDir()

	baseCommits := 5000
	headSHA := generateLinearRepo(t, srcDir, baseCommits)

	// Initial push of base repository
	_ = exec.CommandContext(t.Context(), "git", "-C", srcDir, "push", "oci://"+registryURL, "main").Run()

	// Add 1 single commit on top
	if err := os.WriteFile(filepath.Join(srcDir, "delta.txt"), []byte("delta update"), 0644); err != nil {
		t.Fatalf("Failed to write delta: %v", err)
	}
	_ = exec.CommandContext(t.Context(), "git", "-C", srcDir, "add", "delta.txt").Run()
	_ = exec.CommandContext(t.Context(), "git", "-C", srcDir, "commit", "-m", "Incremental commit delta").Run()

	// Measure incremental push latency
	incStart := time.Now()
	cmdIncPush := exec.CommandContext(t.Context(), "git", "-C", srcDir, "push", "oci://"+registryURL, "main")
	if out, err := cmdIncPush.CombinedOutput(); err != nil {
		t.Fatalf("Incremental push failed: %v\nOutput: %s", err, string(out))
	}
	incDur := time.Since(incStart)

	recordResult(benchmarkResult{
		category:       "Incremental",
		operation:      "Push (1 commit)",
		target:         "git-remote-oci (Registry)",
		duration:       incDur,
		itemsCount:     1,
		throughput:     1.0 / incDur.Seconds(),
		throughputUnit: "ops/s",
		extraInfo:      fmt.Sprintf("1 commit on top of %d base commits (head: %s)", baseCommits, headSHA[:8]),
	})

	// Measure Git HTTP Incremental Push
	gitServerDir := t.TempDir()
	bareRepoDir := filepath.Join(gitServerDir, "inc.git")
	_ = exec.CommandContext(t.Context(), "git", "init", "--bare", bareRepoDir).Run()
	_ = exec.CommandContext(t.Context(), "git", "--git-dir="+bareRepoDir, "config", "http.receivepack", "true").Run()
	_ = exec.CommandContext(t.Context(), "git", "--git-dir="+bareRepoDir, "config", "http.postBuffer", "524288000").Run()

	serverURL, cleanupServer := startGitHTTPServer(t, gitServerDir)
	defer cleanupServer()

	httpTargetURL := serverURL + "/inc.git"
	_ = exec.CommandContext(t.Context(), "git", "-C", srcDir, "-c", "http.postBuffer=524288000", "push", httpTargetURL, "HEAD~1:refs/heads/main").Run()

	incHTTPStart := time.Now()
	cmdIncPushHTTP := exec.CommandContext(t.Context(), "git", "-C", srcDir, "-c", "http.postBuffer=524288000", "push", httpTargetURL, "main")
	if out, err := cmdIncPushHTTP.CombinedOutput(); err == nil {
		incHTTPDur := time.Since(incHTTPStart)
		recordResult(benchmarkResult{
			category:       "Incremental",
			operation:      "Push (1 commit)",
			target:         "git-http-backend (HTTP)",
			duration:       incHTTPDur,
			itemsCount:     1,
			throughput:     1.0 / incHTTPDur.Seconds(),
			throughputUnit: "ops/s",
			extraInfo:      fmt.Sprintf("1 commit on top of %d base commits", baseCommits),
		})
	} else {
		t.Logf("Git HTTP Incremental Push warning: %v\nOutput: %s", err, string(out))
	}

	printUnifiedBenchmarkReport(t)
}

// -----------------------------------------------------------------------------
// Benchmark 3: Large File Blobs Benchmark (Payload Throughput MB/s)
// -----------------------------------------------------------------------------
func TestBenchmarkLargeBlobs(t *testing.T) {
	buildGitRemoteOCI(t)

	registryURL, cleanupDocker := startRegistryContainer(t)
	defer cleanupDocker()

	srcDir := t.TempDir()

	_ = exec.CommandContext(t.Context(), "git", "-C", srcDir, "init", "-b", "main").Run()
	_ = exec.CommandContext(t.Context(), "git", "-C", srcDir, "config", "user.name", "Bench").Run()
	_ = exec.CommandContext(t.Context(), "git", "-C", srcDir, "config", "user.email", "bench@test.com").Run()

	// Generate 5 x 10MB random binary files = 50MB total payload
	totalMB := 50
	numFiles := 5
	fileSize := (totalMB * 1024 * 1024) / numFiles
	for i := 1; i <= numFiles; i++ {
		payload := make([]byte, fileSize)
		rand.Read(payload)
		_ = os.WriteFile(filepath.Join(srcDir, fmt.Sprintf("large_file_%d.bin", i)), payload, 0644)
	}

	_ = exec.CommandContext(t.Context(), "git", "-C", srcDir, "add", ".").Run()
	_ = exec.CommandContext(t.Context(), "git", "-C", srcDir, "commit", "-m", "Add 50MB binary blobs").Run()

	// Measure Push Throughput
	pushStart := time.Now()
	cmdPush := exec.CommandContext(t.Context(), "git", "-C", srcDir, "push", "oci://"+registryURL, "main")
	if out, err := cmdPush.CombinedOutput(); err != nil {
		t.Fatalf("Large blob push failed: %v\nOutput: %s", err, string(out))
	}
	pushDur := time.Since(pushStart)
	recordResult(benchmarkResult{
		category:       "Large Blobs",
		operation:      "Push (50MB)",
		target:         "git-remote-oci (Registry)",
		duration:       pushDur,
		itemsCount:     totalMB,
		throughput:     float64(totalMB) / pushDur.Seconds(),
		throughputUnit: "MB/s",
		extraInfo:      fmt.Sprintf("%d MB payload across %d files", totalMB, numFiles),
	})

	// Measure Clone Throughput
	cloneDir := t.TempDir()

	cloneStart := time.Now()
	cmdClone := exec.CommandContext(t.Context(), "git", "clone", "oci://"+registryURL, cloneDir)
	if out, err := cmdClone.CombinedOutput(); err != nil {
		t.Fatalf("Large blob clone failed: %v\nOutput: %s", err, string(out))
	}
	cloneDur := time.Since(cloneStart)

	recordResult(benchmarkResult{
		category:       "Large Blobs",
		operation:      "Clone (50MB)",
		target:         "git-remote-oci (Registry)",
		duration:       cloneDur,
		itemsCount:     totalMB,
		throughput:     float64(totalMB) / cloneDur.Seconds(),
		throughputUnit: "MB/s",
		extraInfo:      fmt.Sprintf("%d MB payload across %d files", totalMB, numFiles),
	})

	// Measure Git HTTP Push & Clone for Large Blobs
	gitServerDir := t.TempDir()
	bareRepoDir := filepath.Join(gitServerDir, "large.git")
	_ = exec.CommandContext(t.Context(), "git", "init", "--bare", bareRepoDir).Run()
	_ = exec.CommandContext(t.Context(), "git", "--git-dir="+bareRepoDir, "config", "http.receivepack", "true").Run()
	_ = exec.CommandContext(t.Context(), "git", "--git-dir="+bareRepoDir, "config", "http.postBuffer", "524288000").Run()

	serverURL, cleanupServer := startGitHTTPServer(t, gitServerDir)
	defer cleanupServer()

	httpTargetURL := serverURL + "/large.git"
	httpPushStart := time.Now()
	cmdPushHTTP := exec.CommandContext(t.Context(), "git", "-C", srcDir, "-c", "http.postBuffer=524288000", "push", httpTargetURL, "main")
	if _, err := cmdPushHTTP.CombinedOutput(); err == nil {
		httpPushDur := time.Since(httpPushStart)
		recordResult(benchmarkResult{
			category:       "Large Blobs",
			operation:      "Push (50MB)",
			target:         "git-http-backend (HTTP)",
			duration:       httpPushDur,
			itemsCount:     totalMB,
			throughput:     float64(totalMB) / httpPushDur.Seconds(),
			throughputUnit: "MB/s",
			extraInfo:      fmt.Sprintf("%d MB payload across %d files", totalMB, numFiles),
		})
	}

	httpCloneDir := t.TempDir()
	httpCloneStart := time.Now()
	cmdCloneHTTP := exec.CommandContext(t.Context(), "git", "clone", httpTargetURL, httpCloneDir)
	if _, err := cmdCloneHTTP.CombinedOutput(); err == nil {
		httpCloneDur := time.Since(httpCloneStart)
		recordResult(benchmarkResult{
			category:       "Large Blobs",
			operation:      "Clone (50MB)",
			target:         "git-http-backend (HTTP)",
			duration:       httpCloneDur,
			itemsCount:     totalMB,
			throughput:     float64(totalMB) / httpCloneDur.Seconds(),
			throughputUnit: "MB/s",
			extraInfo:      fmt.Sprintf("%d MB payload across %d files", totalMB, numFiles),
		})
	}

	printUnifiedBenchmarkReport(t)
}

// -----------------------------------------------------------------------------
// Benchmark 4: Wide References Benchmark (1,000 Ref Listing & Indexing)
// -----------------------------------------------------------------------------
func TestBenchmarkWideRefs1k(t *testing.T) {
	buildGitRemoteOCI(t)

	registryURL, cleanupDocker := startRegistryContainer(t)
	defer cleanupDocker()

	srcDir := t.TempDir()

	_ = generateLinearRepo(t, srcDir, 10)

	// Create 1,000 branches
	refCount := 1000
	t.Logf("Creating %d branches for wide refs benchmark...", refCount)
	for i := 1; i <= refCount; i++ {
		branchName := fmt.Sprintf("branch-%04d", i)
		_ = exec.CommandContext(t.Context(), "git", "-C", srcDir, "branch", branchName, "main").Run()
	}

	// Push all 1,000 branches to OCI registry
	pushStart := time.Now()
	cmdPushAll := exec.CommandContext(t.Context(), "git", "-C", srcDir, "push", "oci://"+registryURL, "--all")
	if out, err := cmdPushAll.CombinedOutput(); err != nil {
		t.Fatalf("Wide refs push failed: %v\nOutput: %s", err, string(out))
	}
	pushDur := time.Since(pushStart)
	recordResult(benchmarkResult{
		category:       "Wide Refs",
		operation:      "Push (1,000 branches)",
		target:         "git-remote-oci (Registry)",
		duration:       pushDur,
		itemsCount:     refCount,
		throughput:     float64(refCount) / pushDur.Seconds(),
		throughputUnit: "refs/s",
		extraInfo:      fmt.Sprintf("%d branch refs index upload", refCount),
	})

	// Measure ls-remote listing duration
	lsStart := time.Now()
	cmdLs := exec.CommandContext(t.Context(), "git", "ls-remote", "oci://"+registryURL)
	outLs, err := cmdLs.CombinedOutput()
	if err != nil {
		t.Fatalf("ls-remote failed: %v\nOutput: %s", err, string(outLs))
	}
	lsDur := time.Since(lsStart)

	lines := strings.Split(strings.TrimSpace(string(outLs)), "\n")
	recordResult(benchmarkResult{
		category:       "Wide Refs",
		operation:      "ls-remote Listing",
		target:         "git-remote-oci (Registry)",
		duration:       lsDur,
		itemsCount:     len(lines),
		throughput:     float64(len(lines)) / lsDur.Seconds(),
		throughputUnit: "refs/s",
		extraInfo:      fmt.Sprintf("Fetched %d ref listing entries from _refs index", len(lines)),
	})

	// Measure Git HTTP Push --all & ls-remote for 1,000 branches
	gitServerDir := t.TempDir()
	bareRepoDir := filepath.Join(gitServerDir, "wide.git")
	_ = exec.CommandContext(t.Context(), "git", "init", "--bare", bareRepoDir).Run()
	_ = exec.CommandContext(t.Context(), "git", "--git-dir="+bareRepoDir, "config", "http.receivepack", "true").Run()
	_ = exec.CommandContext(t.Context(), "git", "--git-dir="+bareRepoDir, "config", "http.postBuffer", "524288000").Run()

	serverURL, cleanupServer := startGitHTTPServer(t, gitServerDir)
	defer cleanupServer()

	httpTargetURL := serverURL + "/wide.git"
	httpPushStart := time.Now()
	cmdPushAllHTTP := exec.CommandContext(t.Context(), "git", "-C", srcDir, "-c", "http.postBuffer=524288000", "push", httpTargetURL, "--all")
	if _, err := cmdPushAllHTTP.CombinedOutput(); err == nil {
		httpPushDur := time.Since(httpPushStart)
		recordResult(benchmarkResult{
			category:       "Wide Refs",
			operation:      "Push (1,000 branches)",
			target:         "git-http-backend (HTTP)",
			duration:       httpPushDur,
			itemsCount:     refCount,
			throughput:     float64(refCount) / httpPushDur.Seconds(),
			throughputUnit: "refs/s",
			extraInfo:      fmt.Sprintf("%d branch refs push", refCount),
		})
	}

	httpLsStart := time.Now()
	cmdLsHTTP := exec.CommandContext(t.Context(), "git", "ls-remote", httpTargetURL)
	if outLsHTTP, err := cmdLsHTTP.CombinedOutput(); err == nil {
		httpLsDur := time.Since(httpLsStart)
		linesHTTP := strings.Split(strings.TrimSpace(string(outLsHTTP)), "\n")
		recordResult(benchmarkResult{
			category:       "Wide Refs",
			operation:      "ls-remote Listing",
			target:         "git-http-backend (HTTP)",
			duration:       httpLsDur,
			itemsCount:     len(linesHTTP),
			throughput:     float64(len(linesHTTP)) / httpLsDur.Seconds(),
			throughputUnit: "refs/s",
			extraInfo:      fmt.Sprintf("Fetched %d ref listing entries via HTTP Smart Protocol", len(linesHTTP)),
		})
	}

	printUnifiedBenchmarkReport(t)
}

// -----------------------------------------------------------------------------
// Benchmark 5: Branchy Graph Topology Benchmark (5,000 Commits + 500 Merges)
// -----------------------------------------------------------------------------
func TestBenchmarkBranchyGraph(t *testing.T) {
	buildGitRemoteOCI(t)

	registryURL, cleanupDocker := startRegistryContainer(t)
	defer cleanupDocker()

	srcDir := t.TempDir()

	_ = exec.CommandContext(t.Context(), "git", "-C", srcDir, "init", "-b", "main").Run()
	_ = exec.CommandContext(t.Context(), "git", "-C", srcDir, "config", "user.name", "Bench").Run()
	_ = exec.CommandContext(t.Context(), "git", "-C", srcDir, "config", "user.email", "bench@test.com").Run()

	_ = os.WriteFile(filepath.Join(srcDir, "base.txt"), []byte("initial"), 0644)
	_ = exec.CommandContext(t.Context(), "git", "-C", srcDir, "add", ".").Run()
	_ = exec.CommandContext(t.Context(), "git", "-C", srcDir, "commit", "-m", "initial").Run()

	// Generate 500 merge commits across feature branches
	mergesCount := 500
	t.Logf("Generating branchy merge graph with %d merges...", mergesCount)
	for i := 1; i <= mergesCount; i++ {
		branchName := fmt.Sprintf("feature-%d", i)
		_ = exec.CommandContext(t.Context(), "git", "-C", srcDir, "checkout", "-b", branchName, "main").Run()
		_ = os.WriteFile(filepath.Join(srcDir, fmt.Sprintf("feat_%d.txt", i)), []byte(fmt.Sprintf("feature %d", i)), 0644)
		_ = exec.CommandContext(t.Context(), "git", "-C", srcDir, "add", ".").Run()
		_ = exec.CommandContext(t.Context(), "git", "-C", srcDir, "commit", "-m", fmt.Sprintf("Feature %d", i)).Run()

		_ = exec.CommandContext(t.Context(), "git", "-C", srcDir, "checkout", "main").Run()
		_ = exec.CommandContext(t.Context(), "git", "-C", srcDir, "merge", "--no-ff", branchName, "-m", fmt.Sprintf("Merge %s", branchName)).Run()
	}

	pushStart := time.Now()
	cmdPush := exec.CommandContext(t.Context(), "git", "-C", srcDir, "push", "oci://"+registryURL, "main")
	if out, err := cmdPush.CombinedOutput(); err != nil {
		t.Fatalf("Branchy graph push failed: %v\nOutput: %s", err, string(out))
	}
	pushDur := time.Since(pushStart)

	recordResult(benchmarkResult{
		category:       "Branchy Graph",
		operation:      "Push",
		target:         "git-remote-oci (Registry)",
		duration:       pushDur,
		itemsCount:     mergesCount * 2,
		throughput:     float64(mergesCount*2) / pushDur.Seconds(),
		throughputUnit: "commits/s",
		extraInfo:      fmt.Sprintf("%d feature branch merge graph commits", mergesCount*2),
	})

	// Measure Git HTTP Push for Branchy Graph
	gitServerDir := t.TempDir()
	bareRepoDir := filepath.Join(gitServerDir, "branchy.git")
	_ = exec.CommandContext(t.Context(), "git", "init", "--bare", bareRepoDir).Run()
	_ = exec.CommandContext(t.Context(), "git", "--git-dir="+bareRepoDir, "config", "http.receivepack", "true").Run()
	_ = exec.CommandContext(t.Context(), "git", "--git-dir="+bareRepoDir, "config", "http.postBuffer", "524288000").Run()

	serverURL, cleanupServer := startGitHTTPServer(t, gitServerDir)
	defer cleanupServer()

	httpTargetURL := serverURL + "/branchy.git"
	httpPushStart := time.Now()
	cmdPushHTTP := exec.CommandContext(t.Context(), "git", "-C", srcDir, "-c", "http.postBuffer=524288000", "push", httpTargetURL, "main")
	if _, err := cmdPushHTTP.CombinedOutput(); err == nil {
		httpPushDur := time.Since(httpPushStart)
		recordResult(benchmarkResult{
			category:       "Branchy Graph",
			operation:      "Push",
			target:         "git-http-backend (HTTP)",
			duration:       httpPushDur,
			itemsCount:     mergesCount * 2,
			throughput:     float64(mergesCount*2) / httpPushDur.Seconds(),
			throughputUnit: "commits/s",
			extraInfo:      fmt.Sprintf("%d feature branch merge graph commits", mergesCount*2),
		})
	}

	printUnifiedBenchmarkReport(t)
}

// -----------------------------------------------------------------------------
// Benchmark 6: Parallel Concurrent Clients (10 Concurrent Worker Threads)
// -----------------------------------------------------------------------------
func TestBenchmarkParallelClients(t *testing.T) {
	buildGitRemoteOCI(t)

	registryURL, cleanupDocker := startRegistryContainer(t)
	defer cleanupDocker()

	numWorkers := 4
	t.Logf("Launching %d concurrent worker push/fetch clients...", numWorkers)

	type workerState struct {
		id         int
		dir        string
		branchName string
	}

	workers := make([]*workerState, numWorkers)
	for i := 0; i < numWorkers; i++ {
		id := i + 1
		wDir := t.TempDir()

		_ = generateLinearRepo(t, wDir, 25)
		branchName := fmt.Sprintf("worker-branch-%d", id)
		_ = exec.CommandContext(t.Context(), "git", "-C", wDir, "branch", "-m", "main", branchName).Run()
		workers[i] = &workerState{id: id, dir: wDir, branchName: branchName}
	}

	startAll := time.Now()

	t.Log("Workers launched...")
	// Phase 1: Parallel push
	var pushWg sync.WaitGroup
	errs := make(chan error, numWorkers*2)
	for _, w := range workers {
		pushWg.Add(1)
		go func(ws *workerState) {
			defer pushWg.Done()
			ctxPush, cancelPush := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancelPush()
			cmdPush := exec.CommandContext(ctxPush, "git", "-C", ws.dir, "push", "oci://"+registryURL, ws.branchName)
			cmdPush.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
			if out, err := cmdPush.CombinedOutput(); err != nil {
				errs <- fmt.Errorf("Worker %d push failed: %v\nOutput: %s", ws.id, err, string(out))
			}
		}(w)
	}
	pushWg.Wait()

	// Phase 2: Parallel fetch
	var fetchWg sync.WaitGroup
	for _, w := range workers {
		fetchWg.Add(1)
		go func(ws *workerState) {
			defer fetchWg.Done()
			ctxFetch, cancelFetch := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancelFetch()
			cmdFetch := exec.CommandContext(ctxFetch, "git", "-C", ws.dir, "fetch", "oci://"+registryURL, ws.branchName)
			cmdFetch.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
			if out, err := cmdFetch.CombinedOutput(); err != nil {
				errs <- fmt.Errorf("Worker %d fetch failed: %v\nOutput: %s", ws.id, err, string(out))
			}
		}(w)
	}
	fetchWg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("Concurrent client worker failed: %v", err)
		}
	}
	totalDur := time.Since(startAll)

	recordResult(benchmarkResult{
		category:       "Concurrency",
		operation:      "Parallel Push/Fetch",
		target:         "git-remote-oci (Registry)",
		duration:       totalDur,
		itemsCount:     numWorkers * 100,
		throughput:     float64(numWorkers*100) / totalDur.Seconds(),
		throughputUnit: "commits/s",
		extraInfo:      fmt.Sprintf("%d parallel workers, 100 commits each", numWorkers),
	})

	// Measure Git HTTP Parallel Push & Fetch
	gitServerDir := t.TempDir()
	bareRepoDir := filepath.Join(gitServerDir, "parallel.git")
	_ = exec.CommandContext(t.Context(), "git", "init", "--bare", bareRepoDir).Run()
	_ = exec.CommandContext(t.Context(), "git", "--git-dir="+bareRepoDir, "config", "http.receivepack", "true").Run()
	_ = exec.CommandContext(t.Context(), "git", "--git-dir="+bareRepoDir, "config", "http.postBuffer", "524288000").Run()

	serverURL, cleanupServer := startGitHTTPServer(t, gitServerDir)
	defer cleanupServer()

	httpTargetURL := serverURL + "/parallel.git"
	var wgHTTP sync.WaitGroup
	errsHTTP := make(chan error, numWorkers)
	startHTTPAll := time.Now()

	for workerID := 1; workerID <= numWorkers; workerID++ {
		wgHTTP.Add(1)
		go func(id int) {
			defer wgHTTP.Done()
			wDir := t.TempDir()

			_ = generateLinearRepo(t, wDir, 25)
			branchName := fmt.Sprintf("worker-branch-%d", id)
			_ = exec.CommandContext(t.Context(), "git", "-C", wDir, "branch", "-m", "main", branchName).Run()

			cmdPushHTTP := exec.CommandContext(t.Context(), "git", "-C", wDir, "-c", "http.postBuffer=524288000", "push", httpTargetURL, branchName)
			if out, err := cmdPushHTTP.CombinedOutput(); err != nil {
				errsHTTP <- fmt.Errorf("Worker %d HTTP push failed: %v\nOutput: %s", id, err, string(out))
				return
			}

			cmdFetchHTTP := exec.CommandContext(t.Context(), "git", "-C", wDir, "fetch", httpTargetURL, branchName)
			if out, err := cmdFetchHTTP.CombinedOutput(); err != nil {
				errsHTTP <- fmt.Errorf("Worker %d HTTP fetch failed: %v\nOutput: %s", id, err, string(out))
				return
			}
		}(workerID)
	}

	wgHTTP.Wait()
	close(errsHTTP)
	hasHTTPWorkerErr := false
	for err := range errsHTTP {
		if err != nil {
			hasHTTPWorkerErr = true
		}
	}
	if !hasHTTPWorkerErr {
		totalHTTPDur := time.Since(startHTTPAll)
		recordResult(benchmarkResult{
			category:       "Concurrency",
			operation:      "Parallel Push/Fetch",
			target:         "git-http-backend (HTTP)",
			duration:       totalHTTPDur,
			itemsCount:     numWorkers * 100,
			throughput:     float64(numWorkers*100) / totalHTTPDur.Seconds(),
			throughputUnit: "commits/s",
			extraInfo:      fmt.Sprintf("%d parallel workers, 100 commits each", numWorkers),
		})
	}

	printUnifiedBenchmarkReport(t)
}

// -----------------------------------------------------------------------------
// Benchmark 7: Shallow Clone Throughput (--depth 1 vs --depth 50 vs Full)
// -----------------------------------------------------------------------------
func TestBenchmarkShallowClone(t *testing.T) {
	buildGitRemoteOCI(t)

	registryURL, cleanupDocker := startRegistryContainer(t)
	defer cleanupDocker()

	srcDir := t.TempDir()

	totalCommits := 3000
	_ = generateLinearRepo(t, srcDir, totalCommits)

	// Push 3,000 commits
	_ = exec.CommandContext(t.Context(), "git", "-C", srcDir, "push", "oci://"+registryURL, "main").Run()

	// 1. Shallow Clone depth 1
	shallow1Dir := t.TempDir()

	depth1Start := time.Now()
	cmdD1 := exec.CommandContext(t.Context(), "git", "clone", "--depth", "1", "oci://"+registryURL, shallow1Dir)
	if out, err := cmdD1.CombinedOutput(); err != nil {
		t.Fatalf("Shallow clone --depth 1 failed: %v\nOutput: %s", err, string(out))
	}
	depth1Dur := time.Since(depth1Start)

	recordResult(benchmarkResult{
		category:       "Shallow Clone",
		operation:      "Clone (--depth 1)",
		target:         "git-remote-oci (Registry)",
		duration:       depth1Dur,
		itemsCount:     1,
		throughput:     1.0 / depth1Dur.Seconds(),
		throughputUnit: "ops/s",
		extraInfo:      fmt.Sprintf("Truncated from %d total commits to 1 commit", totalCommits),
	})

	// 2. Shallow Clone depth 50
	shallow50Dir := t.TempDir()

	depth50Start := time.Now()
	cmdD50 := exec.CommandContext(t.Context(), "git", "clone", "--depth", "50", "oci://"+registryURL, shallow50Dir)
	if out, err := cmdD50.CombinedOutput(); err != nil {
		t.Fatalf("Shallow clone --depth 50 failed: %v\nOutput: %s", err, string(out))
	}
	depth50Dur := time.Since(depth50Start)

	recordResult(benchmarkResult{
		category:       "Shallow Clone",
		operation:      "Clone (--depth 50)",
		target:         "git-remote-oci (Registry)",
		duration:       depth50Dur,
		itemsCount:     50,
		throughput:     50.0 / depth50Dur.Seconds(),
		throughputUnit: "commits/s",
		extraInfo:      fmt.Sprintf("Truncated from %d total commits to 50 commits", totalCommits),
	})

	// Measure Git HTTP Shallow Clone (--depth 1 and --depth 50)
	gitServerDir := t.TempDir()
	bareRepoDir := filepath.Join(gitServerDir, "shallow.git")
	_ = exec.CommandContext(t.Context(), "git", "init", "--bare", bareRepoDir).Run()
	_ = exec.CommandContext(t.Context(), "git", "--git-dir="+bareRepoDir, "config", "http.receivepack", "true").Run()
	_ = exec.CommandContext(t.Context(), "git", "--git-dir="+bareRepoDir, "config", "http.postBuffer", "524288000").Run()

	serverURL, cleanupServer := startGitHTTPServer(t, gitServerDir)
	defer cleanupServer()

	httpTargetURL := serverURL + "/shallow.git"
	_ = exec.CommandContext(t.Context(), "git", "-C", srcDir, "-c", "http.postBuffer=524288000", "push", httpTargetURL, "main").Run()

	httpDepth1Dir := t.TempDir()
	d1HTTPStart := time.Now()
	cmdD1HTTP := exec.CommandContext(t.Context(), "git", "clone", "--depth", "1", httpTargetURL, httpDepth1Dir)
	if _, err := cmdD1HTTP.CombinedOutput(); err == nil {
		d1HTTPDur := time.Since(d1HTTPStart)
		recordResult(benchmarkResult{
			category:       "Shallow Clone",
			operation:      "Clone (--depth 1)",
			target:         "git-http-backend (HTTP)",
			duration:       d1HTTPDur,
			itemsCount:     1,
			throughput:     1.0 / d1HTTPDur.Seconds(),
			throughputUnit: "ops/s",
			extraInfo:      fmt.Sprintf("Truncated from %d total commits to 1 commit", totalCommits),
		})
	}

	httpDepth50Dir := t.TempDir()
	d50HTTPStart := time.Now()
	cmdD50HTTP := exec.CommandContext(t.Context(), "git", "clone", "--depth", "50", httpTargetURL, httpDepth50Dir)
	if _, err := cmdD50HTTP.CombinedOutput(); err == nil {
		d50HTTPDur := time.Since(d50HTTPStart)
		recordResult(benchmarkResult{
			category:       "Shallow Clone",
			operation:      "Clone (--depth 50)",
			target:         "git-http-backend (HTTP)",
			duration:       d50HTTPDur,
			itemsCount:     50,
			throughput:     50.0 / d50HTTPDur.Seconds(),
			throughputUnit: "commits/s",
			extraInfo:      fmt.Sprintf("Truncated from %d total commits to 50 commits", totalCommits),
		})
	}

	printUnifiedBenchmarkReport(t)
}

// -----------------------------------------------------------------------------
// Benchmark 8: Shallow snapshot (--depth 1 cost and benefit)
// -----------------------------------------------------------------------------

// registryStoredBytes sums the unique blobs a repository occupies, by walking
// its tags and adding up the config and layer sizes each manifest declares.
//
// Bytes rather than wall-clock, because that is what the snapshot trades. Both
// sides of this benchmark run against a registry on loopback, where a transfer
// saving of several times over barely registers as time.
func registryStoredBytes(tb testing.TB, repoRef string) int64 {
	tb.Helper()

	host, name, ok := strings.Cut(repoRef, "/")
	if !ok {
		tb.Fatalf("cannot split %q into host and repository", repoRef)
	}
	base := "http://" + host + "/v2/" + name

	tagsReq, err := http.NewRequestWithContext(tb.Context(), http.MethodGet, base+"/tags/list", nil)
	if err != nil {
		tb.Fatalf("list tags: %v", err)
	}
	resp, err := http.DefaultClient.Do(tagsReq)
	if err != nil {
		tb.Fatalf("list tags: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var tagList struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tagList); err != nil {
		tb.Fatalf("decode tags: %v", err)
	}

	seen := map[string]int64{}
	for _, tag := range tagList.Tags {
		req, err := http.NewRequestWithContext(tb.Context(), http.MethodGet, base+"/manifests/"+tag, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Accept", strings.Join([]string{
			"application/vnd.oci.image.manifest.v1+json",
			"application/vnd.oci.image.index.v1+json",
		}, ", "))
		mResp, err := http.DefaultClient.Do(req)
		if err != nil {
			continue
		}
		var m struct {
			Config struct {
				Digest string `json:"digest"`
				Size   int64  `json:"size"`
			} `json:"config"`
			Layers []struct {
				Digest string `json:"digest"`
				Size   int64  `json:"size"`
			} `json:"layers"`
		}
		decErr := json.NewDecoder(mResp.Body).Decode(&m)
		_ = mResp.Body.Close()
		if decErr != nil {
			continue
		}
		if m.Config.Digest != "" {
			seen[m.Config.Digest] = m.Config.Size
		}
		for _, l := range m.Layers {
			seen[l.Digest] = l.Size
		}
	}

	var total int64
	for _, size := range seen {
		total += size
	}
	return total
}

// dirBytes is how much a clone actually landed on disk.
func dirBytes(tb testing.TB, dir string) int64 {
	tb.Helper()
	var total int64
	// The directory was created moments ago by a clone that succeeded, so an
	// error here means the measurement is wrong rather than merely incomplete.
	err := filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		tb.Fatalf("walk %s: %v", dir, err)
	}
	return total
}

// TestBenchmarkShallowSnapshot measures what the tip snapshot costs and what it
// buys, which are two different numbers and pull in opposite directions.
//
// A shallow clone needs the boundary commit's complete tree, and the stored
// packfiles are incremental, so --depth 1 used to transfer the whole history.
// The snapshot is a self-contained copy of the tip published at push time: the
// clone gets cheaper, and every push gets more expensive by one undeltified
// copy of the tip's tree.
//
// The fixture deliberately has both a long history and a substantial tip. A
// repository with a tiny tip would show the benefit and hide the cost.
func TestBenchmarkShallowSnapshot(t *testing.T) {
	buildGitRemoteOCI(t)

	registryURL, cleanupDocker := startRegistryContainer(t)
	defer cleanupDocker()
	host, _, _ := strings.Cut(registryURL, "/")

	srcDir := t.TempDir()

	const (
		historyCommits = 60
		tipFiles       = 8
		fileBytes      = 64 * 1024
	)

	runGit := func(dir string, args ...string) {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = os.Environ()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	writeRandom := func(name string) {
		payload := make([]byte, fileBytes)
		if _, err := rand.Read(payload); err != nil {
			t.Fatalf("rand: %v", err)
		}
		if err := os.WriteFile(filepath.Join(srcDir, name), payload, 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	// History that actually weighs something: every commit replaces one file
	// with fresh incompressible content, so the accumulated history is far
	// larger than the tip tree. That gap is exactly what a shallow clone is
	// trying not to pay for, and a fixture without it would make the snapshot
	// look free.
	runGit(srcDir, "init", "-q", "-b", "main")
	runGit(srcDir, "config", "user.name", "Benchmark Tester")
	runGit(srcDir, "config", "user.email", "benchmark@example.com")
	for i := range tipFiles {
		writeRandom(fmt.Sprintf("asset%02d.bin", i))
	}
	runGit(srcDir, "add", ".")
	runGit(srcDir, "commit", "-q", "-m", "initial payload")
	for i := range historyCommits {
		writeRandom(fmt.Sprintf("asset%02d.bin", i%tipFiles))
		runGit(srcDir, "add", ".")
		runGit(srcDir, "commit", "-q", "-m", fmt.Sprintf("revision %d", i))
	}

	t.Logf("Fixture: %d commits, tip tree is %d files of %d KB (~%d KB), history ~%d KB",
		historyCommits+1, tipFiles, fileBytes/1024,
		tipFiles*fileBytes/1024, (historyCommits+tipFiles)*fileBytes/1024)

	// Same repository pushed twice, differing only in the setting under test.
	type variant struct {
		name     string
		repo     string
		snapshot string
	}
	for _, v := range []variant{
		{"snapshot on", host + "/bench-snap/on", "true"},
		{"snapshot off", host + "/bench-snap/off", "false"},
	} {
		runGit(srcDir, "config", "ociremote.shallowSnapshot", v.snapshot)

		pushStart := time.Now()
		runGit(srcDir, "push", "oci://"+v.repo, "main")
		pushDur := time.Since(pushStart)
		stored := registryStoredBytes(t, v.repo)

		recordResult(benchmarkResult{
			category:       "Shallow Snapshot",
			operation:      "Push (" + v.name + ")",
			target:         "git-remote-oci (Registry)",
			duration:       pushDur,
			itemsCount:     historyCommits + 1,
			throughput:     float64(stored) / 1024 / 1024 / pushDur.Seconds(),
			throughputUnit: "MB/s",
			extraInfo:      fmt.Sprintf("registry holds %.2f MB", float64(stored)/1024/1024),
		})

		// The sharpest cost: one small commit on top. The incremental packfile
		// is a delta, and the snapshot beside it is not.
		writeRandom("asset00.bin")
		runGit(srcDir, "add", ".")
		runGit(srcDir, "commit", "-q", "-m", "one more revision")
		before := stored
		incStart := time.Now()
		runGit(srcDir, "push", "oci://"+v.repo, "main")
		incDur := time.Since(incStart)
		added := registryStoredBytes(t, v.repo) - before

		recordResult(benchmarkResult{
			category:       "Shallow Snapshot",
			operation:      "Incremental push (" + v.name + ")",
			target:         "git-remote-oci (Registry)",
			duration:       incDur,
			itemsCount:     1,
			throughput:     float64(added) / 1024 / incDur.Seconds(),
			throughputUnit: "KB/s",
			extraInfo:      fmt.Sprintf("one commit added %.0f KB", float64(added)/1024),
		})
		t.Logf("%-14s incremental push added %.0f KB", v.name, float64(added)/1024)

		for _, clone := range []struct {
			label string
			args  []string
		}{
			{"Clone (--depth 1)", []string{"clone", "--depth", "1"}},
			{"Clone (full)", []string{"clone"}},
		} {
			dst := t.TempDir()

			start := time.Now()
			cmd := exec.CommandContext(t.Context(), "git", append(append([]string{}, clone.args...), "oci://"+v.repo, dst)...)
			cmd.Env = os.Environ()
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s (%s) failed: %v\n%s", clone.label, v.name, err, out)
			}
			dur := time.Since(start)
			received := dirBytes(t, filepath.Join(dst, ".git"))
			_ = os.RemoveAll(dst)

			recordResult(benchmarkResult{
				category:       "Shallow Snapshot",
				operation:      clone.label + " " + v.name,
				target:         "git-remote-oci (Registry)",
				duration:       dur,
				itemsCount:     1,
				throughput:     float64(received) / 1024 / 1024 / dur.Seconds(),
				throughputUnit: "MB/s",
				extraInfo:      fmt.Sprintf(".git is %.2f MB", float64(received)/1024/1024),
			})
			t.Logf("%-18s %-18s .git=%.2f MB in %s",
				v.name, clone.label, float64(received)/1024/1024, dur.Round(time.Millisecond))
		}
	}

	printUnifiedBenchmarkReport(t)
}
