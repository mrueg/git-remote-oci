package oci

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mrueg/git-remote-oci/pkg/config"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/errcode"
)

const (
	MediaTypeGitConfig = ocispec.MediaTypeImageConfig
	MediaTypeGitIndex  = "application/vnd.git.repository.index.v1+json"
	TagRefIndex        = "_refs"
	TagOCIIndex        = "_index"

	AnnotationGitRef     = "io.git-remote-oci.ref"
	AnnotationGitParents = "io.git-remote-oci.parents"
	AnnotationGitType    = "io.git-remote-oci.type"

	// AnnotationGitPackBases names the commits this manifest's packfile was cut
	// against, as a comma-separated list of hex commit ids, or the
	// literal PackBasesNone when the packfile is self-contained.
	//
	// It is the packfile dependency graph, which is not the same thing as the
	// commit graph in AnnotationGitParents: a push carrying several commits
	// publishes a manifest only for its tip, so the tip's parent usually has no
	// manifest at all while the base it was packed against does. Fetch has to
	// follow this annotation, not the parents, or it stops at the first parent
	// that was never a tip and silently leaves the object graph incomplete.
	//
	// It is mandatory. A manifest without it is rejected rather than guessed
	// at, which is why the self-contained case is spelled out as PackBasesNone
	// instead of being left empty.
	AnnotationGitPackBases = "io.git-remote-oci.pack-bases"

	AnnotationGitTagger     = "io.git-remote-oci.tagger"
	AnnotationGitTagMessage = "io.git-remote-oci.tag-message"
	AnnotationGitTagSig     = "io.git-remote-oci.tag-signature"
	AnnotationGitTagObj     = "io.git-remote-oci.tag-object"

	// AnnotationFormatVersion records the on-registry format version on the
	// _refs index manifest. Readers refuse anything they do not recognise.
	AnnotationFormatVersion = "io.git-remote-oci.format-version"

	// AnnotationGitDeleted marks a ref tag as a tombstone: the ref was deleted,
	// but the registry would not let the manifest itself be removed.
	//
	// Without it, a tag that survives deletion is rediscovered by tag
	// enumeration and the ref reappears on the next push.
	AnnotationGitDeleted = "io.git-remote-oci.deleted"

	// AnnotationGitHead records which ref the remote's HEAD points at, on the
	// _refs index and the _index image index.
	//
	// Without it a reader has to guess, and the guess was wrong for any
	// repository whose default branch is neither main nor master.
	AnnotationGitHead = "io.git-remote-oci.head"

	// PackBasesNone is the AnnotationGitPackBases value for a packfile that
	// depends on nothing else.
	PackBasesNone = "none"
)

// FormatVersion is the on-registry format this build reads and writes. It is
// the only version there is.
//
// It moves when a change would make a reader that does not know about it
// *misread* a repository, and not otherwise. An addition a reader can ignore
// and still be correct -- an optional layer, an advisory annotation -- leaves
// it alone, because bumping would refuse repositories to readers that would
// have handled them perfectly well. See FORMAT.md §11, which is the authority.
//
// It is a tripwire, not a release counter: it does not track this project's
// version and is not expected to move often. FORMAT.md is the changelog and has
// to be updated in the same commit either way.
const FormatVersion = "1"

// ErrUnsupportedFormat reports a repository written in a format this build does
// not implement.
//
// Refusing is the point. Silently reading a layout whose meaning has changed is
// how a fetch ends up quietly missing objects, so an unrecognised version is a
// hard stop with an explanation rather than a best effort.
var ErrUnsupportedFormat = errors.New("unsupported on-registry format version")

var (
	ErrNotAnImageManifest = errors.New("reference is not an OCI image manifest")
	ErrManifestNotFound   = errors.New("manifest reference not found")
)

// checkFormatVersion rejects a repository this build cannot read.
//
// An absent annotation is treated as unrecognised rather than assumed current:
// every version this build writes sets it, so its absence means the repository
// was written by something else.
func checkFormatVersion(version string) error {
	if version == FormatVersion {
		return nil
	}
	found := version
	if found == "" {
		found = "none"
	}
	return fmt.Errorf(
		"%w: the repository declares format version %s, this build implements %s. "+
			"The on-registry format is not stable and no compatibility path is provided",
		ErrUnsupportedFormat, found, FormatVersion)
}

// IsNotFound reports whether err means "this does not exist on the registry",
// as opposed to "we could not find out".
//
// The distinction matters: a missing repository or index is the normal state of
// a registry that has never been pushed to, whereas an authentication failure,
// a 5xx, or a network error leaves the remote's contents unknown. Treating the
// second case as "empty" makes an unreachable remote look like a fresh one.
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrManifestNotFound) || errors.Is(err, errdef.ErrNotFound) {
		return true
	}
	var errResp *errcode.ErrorResponse
	if errors.As(err, &errResp) && errResp.StatusCode == http.StatusNotFound {
		return true
	}
	var codeErr errcode.Error
	if errors.As(err, &codeErr) {
		switch codeErr.Code {
		case errcode.ErrorCodeNameUnknown, errcode.ErrorCodeManifestUnknown:
			return true
		}
	}
	return false
}

// Default TTLs for the index locks. Both are Client fields rather than
// constants because the right value depends on the link and the repository,
// not on the code.
const (
	// DefaultRefsIndexLockTTL has to cover the whole critical section:
	// fetching the current index, listing refs, and pushing the index blob,
	// the config blob, the index manifest and _index. At 15 seconds that
	// routinely expired mid-update on a slow link or a repository with many
	// refs, after which another client legitimately took the lock and the two
	// interleaved their read-modify-write of _refs — the exact loss the lock
	// exists to prevent.
	//
	// Erring long costs a stalled ref until the TTL runs out; erring short
	// costs correctness. Hence five minutes, and hence configurable.
	DefaultRefsIndexLockTTL = 5 * time.Minute

	// DefaultLFSLocksIndexTTL covers a read-modify-write of one blob, which is
	// a much shorter critical section than the ref index.
	DefaultLFSLocksIndexTTL = 15 * time.Second
)

// ApplyConfig copies the registry-side tunables from a resolved git config.
//
// The client is *told* its settings rather than going looking for them: it has
// no business discovering a git repository, and the subcommands can be run
// outside one. Callers that have a remote name should resolve the config with
// it, so `remote.<name>.oci*` takes effect.
func (c *Client) ApplyConfig(cfg *config.Config) {
	// The environment keeps precedence. OCI_COMPRESSION predates this and a
	// one-off override should not require editing a config file.
	if c.Compression == "" {
		c.Compression = cfg.String(config.KeyCompression, "")
	}
	c.UploadChunkSize = cfg.Bytes(config.KeyChunkSize, DefaultChunkSize)
	c.RefsIndexLockTTL = cfg.Duration(config.KeyIndexLockTTL, DefaultRefsIndexLockTTL)
	c.LFSLocksIndexTTL = cfg.Duration(config.KeyLFSIndexLockTTL, DefaultLFSLocksIndexTTL)
	c.Concurrency = cfg.Int(config.KeyConcurrency, DefaultConcurrency)
}

// DefaultConcurrency is how many registry requests the client issues at once
// where it fans out on its own. It matches the helper's transfer pool default,
// which reads the same config key.
const DefaultConcurrency = 12

// warnf reports something that did not fail the operation but that the user
// should know about.
//
// Everything in this package that used to write to stderr directly goes
// through here, so that the helper can route it through its verbosity-aware
// logging and so that nothing in this package can ever reach stdout, which is
// the wire protocol.
func (c *Client) warnf(format string, args ...any) {
	if c.Warnf != nil {
		c.Warnf(format, args...)
		return
	}
	fmt.Fprintf(os.Stderr, "git-remote-oci: warning: "+format+"\n", args...)
}

// verbosef reports something that went right in an unusual way. Unlike warnf
// it is silent without a hook: it is detail nobody asked for unless they
// turned verbosity up.
func (c *Client) verbosef(format string, args ...any) {
	if c.Verbosef != nil {
		c.Verbosef(format, args...)
	}
}

// concurrency is Concurrency with the zero value and nonsense mapped to the
// default, so a Client built without NewClient still fans out sensibly.
func (c *Client) concurrency() int {
	if c.Concurrency <= 0 {
		return DefaultConcurrency
	}
	return c.Concurrency
}

type Client struct {
	Repo *remote.Repository

	// Warnf receives diagnostics that do not fail the operation, without a
	// prefix or trailing newline. The helper sets it so warnings respect
	// `option verbosity`; nil writes them to stderr prefixed
	// "git-remote-oci: warning: ".
	Warnf func(format string, args ...any)

	// Verbosef receives diagnostics worth seeing only when asked for, in the
	// same shape as Warnf. nil discards them.
	Verbosef func(format string, args ...any)

	// Concurrency bounds how many registry requests the client makes in
	// parallel where it fans out on its own, such as resolving every ref
	// while building _index. Zero means DefaultConcurrency.
	Concurrency int

	// Compression is the layer compression algorithm: "none", "gzip" or
	// "zstd". NewClient seeds it from OCI_COMPRESSION; a caller that reads
	// git config may overwrite it before the client is used.
	Compression string

	// UploadChunkSize is how much of a large blob is sent per request, and the
	// size a blob must exceed before it is chunked at all. 0 sends every blob
	// in one request, which is what a registry that cannot do better gets.
	UploadChunkSize int64

	// RefsIndexLockTTL and LFSLocksIndexTTL bound how long this client may
	// hold the lock on the _refs and _lfs_locks indexes. They were constants;
	// see the comments on their defaults for why the right value depends on
	// the link and the repository rather than on the code.
	RefsIndexLockTTL time.Duration
	LFSLocksIndexTTL time.Duration

	manifestCache    boundedMap
	pushedBlobsCache boundedMap
	// refTagDigests maps a ref tag to the digest this client last published
	// under it, so a re-push of the same content is skipped without skipping a
	// re-push of *different* content.
	refTagDigests boundedMap
	// pushedCommits holds the commit ids whose manifest this client has
	// itself pushed, and pushedRefs maps a ref tag to the commit id this
	// client last published the ref manifest for. Together they are the only
	// evidence IsRefFullyPushed accepts. It used to infer "already pushed"
	// from manifestCache, which is also filled by every fetch and every tag
	// enumeration — and having *read* a manifest is not having pushed it, so
	// a push after a list could skip publishing the ref entirely.
	pushedCommits sync.Map
	pushedRefs    sync.Map
	// authFrom records which credential source the client was built with.
	authFrom atomic.Value
	// authWorked records that the registry accepted a request at least once.
	//
	// A 401 on the first request means the credentials are wrong or missing. A
	// 401 after the registry has already served this client means something
	// expired mid-operation, which is a different problem with a different
	// answer, and reporting both as "unauthorized" sends people to look in the
	// wrong place.
	authWorked atomic.Bool
	// heldLocks maps ref name -> lock id for locks this client acquired, so
	// ReleaseRefLock can refuse to release someone else's lock.
	heldLocks sync.Map
	// packChainEdges are commit -> pack-bases recorded by this client's pushes,
	// merged into the published chain when the _refs index is written.
	packChainEdges sync.Map
	// packChain caches the published chain; see FetchPackChain.
	packChain atomic.Value
	// packChainReset makes the next _refs push replace the chain instead of
	// merging with it. See ResetPackChain.
	packChainReset atomic.Bool
}

func (c *Client) pushBlobOnce(ctx context.Context, desc ocispec.Descriptor, content []byte) error {
	digestStr := desc.Digest.String()
	if _, cached := c.pushedBlobsCache.Load(digestStr); cached {
		return nil
	}
	if err := c.Repo.Push(ctx, desc, bytes.NewReader(content)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return err
	}
	c.pushedBlobsCache.Store(digestStr, true)
	return nil
}

// NewClientForURL builds a client for a URL as a user typed it, applying the
// scheme and plain-HTTP rules that every entry point has to agree on.
//
// The remote helper and the subcommands both receive an oci:// URL on the
// command line and both have to strip the scheme and decide whether to speak
// plain HTTP. They used to do it in two places with two copies of the rule,
// which can only diverge silently: a subcommand reaching a local registry over
// HTTPS while the helper reaches it over HTTP, or the reverse.
//
// getenv reads the environment; nil means os.Getenv.
func NewClientForURL(rawURL string, getenv func(string) string) (*Client, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	repoRef := strings.TrimPrefix(rawURL, "oci://")

	insecure := getenv("OCI_INSECURE")
	plainHTTP := insecure == "1" || insecure == "true" ||
		strings.HasPrefix(repoRef, "localhost:") ||
		strings.HasPrefix(repoRef, "127.0.0.1:")

	return NewClient(repoRef, plainHTTP)
}

// NewClient initialises a new OCI registry client for the given repository reference (e.g. "registry.example.com/repo").
func NewClient(repoRef string, plainHTTP bool) (*Client, error) {
	repo, err := remote.NewRepository(repoRef)
	if err != nil {
		return nil, fmt.Errorf("failed to parse OCI repository reference %q: %w", repoRef, err)
	}
	repo.PlainHTTP = plainHTTP

	// Configure transport-level timeouts to prevent hanging during TCP dial, TLS handshake, or header reads
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}

	retryTr := newRetryTransport(tr)

	authClient := &auth.Client{
		Client: &http.Client{
			Transport: retryTr,
		},
		Cache: auth.NewCache(),
	}

	var dockerStore credentials.Store
	if store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{}); err == nil {
		dockerStore = store
	}

	client := &Client{
		Repo:             repo,
		manifestCache:    boundedMap{max: maxCachedManifests},
		pushedBlobsCache: boundedMap{max: maxCachedPushedBlobs},
		refTagDigests:    boundedMap{max: maxCachedManifests},
		Compression:      os.Getenv("OCI_COMPRESSION"),
		UploadChunkSize:  DefaultChunkSize,
		RefsIndexLockTTL: DefaultRefsIndexLockTTL,
		LFSLocksIndexTTL: DefaultLFSLocksIndexTTL,
		Concurrency:      DefaultConcurrency,
	}
	// The transport reports back so the client can tell a credential that never
	// worked from one that stopped working part-way through.
	retryTr.observe = client.noteResponse

	client.authFrom.Store(originAnonymous)

	authClient.Credential = func(ctx context.Context, serverAddress string) (auth.Credential, error) {
		// 1. Environment variable overrides
		if token := os.Getenv("OCI_BEARER_TOKEN"); token != "" {
			client.authFrom.Store(originEnvBearer)
			return auth.Credential{AccessToken: token}, nil
		}
		if token := os.Getenv("OCI_TOKEN"); token != "" {
			client.authFrom.Store(originEnvToken)
			return auth.Credential{AccessToken: token}, nil
		}
		user := os.Getenv("OCI_USERNAME")
		pass := os.Getenv("OCI_PASSWORD")
		if user != "" || pass != "" {
			client.authFrom.Store(originEnvUserPass)
			return auth.Credential{Username: user, Password: pass}, nil
		}

		// 2. Docker credential store (~/.docker/config.json and native credential helpers)
		if dockerStore != nil {
			cred, err := credentials.Credential(dockerStore)(ctx, serverAddress)
			if err == nil && (cred.Username != "" || cred.Password != "" || cred.AccessToken != "" || cred.RefreshToken != "") {
				client.authFrom.Store(originDockerStore)
				return cred, nil
			}
		}

		// 3. Fallback to empty credential for public access
		client.authFrom.Store(originAnonymous)
		return auth.EmptyCredential, nil
	}

	repo.Client = authClient
	return client, nil
}

// emptyLayers returns the layer list for a manifest that carries its payload in
// its config or its annotations rather than in a layer.
//
// The field cannot simply be omitted: ocispec.Manifest tags Layers as `layers`
// with no omitempty, so leaving it nil serialises as `"layers": null`, and the
// image-spec requires an array. Registries that validate manifests reject that,
// which broke ref locking and LFS locking on them while ordinary push and fetch
// kept working - a confusing partial failure.
//
// The single empty-JSON descriptor is the idiom the spec defines for exactly
// this case, and is better supported than an empty array.
func emptyLayers() []ocispec.Descriptor {
	return []ocispec.Descriptor{ocispec.DescriptorEmptyJSON}
}

// pushEmptyBlob uploads the `{}` blob that emptyLayers refers to.
//
// A manifest whose layer is not in the registry is rejected by anything that
// validates, so this has to run before the manifest that names it.
func (c *Client) pushEmptyBlob(ctx context.Context) error {
	if err := c.pushBlobOnce(ctx, ocispec.DescriptorEmptyJSON, ocispec.DescriptorEmptyJSON.Data); err != nil {
		return fmt.Errorf("failed to push the empty layer blob: %w", err)
	}
	return nil
}

// objectIDLenSHA1 and objectIDLenSHA256 are the hex lengths of the two hash
// algorithms git uses.
const (
	objectIDLenSHA1   = 40
	objectIDLenSHA256 = 64
)

// isObjectID reports whether s is a git object id in hex, under either hash
// algorithm.
//
// Accepting both is what makes SHA-256 repositories work: object ids appear as
// tag names, as pack-base entries and as revision annotations, and every one of
// those checks used to insist on exactly 40 characters.
func isObjectID(s string) bool {
	if len(s) != objectIDLenSHA1 && len(s) != objectIDLenSHA256 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

// IsCommitID reports whether s is a commit id this implementation can address.
//
// Exported so callers can validate ids that arrive in registry annotations
// before using them as tag names or filesystem components.
func IsCommitID(s string) bool { return isObjectID(s) }

// FormatPackBases renders bases for AnnotationGitPackBases.
//
// An empty list is PackBasesNone rather than the empty string, so that a
// self-contained packfile is a positive statement instead of an absent one.
func FormatPackBases(bases []string) string {
	if len(bases) == 0 {
		return PackBasesNone
	}
	return strings.Join(bases, ",")
}

// ParsePackBases reads AnnotationGitPackBases.
//
// The annotation is mandatory. Absent, empty or malformed is an error rather
// than an empty list, because an empty list means "self-contained" and acting
// on that guess is exactly how a fetch ends up silently missing objects.
func ParsePackBases(annotations map[string]string) ([]string, error) {
	raw, ok := annotations[AnnotationGitPackBases]
	if !ok {
		return nil, fmt.Errorf("manifest has no %s annotation", AnnotationGitPackBases)
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("manifest has an empty %s annotation; a self-contained packfile must say %q", AnnotationGitPackBases, PackBasesNone)
	}
	if raw == PackBasesNone {
		return nil, nil
	}
	var bases []string
	for _, field := range strings.Split(raw, ",") {
		base := strings.TrimSpace(field)
		if base == "" {
			continue
		}
		// These become tag names on the next request, so they are validated
		// here rather than at the point of use.
		if !isObjectID(base) {
			return nil, fmt.Errorf("malformed %s entry %q: not a hex commit id of 40 or 64 characters", AnnotationGitPackBases, base)
		}
		bases = append(bases, base)
	}
	return bases, nil
}

// RefManifestTag returns the tag under which the ref manifest for refName is
// published, or "" if refName cannot be represented as an OCI tag.
//
// There is exactly one tag per ref, for both reading and writing.
//
// It is not simply EncodeRefTag: a ref whose encoded name happens to look like
// a commit id is prefixed so it cannot be mistaken for one of the ref-agnostic
// commit manifests. Anything reasoning about what a tag means - garbage
// collection, for one - must use this rather than EncodeRefTag.
func RefManifestTag(refName string) string {
	tag := EncodeRefTag(refName)
	if tag == "" {
		return ""
	}
	// A branch whose encoded name happens to look like a commit id would
	// collide with the ref-agnostic commit-SHA manifests, so give it a prefix.
	if isObjectID(tag) {
		return "ref-" + tag
	}
	return tag
}

// InvalidateManifestCache removes a cached manifest entry for the given tag or digest.
func (c *Client) InvalidateManifestCache(tagOrDigest string) {
	if tagOrDigest != "" {
		c.manifestCache.Delete(tagOrDigest)
	}
}

// ClearManifestCache clears all cached manifests.
func (c *Client) ClearManifestCache() {
	c.manifestCache.Range(func(key, value any) bool {
		c.manifestCache.Delete(key)
		return true
	})
}

func isPackfileMediaType(mediaType string) bool {
	return mediaType == MediaTypeGitPackfile ||
		mediaType == MediaTypeGitPackfileGzip ||
		mediaType == MediaTypeGitPackfileZstd
}

func hasGitPackfileLayer(manifest *ocispec.Manifest) bool {
	for i := range manifest.Layers {
		// A snapshot layer carries a packfile media type too, so it has to be
		// excluded by name rather than by relying on layer order.
		if isSnapshotLayer(manifest.Layers[i]) {
			continue
		}
		if isPackfileMediaType(baseMediaType(manifest.Layers[i].MediaType)) {
			return true
		}
	}
	return false
}

// validRefName reports whether a name can be a git ref at all.
//
// This is not git-check-ref-format(1) in full — it is the subset that decides
// whether a name is safe to *emit*. A remote helper's `list` output is
// newline-delimited protocol on stdout, so a ref name carrying a newline does
// not produce a badly named ref: it produces an extra line, which git reads as
// another ref entirely, at whatever object id the rest of the line supplies. A
// space does the same thing to the field boundary within a line.
//
// Both are already illegal in a git ref name, so nothing legitimate is lost by
// refusing them, and the check stays deliberately narrow for that reason: the
// job here is to make the output unambiguous, not to re-implement git's
// validation and start rejecting refs git itself accepts.
func validRefName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == ' ' {
			return false
		}
	}
	return true
}

// maxMetadataBytes bounds any registry document read whole into memory.
//
// Manifests, index blobs and lock lists are all read entire, because they are
// JSON and there is no useful way to stream them. Their size comes from the
// registry, so an unbounded read is an invitation: a hostile or compromised
// registry declares a very large blob, serves it, and the client dies holding
// it. Packfiles are exempt from the problem by construction — they are streamed
// to disk, never buffered.
//
// 64 MiB is far above anything this format produces. The largest document here
// is the `_refs` index, at roughly two hundred bytes per ref; the ceiling is
// reached somewhere past three hundred thousand refs, which is an order of
// magnitude beyond the largest repositories in existence. It is a backstop, not
// a quota.
const maxMetadataBytes = 64 << 20

// readMetadataBlob reads a registry document with both bounds applied: what the
// descriptor claims, and what this client is willing to hold regardless.
//
// declaredSize of zero or less means the caller has no descriptor to go on —
// FetchReference hands back a stream before the size is known — and only the
// absolute cap applies.
func readMetadataBlob(rc io.Reader, declaredSize int64, what string) ([]byte, error) {
	limit := int64(maxMetadataBytes)
	if declaredSize > 0 {
		if declaredSize > limit {
			return nil, fmt.Errorf("%s declares %d bytes, above the %d-byte limit this reads",
				what, declaredSize, limit)
		}
		limit = declaredSize
	}

	// One byte past the limit, so that overrunning it is distinguishable from
	// exactly reaching it.
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than the %d bytes it declared", what, limit)
	}
	return data, nil
}
