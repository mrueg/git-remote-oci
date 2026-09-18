package oci

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestAuthWorkedCountsOnlyRepositorySuccesses pins what "the registry accepted
// this client" means. The token endpoint answers 200 to a credential with no
// access to the repository, and the /v2/ ping answers 200 to one with no
// scope; counting either meant a plain "wrong credentials" was explained as a
// session that had expired.
func TestAuthWorkedCountsOnlyRepositorySuccesses(t *testing.T) {
	client, err := NewClient("registry.example.com/team/repo", false)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	response := func(status int, rawURL string) *http.Response {
		u, err := url.Parse(rawURL)
		if err != nil {
			t.Fatalf("parse %s: %v", rawURL, err)
		}
		return &http.Response{StatusCode: status, Request: &http.Request{URL: u}}
	}

	for _, tc := range []struct {
		name string
		resp *http.Response
		want bool
	}{
		{"token endpoint on another host", response(200, "https://auth.example.com/token?scope=repository:team/repo:pull"), false},
		{"token endpoint on the registry host", response(200, "https://registry.example.com/token"), false},
		{"version ping", response(200, "https://registry.example.com/v2/"), false},
		{"another repository", response(200, "https://registry.example.com/v2/other/repo/tags/list"), false},
		{"repository 401", response(401, "https://registry.example.com/v2/team/repo/tags/list"), false},
		{"repository 404", response(404, "https://registry.example.com/v2/team/repo/manifests/_refs"), false},
		{"repository 500", response(500, "https://registry.example.com/v2/team/repo/manifests/_refs"), false},
		{"repository 200", response(200, "https://registry.example.com/v2/team/repo/tags/list"), true},
		{"repository 201", response(201, "https://registry.example.com/v2/team/repo/manifests/main"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client.authWorked.Store(false)
			client.noteResponse(tc.resp)
			if got := client.authWorked.Load(); got != tc.want {
				t.Errorf("authWorked = %v after %d %s, want %v", got, tc.resp.StatusCode, tc.resp.Request.URL, tc.want)
			}
		})
	}
	// A nil response must not panic.
	client.noteResponse(nil)
}

// TestSpoolSweepRemovesOnlyStaleSpoolFiles: the sweep exists to reclaim what a
// killed push left behind, and must touch nothing else in the directory.
func TestSpoolSweepRemovesOnlyStaleSpoolFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, age time.Duration) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		stamp := time.Now().Add(-age)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		return path
	}
	stale := write("packfile-123", 48*time.Hour)
	staleSnapshot := write("snapshot-456", 48*time.Hour)
	fresh := write("packfile-789", time.Minute)
	unrelated := write("keep-me.pack", 48*time.Hour)
	if err := os.Mkdir(filepath.Join(dir, "packfile-dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	sweepSpoolDirNow(dir, time.Now().Add(-spoolStaleAfter))

	for _, gone := range []string{stale, staleSnapshot} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s should have been swept", filepath.Base(gone))
		}
	}
	for _, kept := range []string{fresh, unrelated, filepath.Join(dir, "packfile-dir")} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s should have been left alone: %v", filepath.Base(kept), err)
		}
	}
}

// TestSpooledBlobIsUsableWithoutItsName: the spool file is unlinked as soon as
// it is open, so a crash cannot strand it. Everything the upload does with it
// goes through the descriptor, and has to keep working with no name behind it.
func TestSpooledBlobIsUsableWithoutItsName(t *testing.T) {
	t.Setenv("GIT_DIR", t.TempDir())
	blob, logical, err := spoolBlob(MediaTypeGitPackfile, "packfile", func(w io.Writer) (int64, error) {
		n, err := w.Write([]byte("staged bytes"))
		return int64(n), err
	})
	if err != nil {
		t.Fatalf("spoolBlob: %v", err)
	}
	defer func() { _ = blob.Close() }()
	if logical != int64(len("staged bytes")) || blob.desc.Size != logical {
		t.Fatalf("spooled %d logical / %d physical bytes, want %d", logical, blob.desc.Size, len("staged bytes"))
	}

	// Re-read from an offset, as a resumed upload does.
	if _, err := blob.file.Seek(7, io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	rest, err := io.ReadAll(blob.file)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(rest) != "bytes" {
		t.Errorf("read %q from offset 7, want %q", rest, "bytes")
	}
	if err := blob.Close(); err != nil {
		t.Errorf("Close after the early unlink: %v", err)
	}
}
