package git

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	billy "github.com/go-git/go-billy/v6"
	"github.com/go-git/go-billy/v6/osfs"
	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/cache"
	"github.com/go-git/go-git/v6/plumbing/format/packfile"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/revlist"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/storage/filesystem"
	"github.com/go-git/go-git/v6/storage/filesystem/dotgit"
	"github.com/mrueg/git-remote-oci/pkg/lfs"
)

// Repository encapsulates go-git operations.
type Repository struct {
	repo   *gogit.Repository
	storer storer.Storer
	// dir pins which git directory the subprocesses address. Empty means the
	// ambient one -- $GIT_DIR, or the nearest .git -- which is right for the
	// repository the helper was invoked in and wrong for a scratch store built
	// beside it. See OpenRepositoryAt.
	dir string
}

// gitDir resolves the repository this one's subprocesses should address,
// preferring an explicitly opened directory over ambient discovery.
func (r *Repository) gitDir() (gitDir, workDir string) {
	if r.dir != "" {
		return r.dir, filepath.Dir(r.dir)
	}
	return gitDirArg()
}

// CommonDir reports the directory holding this repository's shared state:
// the object store, the refs, the shallow file, the LFS cache.
//
// For an ordinary repository that is the git directory itself. For a linked
// worktree it is not: git hands a remote helper GIT_DIR=<repo>/.git/worktrees/
// <name>, a directory holding only what is per-worktree -- HEAD, index, its own
// logs -- and a `commondir` file naming the directory everything else lives
// in. Building paths under GIT_DIR from a linked worktree wrote the shallow
// boundary and looked for imported packs where git itself never puts them.
func (r *Repository) CommonDir() string {
	gitDir, _ := r.gitDir()
	return resolveCommonDir(gitDir)
}

// ObjectsDir reports the directory this repository's objects are stored in.
//
// GIT_OBJECT_DIRECTORY wins if it is set, for the same reason the package-level
// ObjectsDir honours it: git itself does, and so does every subprocess this
// package runs.
func (r *Repository) ObjectsDir() string {
	if env := os.Getenv("GIT_OBJECT_DIRECTORY"); env != "" {
		if abs, err := filepath.Abs(env); err == nil {
			return abs
		}
		return env
	}
	return filepath.Join(r.CommonDir(), "objects")
}

// IsShallow reports whether the repository's history is truncated by a
// $GIT_DIR/shallow graft.
//
// It matters to anything that packs "everything reachable from a tip" out of
// this repository: `git pack-objects --revs` stops at the shallow boundary
// without a word, so a pack built here looks complete and is not.
func (r *Repository) IsShallow() bool {
	content, err := os.ReadFile(filepath.Join(r.CommonDir(), "shallow"))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(content)) != ""
}

// IsPartial reports whether the repository is a partial (promisor) clone, one
// that may be missing objects it expects to fetch lazily on demand.
//
// The signs are git's own: a promisor remote in the configuration, the
// partialClone repository extension, or a promisor pack in the object store.
// Any of them means a walk over local objects can hit a hole, and a pack
// built from it can be short.
func (r *Repository) IsPartial() bool {
	if cfg, err := r.repo.Config(); err == nil && cfg != nil {
		if cfg.Raw.Section("extensions").Option("partialclone") != "" {
			return true
		}
		for _, remote := range cfg.Raw.Section("remote").Subsections {
			if strings.EqualFold(remote.Option("promisor"), "true") {
				return true
			}
		}
	}
	promisors, _ := filepath.Glob(filepath.Join(r.ObjectsDir(), "pack", "*.promisor"))
	return len(promisors) > 0
}

// resolveCommonDir follows a git directory's `commondir` file, if it has one.
//
// The file names the shared directory, relative to the git directory when it
// is not absolute; that is the rule from gitrepository-layout(5). A git
// directory without one is its own common directory.
func resolveCommonDir(gitDir string) string {
	content, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return gitDir
	}
	target := strings.TrimSpace(string(content))
	if target == "" {
		return gitDir
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(gitDir, target)
	}
	return filepath.Clean(target)
}

// newStorage builds go-git storage over a git directory, routing the shared
// state to the common directory when the two differ.
func newStorage(gitDir string) *filesystem.Storage {
	fs := osfs.New(gitDir)
	if commonDir := resolveCommonDir(gitDir); commonDir != gitDir {
		fs = dotgit.NewRepositoryFilesystem(osfs.New(gitDir), osfs.New(commonDir))
	}
	return filesystem.NewStorageWithOptions(
		fs,
		cache.NewObjectLRUDefault(),
		filesystem.Options{LargeObjectThreshold: largeObjectThreshold},
	)
}

// OpenRepositoryAt opens a specific git directory rather than discovering one.
//
// gc needs this. It builds consolidated packfiles out of a scratch object store
// hydrated from the registry, and every subprocess helper here otherwise
// resolves $GIT_DIR, which points at whatever repository the command was run
// in -- or at nothing at all, when it was not run in one.
func OpenRepositoryAt(gitDir string) (*Repository, error) {
	if gitDir == "" {
		return nil, fmt.Errorf("no git directory given")
	}
	abs, err := filepath.Abs(gitDir)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve %s: %w", gitDir, err)
	}
	st := newStorage(abs)
	repo, err := gogit.Open(st, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to open the git repository at %s: %w", abs, err)
	}
	return &Repository{repo: repo, storer: repo.Storer, dir: abs}, nil
}

// largeObjectThreshold is the size above which go-git reads an object straight
// from disk instead of materialising it in memory.
//
// Without it, storage/filesystem.ObjectStorage.getFromUnpacked copies every
// object into a plumbing.MemoryObject, so peak memory tracks the largest object
// in the push. Profiling a 120 MB blob showed 98.9% of all allocation in
// MemoryObject.Write, and setting this threshold took peak RSS for that push
// from 365 MB to 9 MB.
//
// The value trades memory against syscalls: objects below it stay in memory and
// are cached, objects above it are re-read from disk each time they are
// touched. 16 MiB keeps ordinary source objects in memory while catching the
// large binaries that actually cost something.
const largeObjectThreshold = 16 << 20

func OpenRepository() (*Repository, error) {
	// Locate the repository first so the storage can be built with our own
	// options. PlainOpen constructs its storage internally, which is why it
	// cannot be used here: it gives no way to set largeObjectThreshold.
	if gitDir, worktreeDir, ok := locateRepository(); ok {
		st := newStorage(gitDir)
		var worktree billy.Filesystem
		if worktreeDir != "" {
			worktree = osfs.New(worktreeDir)
		}
		if repo, err := gogit.Open(st, worktree); err == nil {
			return &Repository{repo: repo, storer: repo.Storer}, nil
		}
	}

	// Fall back to go-git's own discovery. This loses the large-object
	// threshold, but opening the repository at all matters more than opening it
	// efficiently.
	gitDir := os.Getenv("GIT_DIR")
	targetPath := gitDir
	if targetPath == "" {
		targetPath = "."
	}
	repo, err := gogit.PlainOpen(targetPath)
	if err != nil {
		repo, err = gogit.PlainOpenWithOptions(targetPath, &gogit.PlainOpenOptions{DetectDotGit: true})
	}
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository at %s: %w", targetPath, err)
	}

	return &Repository{
		repo:   repo,
		storer: repo.Storer,
	}, nil
}

// locateRepository resolves the git directory and its worktree.
//
// It reports ok=false rather than guessing when it cannot tell, leaving the
// caller to fall back to go-git's discovery.
func locateRepository() (gitDir, worktreeDir string, ok bool) {
	if env := os.Getenv("GIT_DIR"); env != "" {
		abs, err := filepath.Abs(env)
		if err != nil {
			return "", "", false
		}
		if !isGitDir(abs) {
			return "", "", false
		}
		// A ".git" directory has its worktree alongside it; anything else is
		// treated as bare, which is what git itself assumes without
		// GIT_WORK_TREE.
		if wt := os.Getenv("GIT_WORK_TREE"); wt != "" {
			if absWT, err := filepath.Abs(wt); err == nil {
				return abs, absWT, true
			}
		}
		if filepath.Base(abs) == ".git" {
			return abs, filepath.Dir(abs), true
		}
		// A linked worktree's git directory lives under <repo>/.git/worktrees/
		// and is not itself a ".git"; its worktree is named by its gitdir file.
		if wt, found := linkedWorktreeOf(abs); found {
			return abs, wt, true
		}
		return abs, "", true
	}

	dir, err := os.Getwd()
	if err != nil {
		return "", "", false
	}
	for {
		candidate := filepath.Join(dir, ".git")
		info, err := os.Stat(candidate)
		switch {
		case err == nil && info.IsDir():
			if isGitDir(candidate) {
				return candidate, dir, true
			}
		case err == nil:
			// A ".git" file points elsewhere: a linked worktree, or a
			// submodule. It names the git directory, which is then handled
			// like any other -- its commondir file, if it has one, says where
			// the shared state is.
			if target, found := gitDirFile(candidate); found && isGitDir(target) {
				return target, dir, true
			}
			return "", "", false
		}
		if isGitDir(dir) {
			return dir, "", true // bare repository
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", false
		}
		dir = parent
	}
}

// gitDirFile reads a ".git" file, which holds `gitdir: <path>`, and returns the
// absolute git directory it names.
func gitDirFile(path string) (string, bool) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	line, _, _ := strings.Cut(string(content), "\n")
	target, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	if !ok {
		return "", false
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return "", false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	return filepath.Clean(target), true
}

// linkedWorktreeOf reports the working tree a linked worktree's git directory
// belongs to, which its `gitdir` file records as the path of the worktree's
// ".git" file.
func linkedWorktreeOf(gitDir string) (string, bool) {
	content, err := os.ReadFile(filepath.Join(gitDir, "gitdir"))
	if err != nil {
		return "", false
	}
	dotGit := strings.TrimSpace(string(content))
	if dotGit == "" || filepath.Base(dotGit) != ".git" {
		return "", false
	}
	if !filepath.IsAbs(dotGit) {
		dotGit = filepath.Join(gitDir, dotGit)
	}
	return filepath.Dir(filepath.Clean(dotGit)), true
}

// isGitDir reports whether path looks like a git directory.
//
// A linked worktree's git directory has a HEAD of its own but no objects and
// no refs: those are in the directory its `commondir` file names, and it is
// that directory which has to look like a repository.
func isGitDir(path string) bool {
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err != nil {
		return false
	}
	common := resolveCommonDir(path)
	for _, entry := range []string{"objects", "refs"} {
		if _, err := os.Stat(filepath.Join(common, entry)); err != nil {
			return false
		}
	}
	return true
}

// ResolveRef resolves a ref string (e.g. "refs/heads/main", "v1.0.0", or "HEAD") to a commit Hash.
// If the ref points to an annotated tag, it peels the tag object to return the target commit Hash.
func (r *Repository) ResolveRef(refName string) (plumbing.Hash, error) {
	var ref *plumbing.Reference
	var err error
	ref, err = r.repo.Reference(plumbing.ReferenceName(refName), true)
	if err != nil && !strings.HasPrefix(refName, "refs/") {
		ref, err = r.repo.Reference(plumbing.ReferenceName("refs/tags/"+refName), true)
		if err != nil {
			ref, err = r.repo.Reference(plumbing.ReferenceName("refs/heads/"+refName), true)
		}
	}
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to resolve ref %s: %w", refName, err)
	}
	return r.peelTag(ref.Hash()), nil
}

// peelTag follows annotated tags until it reaches something that is not one.
//
// A tag may point at another tag -- `git tag -a v1 v1-rc`, say -- and peeling
// one level then hands back a tag object where a commit was promised. Bounded
// so that a store describing a tag cycle cannot spin this forever; git
// refuses to create one, but the objects came from somewhere.
func (r *Repository) peelTag(hash plumbing.Hash) plumbing.Hash {
	for range 32 {
		tagObj, err := r.repo.TagObject(hash)
		if err != nil {
			return hash
		}
		hash = tagObj.Target
	}
	return hash
}

// GetCommitInfo retrieves metadata for a commit.
func (r *Repository) GetCommitInfo(hash plumbing.Hash) (*object.Commit, error) {
	return r.repo.CommitObject(hash)
}

// CreatePackfileTo writes a packfile carrying the objects reachable from
// wantHash but not from haveHashes.
//
// The pack is *thin*: objects may be stored as deltas against bases that are
// not in the pack itself. That is safe here, and only here, because the format
// records those bases in io.git-remote-oci.pack-bases and a reader is required
// to fetch and import them first, so by the time this pack is indexed its bases
// are on disk. Import already runs `git index-pack --fix-thin`, which completes
// the pack from them.
//
// It matters most for the case this tool is otherwise worst at: a large file
// with a small change was previously stored in full on every push, because
// go-git's encoder only deltifies within the object set it is handed and so
// could never delta against a base it was told to exclude.
//
// Falls back to the pure-Go encoder if git cannot be used. A non-thin pack is
// always valid, so degrading costs bandwidth and nothing else.
func (r *Repository) CreatePackfileTo(writer io.Writer, wantHash plumbing.Hash, haveHashes []plumbing.Hash) error {
	if err := r.createThinPackfile(writer, wantHash, haveHashes); err == nil {
		return nil
	} else if errors.Is(err, errPackWritten) {
		// The failure happened after bytes reached the writer, so the stream is
		// already poisoned and cannot be retried with a different encoder.
		return err
	}
	return r.createPackfileWithGoGit(writer, wantHash, haveHashes)
}

// errPackWritten marks a failure that occurred after output had begun.
var errPackWritten = errors.New("packfile partially written")

// createThinPackfile shells out to git pack-objects.
func (r *Repository) createThinPackfile(writer io.Writer, wantHash plumbing.Hash, haveHashes []plumbing.Hash) error {
	gitDir, workDir := r.gitDir()

	peeled := wantHash
	if tagObj, err := r.repo.TagObject(wantHash); err == nil {
		peeled = tagObj.Target
	}

	// --revs reads "<want>" and "^<have>" from stdin, which is the same set
	// revlist.Objects computes for the fallback path.
	var revs strings.Builder
	revs.WriteString(wantHash.String() + "\n")
	if peeled != wantHash {
		revs.WriteString(peeled.String() + "\n")
	}
	for _, have := range haveHashes {
		revs.WriteString("^" + have.String() + "\n")
	}

	// git's complaint is the only thing that says *why* a pack could not be
	// built -- a missing object, a bad revision -- so it is kept, bounded, and
	// put in the error rather than thrown away.
	//
	// context.Background: CreatePackfileTo is called from pkg/helper without a
	// context and its signature is kept. The process is bounded by the writer
	// it feeds instead -- a closed pipe ends it.
	var stderr boundedBuffer
	cmd := exec.CommandContext(context.Background(), "git", "--git-dir="+gitDir, "pack-objects",
		"--thin", "--revs", "--stdout", "--delta-base-offset", "--quiet")
	cmd.Dir = workDir
	cmd.Stdin = strings.NewReader(revs.String())
	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	written, copyErr := io.Copy(writer, stdout)
	waitErr := cmd.Wait()
	if copyErr != nil || waitErr != nil {
		err := copyErr
		if err == nil {
			err = fmt.Errorf("git pack-objects failed: %w", waitErr)
			if msg := stderr.String(); msg != "" {
				err = fmt.Errorf("%w: %s", err, msg)
			}
		}
		if written > 0 {
			return fmt.Errorf("%w: %w", errPackWritten, err)
		}
		return err
	}
	if written == 0 {
		return fmt.Errorf("git pack-objects produced no output")
	}
	return nil
}

func (r *Repository) createPackfileWithGoGit(writer io.Writer, wantHash plumbing.Hash, haveHashes []plumbing.Hash) error {
	peeledHash := wantHash
	if tagObj, err := r.repo.TagObject(wantHash); err == nil {
		peeledHash = tagObj.Target
	}

	wants := []plumbing.Hash{peeledHash}
	if peeledHash != wantHash {
		wants = append(wants, wantHash)
	}

	hashes, err := revlist.Objects(r.storer, wants, haveHashes)
	if err != nil {
		return fmt.Errorf("failed to calculate revlist: %w", err)
	}

	enc := packfile.NewEncoder(writer, r.storer, true)
	if _, err := enc.Encode(hashes, 10); err != nil {
		return fmt.Errorf("failed to encode packfile: %w", err)
	}
	return nil
}

// ImportPackfile imports objects from a packfile reader into the local repository.
//
// On success it returns the absolute path of the ".keep" file that git index-pack
// created for the new pack, or "" if no pack was kept. Callers report that path to
// git as a "lock <file>" line; gitremote-helpers(7) requires it to be an absolute
// path under $GIT_DIR/objects/pack ending in ".keep", because git unlinks exactly
// that path once refs have been updated.
//
// If neither git index-pack nor the git unpack-objects fallback can import the
// data, the error is returned. Reporting success here would tell git that objects
// landed when they did not.
func (r *Repository) ImportPackfile(reader io.Reader) (string, error) {
	gitDir, _ := r.gitDir()

	// The object store, not $GIT_DIR/objects: the two differ in a linked
	// worktree and under GIT_OBJECT_DIRECTORY, and git index-pack writes to the
	// former in both cases. Looking for the .keep under the latter found
	// nothing, so no lock line was reported and the .keep was left behind for
	// good.
	packDir := filepath.Join(r.ObjectsDir(), "pack")
	if mkErr := os.MkdirAll(packDir, 0755); mkErr != nil {
		return "", fmt.Errorf("failed to create pack directory %s: %w", packDir, mkErr)
	}

	// Stage the incoming packfile on disk rather than reading it into memory:
	// it is as large as the history it carries, and the unpack-objects fallback
	// below needs a second pass over the same bytes, so the stream has to be
	// replayable regardless. The spool sits beside the object store, which is
	// where the pack is headed anyway and is real disk rather than a tmpfs.
	//
	// Named tmp_* because that is what git's own temporaries in this directory
	// are called, and what `git prune` sweeps up: a spool orphaned by a kill
	// mid-import is then reclaimed by the next maintenance run instead of
	// sitting there forever.
	spool, err := os.CreateTemp(packDir, "tmp_git-remote-oci-*")
	if err != nil {
		return "", fmt.Errorf("failed to stage the incoming packfile: %w", err)
	}
	spoolPath := spool.Name()
	defer func() {
		_ = spool.Close()
		_ = os.Remove(spoolPath)
	}()

	size, err := io.Copy(spool, reader)
	if err != nil {
		return "", fmt.Errorf("failed to read packfile data: %w", err)
	}
	if size == 0 {
		return "", nil
	}

	rewind := func() error {
		_, seekErr := spool.Seek(0, io.SeekStart)
		return seekErr
	}
	if err := rewind(); err != nil {
		return "", fmt.Errorf("failed to rewind the staged packfile: %w", err)
	}

	// 1. Try native 'git index-pack --fix-thin --keep=git-remote-oci --stdin' via stdin
	//
	// stdout carries the answer -- `pack\t<sha>` or `keep\t<sha>` -- and stderr
	// carries whatever git wants to say about it. Reading the two together put
	// a warning ahead of the answer and the parse below then found no id,
	// which reported no lock and leaked the .keep. They are kept apart, and
	// stderr is only ever quoted in an error.
	//
	// context.Background: ImportPackfile is called from pkg/helper without a
	// context and its signature is kept. Both subprocesses read a spool that
	// is already complete, so there is nothing upstream to cancel them from.
	ctx := context.Background()
	var out, indexStderr boundedBuffer
	cmd := exec.CommandContext(ctx, "git", "--git-dir="+gitDir, "index-pack", "--fix-thin", "--keep=git-remote-oci", "--stdin")
	cmd.Dir = filepath.Dir(gitDir)
	cmd.Stdin = spool
	cmd.Stdout = &out
	cmd.Stderr = &indexStderr
	indexErr := cmd.Run()

	if indexErr == nil {
		fields := strings.Fields(out.String())
		var sha string
		if len(fields) >= 2 && (fields[0] == "pack" || fields[0] == "keep") {
			sha = fields[1]
		} else if len(fields) >= 1 && isHexObjectID(fields[0]) {
			sha = fields[0]
		}
		if sha == "" {
			return "", nil
		}
		// --keep=<msg> makes index-pack write pack-<sha>.keep next to the pack.
		// Only report it if it is really there, so git is never asked to unlink
		// a path that does not exist. Its absence is not an error: it just
		// means there is no pack to keep.
		keepPath := filepath.Join(packDir, "pack-"+sha+".keep")
		if _, statErr := os.Stat(keepPath); statErr == nil {
			return keepPath, nil
		}
		return "", nil
	}

	// 2. Fallback to git unpack-objects to unpack loose objects into .git/objects/
	cmdUnpack := exec.CommandContext(ctx, "git", "--git-dir="+gitDir, "unpack-objects")
	cmdUnpack.Dir = filepath.Dir(gitDir)
	if err := rewind(); err != nil {
		return "", fmt.Errorf("failed to rewind the staged packfile for the unpack-objects fallback: %w", err)
	}
	cmdUnpack.Stdin = spool
	outUnpack, unpackErr := cmdUnpack.CombinedOutput()
	if unpackErr != nil {
		return "", fmt.Errorf(
			"failed to import packfile: git index-pack failed (%w: %s) and git unpack-objects failed (%w: %s)",
			indexErr, strings.TrimSpace(indexStderr.String()),
			unpackErr, strings.TrimSpace(string(outUnpack)),
		)
	}

	// unpack-objects writes loose objects; there is no pack to keep.
	return "", nil
}

// boundedBuffer keeps the first boundedBufferLimit bytes written to it and
// drops the rest, which is what a subprocess's stderr wants: enough to quote in
// an error, never enough to matter if the process is chatty.
type boundedBuffer struct {
	buf strings.Builder
}

const boundedBufferLimit = 8 << 10

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := boundedBufferLimit - b.buf.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		b.buf.Write(p)
	}
	// Report the full length so the writer never sees a short write.
	return n, nil
}

func (b *boundedBuffer) String() string {
	return strings.TrimSpace(b.buf.String())
}

// gitDirArg resolves the repository the git subprocesses should address.
//
// The working directory is the git directory's parent, which is right for the
// ".git" of a worktree and harmless for a bare repository, where the
// --git-dir argument is what the subprocess actually goes on.
func gitDirArg() (gitDir, workDir string) {
	gitDir, _ = GitDir()
	return gitDir, filepath.Dir(gitDir)
}

// refreshPacks makes packfiles written since the repository was opened visible.
//
// go-git reads the pack directory once and caches what it found, which is the
// right default and wrong for this process: it imports packfiles and then asks
// questions about their contents in the same run.
func (r *Repository) refreshPacks() {
	if fs, ok := r.storer.(*filesystem.Storage); ok {
		_ = fs.Reindex()
	}
}

// GitDir reports the absolute path of the repository's git directory.
//
// This is what `git rev-parse --absolute-git-dir` answers, worked out the same
// way locateRepository already does it for opening the repository: GIT_DIR if
// set, otherwise the nearest .git walking upwards. Asking git meant a process
// per call to learn something this package had already established.
//
// ok is false when neither could be established. A path is still returned in
// that case — GIT_DIR verbatim if it is set at all, otherwise ".git" against the
// working directory — because that is the guess five separate callers used to
// make inline, and each one of them needs *some* path to carry on with. Two
// callers can do better than a guess and check ok; the rest are honest about
// taking it.
func GitDir() (string, bool) {
	if gitDir, _, ok := locateRepository(); ok {
		return gitDir, true
	}
	// GIT_DIR set but not recognisable as a git directory still beats ignoring
	// it: git handed it over, and a bare repository mid-creation is a real
	// state that isGitDir declines.
	guess := os.Getenv("GIT_DIR")
	if guess == "" {
		guess = ".git"
	}
	if abs, err := filepath.Abs(guess); err == nil {
		return abs, false
	}
	return guess, false
}

// CommonDir reports the absolute path of the directory holding the
// repository's shared state, which is what `git rev-parse --git-common-dir`
// answers: the git directory itself, or for a linked worktree the directory
// its `commondir` file names.
//
// Everything that is not per-worktree lives here -- objects, refs, the shallow
// file, the LFS object cache -- so this, not GitDir, is what a path to any of
// them has to be built on. ok has the same meaning as GitDir's.
func CommonDir() (string, bool) {
	gitDir, ok := GitDir()
	return resolveCommonDir(gitDir), ok
}

// ObjectsDir reports the absolute path of the repository's object store, which
// is what `git rev-parse --git-path objects` answers.
//
// GIT_OBJECT_DIRECTORY wins if it is set, because that is what git itself
// honours and this process may have been handed one.
func ObjectsDir() (string, bool) {
	if env := os.Getenv("GIT_OBJECT_DIRECTORY"); env != "" {
		if abs, err := filepath.Abs(env); err == nil {
			return abs, true
		}
		return env, true
	}
	commonDir, ok := CommonDir()
	if !ok {
		return "", false
	}
	return filepath.Join(commonDir, "objects"), true
}

// OpenObjectStore opens a directory laid out like a git directory — one holding
// an objects/ subdirectory — as a read-only object store.
//
// It is how the protocol-v2 staging area is read: that directory is not a
// repository, it has no refs and no config, but it has the objects, and
// objects/info/alternates points at the real repository so a lookup falls
// through to what the client already has.
//
// Each call opens afresh. The staging area grows as packfiles are imported into
// it, and a storer caches the pack list it found when it opened, so a reused one
// would keep answering with the view from before the last import.
func OpenObjectStore(gitDir string) (storer.EncodedObjectStorer, error) {
	if gitDir == "" {
		return nil, fmt.Errorf("no object store directory given")
	}
	return filesystem.NewStorageWithOptions(
		osfs.New(gitDir),
		cache.NewObjectLRUDefault(),
		filesystem.Options{LargeObjectThreshold: largeObjectThreshold},
	), nil
}

// HasObjects reports which of the given object ids the store cannot resolve.
//
// The order of the result follows the order asked, so a caller can name the
// first one that is missing rather than just report a count.
func HasObjects(store storer.EncodedObjectStorer, oids []string) []string {
	var missing []string
	for _, oid := range oids {
		if !isHexObjectID(oid) {
			missing = append(missing, oid)
			continue
		}
		if _, err := store.EncodedObject(plumbing.AnyObject, plumbing.NewHash(oid)); err != nil {
			missing = append(missing, oid)
		}
	}
	return missing
}

// DeepenRule decides whether a commit belongs in a truncated view of history.
//
// level is the commit's distance in generations from the nearest tip, which is
// what a depth is counted in. A rule that does not care about depth ignores it.
type DeepenRule func(commit *object.Commit, level int) bool

// AtDepth includes the first n generations.
//
// Generations, not commits. Deriving this from `rev-list --max-count=N` — which
// it used to be — counts commits instead, and the two stop agreeing the moment
// the history has a merge in it: two parents at the same generation consume two
// of the count, and the boundary lands short of the depth that was asked for.
func AtDepth(n int) DeepenRule {
	return func(_ *object.Commit, level int) bool { return level < n }
}

// Since includes commits committed no earlier than t, which is what
// `--shallow-since` asks for.
//
// The committer date is the one git uses here, not the author date: an old
// patch committed today is part of today's history, and rebasing does not move
// a boundary out from under a client that already fetched past it.
func Since(t time.Time) DeepenRule {
	return func(commit *object.Commit, _ int) bool {
		return !commit.Committer.When.Before(t)
	}
}

// Excluding includes commits that are not in the given set, which is what
// `--shallow-exclude` asks for once its refs have been resolved to their
// reachable commits.
func Excluding(excluded map[string]bool) DeepenRule {
	return func(commit *object.Commit, _ int) bool {
		return !excluded[commit.Hash.String()]
	}
}

// AllOf includes a commit only when every rule accepts it.
//
// git may send several deepen arguments at once — a date and an excluded ref,
// say — and the shallowest cut wins, because each one is a statement about what
// the client does not want.
func AllOf(rules ...DeepenRule) DeepenRule {
	return func(commit *object.Commit, level int) bool {
		for _, rule := range rules {
			if !rule(commit, level) {
				return false
			}
		}
		return true
	}
}

// Reachable returns every commit reachable from the given starting points.
//
// This is what `--shallow-exclude` needs: the refs it names are not themselves
// the boundary, everything behind them is. A start that cannot be read
// contributes nothing rather than failing, for the same reason the boundary
// walk tolerates it — the store may be a partial view.
func Reachable(store storer.EncodedObjectStorer, starts []string) map[string]bool {
	seen := make(map[string]bool)
	frontier := append([]string(nil), starts...)
	for len(frontier) > 0 {
		var next []string
		for _, sha := range frontier {
			if seen[sha] || !isHexObjectID(sha) {
				continue
			}
			seen[sha] = true
			commit, err := object.GetCommit(store, plumbing.NewHash(sha))
			if err != nil {
				continue
			}
			for _, parent := range commit.ParentHashes {
				next = append(next, parent.String())
			}
		}
		frontier = next
	}
	return seen
}

// BoundaryFor computes a truncated view of some commits: the set the rule
// admits, and the commits on its edge that still have a parent outside it.
//
// The walk is breadth-first by generation, which is what lets one traversal
// serve every way git asks for a truncated history — a depth counts the levels,
// a date and an excluded ref ignore them, and the edge is found the same way
// regardless: a commit that is in, with a parent that is not.
//
// A parent that cannot be read is treated as outside the set rather than as an
// error, which is what `rev-list --missing=allow-any` was for. The store being
// walked may be a partial or shallow view, and a boundary is exactly the place
// where the history is expected to stop.
func BoundaryFor(store storer.EncodedObjectStorer, tips []string, rule DeepenRule) (map[string]bool, []string, error) {
	within := make(map[string]bool)
	if rule == nil {
		return within, nil, nil
	}

	parents := make(map[string][]string)
	frontier := make([]string, 0, len(tips))
	for _, tip := range tips {
		if !isHexObjectID(tip) {
			return nil, nil, fmt.Errorf("invalid object id %q", tip)
		}
		frontier = append(frontier, tip)
	}

	for level := 0; len(frontier) > 0; level++ {
		var next []string
		for _, sha := range frontier {
			if within[sha] {
				continue
			}
			commit, err := object.GetCommit(store, plumbing.NewHash(sha))
			if err != nil {
				// Not a commit, or not present. Either way there is nothing
				// below it to walk. It is not recorded as within, so whatever
				// pointed at it becomes a boundary.
				continue
			}
			if !rule(commit, level) {
				continue
			}
			within[sha] = true
			for _, parent := range commit.ParentHashes {
				parents[sha] = append(parents[sha], parent.String())
				next = append(next, parent.String())
			}
		}
		frontier = next
	}

	// Sorted so the same request produces the same answer twice, which matters
	// where this ends up: a shallow-info section and a $GIT_DIR/shallow file.
	var boundary []string
	for sha := range within {
		for _, parent := range parents[sha] {
			if !within[parent] {
				boundary = append(boundary, sha)
				break
			}
		}
	}
	sort.Strings(boundary)
	return within, boundary, nil
}

// BoundaryAtDepth is BoundaryFor with a depth, which is the common case.
func BoundaryAtDepth(store storer.EncodedObjectStorer, tips []string, depth int) (map[string]bool, []string, error) {
	if depth <= 0 {
		return map[string]bool{}, nil, nil
	}
	return BoundaryFor(store, tips, AtDepth(depth))
}

// ShallowBoundary returns the commits that should be recorded in
// $GIT_DIR/shallow to truncate tip's history to depth commits.
//
// A boundary commit is one inside the depth window that has a parent outside
// it. Git grafts there, so history stops at exactly the requested depth even
// when more objects were downloaded - which happens here because a packfile is
// fetched whole and may carry more commits than were asked for.
//
// Returns nil when the history is shorter than depth, which needs no boundary.
func (r *Repository) ShallowBoundary(tip string, depth int) ([]string, error) {
	if !isHexObjectID(tip) {
		return nil, fmt.Errorf("invalid object id %q", tip)
	}
	// The boundary is computed after the fetch has imported its packfiles, and
	// a storer caches the pack list it found when it was opened — so without
	// this the walk cannot see the very commits it is being asked about. The
	// subprocess this replaced got a fresh view by virtue of being a new
	// process; Reindex is how to ask for one here.
	r.refreshPacks()

	_, boundary, err := BoundaryAtDepth(r.storer, []string{tip}, depth)
	if err != nil {
		return nil, fmt.Errorf("failed to compute the shallow boundary for %s: %w", tip, err)
	}
	return boundary, nil
}

// isHexObjectID reports whether s looks like a git object ID in hex form
// (40 characters for SHA-1, 64 for SHA-256).
func isHexObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// SetReference updates or creates a git reference.
func (r *Repository) SetReference(refName string, hash plumbing.Hash) error {
	ref := plumbing.NewHashReference(plumbing.ReferenceName(refName), hash)
	if err := r.storer.SetReference(ref); err != nil {
		return fmt.Errorf("failed to set reference %s to %s: %w", refName, hash, err)
	}
	return nil
}

// IsAncestor returns true if ancestor is reachable from descendant via parent chain.
// This is used for fast-forward checking during push.
func (r *Repository) IsAncestor(ancestor, descendant plumbing.Hash) (bool, error) {
	if ancestor == descendant {
		return true, nil
	}

	// Walk the commit history from descendant looking for ancestor
	commit, err := r.repo.CommitObject(descendant)
	if err != nil {
		return false, fmt.Errorf("failed to get commit %s: %w", descendant, err)
	}

	iter := object.NewCommitPreorderIter(commit, nil, nil)
	defer iter.Close()

	found := false
	err = iter.ForEach(func(c *object.Commit) error {
		if c.Hash == ancestor {
			found = true
			return storer.ErrStop
		}
		return nil
	})
	// ForEach returns storer.ErrStop when iteration is stopped early; this is not an error
	if err != nil && !errors.Is(err, storer.ErrStop) {
		return false, fmt.Errorf("failed to walk commit history: %w", err)
	}
	return found, nil
}

// lfsPointerMaxSize bounds what is worth reading as a possible LFS pointer.
//
// A pointer is a three-line text file of about 130 bytes; the allowance is
// generous so that extra keys, which the spec permits, still parse.
const lfsPointerMaxSize = 1024

// ScanLFSPointers returns the Git LFS pointers among the objects a push of
// wantHash against haveHashes would carry.
//
// It enumerates exactly the object set the packfile will contain, rather than
// walking the tip commit's tree. Those are different: the tip tree misses a
// pointer added by one commit in the range and removed by a later one, so the
// object was never uploaded and checking out that commit later produced a
// dangling pointer. It also scanned unchanged history on every push, since the
// tip tree includes files that have not been touched in years.
//
// Working from the pack's own object list makes the two consistent by
// construction: every blob that ships is considered, and nothing else is.
func (r *Repository) ScanLFSPointers(wantHash plumbing.Hash, haveHashes []plumbing.Hash) ([]*lfs.Pointer, error) {
	hashes, err := r.RevList(wantHash, haveHashes)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate revlist for LFS scan: %w", err)
	}
	return r.ScanLFSPointersIn(hashes)
}

// RevList lists every object reachable from wantHash and not from haveHashes:
// the object set a packfile cut for that range carries.
//
// ScanLFSPointers, PackedObjects and the go-git packing fallback all want this
// same list, and each used to walk the repository again to get it. The walk is
// the expensive part of all three -- it touches every tree in the range -- so a
// caller pushing one ref computes it once here and hands it to the *In / *Of
// variants below.
func (r *Repository) RevList(wantHash plumbing.Hash, haveHashes []plumbing.Hash) ([]plumbing.Hash, error) {
	peeled := r.peelTag(wantHash)
	wants := []plumbing.Hash{peeled}
	if peeled != wantHash {
		wants = append(wants, wantHash)
	}
	return revlist.Objects(r.storer, wants, haveHashes)
}

// ScanLFSPointersIn is ScanLFSPointers over an object list already computed by
// RevList.
func (r *Repository) ScanLFSPointersIn(hashes []plumbing.Hash) ([]*lfs.Pointer, error) {
	var pointers []*lfs.Pointer
	seenOIDs := make(map[string]bool)

	for _, h := range hashes {
		obj, err := r.storer.EncodedObject(plumbing.BlobObject, h)
		if err != nil {
			// Not a blob, or not present. Neither is a reason to fail the push.
			continue
		}
		if obj.Size() > lfsPointerMaxSize {
			continue
		}

		reader, err := obj.Reader()
		if err != nil {
			continue
		}
		content, readErr := io.ReadAll(reader)
		_ = reader.Close()
		if readErr != nil {
			continue
		}

		ptr := lfs.ParsePointer(content)
		if ptr == nil || seenOIDs[ptr.Oid] {
			continue
		}
		seenOIDs[ptr.Oid] = true
		pointers = append(pointers, ptr)
	}

	return pointers, nil
}

// TagInfo holds name and target commit hash of a local tag.
type TagInfo struct {
	Name       string
	CommitHash plumbing.Hash
}

// GetReachableTags returns all local tags whose commit is reachable from any of the given source commit hashes.
func (r *Repository) GetReachableTags(pushedHashes []plumbing.Hash) ([]TagInfo, error) {
	if len(pushedHashes) == 0 {
		return nil, nil
	}

	// 1. Collect the commits reachable from pushedHashes.
	//
	// Commits only. A tag points at a commit, so that is the set the question
	// is about; walking every tree and blob as well -- which is what a revlist
	// does -- visited the whole repository's contents to answer a question
	// about its history, on every push.
	reachableSet := make(map[plumbing.Hash]bool, len(pushedHashes))
	for _, h := range pushedHashes {
		commit, err := r.repo.CommitObject(r.peelTag(h))
		if err != nil {
			// A tag object, a tree, or something not in the store: nothing
			// with parents to walk.
			reachableSet[h] = true
			continue
		}
		// The set doubles as the walker's "already seen" list, so a commit
		// reached from an earlier tip is not walked again from a later one.
		// It is filled by the callback rather than up front because the
		// walker skips a start it has already been told about.
		iter := object.NewCommitPreorderIter(commit, reachableSet, nil)
		err = iter.ForEach(func(c *object.Commit) error {
			reachableSet[c.Hash] = true
			return nil
		})
		iter.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to walk the history of %s: %w", h, err)
		}
		reachableSet[h] = true
	}

	// 2. Iterate through all local tags (refs/tags/*)
	tagIter, err := r.repo.Tags()
	if err != nil {
		return nil, fmt.Errorf("failed to list tags: %w", err)
	}

	var matchedTags []TagInfo
	err = tagIter.ForEach(func(ref *plumbing.Reference) error {
		tagName := ref.Name().String()
		targetHash := ref.Hash()

		// Resolve annotated tag target object if applicable
		if tagObj, tagErr := r.repo.TagObject(targetHash); tagErr == nil {
			targetHash = tagObj.Target
		}

		if reachableSet[targetHash] {
			matchedTags = append(matchedTags, TagInfo{
				Name:       tagName,
				CommitHash: targetHash,
			})
		}
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed iterating tags: %w", err)
	}

	return matchedTags, nil
}

// AnnotatedTagInfo contains metadata for an annotated Git tag.
type AnnotatedTagInfo struct {
	Name       string
	ObjectHash string
	TargetHash string
	Tagger     string
	Message    string
	Signature  string
}

// GetAnnotatedTagInfo retrieves annotated tag metadata for a ref if the ref points to a TagObject.
func (r *Repository) GetAnnotatedTagInfo(refName string) (*AnnotatedTagInfo, error) {
	var ref *plumbing.Reference
	var err error
	ref, err = r.repo.Reference(plumbing.ReferenceName(refName), false)
	if err != nil && !strings.HasPrefix(refName, "refs/") {
		ref, err = r.repo.Reference(plumbing.ReferenceName("refs/tags/"+refName), false)
	}
	if err != nil {
		return nil, fmt.Errorf("ref %s not found: %w", refName, err)
	}

	tagObj, err := r.repo.TagObject(ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("hash %s is not a TagObject: %w", ref.Hash().String(), err)
	}

	taggerStr := ""
	if tagObj.Tagger.Name != "" || tagObj.Tagger.Email != "" {
		taggerStr = fmt.Sprintf("%s <%s> %d", tagObj.Tagger.Name, tagObj.Tagger.Email, tagObj.Tagger.When.Unix())
	}

	return &AnnotatedTagInfo{
		Name:       refName,
		ObjectHash: tagObj.Hash.String(),
		TargetHash: tagObj.Target.String(),
		Tagger:     taggerStr,
		Message:    strings.TrimSpace(tagObj.Message),
		Signature:  tagObj.Signature,
	}, nil
}

// PackedObjects lists the object ids a packfile cut for wantHash against
// haveHashes will contain, sorted, in lowercase hex.
//
// This is what CreatePackfileTo is about to write, computed from the same
// revision range, and it is published beside the packfile so a reader can find
// out what is in a pack without downloading it.
//
// It matters for one case in particular. A lazy fetch in a partial clone asks
// for a bare object id, and nothing else in the format says which packfile
// holds it — every other annotation is about commits. Without this the only
// way to answer is to stage history and look, which costs the repository to
// serve one blob.
//
// It is derived rather than read back out of the pack, because the pack is
// thin: it is written to a stream this process does not keep, and indexing it
// standalone is not possible when its delta bases are deliberately absent.
func (r *Repository) PackedObjects(wantHash plumbing.Hash, haveHashes []plumbing.Hash) ([]PackedObject, error) {
	hashes, err := r.RevList(wantHash, haveHashes)
	if err != nil {
		return nil, fmt.Errorf("failed to list the objects for %s: %w", wantHash, err)
	}
	return r.PackedObjectsOf(hashes), nil
}

// PackedObjectsOf is PackedObjects over an object list already computed by
// RevList.
func (r *Repository) PackedObjectsOf(hashes []plumbing.Hash) []PackedObject {
	out := make([]PackedObject, 0, len(hashes))
	for _, h := range hashes {
		entry := PackedObject{OID: h.String()}
		// The size is the object's own uncompressed length, which is what git
		// reports and what a client asking `object-info` wants. An object the
		// store cannot size is still listed: an entry without a size costs a
		// size lookup that has to go the slow way, whereas dropping the entry
		// would make the pack look as though it did not hold the object.
		if obj, err := r.storer.EncodedObject(plumbing.AnyObject, h); err == nil && obj != nil {
			entry.Size = obj.Size()
		}
		out = append(out, entry)
	}
	// Sorted so a reader can binary-search it, which is the only reason to
	// publish it rather than let the reader work it out.
	sort.Slice(out, func(i, j int) bool { return out[i].OID < out[j].OID })
	return out
}

// PackedObject is one object in a packfile: its id and its uncompressed size.
type PackedObject struct {
	OID  string
	Size int64
}

// ObjectSizes reports the uncompressed size of each object present in store,
// and the ids it could not find.
//
// The sizes are what git's `object-info` command answers with, and the missing
// list is what decides whether more has to be fetched before it can be
// answered at all.
func ObjectSizes(store storer.EncodedObjectStorer, oids []string) (map[string]int64, []string) {
	sizes := make(map[string]int64, len(oids))
	var missing []string
	for _, oid := range oids {
		obj, err := store.EncodedObject(plumbing.AnyObject, plumbing.NewHash(oid))
		if err != nil || obj == nil {
			missing = append(missing, oid)
			continue
		}
		sizes[oid] = obj.Size()
	}
	return sizes, missing
}
