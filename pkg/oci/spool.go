package oci

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	opencontainers "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// A registry blob must be described by its digest and length before the upload
// can start, but a packfile is produced by a generator whose output size is not
// known in advance. Measuring it by buffering it costs as much memory as the
// packfile is large.
//
// spooledBlob stages the bytes on disk instead, digesting them as they are
// written, so memory stays flat regardless of how much history is being pushed.

// spoolDir picks somewhere with real disk behind it.
//
// $GIT_DIR is preferred over the system temporary directory because /tmp is a
// tmpfs on many distributions, which would put the artifact back into RAM and
// defeat the point.
func spoolDir() string {
	if gitDir := os.Getenv("GIT_DIR"); gitDir != "" {
		dir := filepath.Join(gitDir, "git-remote-oci-tmp")
		if err := os.MkdirAll(dir, 0755); err == nil {
			sweepSpoolDir(dir)
			return dir
		}
	}
	return os.TempDir()
}

// spoolStaleAfter is how old a spool file has to be before the sweep removes
// it. A push holds its spool file for as long as the upload takes, so the
// threshold has to be longer than any upload plausibly is; a day is well past
// that and still short enough to matter on a repository pushed daily.
const spoolStaleAfter = 24 * time.Hour

// spoolSweepOnce limits the sweep to one per process. The directory is a
// per-repository fixed path, so sweeping it on every spoolBlob would re-scan
// the same listing once per pushed ref.
var spoolSweepOnce sync.Once

// sweepSpoolDir removes spool files left behind by earlier runs.
//
// Nothing outside the system temp directory is ever cleaned by the platform,
// and a helper killed mid-push — or one that predates the unlink below — left
// its staged packfile in $GIT_DIR for good. Only files this package names are
// touched, and only when they are older than any live upload could be; a
// failure to remove one is not worth reporting, since the next run will try
// again.
func sweepSpoolDir(dir string) {
	spoolSweepOnce.Do(func() { sweepSpoolDirNow(dir, time.Now().Add(-spoolStaleAfter)) })
}

// sweepSpoolDirNow removes this package's spool files in dir last modified
// before cutoff. Separate from the once-only wrapper so it can be tested.
func sweepSpoolDirNow(dir string, cutoff time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !isSpoolFileName(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, entry.Name()))
	}
}

// spoolPrefixes are the namePrefix values spoolBlob is called with; the sweep
// removes nothing named otherwise.
var spoolPrefixes = []string{"packfile-", "snapshot-"}

func isSpoolFileName(name string) bool {
	for _, prefix := range spoolPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// spooledBlob is an artifact staged on disk together with its descriptor.
type spooledBlob struct {
	file *os.File
	desc ocispec.Descriptor
}

// Reader returns the staged content, positioned at the start.
func (s *spooledBlob) Reader() io.Reader { return s.file }

// Close releases the staged file. It is safe to call more than once.
func (s *spooledBlob) Close() error {
	if s.file == nil {
		return nil
	}
	name := s.file.Name()
	err := s.file.Close()
	s.file = nil
	if rmErr := os.Remove(name); err == nil && !os.IsNotExist(rmErr) {
		err = rmErr
	}
	return err
}

// spoolBlob stages everything write produces, digesting as it goes, and returns
// the staged file rewound to the start along with a descriptor for it.
//
// write receives the destination and reports how many *logical* bytes it
// consumed from its source, which differs from the number written whenever a
// compressor sits in between.
func spoolBlob(mediaType, namePrefix string, write func(io.Writer) (int64, error)) (blob *spooledBlob, logical int64, err error) {
	f, err := os.CreateTemp(spoolDir(), namePrefix+"-*")
	if err != nil {
		return nil, 0, fmt.Errorf("failed to create a spool file: %w", err)
	}
	// Unlink straight away where the platform allows it. The file is only ever
	// used through this descriptor — read, seeked, and re-read on a resumed
	// upload — so it does not need a name, and without one a crash cannot
	// strand it. Windows refuses to unlink an open file, so there the file
	// keeps its name and Close removes it, as it always did.
	if runtime.GOOS != "windows" {
		_ = os.Remove(f.Name())
	}
	defer func() {
		if err != nil {
			name := f.Name()
			_ = f.Close()
			_ = os.Remove(name)
		}
	}()

	digester := opencontainers.SHA256.Digester()
	logical, err = write(io.MultiWriter(f, digester.Hash()))
	if err != nil {
		return nil, 0, err
	}

	size, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to measure the spooled blob: %w", err)
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return nil, 0, fmt.Errorf("failed to rewind the spooled blob: %w", err)
	}

	return &spooledBlob{
		file: f,
		desc: ocispec.Descriptor{MediaType: mediaType, Digest: digester.Digest(), Size: size},
	}, logical, nil
}
