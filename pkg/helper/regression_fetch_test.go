package helper_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mrueg/git-remote-oci/internal/registrytest"
	"github.com/mrueg/git-remote-oci/pkg/helper"
)

// TestConcurrentFetchProducesCleanProtocolOutput drives a multi-spec fetch,
// which fans out over an errgroup, and checks that the responses written to
// stdout are well-formed.
//
// Stdout is the remote-helper wire protocol: before printfOut/printlnOut were
// serialised, concurrent workers could interleave mid-line and corrupt the
// session. Run under -race this also covers the shared Helper state those
// workers touch.
func TestConcurrentFetchProducesCleanProtocolOutput(t *testing.T) {
	reg := registrytest.New()
	ts := reg.Serve(t)

	srcDir := newCommitRepo(t)
	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")

	branch := currentBranch(t, srcDir)

	// Publish several refs pointing at the same commit so the fetch batch has
	// multiple specs to run concurrently.
	pushScript := "push " + branch + ":refs/heads/a\n" +
		"push " + branch + ":refs/heads/b\n" +
		"push " + branch + ":refs/heads/c\n" +
		"push " + branch + ":refs/heads/d\n\n"
	if out, err := runHelper(t, registry, pushScript); err != nil {
		t.Fatalf("seeding push failed: %v (output %q)", err, out)
	}

	// Fetch them back into a fresh repository.
	dstDir := t.TempDir()
	if out, err := exec.CommandContext(t.Context(), "git", "init", "--bare", dstDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, out)
	}
	t.Setenv("GIT_DIR", dstDir)

	var listOut strings.Builder
	fetchScript := "list\n"
	for _, name := range []string{"a", "b", "c", "d"} {
		fetchScript += "fetch " + shaOfRef(t, reg, name) + " refs/heads/" + name + "\n"
	}
	fetchScript += "\n"

	h, err := helper.NewHelper("origin", registry, strings.NewReader(fetchScript), &listOut)
	if err != nil {
		t.Fatalf("NewHelper: %v", err)
	}
	if err := h.Run(context.Background()); err != nil {
		t.Fatalf("concurrent fetch failed: %v", err)
	}

	// Every emitted line must be a valid protocol line: a list entry, a lock
	// line, or empty. A torn write shows up here as an unparseable line.
	for _, line := range strings.Split(listOut.String(), "\n") {
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "@"):
		case strings.HasPrefix(line, "lock "):
			if !strings.HasSuffix(line, ".keep") {
				t.Errorf("lock line must name a .keep file, got %q", line)
			}
		default:
			fields := strings.Fields(line)
			if len(fields) != 2 || len(fields[0]) != 40 {
				t.Errorf("malformed protocol line %q", line)
			}
		}
	}
}

// shaOfRef reads back the commit SHA the registry recorded for a ref.
func shaOfRef(t *testing.T, reg *registrytest.Registry, name string) string {
	t.Helper()

	var m ocispec.Manifest
	if err := json.Unmarshal(reg.RawManifest(t, name), &m); err != nil {
		t.Fatalf("unmarshal manifest %q: %v", name, err)
	}
	sha := m.Annotations[ocispec.AnnotationRevision]
	if sha == "" {
		t.Fatalf("manifest %q carries no revision annotation", name)
	}
	return sha
}

// TestFetchDoesNotPullUnrequestedRefs pins that a fetch transfers only what git
// asked for.
//
// handleFetchBatch used to follow every requested spec with a sweep over all
// known remote refs, so `git fetch origin one-branch` downloaded every branch
// on the remote. Git already tells the helper exactly which objects it wants,
// and asks again for anything else it needs.
func TestFetchDoesNotPullUnrequestedRefs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}

	reg := registrytest.New()
	ts := reg.Serve(t)

	// Two *divergent* histories, so the commit behind one ref is not reachable
	// from the other and cannot arrive as a side effect.
	srcDir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = srcDir
		// GIT_DIR must be absent, not empty: git rejects an empty value. An
		// earlier subtest may have exported it via t.Setenv.
		env := make([]string, 0, len(os.Environ()))
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "GIT_DIR=") {
				env = append(env, kv)
			}
		}
		env = append(env,
			"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com")
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "wanted", ".")
	if err := os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("a\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	run("add", "-A")
	run("commit", "-qm", "wanted side")
	run("checkout", "-q", "--orphan", "unwanted")
	run("rm", "-rqf", ".")
	if err := os.WriteFile(filepath.Join(srcDir, "b.txt"), []byte("b\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	run("add", "-A")
	run("commit", "-qm", "unwanted side")

	registry := strings.TrimPrefix(ts.URL, "http://") + "/test-repo"
	t.Setenv("OCI_INSECURE", "1")
	t.Setenv("GIT_DIR", filepath.Join(srcDir, ".git"))

	seed := "push refs/heads/wanted:refs/heads/wanted\npush refs/heads/unwanted:refs/heads/unwanted\n\n"
	if out, err := runHelper(t, registry, seed); err != nil {
		t.Fatalf("seeding push failed: %v (%s)", err, out)
	}

	wantedSHA := shaOfRef(t, reg, "wanted")
	unwantedSHA := shaOfRef(t, reg, "unwanted")
	if wantedSHA == unwantedSHA {
		t.Fatalf("test setup produced identical commits")
	}

	dstDir := t.TempDir()
	if out, err := exec.CommandContext(t.Context(), "git", "init", "--bare", dstDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, out)
	}
	t.Setenv("GIT_DIR", dstDir)

	if _, err := runHelper(t, registry, "list\nfetch "+wantedSHA+" refs/heads/wanted\n\n"); err != nil {
		t.Fatalf("fetch failed: %v", err)
	}

	if out, err := exec.CommandContext(t.Context(), "git", "--git-dir="+dstDir, "cat-file", "-e", wantedSHA).CombinedOutput(); err != nil {
		t.Errorf("requested commit %s is missing: %v: %s", wantedSHA, err, out)
	}
	if err := exec.CommandContext(t.Context(), "git", "--git-dir="+dstDir, "cat-file", "-e", unwantedSHA).Run(); err == nil {
		t.Errorf("unrequested commit %s was fetched anyway", unwantedSHA)
	}
}
