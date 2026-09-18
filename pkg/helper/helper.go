package helper

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/mrueg/git-remote-oci/pkg/config"
	"github.com/mrueg/git-remote-oci/pkg/git"
	"github.com/mrueg/git-remote-oci/pkg/lfs"
	"github.com/mrueg/git-remote-oci/pkg/oci"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/errgroup"
)

type Helper struct {
	in  io.Reader
	out io.Writer
	// outMu serialises writes to out. Both the fetch and push paths emit
	// protocol responses from errgroup workers, and stdout *is* the wire
	// protocol: an interleaved write corrupts the session.
	outMu     sync.Mutex
	ociClient *oci.Client
	gitRepo   *git.Repository
	// refsMu guards remoteRefs and richRemoteRefs, which are read and updated
	// concurrently by push workers.
	refsMu         sync.Mutex
	remoteRefs     map[string]string       // cached from last handleList call
	richRemoteRefs map[string]oci.RefEntry // cached rich ref metadata entries
	// shallowMu serialises updates to $GIT_DIR/shallow, which fetch workers
	// would otherwise read-modify-write concurrently and lose entries from.
	shallowMu sync.Mutex
	// remoteRefsKnown records whether remoteRefs reflects a successful listing.
	// Without it an empty map is ambiguous: it could mean "the remote has no
	// refs" or "we never managed to find out", and treating the latter as the
	// former makes every push look like a create and skips the
	// fast-forward check.
	remoteRefsKnown bool
	casRefs         map[string]string // expected remote ref SHAs for CAS (force-with-lease)
	atomic          bool
	dryRun          bool
	depth           int
	filter          string // blob filter spec (e.g. "blob:none", "blob:limit=100k")
	verbosity       int    // verbosity level: 0 = quiet, 1 = normal (default), >= 2 = verbose
	progress        bool   // progress reporting enabled via option progress true
	followTags      bool   // followtags enabled via option followtags true
	// transferWorkers and blobWorkers size the errgroup pools that fetch and
	// push fan out over. They were literals; see the defaults for what each
	// pool is doing and why the right number depends on the link.
	transferWorkers int
	blobWorkers     int
	// pushLockTTL bounds how long one ref's push may hold its lock.
	pushLockTTL time.Duration
	// shallowSnapshot enables *publishing* a self-contained snapshot of each
	// ref tip on push. Reading one is unconditional: it costs the reader
	// nothing, and a cloner has no configuration of its own to consult.
	// Off by default; see defaultShallowSnapshot for why.
	shallowSnapshot bool
	// protocolV2 enables serving wire protocol v2 over stateless-connect. See
	// v2.go; on by default, with the simple path kept as the way back.
	protocolV2 bool
	// compactAfter is how many published commit manifests may accumulate
	// before a push compacts the repository. 0 disables it.
	compactAfter int
	// timer records where the time went, when GIT_REMOTE_OCI_TIMING asks.
	timer *phaseTimer
	// reader is the command stream, shared between the line-oriented loop in
	// Run and the pkt-line reader stateless-connect switches to.
	reader *bufio.Reader
	// objectFormat records that git asked for the remote's hash algorithm to be
	// reported back on list. It only sets this when fetching refs.
	objectFormat bool
	refPrefixes  []string // refspec prefix filters sent prior to list/list for-push
}

// Defaults for the tunables git config can override. Each was a literal in the
// code; the value is a guess about the link, and a guess is exactly the kind of
// thing a user should be able to correct.
const (
	// defaultTransferWorkers sizes the pools that fetch manifests and packs
	// and that upload LFS objects. Twelve keeps a fast link busy without
	// opening so many connections that a registry starts rate-limiting.
	defaultTransferWorkers = 12

	// defaultBlobWorkers sizes the wide fan-out that pushes many refs' blobs
	// at once. It is deliberately much larger: those requests are mostly
	// waiting on the registry rather than on this process.
	defaultBlobWorkers = 64

	// defaultShallowSnapshot is off.
	//
	// The snapshot makes `git clone --depth 1` cheap, but it costs a second,
	// undeltified copy of the tip's tree on every push that publishes one —
	// enough to undo the thin-pack saving entirely: a seven-byte edit to a
	// 512 KB file costs 1.3 KB as an incremental packfile and 336 KB with a
	// snapshot beside it.
	//
	// Most repositories are pushed to far more often than they are cloned
	// shallowly, so the default declines the trade. A repository that is
	// mostly a source for CI checkouts should take it, with
	// `git config ociremote.shallowSnapshot true`.
	defaultShallowSnapshot = false

	// defaultProtocolV2 is on. It is what a remote helper has to speak for
	// `--filter` to mean anything -- the simple `fetch` command is defined as
	// delivering a complete object graph and git verifies that -- and it is
	// where `--depth` is applied while the pack is built rather than after it
	// arrives. Leaving it off meant the better implementation was the one
	// nobody got unless they had read far enough to find the switch.
	//
	// Turning it off is still a supported answer: `stateless-connect` may
	// decline with `fallback`, which returns to the simple path for that
	// connection, so `ociremote.protocolV2=false` costs features and not
	// correctness.
	defaultProtocolV2 = true

	// defaultPushLockTTL has to cover generating the packfile and uploading
	// it, which on a large history over a slow link is minutes rather than
	// seconds. A lock that expires mid-push is worse than no lock: another
	// client acquires it legitimately and the two interleave exactly the
	// update the lock exists to serialise. Erring long costs a ref blocked
	// until the TTL runs out after a client dies.
	defaultPushLockTTL = 10 * time.Minute

	// defaultCompactAfter is how many published commits accumulate before a
	// push compacts the repository.
	//
	// Every push publishes one commit manifest and one commit tag, and both
	// are load-bearing: the tag is a pack base later pushes were cut against,
	// so nothing can be removed until the history is repacked to stand alone.
	// Left to itself the count grows without bound, and it is what a clone
	// pays -- the pack-base graph is one manifest deep per push, so a thousand
	// pushes is a thousand manifests to fetch and a thousand packs to index.
	//
	// The published pack chain removed the round-trip cost of that graph but
	// not the graph, so compaction is still what actually fixes it. Making it
	// a command meant it happened when someone remembered; a threshold means
	// it happens.
	//
	// 50 keeps the graph small enough that a clone is a handful of parallel
	// fetches, while compacting rarely enough that the cost -- repacking the
	// whole history and re-uploading it -- lands on roughly one push in fifty.
	// `ociremote.compactAfter` moves it, and 0 turns it off for anyone who
	// would rather schedule `git-remote-oci gc` themselves.
	defaultCompactAfter = 50
)

func NewHelper(remoteName, rawURL string, in io.Reader, out io.Writer) (*Helper, error) {
	client, err := oci.NewClientForURL(rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to initialise OCI client: %w", err)
	}

	// remoteName is what makes the per-remote scope work: pushing to a local
	// registry and to a hosted one from the same clone want different
	// concurrency and different timeouts, and `remote.<name>.oci*` is how a
	// user says so.
	cfg := config.Load(remoteName)
	client.ApplyConfig(cfg)

	h := &Helper{
		in:              in,
		out:             out,
		ociClient:       client,
		verbosity:       1,
		transferWorkers: cfg.Int(config.KeyConcurrency, defaultTransferWorkers),
		blobWorkers:     cfg.Int(config.KeyBlobConcurrency, defaultBlobWorkers),
		pushLockTTL:     cfg.Duration(config.KeyPushLockTTL, defaultPushLockTTL),
		shallowSnapshot: cfg.Bool(config.KeyShallowSnapshot, defaultShallowSnapshot),
		protocolV2:      cfg.Bool(config.KeyProtocolV2, defaultProtocolV2),
		// Count, not Int: zero is a real setting here and means "never".
		compactAfter: cfg.Count(config.KeyCompactAfter, defaultCompactAfter),
		timer:        newPhaseTimer(os.Getenv),
	}
	// Registry-side warnings go through the helper's logger so they carry the
	// same prefix as its own and can never reach stdout.
	client.Warnf = func(format string, args ...any) {
		h.logWarn("git-remote-oci: warning: "+format+"\n", args...)
	}
	return h, nil
}

type fetchSpec struct {
	sha string
	ref string
}

func (h *Helper) Run(ctx context.Context) error {
	// stderr, not the protocol stream: stdout is the wire and anything written
	// there that git did not ask for corrupts the session.
	defer h.timer.report(os.Stderr)

	// One reader for the whole session, kept on the helper so that
	// stateless-connect can carry on from the same buffer.
	//
	// It used to be a bufio.Scanner here and a fresh reader over h.in inside
	// serveStatelessConnect, which loses anything the scanner had read ahead
	// into its buffer. Nothing breaks today because git waits for the helper's
	// answer to `stateless-connect` before sending the first pkt-line, so the
	// read that returns that command returns nothing after it -- but the
	// protocol does not promise that, and the failure it would produce is the
	// helper waiting for a request that has already arrived and been thrown
	// away.
	h.reader = bufio.NewReader(h.in)
	var fetchBatch []fetchSpec
	var pushBatch []string

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		raw, readErr := h.reader.ReadString('\n')
		if readErr != nil && raw == "" {
			if !errors.Is(readErr, io.EOF) {
				return fmt.Errorf("failed to read remote helper command stream: %w", readErr)
			}
			break
		}

		line := strings.TrimSpace(raw)
		if line == "" {
			// Empty line signifies end of a command batch (fetch or push or list)
			if err := h.flushBatches(ctx, &fetchBatch, &pushBatch); err != nil {
				return err
			}
			continue
		}

		parts := strings.Fields(line)
		cmd := parts[0]

		switch cmd {
		case "capabilities":
			// Only real gitremote-helpers(7) capability names belong here.
			// "pushcert", "push-option" and "ref-prefix" are not capabilities,
			// and "filter"/"deepen" are *option* names, not capabilities - the
			// mandatory marker is "*", not "+". Git silently ignores unknown
			// non-mandatory lines, so advertising them achieved nothing except
			// to suggest support that does not exist.
			h.printlnOut("fetch")
			h.printlnOut("push")
			h.printlnOut("option")
			// Lets git ask for the remote's hash algorithm, which it needs
			// before it will talk to a SHA-256 repository.
			h.printlnOut("object-format")
			// Serving wire protocol v2. Advertising it unconditionally is safe
			// because the command itself may answer `fallback`, which is how a
			// helper declines a smart transport; git then uses the lines above.
			h.printlnOut("stateless-connect")
			h.printlnOut() // Blank line terminates capabilities list

		case "stateless-connect":
			if len(parts) < 2 {
				return fmt.Errorf("stateless-connect requires a service name")
			}
			// A served conversation runs to completion on this pipe and the
			// helper exits when it ends. A declined one returns here, and the
			// simple protocol carries on as though nothing had happened.
			served, err := h.serveStatelessConnect(ctx, parts[1])
			if err != nil {
				return err
			}
			if served {
				return nil
			}

		case "ref-prefix":
			// Not advertised, and git does not send this to remote helpers
			// (it is a protocol-v2 ls-refs argument). Kept because the
			// filtering it drives is harmless and correct if anything ever
			// does send it.
			if len(parts) >= 2 {
				h.refPrefixes = append(h.refPrefixes, parts[1])
			}

		case "list":
			forPush := len(parts) > 1 && parts[1] == "for-push"
			if err := h.handleList(ctx, forPush); err != nil {
				return fmt.Errorf("list error: %w", err)
			}

		case "fetch":
			if len(parts) >= 3 {
				fetchBatch = append(fetchBatch, fetchSpec{sha: parts[1], ref: parts[2]})
			} else if len(parts) == 2 {
				fetchBatch = append(fetchBatch, fetchSpec{sha: parts[1], ref: ""})
			}

		case "push":
			if len(parts) >= 2 {
				pushBatch = append(pushBatch, parts[1]) // parts[1] is src:dst
			}

		case "option":
			if len(parts) >= 3 {
				switch parts[1] {
				case "atomic":
					switch parts[2] {
					case "true":
						h.atomic = true
						h.printlnOut("ok")
					case "false":
						h.atomic = false
						h.printlnOut("ok")
					default:
						h.printlnOut("unsupported")
					}
				case "dry-run":
					switch parts[2] {
					case "true":
						h.dryRun = true
						h.printlnOut("ok")
					case "false":
						h.dryRun = false
						h.printlnOut("ok")
					default:
						h.printlnOut("unsupported")
					}
				case "depth":
					// gitremote-helpers(7) defines `depth`; the deepen-*
					// options are `deepen-since`, `deepen-not` and
					// `deepen-relative`, and a bare "deepen" is none of them.
					depthVal, err := strconv.Atoi(parts[2])
					if err == nil && depthVal >= 0 {
						h.depth = depthVal
						h.printlnOut("ok")
					} else {
						h.printlnOut("unsupported")
					}
				case "filter":
					h.filter = parts[2]
					h.printlnOut("ok")
				case "cas":
					ref, expectedSHA, ok := strings.Cut(parts[2], ":")
					if ok && ref != "" {
						if h.casRefs == nil {
							h.casRefs = make(map[string]string)
						}
						h.casRefs[ref] = expectedSHA
						h.printlnOut("ok")
					} else {
						h.printlnOut("unsupported")
					}
				case "verbosity":
					vVal, err := strconv.Atoi(parts[2])
					if err == nil && vVal >= 0 {
						h.verbosity = vVal
						h.printlnOut("ok")
					} else {
						h.printlnOut("unsupported")
					}
				case "progress":
					switch parts[2] {
					case "true":
						h.progress = true
						h.printlnOut("ok")
					case "false":
						h.progress = false
						h.printlnOut("ok")
					default:
						h.printlnOut("unsupported")
					}
				case "followtags":
					switch parts[2] {
					case "true":
						h.followTags = true
						h.printlnOut("ok")
					case "false":
						h.followTags = false
						h.printlnOut("ok")
					default:
						h.printlnOut("unsupported")
					}
				case "object-format":
					// "option object-format true" asks for the algorithm to be
					// reported on list. It is not a request to change it: the
					// algorithm is a property of what was pushed.
					switch parts[2] {
					case "true":
						h.objectFormat = true
						h.printlnOut("ok")
					case "false":
						h.objectFormat = false
						h.printlnOut("ok")
					default:
						h.printlnOut("unsupported")
					}

				case "pushcert":
					// Accepted so git does not error out, and then nothing is
					// done with it: there is no server here to verify a push
					// certificate against. The value used to be recorded on the
					// manifest, which was worse than dropping it — it went into
					// org.opencontainers.image.signature, where "true" reads as
					// a signature to anything that trusts that key.
					switch parts[2] {
					case "true", "false", "if-asked":
						h.printlnOut("ok")
					default:
						h.printlnOut("unsupported")
					}
				case "push-option":
					// Only followtags is acted on. Everything else used to be
					// answered "ok", which told git an arbitrary `-o foo=bar`
					// had been accepted when it was dropped on the floor.
					// "unsupported" is the protocol's way of saying so, and git
					// handles it without failing the push.
					switch parts[2] {
					case "followtags=true", "followtags":
						h.followTags = true
						h.printlnOut("ok")
					case "followtags=false":
						h.followTags = false
						h.printlnOut("ok")
					default:
						h.printlnOut("unsupported")
					}
				default:
					h.printlnOut("unsupported")
				}
			} else {
				h.printlnOut("unsupported")
			}

		case "quit":
			// Flush before leaving: a batch accumulated but not yet terminated
			// by a blank line would otherwise be dropped silently.
			return h.flushBatches(ctx, &fetchBatch, &pushBatch)

		default:
			h.logWarn("git-remote-oci: unknown command %s\n", line)
		}
	}

	// Flush any pending batch on EOF to avoid dropping the final commands
	// when stdin closes without a trailing blank line.
	return h.flushBatches(ctx, &fetchBatch, &pushBatch)
}

// flushBatches runs whichever command batch has accumulated and sees that its
// responses are terminated with the mandatory blank line.
//
// The blank line is emitted even when the batch fails, because git reads
// responses until it sees one. Returning early without it leaves git waiting on
// a stream that is about to close, which surfaces as a confusing protocol error
// rather than the real cause. A push batch that gets as far as per-ref results
// writes the line itself, so that what it does afterwards -- compaction -- runs
// once git has been answered; see finishPushBatch.
func (h *Helper) flushBatches(ctx context.Context, fetchBatch *[]fetchSpec, pushBatch *[]string) error {
	switch {
	case len(*fetchBatch) > 0:
		specs := *fetchBatch
		*fetchBatch = nil
		err := h.handleFetchBatch(ctx, specs)
		h.printlnOut()
		return err

	case len(*pushBatch) > 0:
		specs := *pushBatch
		*pushBatch = nil
		if err := h.handlePushBatch(ctx, specs); err != nil {
			// A batch-level failure (cannot open the repo, cannot determine
			// remote state) is not attributable to one ref, so report it
			// against every ref in the batch rather than dying silently.
			for _, spec := range specs {
				h.printlnOut(errorLine(pushSpecDst(spec), "%v", err))
			}
			h.printlnOut()
		}
		return nil
	}
	return nil
}

// pushReport is one ref's protocol response, held back until the push has
// actually finished.
//
// Reporting from inside the worker announced success before the _refs index was
// rewritten. A failed index update then left the remote advertising the old SHA
// while git believed the push had landed, and the next push made its
// fast-forward and force-with-lease decisions from that stale value.
type pushReport struct {
	dstRef string
	line   string
	ok     bool
}

func okReport(dstRef string) pushReport {
	return pushReport{dstRef: dstRef, line: "ok " + dstRef, ok: true}
}

func failReport(dstRef, format string, a ...any) pushReport {
	return pushReport{dstRef: dstRef, line: errorLine(dstRef, format, a...)}
}

// errorLine formats one `error <ref> <why>` response.
//
// The reason is very often a registry's own words -- an HTTP error body, a
// message from a proxy in between -- and those are not this helper's to trust
// on the protocol stream. See sanitiseProtocolText.
func errorLine(dstRef, format string, a ...any) string {
	return "error " + dstRef + " " + sanitiseProtocolText(fmt.Sprintf(format, a...))
}

// sanitiseProtocolText makes untrusted text safe to carry on one protocol
// line.
//
// stdout is line-delimited with no framing to hide behind, so a newline in a
// registry's error message does not produce an odd-looking reason: it ends the
// `error <ref>` line early and starts another, which git parses as a response
// to a ref that was never in the batch. Every control character becomes a
// space, so one message stays one line.
func sanitiseProtocolText(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

// headHintForRefs picks the branch a repository with no recorded HEAD should
// adopt, from the refs a push has just moved.
//
// The remote-helper protocol never tells the helper what the remote's default
// branch ought to be, so the best available signal is the branch being pushed.
// Only used when nothing is recorded yet; see PushRichRefIndexWithHead. Passed
// only what was pushed, never what was deleted: a deletion of refs/heads/main
// is not a vote for it.
func headHintForRefs(refs map[string]oci.RefEntry) string {
	best := ""
	for refName := range refs {
		if !strings.HasPrefix(refName, "refs/heads/") {
			continue
		}
		if refName == "refs/heads/main" || refName == "refs/heads/master" {
			return refName
		}
		if best == "" || refName < best {
			best = refName
		}
	}
	return best
}

// objectFormatOf reports the hash algorithm the published ids use.
//
// Returns "" for a repository with no refs, which has no algorithm to report;
// git then uses its own default, which is the right answer for something about
// to be pushed to for the first time.
func objectFormatOf(refs map[string]string) string {
	for _, sha := range refs {
		switch len(sha) {
		case 64:
			return "sha256"
		case 40:
			return "sha1"
		}
	}
	return ""
}

// pushSpecDst extracts the destination ref from a "<src>:<dst>" push spec.
func pushSpecDst(spec string) string {
	spec = strings.TrimPrefix(spec, "+")
	if _, dst, ok := strings.Cut(spec, ":"); ok && dst != "" {
		return dst
	}
	return spec
}

// discoverRemoteRefs resolves the remote's refs, preferring the _refs index and
// falling back to tag enumeration for repositories that predate it.
//
// It deliberately distinguishes "the remote has no refs" from "we could not
// find out what refs the remote has". Only a genuinely absent repository or
// index counts as empty; every other failure is returned to the caller. Callers
// use the fast-forward state derived from these refs to decide whether a push
// would clobber someone else's work, so silently reporting an empty remote here
// turns an auth failure or a network blip into a forced overwrite of every ref.
//
// The same rule decides when to fall through to the next source: only when
// the one before it found nothing there. Anything else it said is the answer.
// The index is where a repository declares its format version, and a refusal
// on those grounds used to be bypassed by the enumeration below, which reads
// the ref tags without ever looking -- exactly the reader the version exists
// to stop.
func (h *Helper) discoverRemoteRefs(ctx context.Context) (map[string]oci.RefEntry, error) {
	richRefs, err := h.ociClient.FetchRichRefIndex(ctx)
	if err != nil && !oci.IsNotFound(err) {
		return nil, fmt.Errorf("failed to determine remote refs: %w", err)
	}
	if len(richRefs) > 0 {
		return richRefs, nil
	}

	refs, err := h.ociClient.ListRefs(ctx)
	if err != nil && !oci.IsNotFound(err) {
		return nil, fmt.Errorf("failed to determine remote refs: %w", err)
	}
	if len(refs) > 0 {
		return plainRefEntries(refs), nil
	}

	tagList, err := h.ociClient.EnumerateTagRefs(ctx)
	if err != nil && !oci.IsNotFound(err) {
		return nil, fmt.Errorf("failed to determine remote refs: %w", err)
	}
	if len(tagList) > 0 {
		return plainRefEntries(tagList), nil
	}

	// Every source came back empty, and each was allowed to only because
	// nothing is there.
	return map[string]oci.RefEntry{}, nil
}

// plainRefEntries lifts a name-to-id map into index entries with no metadata,
// which is all the fallback sources know.
func plainRefEntries(refs map[string]string) map[string]oci.RefEntry {
	out := make(map[string]oci.RefEntry, len(refs))
	for refName, sha := range refs {
		out[refName] = oci.RefEntry{SHA: sha}
	}
	return out
}

// v2RemoteRefs returns the published refs, discovering them once per process.
//
// discoverRemoteRefs is a network round-trip every time — FetchRichRefIndex
// drops its cache entry before fetching, deliberately, so that a push reads
// fresh state. Serving one protocol-v2 conversation asks for the ref set four
// or five times over (the advertisement, ls-refs, resolving wants to the refs
// that name them, and the promisor fallback), and paying for a listing at each
// of them is both slow and wrong: a push landing mid-conversation would make
// ls-refs and the pack that follows it disagree about what the remote holds.
//
// The simple path already caches this way, via handleList; this is the same
// cache, reached from the interface that has no list command.
func (h *Helper) v2RemoteRefs(ctx context.Context) (map[string]oci.RefEntry, error) {
	h.refsMu.Lock()
	if h.remoteRefsKnown {
		refs := h.richRemoteRefs
		h.refsMu.Unlock()
		return refs, nil
	}
	h.refsMu.Unlock()

	refs, err := h.discoverRemoteRefs(ctx)
	if err != nil {
		return nil, err
	}
	h.setRemoteRefs(refs)
	return refs, nil
}

// setRemoteRefs installs a successfully discovered ref set.
func (h *Helper) setRemoteRefs(richRefs map[string]oci.RefEntry) {
	h.refsMu.Lock()
	defer h.refsMu.Unlock()
	h.richRemoteRefs = richRefs
	h.remoteRefs = make(map[string]string, len(richRefs))
	for k, v := range richRefs {
		h.remoteRefs[k] = v.SHA
	}
	h.remoteRefsKnown = true
}

func (h *Helper) handleList(ctx context.Context, forPush bool) error {
	_ = forPush // Protocol parameter explicitly ignored in list output formatting

	richRefs, err := h.discoverRemoteRefs(ctx)
	if err != nil {
		return err
	}

	h.setRemoteRefs(richRefs)

	// Filter remote refs by requested refPrefixes if any ref-prefix lines were provided
	if len(h.refPrefixes) > 0 {
		filteredRich := make(map[string]oci.RefEntry)
		filteredRefs := make(map[string]string)
		for refName, entry := range h.richRemoteRefs {
			for _, prefix := range h.refPrefixes {
				if strings.HasPrefix(refName, prefix) {
					filteredRich[refName] = entry
					filteredRefs[refName] = entry.SHA
					break
				}
			}
		}
		richRefs = filteredRich
		h.richRemoteRefs = filteredRich
		h.remoteRefs = filteredRefs
		// The prefixes stay in effect for the rest of the session. Clearing
		// them here made a second "list" in the same session silently
		// unfiltered.
	}

	// Sort ref names for deterministic output ordering
	refNames := make([]string, 0, len(h.remoteRefs))
	for refName := range h.remoteRefs {
		refNames = append(refNames, refName)
	}
	sort.Strings(refNames)

	// The hash algorithm is reported before the refs, per gitremote-helpers(7),
	// and only when git asked for it. It is derived from the ids already
	// published rather than recorded separately: a stored value could disagree
	// with the ids beside it, and this cannot.
	if h.objectFormat {
		if algo := objectFormatOf(h.remoteRefs); algo != "" {
			h.printfOut(":object-format %s\n", algo)
		}
	}

	var headTarget string
	var branches []string
	for _, refName := range refNames {
		// gitremote-helpers(7) defines each list line as "<value> <name>",
		// where <value> is an object id, "@<dest>", ":<keyword> <value>", or
		// "?". There is no "^<oid> <name>^{}" peel form - that is dumb-HTTP
		// info/refs syntax, and emitting it here registers a bogus ref
		// literally named "<name>^{}" with an unparseable value.
		//
		// For an annotated tag we therefore advertise the tag object itself.
		// The tag object rides along in the packfile, so git peels it locally.
		value := h.remoteRefs[refName]
		if richEntry, hasRich := richRefs[refName]; hasRich && richEntry.TagObject != "" {
			value = richEntry.TagObject
		}
		h.printfOut("%s %s\n", value, refName)
		if strings.HasPrefix(refName, "refs/heads/") {
			branches = append(branches, refName)
		}
	}

	// Prefer the HEAD the repository actually records. Guessing is a fallback
	// for repositories written before it was recorded, and it is only ever a
	// guess: a repository whose default branch is neither main nor master used
	// to clone onto the wrong branch entirely.
	if recorded, err := h.ociClient.FetchHead(ctx); err == nil && recorded != "" {
		if _, live := h.remoteRefs[recorded]; live {
			headTarget = recorded
		}
	}
	if headTarget == "" {
		for _, refName := range refNames {
			if refName == "refs/heads/main" {
				headTarget = refName
			} else if refName == "refs/heads/master" && headTarget != "refs/heads/main" {
				headTarget = refName
			}
		}
	}
	// Deterministic fallback: sort branch names and pick the first lexicographically
	if headTarget == "" && len(branches) > 0 {
		sort.Strings(branches)
		headTarget = branches[0]
	}

	if headTarget != "" {
		h.printfOut("@%s HEAD\n", headTarget)
	}

	h.printlnOut() // End of list
	return nil
}

// keptPacks collects the .keep files git index-pack created during one fetch
// batch.
//
// gitremote-helpers(7) lets a fetch report "lock <file>" so git holds a pack
// until refs are updated, but git tracks only the first such line per batch and
// warns about any others. Because this helper imports one pack per commit, a
// batch produces many .keep files: one is handed to git, and the rest are
// removed once the batch is done rather than left behind to pin packs against
// gc forever.
type keptPacks struct {
	mu    sync.Mutex
	paths []string
}

func (k *keptPacks) add(path string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.paths = append(k.paths, path)
}

func (k *keptPacks) all() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.paths...)
}

// maxPackGraphNodes bounds how many manifests one fetch batch will resolve.
//
// The graph is described entirely by registry content, so its size is not ours
// to trust. A repository with this many pushes behind a single ref is already
// well past the point where it should have been compacted with gc.
const maxPackGraphNodes = 50000

// packGraph is the pack-base DAG for one fetch batch, resolved up front.
//
// The walk used to be recursive: fetch a manifest, then recurse into each base
// and wait for it before importing. That had two problems. A base named by two
// refs had to be claimed and waited on, which meant a *cycle* in the graph
// deadlocked — the goroutine holding a node's claim ended up waiting on itself,
// and nothing broke the wait because no operation carries a deadline. And depth
// was one goroutine per push generation, which is unbounded in the same way.
//
// Resolving the graph first makes both go away. A cycle is a graph property, so
// it is found by looking at the graph rather than by hanging on it, and the
// import order comes out of a topological sort instead of the call stack.
type packGraph struct {
	manifests map[string]*ocispec.Manifest
	bases     map[string][]string
	// satisfied are commits already present locally: nothing to fetch, and
	// their bases are not walked either.
	satisfied map[string]bool
}

// resolvePackGraph walks the pack-base graph breadth-first from the requested
// commits, fetching each manifest exactly once.
//
// Each level is fetched concurrently; the level after it is whatever new bases
// that level named. Nothing is imported here.
//
// skipLocal stops the walk at commits the local repository already holds. That
// is what importing wants — a commit already on disk needs neither its pack nor
// the bases behind it — but it is an assumption about what "present" implies,
// and a partial clone breaks it: the commit is there and its blobs are not. A
// caller that has to produce the objects themselves, rather than merely ensure
// the commit is reachable, must pass false.
func (h *Helper) resolvePackGraph(ctx context.Context, specs []fetchSpec, skipLocal bool) (*packGraph, error) {
	defer h.timer.phase("resolve pack graph")()

	g := &packGraph{
		manifests: make(map[string]*ocispec.Manifest),
		bases:     make(map[string][]string),
		satisfied: make(map[string]bool),
	}

	// refFor only matters for the commits git named: a commit is addressable by
	// id only if it was the tip of some push, so a requested ref may resolve
	// when its commit id does not. Discovered bases are always tips by
	// construction and need no fallback.
	refFor := make(map[string]string, len(specs))
	frontier := make([]string, 0, len(specs))
	for _, s := range specs {
		if _, seen := refFor[s.sha]; seen {
			continue
		}
		refFor[s.sha] = s.ref
		frontier = append(frontier, s.sha)
	}

	seen := make(map[string]bool, len(frontier))
	for _, sha := range frontier {
		seen[sha] = true
	}

	// namedBy records which commit declared a base, so a base that cannot be
	// fetched is reported as the broken dependency it is rather than as an
	// unexplained missing manifest.
	namedBy := make(map[string]string)

	// Pack bases form a chain -- each push cut against the one before it -- so
	// discovering them by reading one manifest's annotations at a time is a
	// strictly sequential walk: the loop below cannot ask for the next link
	// until the current one arrives, and there is never more than one link in
	// flight. That makes a clone cost one round trip per push since the last
	// gc, before any packfile moves.
	//
	// The published chain (oci.MediaTypePackChain) is the same graph in one
	// blob, on a manifest every operation reads anyway. Expanding the frontier
	// with it up front turns the walk into a single parallel wave.
	//
	// It is a hint and nothing more. Whatever it says, the loop below still
	// reads each manifest's real pack-bases annotation and queues anything the
	// chain did not mention -- so a chain that is stale, truncated, or absent
	// costs round trips and never correctness. That matters: believing an
	// incomplete chain would mean skipping a packfile and producing a
	// repository quietly missing objects.
	if chain, ok := h.ociClient.FetchPackChain(ctx); ok {
		for i := 0; i < len(frontier); i++ {
			sha := frontier[i]
			// Mirrors the local-store shortcut in the loop: a commit already
			// present needs neither its pack nor its bases, so expanding
			// through it would queue lookups for history nobody will fetch.
			if skipLocal {
				if _, err := h.gitRepo.GetCommitInfo(plumbing.NewHash(sha)); err == nil {
					continue
				}
			}
			for _, base := range chain[sha] {
				if seen[base] {
					continue
				}
				if len(seen) > maxPackGraphNodes {
					break
				}
				seen[base] = true
				namedBy[base] = sha
				frontier = append(frontier, base)
			}
		}
		if len(frontier) > len(specs) {
			h.logVerbose("git-remote-oci: [verbose] the published pack chain resolved %d manifests in one round\n",
				len(frontier))
		}
	}

	for len(frontier) > 0 {
		if len(seen) > maxPackGraphNodes {
			return nil, fmt.Errorf("the pack-base graph exceeds %d manifests, which is more than this can be expected to fetch; run `git-remote-oci gc` against the remote", maxPackGraphNodes)
		}

		type resolved struct {
			sha       string
			manifest  *ocispec.Manifest
			bases     []string
			satisfied bool
		}
		out := make([]resolved, len(frontier))

		lvl, lvlCtx := errgroup.WithContext(ctx)
		lvl.SetLimit(h.transferWorkers)
		for i, sha := range frontier {
			idx, s := i, sha
			lvl.Go(func() error {
				// Already in the local object store: neither its pack nor
				// anything it was cut against is needed.
				if skipLocal {
					if _, err := h.gitRepo.GetCommitInfo(plumbing.NewHash(s)); err == nil {
						out[idx] = resolved{sha: s, satisfied: true}
						return nil
					}
				}
				manifest, err := h.resolveCommitManifest(lvlCtx, s, refFor[s])
				if err != nil {
					if parent, ok := namedBy[s]; ok {
						return fmt.Errorf("commit %s was packed against %s, which could not be fetched: %w",
							shortSHA(parent), shortSHA(s), err)
					}
					return err
				}
				bases, err := oci.ParsePackBases(manifest.Annotations)
				if err != nil {
					return fmt.Errorf("commit %s: %w", s, err)
				}
				out[idx] = resolved{sha: s, manifest: manifest, bases: bases}
				return nil
			})
		}
		if err := lvl.Wait(); err != nil {
			return nil, err
		}

		next := make([]string, 0)
		for _, r := range out {
			if r.satisfied {
				g.satisfied[r.sha] = true
				continue
			}
			g.manifests[r.sha] = r.manifest
			g.bases[r.sha] = r.bases
			for _, b := range r.bases {
				if !seen[b] {
					seen[b] = true
					namedBy[b] = r.sha
					next = append(next, b)
				}
			}
		}
		frontier = next
	}

	return g, nil
}

// resolveCommitManifest finds a commit's manifest, falling back to the ref.
func (h *Helper) resolveCommitManifest(ctx context.Context, sha, ref string) (*ocispec.Manifest, error) {
	manifest, err := h.ociClient.FetchManifest(ctx, sha)
	if err == nil {
		return manifest, nil
	}
	if ref != "" {
		if m, refErr := h.fetchManifestByRef(ctx, ref); refErr == nil {
			return m, nil
		}
	}
	for remoteRef, refSHA := range h.snapshotRemoteRefs() {
		if refSHA != sha {
			continue
		}
		if m, refErr := h.fetchManifestByRef(ctx, remoteRef); refErr == nil {
			return m, nil
		}
	}
	return nil, fmt.Errorf("failed to fetch manifest for commit %s: %w", sha, err)
}

// importOrder returns the graph's commits with every base ahead of the commits
// cut against it, grouped into levels that may be imported concurrently.
//
// A commit left over when nothing more can be ordered is part of a cycle, which
// is the condition that used to hang the fetch instead of failing it.
func (g *packGraph) importOrder() ([][]string, error) {
	remaining := make(map[string][]string, len(g.bases))
	dependents := make(map[string][]string, len(g.bases))
	for sha, bases := range g.bases {
		pending := make([]string, 0, len(bases))
		for _, b := range bases {
			// A base already in the local store imposes no ordering.
			if g.satisfied[b] {
				continue
			}
			pending = append(pending, b)
			dependents[b] = append(dependents[b], sha)
		}
		remaining[sha] = pending
	}

	var levels [][]string
	for len(remaining) > 0 {
		ready := make([]string, 0)
		for sha, pending := range remaining {
			if len(pending) == 0 {
				ready = append(ready, sha)
			}
		}
		if len(ready) == 0 {
			stuck := make([]string, 0, len(remaining))
			for sha := range remaining {
				stuck = append(stuck, shortSHA(sha))
			}
			sort.Strings(stuck)
			return nil, fmt.Errorf("the pack-base graph contains a cycle involving %s; "+
				"a packfile cannot be cut against itself, so this repository's metadata is corrupt",
				strings.Join(stuck, ", "))
		}
		// Deterministic order within a level, so a failure reproduces.
		sort.Strings(ready)
		for _, sha := range ready {
			delete(remaining, sha)
			for _, dep := range dependents[sha] {
				pending := remaining[dep]
				for i, p := range pending {
					if p == sha {
						remaining[dep] = append(pending[:i], pending[i+1:]...)
						break
					}
				}
			}
		}
		levels = append(levels, ready)
	}
	return levels, nil
}

func (h *Helper) handleFetchBatch(ctx context.Context, specs []fetchSpec) error {
	kept := &keptPacks{}

	// Open the repository once, up front. The workers below run concurrently
	// and go-git's storage is not safe for concurrent initialisation.
	if err := h.ensureGitRepo(); err != nil {
		return err
	}

	for i := range specs {
		if specs[i].sha == "" {
			if remoteSHA, exists := h.lookupRemoteRef(specs[i].ref); exists {
				specs[i].sha = remoteSHA
			} else {
				if desc, err := h.ociClient.ResolveRefManifest(ctx, specs[i].ref); err == nil {
					if manifest, mErr := h.ociClient.FetchManifest(ctx, desc.Digest.String()); mErr == nil {
						if commitSHA := manifest.Annotations[ocispec.AnnotationRevision]; commitSHA != "" {
							specs[i].sha = commitSHA
						}
					}
				}
			}
		}
	}

	// A spec whose SHA could not be resolved would otherwise be fetched as the
	// zero hash, producing a confusing "manifest not found" far from the cause.
	for _, spec := range specs {
		if spec.sha == "" {
			return fmt.Errorf("cannot resolve fetch request for ref %q to a commit", spec.ref)
		}
	}

	// A depth-1 clone wants the tip's complete tree and nothing else, which is
	// exactly what a snapshot layer holds. Taking it skips the pack-base graph
	// entirely: the incremental packs are the history, and history is what was
	// asked to be left out.
	// Deliberately not gated on shallowSnapshot: that key decides whether this
	// client *publishes* snapshots, and a fresh clone has no configuration to
	// consult anyway. Using one that exists is free and always better.
	if h.depth == 1 {
		done, err := h.fetchFromSnapshots(ctx, specs, kept)
		if err != nil {
			return err
		}
		if done {
			for _, spec := range specs {
				if err := h.recordShallowBoundary(spec.sha); err != nil {
					h.logWarn("git-remote-oci: warning: %v\n", err)
				}
			}
			h.reportKeptPacks(kept)
			return nil
		}
	}

	// Resolve the whole pack-base graph before importing anything, then import
	// in dependency order. A base's objects must be in place before a pack cut
	// against it is imported, and a topological order is how that is guaranteed
	// without one goroutine waiting on another.
	graph, err := h.resolvePackGraph(ctx, specs, true)
	if err != nil {
		return err
	}
	levels, err := graph.importOrder()
	if err != nil {
		return err
	}
	for _, level := range levels {
		lvl, lvlCtx := errgroup.WithContext(ctx)
		lvl.SetLimit(h.transferWorkers)
		for _, sha := range level {
			s := sha
			lvl.Go(func() error {
				return h.importCommitArtifacts(lvlCtx, s, graph.manifests[s], kept)
			})
		}
		if err := lvl.Wait(); err != nil {
			return err
		}
	}

	// The boundary is written after everything has been imported, from the
	// real commit graph. Marking it during the walk measured *push
	// generations* rather than commits, so any depth above 1 truncated at the
	// wrong place.
	if h.depth > 0 {
		for _, spec := range specs {
			if err := h.recordShallowBoundary(spec.sha); err != nil {
				h.logWarn("git-remote-oci: warning: %v\n", err)
			}
		}
	}

	h.reportKeptPacks(kept)
	return nil
}

// reportKeptPacks hands git one .keep file to hold and removes the rest.
//
// Git records only the first "lock" line of a batch, so reporting all of them
// produces a warning per extra line and silently leaks every .keep but the
// first. A leaked .keep pins its pack against gc and repack indefinitely.
func (h *Helper) reportKeptPacks(kept *keptPacks) {
	paths := kept.all()
	if len(paths) == 0 {
		return
	}

	// Deterministic choice, so repeated runs behave the same way.
	sort.Strings(paths)
	h.printfOut("lock %s\n", paths[0])

	for _, path := range paths[1:] {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			h.logWarn("git-remote-oci: warning: failed to remove %s: %v\n", path, err)
		}
	}
}

// markShallowBoundary records sha as a shallow boundary in $GIT_DIR/shallow.
//
// Fetch workers run concurrently, so the read-modify-write is serialised under
// shallowMu and the result is written via a temporary file and a rename, rather
// than truncating the real file in place.
func (h *Helper) markShallowBoundary(sha string) error {
	h.shallowMu.Lock()
	defer h.shallowMu.Unlock()

	// The git directory, verbatim - not with ".git" appended when the basename
	// is not already ".git". That is what this used to do, and in a bare
	// repository, the shape every clone target has, it wrote the boundary into
	// a directory that does not exist and the write simply failed.
	//
	// The common directory, not GIT_DIR: in a linked worktree GIT_DIR is the
	// per-worktree directory and git reads the shallow file from the shared one.
	gitDir, _ := git.CommonDir()
	shallowPath := filepath.Join(gitDir, "shallow")

	content, err := os.ReadFile(shallowPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	// Compare whole lines: a substring match would treat an abbreviated or
	// unrelated id containing this one as already present.
	for line := range strings.SplitSeq(string(content), "\n") {
		if strings.TrimSpace(line) == sha {
			return nil
		}
	}

	updated := content
	if len(updated) > 0 && !strings.HasSuffix(string(updated), "\n") {
		updated = append(updated, '\n')
	}
	updated = append(updated, []byte(sha+"\n")...)

	tmp, err := os.CreateTemp(gitDir, "shallow.tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(updated); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, 0644); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, shallowPath)
}

// fetchManifestByRef resolves a ref to its manifest, trying every tag the ref
// could be published under.
func (h *Helper) fetchManifestByRef(ctx context.Context, refName string) (*ocispec.Manifest, error) {
	desc, err := h.ociClient.ResolveRefManifest(ctx, refName)
	if err != nil {
		return nil, err
	}
	return h.ociClient.FetchManifest(ctx, desc.Digest.String())
}

// packBases returns the commits srcHash's packfile may be cut against.
//
// Excluding objects from a packfile is only safe if a fetcher can get them from
// somewhere else, which means every base has to satisfy three things at once:
//
//   - a remote ref points at it, so it is published rather than merely local;
//   - it is an ancestor of srcHash, so the walk back from srcHash reaches it.
//     This is what makes a force push safe: after an amend the old tip is not an
//     ancestor, so it is not a base and its objects stay in the pack;
//   - the registry really serves a manifest for it, because the ref index can
//     name a commit whose manifest was never written or has since been pruned.
//
// A registry error is returned rather than skipped. Dropping a base on error
// would be the safe direction for correctness, but it silently turns an
// incremental push into a full one, and an unreachable registry is worth
// reporting rather than papering over.
//
// ancestors is remoteAncestors' answer for srcHash: the walk is done once per
// pushed ref and shared with the fast-forward check, where it used to be one
// walk per remote ref here and another there. A nil set -- the walk failed --
// selects no bases, which is what a failed walk per candidate used to do.
func (h *Helper) packBases(ctx context.Context, srcHash plumbing.Hash, remoteSnapshot map[string]string, ancestors map[plumbing.Hash]bool) ([]plumbing.Hash, error) {
	if os.Getenv("GIT_REMOTE_OCI_FULL_PACK") != "" {
		return nil, nil
	}

	commitSHA := srcHash.String()
	seen := make(map[plumbing.Hash]bool)
	candidates := make([]string, 0, len(remoteSnapshot))
	for _, remoteSHA := range remoteSnapshot {
		if remoteSHA == "" || remoteSHA == commitSHA || !oci.IsCommitID(remoteSHA) {
			continue
		}
		rHash := plumbing.NewHash(remoteSHA)
		if seen[rHash] {
			continue
		}
		seen[rHash] = true

		if !ancestors[rHash] {
			continue
		}
		candidates = append(candidates, remoteSHA)
	}

	// Deterministic, so the same push produces the same manifest twice running.
	sort.Strings(candidates)

	bases := make([]plumbing.Hash, 0, len(candidates))
	for _, base := range candidates {
		exists, err := h.ociClient.CommitManifestExists(ctx, base)
		if err != nil {
			return nil, fmt.Errorf("could not confirm the registry serves commit %s: %w", shortSHA(base), err)
		}
		if !exists {
			h.logVerbose("git-remote-oci: [verbose] not packing against %s: the registry has no manifest for it\n", shortSHA(base))
			continue
		}
		bases = append(bases, plumbing.NewHash(base))
	}
	return bases, nil
}

// remoteAncestors walks srcHash's history once and reports which remote refs
// it reaches: the input to both the fast-forward check and packBases.
//
// Every published commit id is a candidate, whether or not it will end up as
// a base, so the one walk answers every question a push asks about the remote
// state. It stops when the last candidate is found; a repository with
// branches the tip does not descend from costs the walk once, where it used
// to cost it once per such branch.
func (h *Helper) remoteAncestors(srcHash plumbing.Hash, remoteSnapshot map[string]string) (map[plumbing.Hash]bool, error) {
	commitSHA := srcHash.String()
	seen := make(map[plumbing.Hash]bool, len(remoteSnapshot))
	candidates := make([]plumbing.Hash, 0, len(remoteSnapshot))
	for _, remoteSHA := range remoteSnapshot {
		if remoteSHA == "" || remoteSHA == commitSHA || !oci.IsCommitID(remoteSHA) {
			continue
		}
		rHash := plumbing.NewHash(remoteSHA)
		if seen[rHash] {
			continue
		}
		seen[rHash] = true
		candidates = append(candidates, rHash)
	}
	return h.gitRepo.AncestorsAmong(srcHash, candidates)
}

// packBaseStrings renders bases for the manifest annotation.
func packBaseStrings(bases []plumbing.Hash) []string {
	out := make([]string, len(bases))
	for i, b := range bases {
		out[i] = b.String()
	}
	return out
}

// ensureGitRepo opens the local repository exactly once.
//
// It must be called before any concurrent worker runs: lazily initialising
// h.gitRepo from inside an errgroup races on the pointer, and can also open the
// same repository several times.
func (h *Helper) ensureGitRepo() error {
	if h.gitRepo != nil {
		return nil
	}
	repo, err := git.OpenRepository()
	if err != nil {
		return fmt.Errorf("failed to open local git repository: %w", err)
	}
	h.gitRepo = repo
	return nil
}

// uploadLFSObjects uploads the LFS objects the pushed commit range references.
//
// The ordinary and the --atomic push path both need this, and they used to
// carry two copies of it that had drifted: the atomic one discarded the scan
// error, discarded a failure to open the local object, discarded a failed
// upload, and then discarded the group's result as well. The effect was that
// `git push --atomic` published a ref whose LFS layers were never uploaded, and
// a later clone produced dangling pointers with nothing having reported a
// problem. One implementation, so the two cannot disagree about it again.
//
// The distinction that matters is between an object that is not here and an
// object that would not go:
//
//   - not in the local LFS store is normal for a partial checkout. It is a
//     warning, and the push carries on without that blob.
//   - a failed upload means the ref about to be published would reference a
//     blob the registry does not have. That fails the ref.
//
// objects is the range's object list from git.Repository.RevList, computed
// once by the caller and shared with the pack index and the packing fallback;
// this used to be one of three walks over the same range on every push.
func (h *Helper) uploadLFSObjects(ctx context.Context, objects []plumbing.Hash, label string) ([]ocispec.Descriptor, error) {
	defer h.timer.phase("upload LFS objects")()

	pointers, err := h.gitRepo.ScanLFSPointersIn(objects)
	if err != nil {
		// Discarding this silently pushed a ref with no LFS objects at all and
		// said nothing about why.
		return nil, fmt.Errorf("failed to scan for Git LFS pointers: %w", err)
	}
	if len(pointers) == 0 {
		return nil, nil
	}

	// git-lfs keeps its objects under the common directory, which in a linked
	// worktree is not GIT_DIR.
	gitDir, _ := git.CommonDir()

	var (
		mu    sync.Mutex
		descs []ocispec.Descriptor
	)
	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(h.transferWorkers)

	for _, p := range pointers {
		ptr := p
		g.Go(func() error {
			lfsPath := lfs.GetLFSObjectPath(gitDir, ptr.Oid)
			if lfsPath == "" {
				return fmt.Errorf("LFS pointer for %s has an invalid OID", shortSHA(ptr.Oid))
			}
			lfsFile, err := os.Open(lfsPath)
			if err != nil {
				h.logWarn("git-remote-oci:%s warning: LFS object %s is not available locally, not uploading it\n", label, shortSHA(ptr.Oid))
				return nil
			}
			defer func() { _ = lfsFile.Close() }()

			h.logInfo("git-remote-oci:%s uploading Git LFS blob %s (%d bytes)...\n", label, shortSHA(ptr.Oid), ptr.Size)
			desc, pushErr := h.ociClient.PushLFSLayer(gCtx, ptr.Oid, lfsFile, ptr.Size)
			if pushErr != nil {
				return fmt.Errorf("failed to upload LFS blob %s: %w", shortSHA(ptr.Oid), pushErr)
			}
			mu.Lock()
			descs = append(descs, desc)
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return descs, nil
}

// fetchFromSnapshots imports a snapshot layer for every requested ref.
//
// It reports false, and imports nothing, unless *every* spec has one: a batch
// that took the shortcut for some refs and the full history for others would
// leave a repository shallow in one place and complete in another, which is
// harder to reason about than simply not taking the shortcut.
func (h *Helper) fetchFromSnapshots(ctx context.Context, specs []fetchSpec, kept *keptPacks) (bool, error) {
	type found struct {
		sha  string
		desc ocispec.Descriptor
	}
	snapshots := make([]found, 0, len(specs))

	for _, spec := range specs {
		manifest, err := h.resolveCommitManifest(ctx, spec.sha, spec.ref)
		if err != nil {
			// Not a reason to fail here. The full walk below resolves the same
			// manifest and will report the problem with the context of which
			// base wanted it, which is a better error than this one.
			h.logVerbose("git-remote-oci: [verbose] no snapshot lookup for %s: %v\n",
				shortSHA(spec.sha), err)
			return false, nil
		}
		desc, ok := oci.SnapshotLayer(manifest)
		if !ok {
			h.logVerbose("git-remote-oci: [verbose] %s has no shallow snapshot; fetching its history\n",
				shortSHA(spec.sha))
			return false, nil
		}
		snapshots = append(snapshots, found{sha: spec.sha, desc: desc})
	}

	for _, s := range snapshots {
		if _, err := h.gitRepo.GetCommitInfo(plumbing.NewHash(s.sha)); err == nil {
			continue
		}
		h.logInfo("git-remote-oci: fetching shallow snapshot for %s...\n", shortSHA(s.sha))
		rc, err := h.ociClient.FetchSnapshotStream(ctx, s.desc)
		if err != nil {
			return false, fmt.Errorf("failed to fetch the shallow snapshot for %s: %w", shortSHA(s.sha), err)
		}
		lockPath, importErr := h.gitRepo.ImportPackfile(&progressReader{
			r:      rc,
			action: "Receiving snapshot",
			ref:    shortSHA(s.sha),
			helper: h,
		})
		_ = rc.Close()
		if importErr != nil {
			return false, fmt.Errorf("failed to import the shallow snapshot for %s: %w", shortSHA(s.sha), importErr)
		}
		if lockPath != "" {
			kept.add(lockPath)
		}
	}
	return true, nil
}

// snapshotLayer builds and uploads a self-contained snapshot of a ref tip.
//
// It returns the zero descriptor and no error when there is nothing to publish:
// the snapshot is an optimisation for shallow clones, and a push that cannot
// produce one is still a correct push. Failing here would trade a working push
// for a faster clone, which is the wrong way round.
func (h *Helper) snapshotLayer(ctx context.Context, commitSHA string, tip plumbing.Hash) (ocispec.Descriptor, bool) {
	if !h.shallowSnapshot {
		return ocispec.Descriptor{}, false
	}

	pr, pw := io.Pipe()
	go func() {
		_ = pw.CloseWithError(h.gitRepo.CreateSnapshotPackfileTo(pw, tip))
	}()

	desc, err := h.ociClient.PushSnapshotLayer(ctx, commitSHA, pr, 0)
	_ = pr.Close()
	if err != nil {
		h.logWarn("git-remote-oci: warning: could not publish a shallow snapshot for %s: %v\n",
			shortSHA(commitSHA), err)
		return ocispec.Descriptor{}, false
	}
	h.logVerbose("git-remote-oci: [verbose] published a shallow snapshot for %s (%d bytes)\n",
		shortSHA(commitSHA), desc.Size)
	return desc, true
}

// packIndexLayer publishes the list of objects the packfile about to be pushed
// contains, so a reader can tell whether it is worth downloading.
//
// The list is derived from the same revision range the packfile was cut from
// rather than read out of a real .idx, because there is no .idx to read: the
// pushed pack is thin, and a thin pack cannot be indexed on its own — the whole
// point of it is that it references bases it does not carry. The range's
// object list is the caller's, from git.Repository.RevList, so the index and
// the LFS scan describe the same objects without either walking again.
//
// Like snapshotLayer, a failure here is not a failed push. The index only ever
// saves a download; a push that publishes none is correct, just less kind to
// whoever clones it.
func (h *Helper) packIndexLayer(ctx context.Context, wantHash plumbing.Hash, objects []plumbing.Hash) (ocispec.Descriptor, bool) {
	defer h.timer.phase("build pack index")()

	packed := h.gitRepo.PackedObjectsOf(objects)
	entries := make([]oci.PackIndexEntry, 0, len(packed))
	for _, o := range packed {
		entries = append(entries, oci.PackIndexEntry{OID: o.OID, Size: o.Size})
	}

	desc, err := h.ociClient.PushPackIndex(ctx, entries)
	if err != nil {
		h.logWarn("git-remote-oci: warning: could not publish the pack index for %s: %v\n",
			shortSHA(wantHash.String()), err)
		return ocispec.Descriptor{}, false
	}
	if desc.Digest == "" {
		return ocispec.Descriptor{}, false
	}
	h.logVerbose("git-remote-oci: [verbose] published a pack index for %s (%d objects)\n",
		shortSHA(wantHash.String()), len(entries))
	return desc, true
}

// recordShallowBoundary truncates tip's history at the requested depth.
//
// This only limits what git *shows*; the objects were all transferred. See the
// shallow-clone row in the README for why a registry cannot do better.
func (h *Helper) recordShallowBoundary(tip string) error {
	boundary, err := h.gitRepo.ShallowBoundary(tip, h.depth)
	if err != nil {
		return fmt.Errorf("failed to compute the shallow boundary for %s: %w", shortSHA(tip), err)
	}
	for _, commit := range boundary {
		if err := h.markShallowBoundary(commit); err != nil {
			return fmt.Errorf("failed to record shallow boundary %s: %w", shortSHA(commit), err)
		}
	}
	return nil
}

// importCommitArtifacts imports one manifest's packfile and LFS layers.
func (h *Helper) importCommitArtifacts(ctx context.Context, sha string, manifest *ocispec.Manifest, kept *keptPacks) error {
	defer h.timer.phase("import packfiles")()

	packStream, err := h.ociClient.FetchPackfileStream(ctx, manifest)
	if err != nil {
		return fmt.Errorf("failed to fetch packfile layer for commit %s: %w", sha, err)
	}

	abbrev := shortSHA(sha)
	rStream := &progressReader{
		r:      packStream,
		action: "Receiving packfile",
		ref:    abbrev,
		helper: h,
	}

	lockPath, err := h.gitRepo.ImportPackfile(rStream)
	_ = packStream.Close()
	if err != nil {
		h.logWarn("git-remote-oci: ImportPackfile failed for commit %s: %v\n", sha, err)
		return fmt.Errorf("failed to import packfile for commit %s: %w", sha, err)
	}
	if lockPath != "" {
		// Collect rather than emit. Git keeps only the first "lock" line of a
		// fetch batch and warns about the rest ("... also locked ..."), so a
		// batch that imports one pack per commit would leak every .keep file
		// but the first. handleFetchBatch reports one and cleans up the others.
		kept.add(lockPath)
	}
	h.logInfo("git-remote-oci: successfully imported packfile for commit %s\n", abbrev)

	return h.downloadLFSObjects(ctx, sha, manifest, h.filter)
}

// downloadLFSObjects stores the manifest's Git LFS blobs into the repository.
//
// Both fetch paths need this and for the same reason: the packfile carries the
// *pointer*, a hundred bytes of text naming an object stored beside it, and a
// working tree checked out without that object holds the pointer instead of the
// file. There is no LFS server behind an oci:// remote to fetch it from later,
// so a fetch that skips this produces a clone that looks complete, passes fsck,
// and is wrong in the only way that matters.
//
// filter is the client's object filter. It is passed rather than read from the
// helper because the two paths learn it differently — one from `option filter`,
// the other from a protocol-v2 fetch argument.
func (h *Helper) downloadLFSObjects(ctx context.Context, sha string, manifest *ocispec.Manifest, filter string) error {
	if manifest == nil {
		return nil
	}
	// The common directory, as in uploadLFSObjects: git-lfs looks there.
	gitDir, _ := git.CommonDir()

	var lfsLayers []ocispec.Descriptor
	for _, layer := range manifest.Layers {
		if layer.MediaType != lfs.MediaTypeGitLFSBlob && layer.Annotations[lfs.AnnotationLFSOID] == "" {
			continue
		}
		lfsOID := layer.Annotations[lfs.AnnotationLFSOID]
		if lfsOID == "" {
			continue
		}
		// The OID comes from a registry-supplied annotation and is used to
		// build a local path, so reject anything malformed before it gets
		// anywhere near the filesystem.
		if _, oidErr := lfs.ValidateOID(lfsOID); oidErr != nil {
			return fmt.Errorf("manifest for commit %s carries an invalid LFS OID: %w", sha, oidErr)
		}
		if filter == "blob:none" {
			h.logInfo("git-remote-oci: filter %s active, skipping automatic LFS blob download for %s\n", filter, shortSHA(lfsOID))
			continue
		}
		if strings.HasPrefix(filter, "blob:limit=") {
			limitBytes, limitErr := parseBlobLimit(strings.TrimPrefix(filter, "blob:limit="))
			if limitErr != nil {
				return fmt.Errorf("cannot apply filter %q: %w", filter, limitErr)
			}
			// Compare the object's own size, which is what the user asked
			// about. layer.Size is the compressed blob, and would let a large
			// object slip past a limit it exceeds uncompressed.
			objectSize := layer.Size
			if declared := layer.Annotations[lfs.AnnotationLFSSize]; declared != "" {
				if parsed, err := strconv.ParseInt(declared, 10, 64); err == nil && parsed >= 0 {
					objectSize = parsed
				}
			}
			if objectSize > limitBytes {
				h.logInfo("git-remote-oci: filter %s active, skipping LFS blob %s (%d bytes > %d bytes)\n", filter, shortSHA(lfsOID), objectSize, limitBytes)
				continue
			}
		}
		lfsLayers = append(lfsLayers, layer)
	}

	if len(lfsLayers) == 0 {
		return nil
	}

	gLFS, gLFSCtx := errgroup.WithContext(ctx)
	gLFS.SetLimit(h.transferWorkers)

	for _, layer := range lfsLayers {
		lLayer := layer
		gLFS.Go(func() error {
			lfsOID := lLayer.Annotations[lfs.AnnotationLFSOID]
			lfsRc, err := h.ociClient.FetchLFSLayer(gLFSCtx, lLayer)
			if err != nil {
				return fmt.Errorf("failed to fetch LFS blob %s: %w", shortSHA(lfsOID), err)
			}
			defer func() { _ = lfsRc.Close() }()

			h.logInfo("git-remote-oci: downloading Git LFS blob %s...\n", shortSHA(lfsOID))
			// A failure here means the working tree would be left with an
			// LFS pointer and no object behind it, so report it instead of
			// completing the fetch as if it had worked.
			if err := lfs.StoreLFSObject(gitDir, lfsOID, lfsRc); err != nil {
				return fmt.Errorf("failed to store LFS blob %s: %w", shortSHA(lfsOID), err)
			}
			return nil
		})
	}
	return gLFS.Wait()
}

func (h *Helper) handlePushBatch(ctx context.Context, pushSpecs []string) error {
	// Open once, before any worker starts; see ensureGitRepo.
	if err := h.ensureGitRepo(); err != nil {
		return err
	}

	// The fast-forward and CAS checks below are only meaningful if we actually
	// know what the remote holds. Test remoteRefsKnown, not the map: an empty
	// map set by a failed listing would otherwise make every ref look new and
	// silently turn this push into a force-push.
	if !h.remoteRefsKnown {
		richRefs, err := h.discoverRemoteRefs(ctx)
		if err != nil {
			return err
		}
		h.setRemoteRefs(richRefs)
	}

	if h.followTags {
		var pushedHashes []plumbing.Hash
		for _, spec := range pushSpecs {
			srcRef, _, ok := strings.Cut(strings.TrimPrefix(spec, "+"), ":")
			if ok && srcRef != "" {
				if hash, err := h.gitRepo.ResolveRef(srcRef); err == nil {
					pushedHashes = append(pushedHashes, hash)
				}
			}
		}

		tags, err := h.gitRepo.GetReachableTags(pushedHashes)
		if err != nil {
			// The branches still go: a tag walk that failed is no reason to
			// refuse the push the user asked for. But pushing no tags without
			// a word is the omission --follow-tags exists to prevent, so it
			// is said out loud rather than passed over.
			h.logWarn("git-remote-oci: warning: --follow-tags could not enumerate the reachable tags, so none were added: %v\n", err)
		}
		if len(tags) > 0 {
			pushedDstRefs := make(map[string]bool)
			for _, spec := range pushSpecs {
				_, dstRef, ok := strings.Cut(strings.TrimPrefix(spec, "+"), ":")
				if ok {
					pushedDstRefs[dstRef] = true
				}
			}

			for _, tag := range tags {
				if !pushedDstRefs[tag.Name] {
					h.logInfo("git-remote-oci: --follow-tags auto-discovered tag %s\n", tag.Name)
					pushSpecs = append(pushSpecs, fmt.Sprintf("%s:%s", tag.Name, tag.Name))
					pushedDstRefs[tag.Name] = true
				}
			}
		}
	}

	if h.atomic {
		h.finishPushBatch(ctx, pushSpecs, h.handlePushBatchAtomic(ctx, pushSpecs))
		return nil
	}

	// changed holds what this batch actually moved, and is all the index
	// update is told about; see the comment above that call. deletedRefs is
	// its counterpart for removals. Both are written by the workers.
	changed := make(map[string]oci.RefEntry)
	deletedRefs := make(map[string]bool)
	var changedMu sync.Mutex

	// pushed records a ref this batch moved: in the session cache, for the
	// checks the other specs in the batch make, and in changed, for the index.
	pushed := func(dstRef string, entry oci.RefEntry) {
		h.recordRemoteRef(dstRef, entry)
		changedMu.Lock()
		changed[dstRef] = entry
		changedMu.Unlock()
	}

	processSpec := func(pCtx context.Context, spec string) pushReport {
		force, srcRef, dstRef := h.splitPushSpec(spec)

		if srcRef == "" {
			// Deletion ran before any dry-run check, so `git push --dry-run
			// origin :branch` really deleted the branch. The atomic path already
			// handled this correctly; this one did not.
			if h.dryRun {
				h.logInfo("git-remote-oci: (dry-run) verified deletion of %s\n", dstRef)
				return okReport(dstRef)
			}

			if err := h.ociClient.DeleteRef(pCtx, dstRef); err != nil {
				return failReport(dstRef, "failed to delete remote ref: %v", err)
			}
			// Recorded only once the registry has agreed. Marking the ref
			// deleted before asking let a refused deletion drop it from the
			// index anyway, so the listing said it was gone while its tag
			// stayed exactly where it was.
			h.forgetRemoteRef(dstRef)
			changedMu.Lock()
			deletedRefs[dstRef] = true
			changedMu.Unlock()
			return okReport(dstRef)
		}

		if !h.dryRun {
			// Take the lock rather than merely testing it. Only the --atomic
			// path used to acquire, so two ordinary `git push` runs both saw an
			// unlocked ref and both proceeded, and the lock constrained nobody
			// except other --atomic pushers.
			if _, lockErr := h.ociClient.AcquireRefLock(pCtx, dstRef, h.pushLockTTL); lockErr != nil {
				if errors.Is(lockErr, oci.ErrRefLocked) {
					return failReport(dstRef, "%v", lockErr)
				}
				return failReport(dstRef, "failed to acquire reference lock: %v", lockErr)
			}
			// Release even when the push failed; see releaseRefLock.
			defer h.releaseRefLock(pCtx, dstRef)
		}

		srcHash, err := h.gitRepo.ResolveRef(srcRef)
		if err != nil {
			return failReport(dstRef, "failed to resolve local ref %s: %v", srcRef, err)
		}
		commitSHA := srcHash.String()

		if expectedSHA, hasCAS := h.casRefs[dstRef]; hasCAS {
			existingSHA, exists := h.lookupRemoteRef(dstRef)
			if expectedSHA != "" {
				if !exists || existingSHA != expectedSHA {
					return failReport(dstRef, "stale info; --force-with-lease expected %s, but remote is %s", expectedSHA, existingSHA)
				}
			} else if exists && existingSHA != "" {
				return failReport(dstRef, "stale info; --force-with-lease expected ref to not exist, but remote is %s", existingSHA)
			}
		}

		// Snapshot rather than hold refsMu: packBases walks history, and
		// keeping the lock across that would serialise every worker.
		remoteSnapshot := h.snapshotRemoteRefs()
		existingSHA, exists := remoteSnapshot[dstRef]

		// One walk answers both the fast-forward check and pack-base
		// selection below.
		ancestors, ancestorErr := h.remoteAncestors(srcHash, remoteSnapshot)

		if exists && existingSHA != commitSHA && !force {
			if ancestorErr != nil {
				return failReport(dstRef, "non-fast-forward update rejected (use '+' to force): remote is %s", shortSHA(existingSHA))
			}
			if !ancestors[plumbing.NewHash(existingSHA)] {
				return failReport(dstRef, "non-fast-forward update rejected (use '+' to force)")
			}
		}

		haveHashes, err := h.packBases(pCtx, srcHash, remoteSnapshot, ancestors)
		if err != nil {
			return failReport(dstRef, "%v", err)
		}

		commit, err := h.gitRepo.GetCommitInfo(srcHash)
		if err != nil {
			commit = nil
		}
		parentsStr := parentsOf(commit)

		refTag := oci.EncodeRefTag(dstRef)
		if refTag == "" {
			return failReport(dstRef, "destination ref %q cannot be represented as an OCI tag", dstRef)
		}

		tagInfo, tagAnnoMap, wantHash := h.tagAnnotations(srcRef, srcHash)

		if h.dryRun {
			pr, pw := io.Pipe()
			go func() {
				err := h.gitRepo.CreatePackfileTo(pw, wantHash, haveHashes)
				_ = pw.CloseWithError(err)
			}()
			_, err := io.Copy(io.Discard, pr)
			_ = pr.Close()
			if err != nil {
				return failReport(dstRef, "dry-run packfile generation failed: %v", err)
			}
			h.logInfo("git-remote-oci: (dry-run) verified push for commit %s to OCI tag %s (%s)\n", commitSHA, refTag, dstRef)
			h.logVerbose("git-remote-oci: [verbose] dry-run packfile verification succeeded for commit %s\n", commitSHA)
			return okReport(dstRef)
		}

		commitAnnotations(tagAnnoMap, dstRef, commit)

		// Skip only when this process has already pushed both the commit
		// manifest and this ref's manifest. Testing the commit alone would
		// leave the ref tag missing from the registry, discoverable only via
		// the _refs index.
		if h.ociClient.IsRefFullyPushed(wantHash.String(), dstRef) {
			pushed(dstRef, refEntryFor(commitSHA, commit, tagInfo))
			return okReport(dstRef)
		}

		// The range's object list, once: the LFS scan, the pack index and the
		// packing fallback all read it, and each used to walk for its own.
		objects, err := h.gitRepo.RevList(wantHash, haveHashes)
		if err != nil {
			return failReport(dstRef, "failed to list the objects the push carries: %v", err)
		}

		pr, pw := io.Pipe()
		go func() {
			err := h.gitRepo.CreatePackfileFromListTo(pw, wantHash, haveHashes, objects)
			_ = pw.CloseWithError(err)
		}()

		var lfsDescs []ocispec.Descriptor
		if descs, lfsErr := h.uploadLFSObjects(pCtx, objects, ""); lfsErr != nil {
			_ = pr.CloseWithError(lfsErr)
			return failReport(dstRef, "%v", lfsErr)
		} else {
			lfsDescs = descs
		}

		// A self-contained snapshot of the tip, so a later --depth 1 clone can
		// fetch it instead of the whole history.
		if snap, ok := h.snapshotLayer(pCtx, commitSHA, wantHash); ok {
			lfsDescs = append(lfsDescs, snap)
		}

		// What is in that packfile, so a lazy fetch can rule it out without
		// downloading it.
		if idx, ok := h.packIndexLayer(pCtx, wantHash, objects); ok {
			lfsDescs = append(lfsDescs, idx)
		}

		forceStr := ""
		if force {
			forceStr = " (force)"
		}
		h.logInfo("git-remote-oci: pushing commit %s to OCI tag %s (%s)%s...\n", commitSHA, refTag, dstRef, forceStr)
		h.logVerbose("git-remote-oci: [verbose] target tag: %s, parents: %q\n", refTag, parentsStr)

		stopPush := h.timer.phase("build and upload packfile")
		err = h.ociClient.PushCommitStream(pCtx, oci.CommitPush{
			CommitSHA:      commitSHA,
			RefName:        dstRef,
			RefTag:         refTag,
			Parents:        parentsStr,
			PackBases:      packBaseStrings(haveHashes),
			TagAnnotations: tagAnnoMap,
			ExtraLayers:    lfsDescs,
		}, pr, 0)
		stopPush()
		if err != nil {
			_ = pr.CloseWithError(err)
			return failReport(dstRef, "%v", err)
		}

		pushed(dstRef, refEntryFor(commitSHA, commit, tagInfo))
		return okReport(dstRef)
	}

	// Results are held rather than written as they happen; see pushReport.
	reports := make([]pushReport, len(pushSpecs))
	if len(pushSpecs) == 1 {
		reports[0] = processSpec(ctx, pushSpecs[0])
	} else {
		g, gCtx := errgroup.WithContext(ctx)
		g.SetLimit(h.blobWorkers)
		for i, spec := range pushSpecs {
			idx, s := i, spec
			g.Go(func() error {
				// Each worker owns one slot, so no lock is needed.
				reports[idx] = processSpec(gCtx, s)
				return nil
			})
		}
		_ = g.Wait()
	}

	// A dry run must leave the registry exactly as it found it, and the index
	// update is a real write: it takes the _refs lock, pushes blobs and pushes a
	// manifest.
	//
	// Only what this batch moved goes into it. The session's ref set is a
	// snapshot taken at `list for-push`, and a ref another client advanced
	// since then is not this push's to report: writing the whole snapshot put
	// that ref back where it was. The index update merges these entries over
	// whatever the registry holds *now*, so everything else is left as it is,
	// and a batch that moved nothing has nothing to write. The snapshot is
	// still what the fast-forward and force-with-lease checks above ran
	// against, which is what it is for.
	if !h.dryRun && (len(changed) > 0 || len(deletedRefs) > 0) {
		if err := h.ociClient.PushRichRefIndexWithHead(ctx, changed, deletedRefs, headHintForRefs(changed)); err != nil {
			// list prefers the _refs index over tag enumeration, so a ref whose
			// index entry did not land is not reliably discoverable and the next
			// push would compare against a stale value. Reporting these as
			// successful is what let that happen silently.
			h.logWarn("git-remote-oci: warning: failed to update _refs index: %v\n", err)
			for i := range reports {
				if reports[i].ok {
					reports[i] = failReport(reports[i].dstRef, "pushed, but the _refs index could not be updated, so the ref is not reliably visible: %v", err)
				}
			}
		}
	}

	h.finishPushBatch(ctx, pushSpecs, reports)
	return nil
}

// finishPushBatch puts a batch's results on the wire and then, once git has
// its answer, considers compacting.
//
// The terminating blank line is written here rather than by flushBatches so
// that compaction really does run after git has been answered. Git reads a
// batch's responses up to that line, and a repack that ran before it was
// written delayed the answer by exactly the work maybeCompact promises not to
// put in front of it -- and could still have changed the outcome, since
// nothing had been committed to the wire yet.
func (h *Helper) finishPushBatch(ctx context.Context, pushSpecs []string, reports []pushReport) {
	pushed := false
	for i, report := range reports {
		if report.line == "" {
			// A spec that produced nothing at all still needs a response, or
			// git waits for a line that never comes.
			report = failReport(pushSpecDst(pushSpecs[i]), "push produced no result")
		}
		pushed = pushed || report.ok
		h.printlnOut(report.line)
	}
	h.printlnOut()

	if pushed {
		h.maybeCompact(ctx)
	}
}

// refLockReleaseTimeout bounds how long releasing a push lock may take. The
// release runs after the push's own context is done with, so without a bound
// of its own a registry that has stopped answering would hold the helper past
// the point where the user gave up on it.
const refLockReleaseTimeout = 10 * time.Second

// releaseRefLock gives a ref's push lock back.
//
// Detached from ctx so that a cancelled push still releases: the alternative
// is holding the ref for the whole TTL over an error that took a moment to
// happen. A release that fails is a warning rather than a failure, because a
// leaked lock expires on its TTL rather than wedging the ref permanently.
func (h *Helper) releaseRefLock(ctx context.Context, dstRef string) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refLockReleaseTimeout)
	defer cancel()
	if err := h.ociClient.ReleaseRefLock(releaseCtx, dstRef); err != nil {
		h.logWarn("git-remote-oci: warning: failed to release the lock on %s: %v\n", dstRef, err)
	}
}

// splitPushSpec parses one push line into the force marker, the source (empty
// for a deletion) and the destination as a full ref name.
func (h *Helper) splitPushSpec(spec string) (force bool, srcRef, dstRef string) {
	force = strings.HasPrefix(spec, "+")
	spec = strings.TrimPrefix(spec, "+")
	srcRef, dstRef, ok := strings.Cut(spec, ":")
	if !ok {
		srcRef, dstRef = spec, spec
	}
	return force, srcRef, h.normaliseDst(srcRef, dstRef)
}

// normaliseDst places a destination git handed over without its refs/ prefix.
//
// An explicit tags/ goes under refs/; a source that is a tag, whether an
// annotated object or merely named under refs/tags/, makes the destination a
// tag too; anything else is a branch.
func (h *Helper) normaliseDst(srcRef, dstRef string) string {
	if strings.HasPrefix(dstRef, "refs/") {
		return dstRef
	}
	if strings.HasPrefix(dstRef, "tags/") {
		return "refs/" + dstRef
	}
	if tagInf, _ := h.gitRepo.GetAnnotatedTagInfo(srcRef); tagInf != nil {
		return "refs/tags/" + dstRef
	}
	if strings.HasPrefix(srcRef, "refs/tags/") {
		return "refs/tags/" + dstRef
	}
	return "refs/heads/" + dstRef
}

// parentsOf renders a commit's parents as the comma-separated list the
// manifest's parents annotation carries. It is metadata; see FORMAT.md for
// why fetch follows pack-bases and not this.
func parentsOf(commit *object.Commit) string {
	if commit == nil || len(commit.ParentHashes) == 0 {
		return ""
	}
	parentSHAs := make([]string, len(commit.ParentHashes))
	for i, p := range commit.ParentHashes {
		parentSHAs[i] = p.String()
	}
	return strings.Join(parentSHAs, ",")
}

// tagAnnotations resolves what srcRef is a push of.
//
// For an annotated tag it returns the tag's metadata, the manifest annotations
// that carry it, and the object the tag points at, which is what the packfile
// is cut for: a manifest is tagged with a commit id and an annotated tag's own
// object has none. For anything else the map is empty and the hash is srcHash.
func (h *Helper) tagAnnotations(srcRef string, srcHash plumbing.Hash) (*git.AnnotatedTagInfo, map[string]string, plumbing.Hash) {
	annotations := make(map[string]string)
	tagInfo, _ := h.gitRepo.GetAnnotatedTagInfo(srcRef)
	if tagInfo == nil {
		return nil, annotations, srcHash
	}
	annotations[oci.AnnotationGitTagger] = tagInfo.Tagger
	annotations[oci.AnnotationGitTagMessage] = tagInfo.Message
	annotations[oci.AnnotationGitTagSig] = tagInfo.Signature
	annotations[oci.AnnotationGitTagObj] = tagInfo.ObjectHash
	return tagInfo, annotations, plumbing.NewHash(tagInfo.ObjectHash)
}

// commitAnnotations adds the standard OCI annotations describing the ref and
// the commit at its tip to a ref manifest's annotation set.
func commitAnnotations(annotations map[string]string, dstRef string, commit *object.Commit) {
	annotations[ocispec.AnnotationTitle] = dstRef
	annotations[ocispec.AnnotationVendor] = "git-remote-oci"
	annotations[ocispec.AnnotationDocumentation] = "https://github.com/mrueg/git-remote-oci"
	if commit == nil {
		return
	}
	annotations[ocispec.AnnotationAuthors] = commit.Author.Name + " <" + commit.Author.Email + ">"
	annotations[ocispec.AnnotationCreated] = commit.Author.When.UTC().Format(time.RFC3339)
	msgLines := strings.Split(strings.TrimSpace(commit.Message), "\n")
	if len(msgLines) > 0 && msgLines[0] != "" {
		annotations[ocispec.AnnotationDescription] = msgLines[0]
	}
}

type parsedPushSpec struct {
	force    bool
	isDelete bool
	srcRef   string
	dstRef   string
	srcHash  plumbing.Hash
	// wantHash is what the packfile is cut for: the target of an annotated
	// tag, and otherwise srcHash. See tagAnnotations.
	wantHash   plumbing.Hash
	commitSHA  string
	commit     *object.Commit
	tagInfo    *git.AnnotatedTagInfo
	tagAnnoMap map[string]string
	parentsStr string
	refTag     string
	haveHashes []plumbing.Hash
	// objects is the range's object list (RevList), computed once at
	// validation and shared by every consumer in the upload phase.
	objects       []plumbing.Hash
	validationErr string
}

// handlePushBatchAtomic pushes a batch all-or-nothing, as far as a registry
// with no transactions allows, and returns one report per spec. Writing them,
// and the terminator, is finishPushBatch's job.
func (h *Helper) handlePushBatchAtomic(ctx context.Context, pushSpecs []string) []pushReport {
	parsedSpecs := make([]parsedPushSpec, len(pushSpecs))
	hasValidationError := false

	// Phase 1: Pre-push validation for all specs in the batch
	for i, spec := range pushSpecs {
		force, srcRef, dstRef := h.splitPushSpec(spec)

		// An empty source means "delete this ref". The non-atomic path has
		// always handled it; without this branch `git push --atomic :branch`
		// fell through to ResolveRef("") and failed the whole batch.
		if srcRef == "" {
			parsedSpecs[i] = parsedPushSpec{dstRef: dstRef, isDelete: true}
			continue
		}

		// Acquire distributed ref lock to prevent multi-developer race conditions
		if !h.dryRun {
			_, lockErr := h.ociClient.AcquireRefLock(ctx, dstRef, h.pushLockTTL)
			if lockErr != nil {
				parsedSpecs[i] = parsedPushSpec{dstRef: dstRef, validationErr: fmt.Sprintf("reference is locked: %v", lockErr)}
				hasValidationError = true
				continue
			}
			defer h.releaseRefLock(ctx, dstRef)
		}

		srcHash, err := h.gitRepo.ResolveRef(srcRef)
		if err != nil {
			parsedSpecs[i] = parsedPushSpec{dstRef: dstRef, validationErr: fmt.Sprintf("failed to resolve local ref %s: %v", srcRef, err)}
			hasValidationError = true
			continue
		}
		commitSHA := srcHash.String()

		// Compare-And-Swap (CAS / --force-with-lease) protection check
		if expectedSHA, hasCAS := h.casRefs[dstRef]; hasCAS {
			existingSHA, exists := h.remoteRefs[dstRef]
			if expectedSHA != "" {
				if !exists || existingSHA != expectedSHA {
					parsedSpecs[i] = parsedPushSpec{dstRef: dstRef, validationErr: fmt.Sprintf("stale info; --force-with-lease expected %s, but remote is %s", expectedSHA, existingSHA)}
					hasValidationError = true
					continue
				}
			} else {
				if exists && existingSHA != "" {
					parsedSpecs[i] = parsedPushSpec{dstRef: dstRef, validationErr: fmt.Sprintf("stale info; --force-with-lease expected ref to not exist, but remote is %s", existingSHA)}
					hasValidationError = true
					continue
				}
			}
		}

		// One walk for the fast-forward check and pack-base selection, as on
		// the non-atomic path.
		ancestors, ancestorErr := h.remoteAncestors(srcHash, h.remoteRefs)

		if existingSHA, exists := h.remoteRefs[dstRef]; exists && existingSHA != commitSHA && !force {
			if ancestorErr != nil {
				parsedSpecs[i] = parsedPushSpec{dstRef: dstRef, validationErr: fmt.Sprintf("non-fast-forward update rejected (use '+' to force): remote is %s", shortSHA(existingSHA))}
				hasValidationError = true
				continue
			}
			if !ancestors[plumbing.NewHash(existingSHA)] {
				parsedSpecs[i] = parsedPushSpec{dstRef: dstRef, validationErr: "non-fast-forward update rejected (use '+' to force)"}
				hasValidationError = true
				continue
			}
		}

		// The same rule as the non-atomic path. This used to accept any remote
		// ref whose commit merely existed locally, which excluded objects that a
		// clone of this ref alone would never be given.
		haveHashes, baseErr := h.packBases(ctx, srcHash, h.remoteRefs, ancestors)
		if baseErr != nil {
			parsedSpecs[i] = parsedPushSpec{dstRef: dstRef, validationErr: baseErr.Error()}
			hasValidationError = true
			continue
		}

		refTag := oci.EncodeRefTag(dstRef)
		if refTag == "" {
			parsedSpecs[i] = parsedPushSpec{dstRef: dstRef, validationErr: fmt.Sprintf("destination ref %q cannot be represented as an OCI tag", dstRef)}
			hasValidationError = true
			continue
		}

		commit, err := h.gitRepo.GetCommitInfo(srcHash)
		if err != nil {
			commit = nil
		}
		tagInfo, tagAnnoMap, wantHash := h.tagAnnotations(srcRef, srcHash)

		// Listed here, before anything is uploaded, so a range that cannot be
		// walked fails validation like every other bad spec rather than a
		// half-applied batch.
		objects, listErr := h.gitRepo.RevList(wantHash, haveHashes)
		if listErr != nil {
			parsedSpecs[i] = parsedPushSpec{dstRef: dstRef, validationErr: fmt.Sprintf("failed to list the objects the push carries: %v", listErr)}
			hasValidationError = true
			continue
		}

		parsedSpecs[i] = parsedPushSpec{
			force:      force,
			srcRef:     srcRef,
			dstRef:     dstRef,
			srcHash:    srcHash,
			wantHash:   wantHash,
			commitSHA:  commitSHA,
			commit:     commit,
			tagInfo:    tagInfo,
			tagAnnoMap: tagAnnoMap,
			parentsStr: parentsOf(commit),
			refTag:     refTag,
			haveHashes: haveHashes,
			objects:    objects,
		}
	}

	reports := make([]pushReport, len(parsedSpecs))

	// Phase 2: If pre-push validation failed for ANY ref in atomic batch, abort all
	if hasValidationError {
		for i, parsed := range parsedSpecs {
			dst := parsed.dstRef
			if dst == "" {
				dst = pushSpecs[i]
			}
			if parsed.validationErr != "" {
				reports[i] = failReport(dst, "%s", parsed.validationErr)
			} else {
				reports[i] = failReport(dst, "atomic push failed: pre-push validation error in batch")
			}
		}
		return reports
	}

	if h.dryRun {
		for i, parsed := range parsedSpecs {
			if parsed.isDelete {
				h.logInfo("git-remote-oci: (atomic dry-run) verified deletion of %s\n", parsed.dstRef)
				reports[i] = okReport(parsed.dstRef)
				continue
			}
			pr, pw := io.Pipe()
			go func(p parsedPushSpec) {
				err := h.gitRepo.CreatePackfileFromListTo(pw, p.wantHash, p.haveHashes, p.objects)
				_ = pw.CloseWithError(err)
			}(parsed)
			_, err := io.Copy(io.Discard, pr)
			_ = pr.Close()
			if err != nil {
				reports[i] = failReport(parsed.dstRef, "dry-run packfile generation failed: %v", err)
				continue
			}
			h.logInfo("git-remote-oci: (atomic dry-run) verified push for commit %s to OCI tag %s (%s)\n", parsed.commitSHA, parsed.refTag, parsed.dstRef)
			h.logVerbose("git-remote-oci: [verbose] atomic dry-run packfile verification succeeded for commit %s\n", parsed.commitSHA)
			reports[i] = okReport(parsed.dstRef)
		}
		return reports
	}

	// Phase 3: Push commit streams & ref manifests without updating _refs index
	changed := make(map[string]oci.RefEntry)
	deletedRefs := make(map[string]bool)

	var pushErr error
	failedDstRef := ""
	var rollback []oci.RefTagSnapshot

	for _, parsed := range parsedSpecs {
		if parsed.isDelete {
			// Deletions run after every upload has landed; see below.
			continue
		}

		commitAnnotations(parsed.tagAnnoMap, parsed.dstRef, parsed.commit)

		pr, pw := io.Pipe()
		go func(p parsedPushSpec) {
			err := h.gitRepo.CreatePackfileFromListTo(pw, p.wantHash, p.haveHashes, p.objects)
			_ = pw.CloseWithError(err)
		}(parsed)

		// An LFS failure here must fail the whole batch: the ref would otherwise
		// be published referencing blobs the registry does not have.
		lfsDescs, lfsErr := h.uploadLFSObjects(ctx, parsed.objects, " (atomic)")
		if lfsErr != nil {
			_ = pr.CloseWithError(lfsErr)
			pushErr = lfsErr
			failedDstRef = parsed.dstRef
			break
		}
		if snap, ok := h.snapshotLayer(ctx, parsed.commitSHA, parsed.wantHash); ok {
			lfsDescs = append(lfsDescs, snap)
		}
		if idx, ok := h.packIndexLayer(ctx, parsed.wantHash, parsed.objects); ok {
			lfsDescs = append(lfsDescs, idx)
		}

		forceStr := ""
		if parsed.force {
			forceStr = " (force)"
		}
		h.logInfo("git-remote-oci: (atomic) pushing commit %s to OCI tag %s (%s)%s...\n", parsed.commitSHA, parsed.refTag, parsed.dstRef, forceStr)
		h.logVerbose("git-remote-oci: [verbose] atomic target tag: %s, parents: %q\n", parsed.refTag, parsed.parentsStr)

		// Record where the tag pointed before overwriting it, so a later
		// failure in this batch can put it back.
		snap, snapErr := h.ociClient.SnapshotRefTag(ctx, parsed.dstRef)
		if snapErr != nil {
			_ = pr.CloseWithError(snapErr)
			pushErr = snapErr
			failedDstRef = parsed.dstRef
			break
		}

		stopPush := h.timer.phase("build and upload packfile")
		err := h.ociClient.PushCommitStream(ctx, oci.CommitPush{
			CommitSHA:      parsed.commitSHA,
			RefName:        parsed.dstRef,
			RefTag:         parsed.refTag,
			Parents:        parsed.parentsStr,
			PackBases:      packBaseStrings(parsed.haveHashes),
			TagAnnotations: parsed.tagAnnoMap,
			ExtraLayers:    lfsDescs,
		}, pr, 0)
		stopPush()
		if err != nil {
			_ = pr.CloseWithError(err)
			pushErr = err
			failedDstRef = parsed.dstRef
			break
		}
		rollback = append(rollback, snap)
		changed[parsed.dstRef] = refEntryFor(parsed.commitSHA, parsed.commit, parsed.tagInfo)
	}

	// Deletions last, and only once every upload has landed. A deletion is
	// the one step here that cannot be rolled back -- the manifest is gone
	// from the registry -- so it has to be the step least likely to be
	// followed by a failure, and it is also the cheapest. Running it first
	// meant `git push --atomic origin :old new` deleted old and then failed
	// on new, which is the half-applied batch --atomic promises not to leave.
	if pushErr == nil {
		for _, parsed := range parsedSpecs {
			if !parsed.isDelete {
				continue
			}
			if err := h.ociClient.DeleteRef(ctx, parsed.dstRef); err != nil {
				pushErr = err
				failedDstRef = parsed.dstRef
				break
			}
			deletedRefs[parsed.dstRef] = true
		}
	}

	if pushErr != nil {
		// Put the ref tags back. The _refs index has not been touched yet, so
		// listing still reports the old state - but a ref tag also resolves
		// directly, and leaving those advanced makes the two disagree, so the
		// next push would run its fast-forward check against a ref that only
		// half moved.
		//
		// Best effort, not a transaction: the commit manifests and their blobs
		// stay behind as garbage, and a registry that refuses deletion cannot
		// have a newly created tag removed at all. Whatever could not be undone
		// is reported rather than passed over -- which includes a ref this
		// batch had already deleted before a later deletion failed.
		var stranded []string
		for _, snap := range rollback {
			if err := h.ociClient.RestoreRefTag(context.WithoutCancel(ctx), snap); err != nil {
				stranded = append(stranded, snap.RefName)
				h.logWarn("git-remote-oci: warning: could not roll back %s: %v\n", snap.RefName, err)
			}
		}
		for name := range deletedRefs {
			stranded = append(stranded, name)
			h.logWarn("git-remote-oci: warning: %s was deleted before the batch failed and cannot be put back\n", name)
		}

		for i, parsed := range parsedSpecs {
			switch {
			case parsed.dstRef == failedDstRef:
				reports[i] = failReport(parsed.dstRef, "%v", pushErr)
			case slices.Contains(stranded, parsed.dstRef):
				reports[i] = failReport(parsed.dstRef, "atomic push failed in %s, and this ref could not be rolled back", failedDstRef)
			default:
				reports[i] = failReport(parsed.dstRef, "atomic push failed due to error in %s", failedDstRef)
			}
		}
		return reports
	}

	// Phase 4: Atomic update of _refs index in single transaction. Only what
	// this batch moved is written; see handlePushBatch for why the session's
	// snapshot of the other refs must not be.
	if len(changed) > 0 || len(deletedRefs) > 0 {
		if err := h.ociClient.PushRichRefIndexWithHead(ctx, changed, deletedRefs, headHintForRefs(changed)); err != nil {
			for i, parsed := range parsedSpecs {
				reports[i] = failReport(parsed.dstRef, "atomic push failed to update _refs index: %v", err)
			}
			return reports
		}
	}

	// Batch push succeeded! Bring the session's view up to date and report ok
	// for every spec.
	for name, entry := range changed {
		h.recordRemoteRef(name, entry)
	}
	for name := range deletedRefs {
		h.forgetRemoteRef(name)
	}
	for i, parsed := range parsedSpecs {
		reports[i] = okReport(parsed.dstRef)
	}
	return reports
}

// printlnOut and printfOut are the only sanctioned ways to write to stdout.
// They serialise on outMu because protocol responses are emitted from
// concurrent workers; see the comment on Helper.outMu.
func (h *Helper) printlnOut(a ...any) {
	h.outMu.Lock()
	defer h.outMu.Unlock()
	_, _ = fmt.Fprintln(h.out, a...)
}

func (h *Helper) printfOut(format string, a ...any) {
	h.outMu.Lock()
	defer h.outMu.Unlock()
	_, _ = fmt.Fprintf(h.out, format, a...)
}

// lookupRemoteRef returns the cached remote SHA for refName.
func (h *Helper) lookupRemoteRef(refName string) (string, bool) {
	h.refsMu.Lock()
	defer h.refsMu.Unlock()
	sha, ok := h.remoteRefs[refName]
	return sha, ok
}

// snapshotRemoteRefs returns a copy of the cached remote refs, so callers can
// iterate without holding refsMu across expensive work such as history walks.
func (h *Helper) snapshotRemoteRefs() map[string]string {
	h.refsMu.Lock()
	defer h.refsMu.Unlock()
	out := make(map[string]string, len(h.remoteRefs))
	for k, v := range h.remoteRefs {
		out[k] = v
	}
	return out
}

// recordRemoteRef updates the cached ref state after a successful push.
func (h *Helper) recordRemoteRef(refName string, entry oci.RefEntry) {
	h.refsMu.Lock()
	defer h.refsMu.Unlock()
	if h.remoteRefs != nil {
		h.remoteRefs[refName] = entry.SHA
	}
	if h.richRemoteRefs != nil {
		h.richRemoteRefs[refName] = entry
	}
}

// forgetRemoteRef drops the cached ref state after a successful deletion.
func (h *Helper) forgetRemoteRef(refName string) {
	h.refsMu.Lock()
	defer h.refsMu.Unlock()
	delete(h.remoteRefs, refName)
	delete(h.richRemoteRefs, refName)
}

// parseBlobLimit parses a blob:limit= value.
//
// Git writes these in human units - "100k", "1m", "500K" - and the previous
// code handed the raw string to strconv.ParseInt, which rejected every one of
// them and then discarded the error, so the filter silently did nothing. The
// parsing lives in pkg/config because the same units appear in the config
// keys, and two copies of a unit table is how they come to disagree.
func parseBlobLimit(spec string) (int64, error) {
	return config.ParseByteSize(spec)
}

// shortSHA abbreviates an object id for display without assuming a length.
// Values read back from a registry index are not guaranteed to be well-formed.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// refEntryFor builds the cached index entry for a pushed ref.
func refEntryFor(commitSHA string, commit *object.Commit, tagInfo *git.AnnotatedTagInfo) oci.RefEntry {
	entry := oci.RefEntry{SHA: commitSHA}
	if commit != nil {
		entry.Author = commit.Author.Name + " <" + commit.Author.Email + ">"
		entry.Timestamp = commit.Author.When.Unix()
		entry.Message = strings.TrimSpace(commit.Message)
	}
	if tagInfo != nil {
		entry.Tagger = tagInfo.Tagger
		entry.TagMessage = tagInfo.Message
		entry.TagSig = tagInfo.Signature
		entry.TagObject = tagInfo.ObjectHash
	}
	return entry
}

func (h *Helper) logInfo(format string, a ...any) {
	if h.verbosity >= 1 {
		fmt.Fprintf(os.Stderr, format, a...)
	}
}

func (h *Helper) logVerbose(format string, a ...any) {
	if h.verbosity >= 2 {
		fmt.Fprintf(os.Stderr, format, a...)
	}
}

// logWarn reports something the user probably needs to know.
//
// Unlike logInfo it is not silenced by "verbosity 0". Git sets that for -q,
// which asks for less chatter, not for warnings to be withheld: it used to hide
// an unavailable LFS object and a failed index update, so a quiet push looked
// clean while losing data.
func (h *Helper) logWarn(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format, a...)
}

type progressReader struct {
	r      io.Reader
	action string
	ref    string
	total  int64
	helper *Helper
}

func (pr *progressReader) Read(p []byte) (n int, err error) {
	n, err = pr.r.Read(p)
	if n > 0 {
		pr.total += int64(n)
		if pr.helper.progress && pr.helper.verbosity >= 1 {
			kb := float64(pr.total) / 1024.0
			pr.helper.logInfo("git-remote-oci: %s (%s): %.1f KiB transferred\r", pr.action, pr.ref, kb)
		}
	}
	if err == io.EOF && pr.total > 0 && pr.helper.progress && pr.helper.verbosity >= 1 {
		kb := float64(pr.total) / 1024.0
		pr.helper.logInfo("git-remote-oci: %s (%s): %.1f KiB completed.\n", pr.action, pr.ref, kb)
	}
	return n, err
}
