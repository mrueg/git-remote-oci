// Package registrytest provides an in-process OCI registry and a seeded
// repository for tests.
//
// It exists because the mock was being written again for each package that
// needed one, and the copies drifted: one could not serve a manifest by digest,
// so every deletion assertion in that package silently passed without a DELETE
// ever being issued. A registry mock is fiddly in exactly the ways that matter
// — deletion arrives by digest, not by tag — so there should be one.
//
// It is in internal/ rather than pkg/ because it is test scaffolding, not part
// of the tool's own dependency graph.
package registrytest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	opencontainers "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mrueg/git-remote-oci/pkg/git"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// Registry is an in-process OCI registry with enough behaviour for a push, a
// fetch, a garbage collection and an fsck.
type Registry struct {
	mu        sync.Mutex
	blobs     map[string][]byte
	manifests map[string][]byte
	byDigest  map[string][]byte
	tags      []string

	// RefuseDelete models GHCR and friends, which restrict manifest deletion.
	RefuseDelete bool

	// server is the httptest server this registry is being served on, so a
	// test can build a second client against it.
	server *httptest.Server

	// observe, if set, is called before each request is handled, with the
	// method and path. It exists so a test can make something happen *during*
	// an operation rather than before or after it -- which is the only way to
	// exercise the interleavings that matter here, since nothing a registry
	// does is transactional.
	//
	// It runs without r.mu held, so it may call the mutators on this type.
	observe func(method, path string)

	// intercept, if set, runs after observe and may answer the request
	// itself: returning true means it wrote the response and the registry
	// does nothing further. It is how a test makes the registry *fail* a
	// particular request -- a 500 on one tag, say -- which observe cannot,
	// since observe only watches.
	//
	// Like observe it runs without r.mu held.
	intercept func(w http.ResponseWriter, req *http.Request) bool

	deleted []string
}

// Observe installs a hook called before each request is served.
func (r *Registry) Observe(hook func(method, path string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observe = hook
}

// Intercept installs a hook that may answer a request in the registry's place.
// Returning true from the hook means it has written the response.
func (r *Registry) Intercept(hook func(w http.ResponseWriter, req *http.Request) bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.intercept = hook
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{
		blobs:     map[string][]byte{},
		manifests: map[string][]byte{},
		byDigest:  map[string][]byte{},
	}
}

// Serve starts the registry and returns its test server.
func (r *Registry) Serve(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(r.handle))
	t.Cleanup(ts.Close)
	r.mu.Lock()
	r.server = ts
	r.mu.Unlock()
	return ts
}

func (r *Registry) handle(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Path

	r.mu.Lock()
	hook, intercept := r.observe, r.intercept
	r.mu.Unlock()
	if hook != nil {
		hook(req.Method, path)
	}
	if intercept != nil && intercept(w, req) {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	switch {
	case path == "/v2/":
		w.WriteHeader(http.StatusOK)

	case strings.HasSuffix(path, "/tags/list"):
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "repo", "tags": r.tags})

	case req.Method == http.MethodPost && strings.Contains(path, "/blobs/uploads/"):
		w.Header().Set("Location", "/v2/repo/blobs/uploads/s1")
		w.WriteHeader(http.StatusAccepted)

	case req.Method == http.MethodPut && strings.Contains(path, "/blobs/uploads/"):
		data, _ := io.ReadAll(req.Body)
		r.blobs[req.URL.Query().Get("digest")] = data
		w.WriteHeader(http.StatusCreated)

	case req.Method == http.MethodHead && strings.Contains(path, "/blobs/"):
		if _, ok := r.blobs[after(path, "/blobs/")]; ok {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)

	case req.Method == http.MethodGet && strings.Contains(path, "/blobs/"):
		if data, ok := r.blobs[after(path, "/blobs/")]; ok {
			_, _ = w.Write(data)
			return
		}
		w.WriteHeader(http.StatusNotFound)

	case req.Method == http.MethodPut && strings.Contains(path, "/manifests/"):
		ref := after(path, "/manifests/")
		data, _ := io.ReadAll(req.Body)
		r.manifests[ref] = data
		r.byDigest[Digest(data)] = data
		r.addTag(ref)
		w.Header().Set("Docker-Content-Digest", Digest(data))
		w.WriteHeader(http.StatusCreated)

	case req.Method == http.MethodDelete && strings.Contains(path, "/manifests/"):
		if r.RefuseDelete {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		r.deleteManifest(after(path, "/manifests/"))
		w.WriteHeader(http.StatusAccepted)

	default: // GET or HEAD of a manifest, by tag or by digest
		ref := after(path, "/manifests/")
		data, ok := r.manifests[ref]
		if !ok {
			data, ok = r.byDigest[ref]
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var probe struct {
			MediaType string `json:"mediaType"`
		}
		_ = json.Unmarshal(data, &probe)
		if probe.MediaType == "" {
			probe.MediaType = ocispec.MediaTypeImageManifest
		}
		w.Header().Set("Content-Type", probe.MediaType)
		w.Header().Set("Docker-Content-Digest", Digest(data))
		if req.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write(data)
	}
}

// deleteManifest removes a manifest and untags everything pointing at it.
//
// A client resolves a tag to a digest and deletes by digest, so a mock that
// only matches tag names never removes anything and every deletion assertion
// passes without a deletion having happened.
func (r *Registry) deleteManifest(ref string) {
	delete(r.byDigest, ref)

	kept := make([]string, 0, len(r.tags))
	for _, tag := range r.tags {
		data, ok := r.manifests[tag]
		if tag == ref || (ok && Digest(data) == ref) {
			delete(r.manifests, tag)
			continue
		}
		kept = append(kept, tag)
	}
	r.tags = kept
	r.deleted = append(r.deleted, ref)
}

func (r *Registry) addTag(tag string) {
	for _, t := range r.tags {
		if t == tag {
			return
		}
	}
	r.tags = append(r.tags, tag)
}

// Tags returns the current tag list.
func (r *Registry) Tags() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.tags...)
}

// Deletions returns the digests and tags deletion was requested for.
func (r *Registry) Deletions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.deleted...)
}

// Blobs returns a copy of the stored blob contents.
func (r *Registry) Blobs() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]byte, 0, len(r.blobs))
	for _, b := range r.blobs {
		out = append(out, append([]byte(nil), b...))
	}
	return out
}

// DropManifest removes a manifest by tag without untagging anything else,
// which is how a repository becomes incomplete in the way fsck exists to find.
func (r *Registry) DropManifest(tag string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if data, ok := r.manifests[tag]; ok {
		delete(r.byDigest, Digest(data))
	}
	delete(r.manifests, tag)
	kept := r.tags[:0]
	for _, t := range r.tags {
		if t != tag {
			kept = append(kept, t)
		}
	}
	r.tags = append([]string(nil), kept...)
}

// Client returns an oci.Client pointed at the server.
func Client(t *testing.T, ts *httptest.Server) *oci.Client {
	t.Helper()
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	c, err := oci.NewClient(u.Host+"/repo", true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// URL renders the server as the oci:// URL a user would type.
func URL(ts *httptest.Server) string {
	return "oci://" + strings.TrimPrefix(ts.URL, "http://") + "/repo"
}

// SeedRepository creates a local repository with n commits and pushes each one
// separately, which is what leaves a packfile and a commit tag per push.
//
// It sets GIT_DIR for the duration of the test and returns the tip.
func SeedRepository(t *testing.T, client *oci.Client, n int) (*git.Repository, string) {
	t.Helper()

	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main", ".")

	t.Setenv("GIT_DIR", filepath.Join(dir, ".git"))
	repo, err := git.OpenRepository()
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}

	ctx := context.Background()
	var tip string
	var prev []string
	for i := range n {
		name := "f" + strconv.Itoa(i) + ".txt"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		run("add", name)
		run("commit", "-q", "-m", "commit "+strconv.Itoa(i))
		tip = run("rev-parse", "HEAD")

		var pack bytes.Buffer
		if err := repo.CreatePackfileTo(&pack, plumbing.NewHash(tip), hashes(prev)); err != nil {
			t.Fatalf("CreatePackfileTo: %v", err)
		}
		if err := client.PushCommitStream(ctx, oci.CommitPush{
			CommitSHA: tip,
			RefName:   "refs/heads/main",
			RefTag:    oci.EncodeRefTag("refs/heads/main"),
			PackBases: prev,
		}, bytes.NewReader(pack.Bytes()), int64(pack.Len())); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
		prev = []string{tip}
	}

	if err := client.PushRichRefIndex(ctx, map[string]oci.RefEntry{
		"refs/heads/main": {SHA: tip},
	}, nil); err != nil {
		t.Fatalf("push ref index: %v", err)
	}
	return repo, tip
}

// Digest is the registry's content address for a manifest.
func Digest(data []byte) string {
	return opencontainers.FromBytes(data).String()
}

func hashes(shas []string) []plumbing.Hash {
	out := make([]plumbing.Hash, 0, len(shas))
	for _, s := range shas {
		out = append(out, plumbing.NewHash(s))
	}
	return out
}

func after(path, sep string) string {
	i := strings.LastIndex(path, sep)
	if i < 0 {
		return ""
	}
	return path[i+len(sep):]
}

// SetPackBases rewrites a tag's pack-bases annotation.
//
// Registry content is untrusted, so this is a registry describing something a
// correct writer never would — a missing base, or a cycle — which is precisely
// what a reader has to survive.
func (r *Registry) SetPackBases(t *testing.T, tag string, bases ...string) {
	t.Helper()

	r.mu.Lock()
	defer r.mu.Unlock()

	raw, ok := r.manifests[tag]
	if !ok {
		t.Fatalf("registry has no manifest tagged %q", tag)
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal manifest %q: %v", tag, err)
	}
	if m.Annotations == nil {
		m.Annotations = map[string]string{}
	}
	m.Annotations[oci.AnnotationGitPackBases] = strings.Join(bases, ",")

	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest %q: %v", tag, err)
	}
	r.manifests[tag] = out
	// Keep it addressable by its new digest: a reader resolves the tag first
	// and then fetches by digest.
	r.byDigest[Digest(out)] = out
}

// SetRefRevision rewrites the tip a ref manifest claims, without touching the
// `_refs` index.
//
// That disagreement is not a corruption: it is precisely the intermediate state
// of a push in flight. A push writes the commit manifest, then the ref
// manifest, then `_refs`, so between the second and third steps the ref
// manifest is ahead of the index. Anything that reads the index and then acts
// on a ref has to cope with finding it already moved.
func (r *Registry) SetRefRevision(t *testing.T, refName, sha string) {
	t.Helper()
	tag := oci.RefManifestTag(refName)
	if tag == "" {
		t.Fatalf("ref %q has no manifest tag", refName)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	raw, ok := r.manifests[tag]
	if !ok {
		t.Fatalf("registry has no manifest tagged %q", tag)
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal manifest %q: %v", tag, err)
	}
	if m.Annotations == nil {
		m.Annotations = map[string]string{}
	}
	m.Annotations[ocispec.AnnotationRevision] = sha

	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest %q: %v", tag, err)
	}
	r.manifests[tag] = out
	r.byDigest[Digest(out)] = out
}

// SetIndexedRef rewrites what the `_refs` index says a ref points at, in place.
//
// This is the other half of a push in flight, and the half SetRefRevision does
// not cover: the index updated by somebody else. It mutates stored state
// directly rather than going through a client, because the realistic way to
// observe this — running a second client's push from inside a request handler —
// serialises the two on the index lock and deadlocks instead of interleaving.
//
// It returns an error rather than taking *testing.T: the interesting call site
// is a request hook, which runs on the server's goroutine where t.Fatalf is not
// allowed.
func (r *Registry) SetIndexedRef(refName, sha string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	raw, ok := r.manifests[oci.TagRefIndex]
	if !ok {
		return fmt.Errorf("registry has no %s manifest", oci.TagRefIndex)
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("unmarshal %s: %w", oci.TagRefIndex, err)
	}

	for i, layer := range m.Layers {
		if layer.MediaType != oci.MediaTypeGitIndex {
			continue
		}
		var entries map[string]oci.RefEntry
		if err := json.Unmarshal(r.blobs[layer.Digest.String()], &entries); err != nil {
			return fmt.Errorf("unmarshal the ref index blob: %w", err)
		}
		entry := entries[refName]
		entry.SHA = sha
		entries[refName] = entry

		blob, err := json.Marshal(entries)
		if err != nil {
			return err
		}
		digest := Digest(blob)
		r.blobs[digest] = blob
		m.Layers[i].Digest = opencontainers.Digest(digest)
		m.Layers[i].Size = int64(len(blob))

		out, err := json.Marshal(m)
		if err != nil {
			return err
		}
		r.manifests[oci.TagRefIndex] = out
		r.byDigest[Digest(out)] = out
		return nil
	}
	return fmt.Errorf("the %s manifest has no index layer", oci.TagRefIndex)
}

// LastServer returns the test server this registry is being served on, so a
// caller can build a second client against it. Conformance checks need one: a
// client that has just written something answers from its own caches, which is
// the opposite of reading a repository the way a stranger would.
func (r *Registry) LastServer() *httptest.Server {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.server
}

// RawManifest returns the stored bytes for a tag.
func (r *Registry) RawManifest(t *testing.T, tag string) []byte {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, ok := r.manifests[tag]
	if !ok {
		t.Fatalf("registry has no manifest tagged %q", tag)
	}
	return append([]byte(nil), raw...)
}

// SetManifestAnnotation rewrites one annotation on a stored manifest.
//
// It edits the JSON generically rather than unmarshalling into a typed
// manifest, because the same call has to work on an image index — whose
// annotations live on a different struct — and a round trip through the wrong
// type would silently drop the layers or the children.
func (r *Registry) SetManifestAnnotation(tag, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	raw, ok := r.manifests[tag]
	if !ok {
		return fmt.Errorf("registry has no manifest tagged %q", tag)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("unmarshal %q: %w", tag, err)
	}
	annotations, _ := doc["annotations"].(map[string]any)
	if annotations == nil {
		annotations = map[string]any{}
	}
	annotations[key] = value
	doc["annotations"] = annotations

	out, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	r.manifests[tag] = out
	r.byDigest[Digest(out)] = out
	return nil
}

// StripLayers removes every layer of a media type from every stored manifest,
// which is how a repository written before an optional layer existed looks.
func (r *Registry) StripLayers(mediaType string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	for tag, raw := range r.manifests {
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			continue
		}
		layers, ok := doc["layers"].([]any)
		if !ok {
			continue
		}
		kept := make([]any, 0, len(layers))
		for _, l := range layers {
			if m, isMap := l.(map[string]any); isMap && m["mediaType"] == mediaType {
				continue
			}
			kept = append(kept, l)
		}
		if len(kept) == len(layers) {
			continue
		}
		doc["layers"] = kept
		out, err := json.Marshal(doc)
		if err != nil {
			return err
		}
		r.manifests[tag] = out
		r.byDigest[Digest(out)] = out
	}
	return nil
}

// MarkIndexEntryDeleted annotates one `_index` child as a tombstone, which is
// what a deletion that reached the mirror leaves behind.
func (r *Registry) MarkIndexEntryDeleted(refName string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	raw, ok := r.manifests[oci.TagOCIIndex]
	if !ok {
		return fmt.Errorf("registry has no %s manifest", oci.TagOCIIndex)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	children, _ := doc["manifests"].([]any)
	found := false
	for _, c := range children {
		child, isMap := c.(map[string]any)
		if !isMap {
			continue
		}
		annotations, _ := child["annotations"].(map[string]any)
		if annotations == nil || annotations[oci.AnnotationGitRef] != refName {
			continue
		}
		annotations[oci.AnnotationGitDeleted] = "true"
		child["annotations"] = annotations
		found = true
	}
	if !found {
		return fmt.Errorf("%s is not listed in %s", refName, oci.TagOCIIndex)
	}

	out, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	r.manifests[oci.TagOCIIndex] = out
	r.byDigest[Digest(out)] = out
	return nil
}

// BlobBytes returns a stored blob by digest, or nil.
func (r *Registry) BlobBytes(digest string) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.blobs[digest]...)
}

// SetBlobBytes replaces the content stored under a digest, leaving the digest
// as it was.
//
// This is a registry serving the wrong bytes for a blob -- a corrupted store,
// a proxy answering from the wrong entry -- which is exactly the thing a
// reader has to notice for itself, because the digest header still matches.
func (r *Registry) SetBlobBytes(digest string, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.blobs[digest] = append([]byte(nil), data...)
}

// PutBlob stores data under its own digest and returns that digest.
func (r *Registry) PutBlob(data []byte) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	digest := opencontainers.FromBytes(data).String()
	r.blobs[digest] = append([]byte(nil), data...)
	return digest
}

// PutManifest stores data under tag, addressable by digest as well, exactly
// as a PUT from another client would leave it.
//
// It exists for the same reason as SetIndexedRef: the realistic way to have a
// second writer land between a client's read and its write -- running that
// writer from inside a request hook -- serialises on the index lock and
// deadlocks instead of interleaving.
func (r *Registry) PutManifest(tag string, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	stored := append([]byte(nil), data...)
	r.manifests[tag] = stored
	r.byDigest[Digest(stored)] = stored
	r.addTag(tag)
}
