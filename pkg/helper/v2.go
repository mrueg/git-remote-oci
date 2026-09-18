package helper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mrueg/git-remote-oci/pkg/git"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// advertisedObjectFormat reports the repository's hash algorithm, which git
// needs before it will talk to a SHA-256 repository at all.
func (h *Helper) advertisedObjectFormat(ctx context.Context) string {
	refs, err := h.v2RemoteRefs(ctx)
	if err != nil {
		return "sha1"
	}
	plain := make(map[string]string, len(refs))
	for name, entry := range refs {
		plain[name] = entry.SHA
	}
	if format := objectFormatOf(plain); format != "" {
		return format
	}
	return "sha1"
}

// v2Head resolves the ref HEAD points at, preferring what the repository
// recorded over guessing — the same order handleList uses, because a clone
// checking out the wrong branch is the same bug on either interface.
func (h *Helper) v2Head(ctx context.Context, refs map[string]oci.RefEntry) string {
	if recorded, err := h.ociClient.FetchHead(ctx); err == nil && recorded != "" {
		if _, live := refs[recorded]; live {
			return recorded
		}
	}

	var branches []string
	for name := range refs {
		if strings.HasPrefix(name, "refs/heads/") {
			branches = append(branches, name)
		}
	}
	if len(branches) == 0 {
		return ""
	}
	sort.Strings(branches)
	for _, preferred := range []string{"refs/heads/main", "refs/heads/master"} {
		for _, b := range branches {
			if b == preferred {
				return b
			}
		}
	}
	return branches[0]
}

// Git's wire protocol version 2, served over the remote-helper
// `stateless-connect` capability.
//
// The rest of this package implements gitremote-helpers(7): git says
// `fetch <sha> <name>` and the helper writes objects into the repository
// itself. That interface is simple and deliberately limited — it has no
// want/have negotiation, no `ref-prefix`, and no `filter`, because those are
// arguments to protocol v2 commands rather than helper capabilities. Partial
// clone in particular is not a storage problem here: a helper's `fetch` is
// defined as delivering a complete object graph and git verifies it, so no
// arrangement of layers on the registry can produce a filtered clone through
// that interface.
//
// stateless-connect is the way out. A helper declaring it is handed a raw v2
// conversation to serve, and git then talks to it as though it were a server —
// which is exactly what git-remote-http does. This file is that server: pkt-line
// framing (pktline.go), a capability advertisement, `ls-refs`, and `fetch`.
//
// It is on by default and `ociremote.protocolV2=false` turns it off; see
// defaultProtocolV2. The simple path works and is well covered, and it is what
// the helper falls back to. This one speaks an interface gitremote-helpers(7)
// calls "experimental; for internal use only".

// v2Agent identifies this implementation in the capability advertisement.
const v2Agent = "git-remote-oci"

// sideband channels, as used by the packfile section.
const (
	// sidebandData is band 1, the packfile itself.
	sidebandData = 1
	// sidebandError is band 3, which git treats as fatal. Band 2 is progress
	// and is not written: git is told `no-progress` by every client that
	// matters here, and the useful reporting already goes to stderr.
	sidebandError = 3
	// sidebandMax is the largest payload one sideband packet may carry: the
	// pkt-line maximum less the one byte naming the band.
	sidebandMax = pktMaxPayload - 1
)

// serveStatelessConnect answers a `stateless-connect <service>` command.
//
// Replying `fallback` is a documented, supported answer meaning "no smart
// transport here", and git then uses the simple fetch/push path. That is what
// makes this safe to ship: anything unsupported declines rather than half-serves
// a conversation git cannot recover from mid-stream.
// It returns false when it declined, in which case the caller must carry on
// serving the simple protocol on the same pipe — replying `fallback` and then
// exiting would leave git with a helper that had answered nothing.
func (h *Helper) serveStatelessConnect(ctx context.Context, service string) (bool, error) {
	if !h.protocolV2 || service != "git-upload-pack" {
		// Push still goes through the simple path; only upload-pack is served.
		h.printlnOut("fallback")
		return false, nil
	}

	// An empty line accepts the connection. Everything after it is pkt-lines.
	h.printlnOut()

	w := newPktWriter(h.out)
	// The same reader Run has been using, not a new one over h.in: a fresh
	// reader would start after whatever the shared buffer already holds.
	r := newPktReader(h.reader)

	// git reads the advertisement first to discover the protocol version, so it
	// is sent unprompted rather than in reply to a request.
	if err := h.v2Advertise(ctx, w); err != nil {
		return true, err
	}

	for {
		if err := ctx.Err(); err != nil {
			return true, err
		}
		req, err := readV2Request(r)
		if errors.Is(err, io.EOF) {
			// git closed the connection; the helper exits.
			return true, nil
		}
		if err != nil {
			return true, err
		}
		if req.command == "" {
			return true, nil
		}

		switch req.command {
		case "ls-refs":
			err = h.v2LsRefs(ctx, w, req)
		case "fetch":
			err = h.v2Fetch(ctx, w, req)
		case "object-info":
			err = h.v2ObjectInfo(ctx, w, req)
		default:
			// Only the commands above are advertised, so this is git asking for
			// something it was never offered. Saying so beats exiting: the
			// helper going quiet reaches the user as "the remote end hung up",
			// which describes neither what happened nor which side to look at.
			if sendErr := sendV2Error(w, fmt.Sprintf("protocol v2: unsupported command %q", req.command)); sendErr != nil {
				return true, sendErr
			}
			return true, fmt.Errorf("protocol v2: unsupported command %q", req.command)
		}
		if err != nil {
			return true, err
		}
	}
}

// v2Advertise writes the capability advertisement.
//
// Only what is actually served is advertised, because git takes the list at its
// word: a capability claimed and then not honoured is a conversation that stops
// mid-stream rather than one that declines.
func (h *Helper) v2Advertise(ctx context.Context, w *pktWriter) error {
	format := h.advertisedObjectFormat(ctx)

	for _, line := range []string{
		"version 2",
		"agent=" + v2Agent,
		"ls-refs=unborn",
		// filter is what makes a partial clone possible at all. It is an
		// argument to the v2 fetch command and has no equivalent in the simple
		// helper protocol, which is why --filter could never work there: that
		// interface is defined as delivering a complete object graph, and git
		// verifies it.
		//
		// shallow covers the deepen arguments behind `clone --depth`. Without
		// it git refuses such a clone up front — "Server does not support
		// shallow requests" — which would make enabling protocol v2 take away
		// something the simple path could already do.
		"fetch=filter shallow",
		// object-info lets a client ask how big an object is without fetching
		// it, which is what --filter=blob:limit needs to make its decision and
		// what `cat-file --batch-check` against a promisor remote uses. See
		// v2objectinfo.go for what it costs here.
		"object-info=size",
		"object-format=" + format,
	} {
		if err := w.WriteLine("%s", line); err != nil {
			return err
		}
	}
	// A flush and no response-end. The advertisement is not a reply to a
	// request — it is what git reads to discover the protocol version before
	// the first request exists — so the response-end packet that terminates a
	// request-response pair would be an extra packet git never consumes, and
	// it surfaces later as "expected flush after ref listing".
	return w.Flush()
}

// endResponse terminates a response: a flush, then the response-end packet
// stateless-connect requires so git knows one request-response pair is over.
func endResponse(w *pktWriter) error {
	if err := w.Flush(); err != nil {
		return err
	}
	return w.ResponseEnd()
}

// v2Request is one parsed command: its name, the capabilities the client
// declared, and the arguments after the delimiter.
type v2Request struct {
	command      string
	capabilities []string
	args         []string
}

// readV2Request reads up to the flush packet that ends a request.
func readV2Request(r *pktReader) (v2Request, error) {
	var req v2Request
	afterDelim := false

	for {
		line, kind, err := r.ReadLine()
		if err != nil {
			return req, err
		}
		switch kind {
		case pktFlush:
			return req, nil
		case pktDelim:
			afterDelim = true
			continue
		case pktResponseEnd:
			return req, nil
		}

		switch {
		case afterDelim:
			req.args = append(req.args, line)
		case strings.HasPrefix(line, "command="):
			req.command = strings.TrimPrefix(line, "command=")
		default:
			req.capabilities = append(req.capabilities, line)
		}
	}
}

// has reports whether the client sent an argument, exactly or as a prefix.
func (r v2Request) has(arg string) bool {
	for _, a := range r.args {
		if a == arg {
			return true
		}
	}
	return false
}

// values returns the remainder of every argument beginning with prefix.
func (r v2Request) values(prefix string) []string {
	var out []string
	for _, a := range r.args {
		if strings.HasPrefix(a, prefix) {
			out = append(out, strings.TrimPrefix(a, prefix))
		}
	}
	return out
}

// shallowArgs is the depth-limiting part of a fetch request.
//
// `shallow <oid>` lines are the client describing its *current* boundary —
// commits it holds whose parents it does not. They arrive on every fetch a
// shallow clone makes, deepening or not, and they matter even when nothing is
// being deepened: a traversal that does not know about them will walk through
// those commits into ancestors the client was never sent.
type shallowArgs struct {
	// deepen is the requested depth in generations, or zero if none was asked
	// for. `--depth` and `--unshallow`.
	deepen int
	// since cuts by committer date instead. `--shallow-since`.
	since time.Time
	// relative counts deepen from the boundary the client already has rather
	// than from the tips. `git fetch --deepen=<n>`.
	relative bool
	// exclude names refs whose history is to be left out. `--shallow-exclude`.
	exclude []string
	// excludedTips are those refs resolved to commit ids, which is what the
	// walk needs. Filled in once the repository's refs are known.
	excludedTips []string
	// clientBoundary is what the client says it is already shallow at.
	clientBoundary []string
	// unsupported names a deepen variant this server cannot compute, if any.
	unsupported string
}

// wants reports whether the request asks for a truncated history at all.
func (s shallowArgs) wants() bool {
	return s.deepen > 0 || !s.since.IsZero() || len(s.exclude) > 0
}

func parseShallowArgs(req v2Request) shallowArgs {
	s := shallowArgs{clientBoundary: req.values("shallow ")}

	// deepen-relative counts the depth from the boundary the client already
	// has rather than from the tips. `git fetch --deepen=<n>`.
	s.relative = req.has("deepen-relative")

	if v := req.values("deepen "); len(v) > 0 {
		if n, err := strconv.Atoi(strings.TrimSpace(v[0])); err == nil && n > 0 {
			s.deepen = n
		}
	}
	// A unix timestamp, in seconds. git has already parsed whatever the user
	// typed; an unparseable one here would be git's bug, and ignoring it would
	// serve an unlimited history to someone who asked for a slice — so it is
	// refused rather than dropped.
	if v := req.values("deepen-since "); len(v) > 0 {
		secs, err := strconv.ParseInt(strings.TrimSpace(v[0]), 10, 64)
		if err != nil {
			s.unsupported = "deepen-since"
		} else {
			s.since = time.Unix(secs, 0)
		}
	}
	for _, ref := range req.values("deepen-not ") {
		if ref = strings.TrimSpace(ref); ref != "" {
			s.exclude = append(s.exclude, ref)
		}
	}
	return s
}

// stagingGit builds a git command against the staging object store — the two
// that have to be git, index-pack and pack-objects. Everything else that used
// to read this store (the want check, the boundary walk) reads it with go-git.
//
// Both of these run inside the client's own repository, which is fine for
// finding objects — that repository is the staging area's alternate — and wrong
// for one thing: if the client is shallow, its .git/shallow grafts its truncated
// view onto the history being served, so a pack is cut short of what was
// promised. An empty --shallow-file says to ignore it, which is what
// upload-pack does for the same reason.
func stagingGit(ctx context.Context, env []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--shallow-file", ""}, args...)...)
	cmd.Env = env
	return cmd
}

// shallowBoundary computes a depth-limited view of the wants against the
// staging object store, because serving a fetch means cutting the history
// assembled there — not the one the client happens to hold.
//
// The client's own .git/shallow does not apply and must not: a shallow client
// asking to deepen needs the history it is missing, and grafting its truncated
// view onto the store being walked would find no parents to deepen into. Reading
// the staging directory directly is what gives that for free — it has objects
// and an alternate, and no shallow file for anything to graft from.
func shallowBoundary(stage string, tips []string, shallow shallowArgs) (map[string]bool, []string, error) {
	store, err := git.OpenObjectStore(stage)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read the staged objects: %w", err)
	}

	// Each deepen argument is a statement about what the client does not want,
	// so they combine by intersection and the shallowest cut wins. git can send
	// more than one at a time.
	var rules []git.DeepenRule
	if shallow.deepen > 0 {
		depth := shallow.deepen
		// A relative deepen counts from where the client's history currently
		// stops, not from the tips: `--deepen=2` asks for two more generations
		// below its boundary. Same walk, started somewhere else — and one level
		// deeper, because the boundary commit is level zero of that walk and
		// the client already has it.
		//
		// With no boundary to count from there is nothing relative about it,
		// and the depth is read from the tips as usual. git does not send this
		// combination; answering it as an ordinary depth beats answering it
		// with an empty view.
		if shallow.relative && len(shallow.clientBoundary) > 0 {
			tips = shallow.clientBoundary
			depth = shallow.deepen + 1
		}
		rules = append(rules, git.AtDepth(depth))
	}
	if !shallow.since.IsZero() {
		rules = append(rules, git.Since(shallow.since))
	}
	if len(shallow.exclude) > 0 {
		// The refs name tips to exclude; what is actually excluded is
		// everything behind them.
		excluded := git.Reachable(store, shallow.excludedTips)
		rules = append(rules, git.Excluding(excluded))
	}
	if len(rules) == 0 {
		return map[string]bool{}, nil, nil
	}
	return git.BoundaryFor(store, tips, git.AllOf(rules...))
}

// v2LsRefs serves the ls-refs command.
//
// This is the first thing v2 buys: git names the prefixes it cares about and
// the advertisement is narrowed to them, instead of every ref in the repository
// being sent on every connection. The simple helper protocol has no way to
// express that — `ref-prefix` is an ls-refs argument, which is why the old
// `list` output was always the whole set.
func (h *Helper) v2LsRefs(ctx context.Context, w *pktWriter, req v2Request) error {
	refs, err := h.v2RemoteRefs(ctx)
	if err != nil {
		// As an ERR packet, for the reason v2Fetch gives: it is the only
		// thing git shows verbatim. Without it a registry refusing the
		// credentials reached the user as "the remote end hung up
		// unexpectedly", which is the least useful thing it could say about
		// a 401. Still an error afterwards: the exit status is what a script
		// reads.
		err = fmt.Errorf("ls-refs: %w", err)
		if sendErr := sendV2Error(w, err.Error()); sendErr != nil {
			return sendErr
		}
		return err
	}

	prefixes := req.values("ref-prefix ")
	wantSymrefs := req.has("symrefs")
	wantPeel := req.has("peel")

	names := make([]string, 0, len(refs))
	for name := range refs {
		if !matchesAnyPrefix(name, prefixes) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	head := h.v2Head(ctx, refs)

	for _, name := range names {
		entry := refs[name]

		// For an annotated tag the ref points at the *tag object*, and peels to
		// the commit. Advertising the commit as the ref value is what the old
		// list output had to do, because the helper protocol has no peel form;
		// v2 has one, so the distinction can finally be told properly.
		value, peeled := entry.SHA, ""
		if entry.TagObject != "" {
			value, peeled = entry.TagObject, entry.SHA
		}

		line := value + " " + name
		if wantPeel && peeled != "" {
			line += " peeled:" + peeled
		}
		if err := w.WriteLine("%s", line); err != nil {
			return err
		}
	}

	// HEAD, so a clone knows which branch to check out. It is only in scope
	// when the client asked for a prefix that covers it, or asked for
	// everything.
	//
	// No `unborn HEAD` line is ever written, though the client asks for one and
	// the advertisement says the argument is understood. That is a property of
	// the storage rather than a gap here: an unborn HEAD is a recorded default
	// branch with no ref behind it, and the ref index refuses to hold one — a
	// push that deletes the branch HEAD names clears the record with it, rather
	// than leave the repository advertising something that is not there. So the
	// case exists in the protocol and cannot arise in this format.
	if head != "" && matchesAnyPrefix("HEAD", prefixes) {
		if entry, ok := refs[head]; ok {
			line := entry.SHA + " HEAD"
			if wantSymrefs {
				line += " symref-target:" + head
			}
			if err := w.WriteLine("%s", line); err != nil {
				return err
			}
		}
	}

	return endResponse(w)
}

// matchesAnyPrefix reports whether a ref is in scope. No prefixes means every
// ref, which is what git sends when it genuinely wants the lot.
func matchesAnyPrefix(name string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// v2Fetch serves the fetch command.
//
// The response is deliberately single-round. If the client said `done` the
// acknowledgments section must be omitted entirely; if it did not, answering
// `ready` says "a cut point has been chosen, the packfile follows in this same
// response", which the grammar allows and which avoids a negotiation loop this
// has no better information to win.
func (h *Helper) v2Fetch(ctx context.Context, w *pktWriter, req v2Request) error {
	wants := req.values("want ")
	haves := req.values("have ")
	done := req.has("done")

	if len(wants) == 0 {
		// Nothing asked for: an empty packfile section still has to be
		// well-formed, or git waits for bytes that never come. The delimiter
		// between the two sections is part of that — the grammar separates
		// every section from the next, and only the last one is followed by a
		// flush.
		if !done {
			if err := writeAcknowledgements(w, nil); err != nil {
				return err
			}
			if err := w.Delim(); err != nil {
				return err
			}
		}
		if err := w.WriteLine("packfile"); err != nil {
			return err
		}
		return endResponse(w)
	}

	if unsupported := parseShallowArgs(req).unsupported; unsupported != "" {
		err := fmt.Errorf("protocol v2: fetch: %s is not supported by this remote", unsupported)
		if sendErr := sendV2Error(w, err.Error()); sendErr != nil {
			return sendErr
		}
		return err
	}

	common := make([]string, 0, len(haves))
	if !done {
		for _, have := range haves {
			if h.hasLocally(have) {
				common = append(common, have)
			}
		}
	}

	// The pack is built before a byte of the response is written, so that a
	// failure can still be reported as an error rather than as a response that
	// is well-formed and wrong.
	pack, cleanup, err := h.buildPackForWants(ctx, wants, haves, req)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		// Nothing has been written yet, so every one of these can still be
		// reported properly. An ERR packet is the only thing git surfaces
		// verbatim; without one the helper just exits and the user is told
		// "the remote end hung up unexpectedly", which names neither what
		// failed nor which end to look at. A registry that timed out or
		// refused the credentials deserves better than that, and it is the
		// same courtesy an unservable want already got.
		if sendErr := sendV2Error(w, err.Error()); sendErr != nil {
			return sendErr
		}
		// Still an error: the exit status is what a script reads.
		return err
	}
	// Closed explicitly once the pack has been streamed, because that is where
	// pack-objects reports a failure; the deferred close only covers the paths
	// that give up before then.
	streamed := false
	defer func() {
		if !streamed {
			_ = pack.Close()
		}
	}()

	if !done {
		if err := writeAcknowledgements(w, common); err != nil {
			return err
		}
		if err := w.Delim(); err != nil {
			return err
		}
	}

	// shallow-info sits between the acknowledgments and the packfile, and tells
	// the client where its history now stops. Omitted when there is nothing to
	// say, which is what an ordinary full fetch reports.
	if len(pack.shallow) > 0 || len(pack.unshallow) > 0 {
		if err := w.WriteLine("shallow-info"); err != nil {
			return err
		}
		for _, oid := range pack.shallow {
			if err := w.WriteLine("shallow %s", oid); err != nil {
				return err
			}
		}
		for _, oid := range pack.unshallow {
			if err := w.WriteLine("unshallow %s", oid); err != nil {
				return err
			}
		}
		if err := w.Delim(); err != nil {
			return err
		}
	}

	if err := w.WriteLine("packfile"); err != nil {
		return err
	}
	if err := streamSideband(w, pack); err != nil {
		return err
	}

	// pack-objects reports a failure by exiting, and by then whatever it did
	// write is already on the wire. Band 3 is all that is left to say so: git
	// treats a packet on it as fatal and throws the transfer away, where
	// staying silent would hand it a truncated pack and let it decide for
	// itself what went wrong.
	streamed = true
	if err := pack.Close(); err != nil {
		return sendSidebandError(w, err.Error())
	}
	return endResponse(w)
}

// errWantNotServed marks a want the staging area could not produce.
//
// It has to be an error rather than an empty pack. git treats a well-formed
// response as an answer, and for a lazy fetch of a missing object it responds
// to "here is nothing" by asking again on the next access — which, when the
// object is never going to arrive, is a loop with no end to it. An ERR packet
// stops the fetch and says why.
var errWantNotServed = errors.New("protocol v2: fetch")

// sendV2Error reports a fatal condition to the client. git recognises a packet
// beginning with "ERR " anywhere in a response and dies with the text.
//
// The message is cut to what one packet can carry, the way sendSidebandError
// does: a registry's error body can run to pages, and refusing to frame it
// would leave the client with no message at all.
func sendV2Error(w *pktWriter, msg string) error {
	const prefix = "ERR "
	if limit := pktMaxPayload - len(prefix) - 1; len(msg) > limit { // -1 for the LF
		msg = msg[:limit]
	}
	if err := w.WriteLine("%s%s", prefix, msg); err != nil {
		return err
	}
	return endResponse(w)
}

// sendSidebandError reports a failure that happened after the packfile section
// had already begun, when an ERR packet would be read as pack bytes.
func sendSidebandError(w *pktWriter, msg string) error {
	payload := append([]byte{sidebandError}, []byte(msg)...)
	if len(payload) > pktMaxPayload {
		payload = payload[:pktMaxPayload]
	}
	if err := w.WriteData(payload); err != nil {
		return err
	}
	return endResponse(w)
}

// hasLocally reports whether an object id is already in the client's own store.
//
// A `have` is a claim, and both places that consume one need it to be true: an
// ACK for an object that is not there strands the negotiation, and a `^have`
// pack-objects cannot resolve fails the whole fetch. The repository is opened
// on demand because a fetch is the one path that reaches here without having
// opened it already, and a client with no objects yet — a fresh clone — is the
// normal case rather than an error.
func (h *Helper) hasLocally(oid string) bool {
	if err := h.ensureGitRepo(); err != nil {
		return false
	}
	_, err := h.gitRepo.GetCommitInfo(plumbing.NewHash(oid))
	return err == nil
}

// writeAcknowledgements emits the acknowledgments section.
func writeAcknowledgements(w *pktWriter, common []string) error {
	if err := w.WriteLine("acknowledgments"); err != nil {
		return err
	}
	for _, oid := range common {
		if err := w.WriteLine("ACK %s", oid); err != nil {
			return err
		}
	}
	if len(common) == 0 {
		if err := w.WriteLine("NAK"); err != nil {
			return err
		}
	}
	return w.WriteLine("ready")
}

// streamSideband copies a packfile into band 1 of the packfile section.
//
// The band byte lives at the front of the same buffer the read fills, so a
// multi-gigabyte pack costs one allocation rather than one per 64 KB packet.
func streamSideband(w *pktWriter, r io.Reader) error {
	packet := make([]byte, sidebandMax+1)
	packet[0] = sidebandData
	for {
		n, err := r.Read(packet[1:])
		if n > 0 {
			if writeErr := w.WriteData(packet[:n+1]); writeErr != nil {
				return writeErr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// buildPackForWants produces the packfile the client asked for.
//
// The objects live in registry packfiles, not in an object database this
// process can pack from directly, so they are staged: every packfile the wants
// depend on is imported into a temporary object directory whose alternate is
// the real repository, and `git pack-objects` then cuts the requested slice out
// of the combined view. The temporary directory is discarded afterwards, so a
// failed fetch leaves nothing behind — unlike the simple path, which writes
// into the repository as it goes.
// newStagingArea creates the scratch object store a v2 request assembles its
// answer in: an objects/ directory whose alternate is the client's real
// repository, so a lookup sees what has been downloaded *and* what the client
// already had, and an environment pointing the git subprocesses at it.
//
// Discarded afterwards, which is what makes a failed request leave nothing
// behind. Shared by every v2 command that has to go and get objects, so the
// alternate wiring and GIT_NO_LAZY_FETCH cannot drift between them -- the LFS
// scan was once two copies of this shape and they did.
func (h *Helper) newStagingArea(filter string, progress bool) (*staging, func(), error) {
	if err := h.ensureGitRepo(); err != nil {
		return nil, func() {}, err
	}

	stage, err := os.MkdirTemp(h.stagingParent(), "git-remote-oci-v2-*")
	if err != nil {
		return nil, func() {}, fmt.Errorf("failed to create a staging directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(stage) }

	objectDir := filepath.Join(stage, "objects")
	if err := os.MkdirAll(filepath.Join(objectDir, "pack"), 0o755); err != nil {
		return nil, cleanup, fmt.Errorf("failed to create the staging object store: %w", err)
	}

	realObjects, err := h.repositoryObjectDir()
	if err != nil {
		return nil, cleanup, err
	}
	// The alternate is recorded in the file as well as the environment. git
	// reads either; go-git reads only the file, and the boundary walk and the
	// want check are go-git now, so without it they would see the staged packs
	// and nothing the client already had.
	if err := os.MkdirAll(filepath.Join(objectDir, "info"), 0o755); err != nil {
		return nil, cleanup, fmt.Errorf("failed to create the staging object store: %w", err)
	}
	if err := os.WriteFile(filepath.Join(objectDir, "info", "alternates"), []byte(realObjects+"\n"), 0o644); err != nil {
		return nil, cleanup, fmt.Errorf("failed to record the staging alternate: %w", err)
	}

	stagingEnv := append(os.Environ(),
		"GIT_OBJECT_DIRECTORY="+objectDir,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES="+realObjects,
		// Every subprocess below runs inside the client's own repository, and
		// when that repository is a partial clone, a missing object is not an
		// error there — it is a cue for git to lazily fetch it. From this
		// remote. Through this helper. So a `cat-file` asking whether a want
		// has been staged yet answers by starting a second helper to go and
		// get it, which asks the same question, and the fetch that should have
		// been one round trip becomes hundreds. It terminates, eventually, and
		// looks only like unexplained slowness.
		//
		// Nothing here wants that: these processes are the thing being asked to
		// produce the object, so "not present" is the answer, not a problem to
		// solve.
		"GIT_NO_LAZY_FETCH=1",
	)

	// Staging is where the time goes on a large fetch -- a packfile downloaded
	// and indexed per push generation -- and it happens before a byte of the
	// response is written, so the client has nothing to show for it. Reporting
	// on sideband band 2 is not available: that band only exists inside the
	// packfile section, which has not begun and deliberately cannot, because
	// building the pack first is what lets a failure be an ERR rather than a
	// truncated stream. stderr is where the rest of this tool reports and git
	// passes it through, so that is where this goes too.
	//
	// `no-progress` is the client saying it does not want it -- git sends it
	// when its own stderr is not a terminal, which is also when this would be
	// noise in a log.
	return &staging{
		dir:      stage,
		env:      stagingEnv,
		filter:   filter,
		staged:   make(map[string]bool),
		progress: progress,
	}, cleanup, nil
}

func (h *Helper) buildPackForWants(ctx context.Context, wants, haves []string, req v2Request) (*builtPack, func(), error) {
	if err := h.ensureGitRepo(); err != nil {
		return nil, nil, err
	}

	st, cleanup, err := h.newStagingArea(firstFilterSpec(req), !req.has("no-progress"))
	if err != nil {
		return nil, cleanup, err
	}
	stage := st.dir

	// Resolve the pack-base graph for the wanted commits and import in
	// dependency order, exactly as the simple fetch path does — the ordering
	// rule is a property of the format, not of which interface asked.
	//
	// A want is an object id, and not every object id is addressable: a
	// manifest is tagged with a *commit* id, so an annotated tag's own object
	// has none. Naming the ref each want came from is what lets the resolver
	// fall back to the ref manifest, which is how the simple path has always
	// reached them.
	// Wants are object ids, and an object id does not say what it is: a blob
	// and a commit are both forty hex digits. So resolution is attempted, and
	// its failure is the signal.
	//
	// A promisor fetch is the case that fails — git asking for blobs it
	// deliberately skipped during a partial clone, which are never manifest
	// tags. There is nothing to resolve, so every ref is staged instead and
	// pack-objects finds the objects in the result. Coarse (the history is
	// staged to serve one blob) but correct, and the staging area is thrown
	// away either way.
	refFor := h.refNamesByObject(ctx)
	specs := make([]fetchSpec, 0, len(wants))
	for _, want := range wants {
		specs = append(specs, fetchSpec{sha: want, ref: refFor[want]})
	}

	// Stopping at commits the client already has is only sound when having the
	// commit means having everything behind it. A shallow client is the case
	// where it does not: it holds the tip and none of its ancestry, so the
	// shortcut would call the graph satisfied, stage nothing, and answer a
	// request to deepen with a pack containing no new history at all.
	shallow := parseShallowArgs(req)
	skipLocal := !shallow.wants() && len(shallow.clientBoundary) == 0

	// --shallow-exclude names refs whose history is to be left out, and leaving
	// it out means knowing what is in it. Those commits are not reachable from
	// the wants — that is the point — so they have to be staged too, or the
	// exclusion covers nothing and the client is quietly sent more history than
	// it asked for.
	if len(shallow.exclude) > 0 {
		published := mustRefs(ctx, h)
		for _, name := range shallow.exclude {
			full, entry, ok := resolveRefName(name, published)
			if !ok {
				// git sends whatever the user typed. A ref this repository does
				// not publish excludes nothing, and saying so beats silently
				// serving a wider history than was asked for.
				return nil, cleanup, fmt.Errorf("%w: --shallow-exclude names %s, which this remote does not publish",
					errWantNotServed, name)
			}
			if entry.SHA == "" {
				continue
			}
			shallow.excludedTips = append(shallow.excludedTips, entry.SHA)
			specs = append(specs, fetchSpec{sha: entry.SHA, ref: full})
		}
	}

	promisor := false
	// The first attempt may stop at commits the client already has, because
	// their objects reach pack-objects through the alternate.
	graph, err := h.resolvePackGraph(ctx, specs, skipLocal)
	if err == nil {
		err = h.stageGraph(ctx, graph, st)
	} else {
		promisor = true
		err = h.stageUntilFound(ctx, wants, st, err)
	}
	if err != nil {
		return nil, cleanup, err
	}

	if missing := missingObjects(stage, wants); len(missing) > 0 {
		return nil, cleanup, fmt.Errorf("%w: %s is not something this remote can serve",
			errWantNotServed, shortSHA(missing[0]))
	}

	// Work out where the history is to be cut, now that there is a staged graph
	// to measure against. Not on the promisor path: those wants are blobs and
	// trees rather than commits, so there is no ancestry to measure, and git
	// does not use the answer — a lazy fetch is for named objects, whatever
	// depth the clone it belongs to was made at.
	var newBoundary, unshallow []string
	if shallow.wants() && !promisor {
		within, boundary, err := shallowBoundary(stage, wants, shallow)
		if err != nil {
			return nil, cleanup, err
		}
		newBoundary = boundary
		// A commit the client is shallow at stops being shallow once this pack
		// carries its parents — that is, once the new view reaches past it.
		inBoundary := make(map[string]bool, len(boundary))
		for _, oid := range boundary {
			inBoundary[oid] = true
		}
		for _, oid := range shallow.clientBoundary {
			if within[oid] && !inBoundary[oid] {
				unshallow = append(unshallow, oid)
			}
		}
		sort.Strings(unshallow)
	}

	// Cut the requested slice. --thin only when the client said it can cope;
	// git's own fetch runs index-pack --fix-thin, but the capability is how it
	// says so.
	// --revs makes stdin revisions to traverse from, so "<want>" pulls in
	// everything reachable and "^<have>" cuts it back. Without it stdin is an
	// exact object list. A promisor fetch wants the latter: git named the
	// objects it is missing, and traversing from them would pack a tree's whole
	// subtree to deliver the tree.
	args := []string{"pack-objects", "--stdout", "--quiet"}
	if !promisor {
		args = append(args, "--revs")
	}
	// A thin pack omits delta bases the client is known to have, which
	// pack-objects takes from the objects marked uninteresting — the "^<have>"
	// exclusions. A promisor fetch has none, so there is nothing for --thin to
	// leave out and passing it is simply inert; it is passed anyway, because
	// which of the two paths is running is not a reason to answer the client's
	// capability differently.
	if hasCapability(req.capabilities, "thin-pack") || req.has("thin-pack") {
		args = append(args, "--thin")
	}
	// Offset deltas only when the client said it can read them. Every git in
	// use sends `ofs-delta`, so assuming it went unnoticed — but the assumption
	// is not free: a client that omits it gets a pack whose deltas point at
	// bases by offset, which is not a format it agreed to and not one it can
	// necessarily parse. upload-pack asks the same question before passing the
	// flag, and this is the sort of thing that only ever breaks for whoever is
	// using jgit or a decade-old client.
	if hasCapability(req.capabilities, "ofs-delta") || req.has("ofs-delta") {
		args = append(args, "--delta-base-offset")
	}
	// The client's filter is applied where a filter can be applied: while the
	// pack is built. Once git receives a filtered pack it marks this remote a
	// promisor and comes back for the omitted objects as it needs them, which
	// arrives here as another fetch whose wants are blob ids.
	for _, spec := range req.values("filter ") {
		args = append(args, "--filter="+spec)
	}

	// pack-objects reads "--shallow <oid>" from the same stream as the revs and
	// treats those commits as parentless for the rest of the traversal. Which
	// boundary to register depends on what is being asked for.
	//
	// Deepening replaces the client's boundary, so only the new one is
	// registered — registering the old one too would stop the traversal at the
	// exact commit the client is asking to see past, and the pack would come
	// back with nothing new in it. The haves go the same way: a `^<have>`
	// excludes everything reachable from it, and for a shallow client that
	// sweeps up the ancestry it is missing and has just asked for. Re-sending
	// what it already holds is the cost of not having to reason about which of
	// its haves are complete; a depth-limited slice is small, and the one case
	// that is not — --unshallow — is a full transfer however it is served.
	//
	// An ordinary fetch from a shallow client is the opposite: its boundary
	// stands, so registering it keeps the traversal inside the history that
	// client actually has, and the haves can be trusted.
	var revs strings.Builder
	deepening := shallow.wants() && !promisor
	registered := shallow.clientBoundary
	if deepening {
		registered = newBoundary
	}
	for _, oid := range dedupe(append([]string(nil), registered...)) {
		revs.WriteString("--shallow " + oid + "\n")
	}
	for _, want := range wants {
		revs.WriteString(want + "\n")
	}
	if !promisor && !deepening {
		for _, have := range haves {
			// Only exclude what is actually present, or pack-objects fails on
			// an object id it cannot resolve.
			if h.hasLocally(have) {
				revs.WriteString("^" + have + "\n")
			}
		}
	}

	cmd := stagingGit(ctx, st.env, args...)
	cmd.Stdin = strings.NewReader(revs.String())
	// Keep what pack-objects says. It is the only account of why a pack came
	// out wrong, and discarding it turned an empty pack into a silent one.
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, cleanup, err
	}
	if err := cmd.Start(); err != nil {
		return nil, cleanup, fmt.Errorf("failed to run pack-objects: %w", err)
	}

	return &builtPack{
		ReadCloser: &packReader{ReadCloser: out, wait: cmd.Wait, stderr: &stderr},
		shallow:    newBoundary,
		unshallow:  unshallow,
	}, cleanup, nil
}

// builtPack is a packfile together with what the response has to say about it
// before the bytes start.
type builtPack struct {
	io.ReadCloser
	// shallow are the commits the client's history now stops at.
	shallow []string
	// unshallow are commits it was shallow at and no longer is.
	unshallow []string
}

// dedupe returns the input with repeats removed, order preserved.
func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// packReader waits for pack-objects when the caller is finished, so a failure
// in the subprocess surfaces rather than being lost with the pipe.
type packReader struct {
	io.ReadCloser
	wait   func() error
	stderr *strings.Builder
}

func (p *packReader) Close() error {
	closeErr := p.ReadCloser.Close()
	if err := p.wait(); err != nil {
		if msg := strings.TrimSpace(p.stderr.String()); msg != "" {
			return fmt.Errorf("pack-objects: %w: %s", err, msg)
		}
		return err
	}
	return closeErr
}

// missingObjects reports which of the given object ids the staged view cannot
// resolve, the staging object store and the repository's own alternate
// together. Asking cat-file is cheaper than discovering the answer from a pack
// that came out empty, and far easier to explain.
func missingObjects(stage string, oids []string) []string {
	store, err := git.OpenObjectStore(stage)
	if err != nil {
		// The store could not be read. Packing is still worth attempting;
		// refusing here would turn a diagnostic into an outage.
		return nil
	}
	return git.HasObjects(store, oids)
}

// stageGraph imports a resolved pack graph in dependency order.
//
// staged carries across calls so that a ref sharing a base with one already
// imported does not fetch and index it twice.
func (h *Helper) stageGraph(ctx context.Context, graph *packGraph, st *staging) error {
	levels, err := graph.importOrder()
	if err != nil {
		return err
	}

	total := 0
	for _, level := range levels {
		total += len(level)
	}
	done := 0
	for _, level := range levels {
		for _, sha := range level {
			if st.staged[sha] {
				continue
			}
			done++
			if st.progress {
				h.logInfo("git-remote-oci: staging packfile %d/%d (%s)...\n", done, total, shortSHA(sha))
			}
			if err := h.stagePackfile(ctx, sha, graph.manifests[sha], st); err != nil {
				return fmt.Errorf("failed to stage the packfile for %s: %w", shortSHA(sha), err)
			}
			st.staged[sha] = true
		}
	}
	return nil
}

// resolveRefName expands a ref shorthand against what the repository publishes.
//
// git passes `--shallow-exclude` through as the user typed it, so "base" has to
// find "refs/tags/base". The order is git's own DWIM order, and each candidate
// is checked against the published set rather than guessed at, because a name
// that resolves to nothing must be reported — an exclusion that silently covers
// nothing sends more history than was asked for.
func resolveRefName(name string, refs map[string]oci.RefEntry) (string, oci.RefEntry, bool) {
	candidates := []string{name}
	if !strings.HasPrefix(name, "refs/") {
		candidates = append(candidates,
			"refs/"+name,
			"refs/tags/"+name,
			"refs/heads/"+name,
		)
	}
	for _, candidate := range candidates {
		if entry, ok := refs[candidate]; ok {
			return candidate, entry, true
		}
	}
	return "", oci.RefEntry{}, false
}

// firstFilterSpec returns the client's object filter, or "" if it sent none.
//
// git sends at most one; taking the first rather than joining them keeps the
// value in the form the LFS size and blob:none checks already expect.
func firstFilterSpec(req v2Request) string {
	if specs := req.values("filter "); len(specs) > 0 {
		return strings.TrimSpace(specs[0])
	}
	return ""
}

// staging is what every step of assembling a fetch needs to know: where the
// objects are being put, what the client asked for, and whether to narrate it.
type staging struct {
	// dir is the staging git directory, read back with go-git.
	dir string
	// env points the git subprocesses at that directory.
	env []string
	// filter is the client's object filter, which decides whether Git LFS
	// blobs are downloaded alongside the pointers that name them.
	filter string
	// staged records which manifests have been imported, so a base shared by
	// two refs is not fetched and indexed twice.
	staged   map[string]bool
	progress bool
}

// stageUntilFound serves a promisor fetch: wants that are not addressable
// commits, which is git coming back for blobs a partial clone left out.
//
// There is no pack-base graph to follow from a blob, so the only way to find it
// is to stage history and look. Staging *all* of it works and is what this used
// to do, but the cost is the whole repository per lazy fetch, and a checkout
// makes one of those per batch of missing blobs. So it goes ref by ref and stops
// as soon as every want has turned up — which for the common case, a blob on the
// branch being checked out, is the first ref tried.
//
// resolveErr is what made this look like a promisor fetch in the first place; it
// is the error to report if there turns out to be nothing to stage.
func (h *Helper) stageUntilFound(ctx context.Context, wants []string, st *staging, resolveErr error) error {
	refs := mustRefs(ctx, h)
	if len(refs) == 0 {
		return resolveErr
	}

	h.logVerbose("git-remote-oci: [verbose] wants are not addressable commits; staging refs until they resolve\n")
	for _, name := range h.refsByLikelihood(ctx, refs) {
		entry := refs[name]
		if entry.SHA == "" {
			continue
		}
		// Every ref of a partial clone is a commit the client already holds, so
		// the local-store shortcut would call the graph satisfied, stage
		// nothing, and produce an empty pack for the one object the client is
		// missing — which git answers by asking again, forever.
		graph, err := h.resolvePackGraph(ctx, []fetchSpec{{sha: entry.SHA, ref: name}}, false)
		if err != nil {
			// One unreachable ref is not fatal here: the object may well be in
			// another. Only failing to find the wants at all is.
			h.logVerbose("git-remote-oci: [verbose] could not resolve %s: %v\n", name, err)
			continue
		}
		// The packfiles in this ref's graph each publish what they contain. If
		// none of them lists a wanted object, the whole ref can be skipped
		// without downloading a single pack — which is the difference between
		// reading a few kilobytes of index and indexing an entire history to
		// find out it was the wrong ref.
		//
		// Only when every manifest in the graph answers is the skip taken. A
		// manifest written before pack indexes existed says nothing about its
		// contents, and "nothing known" has to mean "cannot be ruled out", or a
		// repository pushed by an older build would have objects declared
		// missing that are sitting right there.
		if known, holds := h.graphMayHold(ctx, graph, wants); known && !holds {
			h.logVerbose("git-remote-oci: [verbose] %s does not hold the requested objects; skipping it\n", name)
			continue
		}

		if st.progress {
			h.logInfo("git-remote-oci: looking for the requested objects in %s...\n", name)
		}
		if err := h.stageGraph(ctx, graph, st); err != nil {
			return err
		}
		if len(missingObjects(st.dir, wants)) == 0 {
			h.logVerbose("git-remote-oci: [verbose] wants resolved after staging %s\n", name)
			return nil
		}
	}
	// Everything staged and still short: let the caller's own check name which
	// want could not be served.
	return nil
}

// graphMayHold asks a ref's packfiles whether any of them contains a wanted
// object, without downloading them.
//
// known is false as soon as one manifest cannot answer — no published index,
// or one that could not be read. The result is then not a decision but an
// absence of one, and the caller must fall back to staging and looking.
// Reporting "does not hold it" on incomplete information would tell a client
// its object does not exist when the pack holding it was simply never asked.
func (h *Helper) graphMayHold(ctx context.Context, graph *packGraph, wants []string) (known, holds bool) {
	defer h.timer.phase("read pack indexes")()

	if graph == nil || len(graph.manifests) == 0 {
		return false, false
	}
	for _, manifest := range graph.manifests {
		if manifest == nil {
			return false, false
		}
		index, ok := h.ociClient.FetchPackIndex(ctx, manifest)
		if !ok {
			return false, false
		}
		if oci.PackIndexContains(index, wants) {
			return true, true
		}
	}
	return true, false
}

// refsByLikelihood orders refs by how likely they are to hold an object a lazy
// fetch is asking for, so the search can stop early.
//
// A lazy fetch nearly always comes from a checkout of the default branch, and
// its objects are in that branch's history; tags and stale branches are the
// least likely and the most likely to be cheap to skip. Ordering is only a
// heuristic — every ref is still tried before giving up — so being wrong costs
// nothing but the ordering it saved.
func (h *Helper) refsByLikelihood(ctx context.Context, refs map[string]oci.RefEntry) []string {
	return orderRefsByLikelihood(h.v2Head(ctx, refs), refs)
}

// orderRefsByLikelihood is refsByLikelihood's ordering, separated from working
// out what HEAD is so the order itself can be checked without a registry.
func orderRefsByLikelihood(head string, refs map[string]oci.RefEntry) []string {
	var branches, others []string
	for name := range refs {
		switch {
		case name == head:
			continue
		case strings.HasPrefix(name, "refs/heads/"):
			branches = append(branches, name)
		default:
			others = append(others, name)
		}
	}
	sort.Strings(branches)
	sort.Strings(others)

	ordered := make([]string, 0, len(refs))
	if head != "" {
		if _, ok := refs[head]; ok {
			ordered = append(ordered, head)
		}
	}
	ordered = append(ordered, branches...)
	return append(ordered, others...)
}

// stagePackfile imports one manifest's packfile into the staging object store,
// and its Git LFS blobs into the repository.
//
// The packfile is staged and thrown away; the LFS blobs are not. They have to
// land in the client's own .git/lfs/objects, because that is where git-lfs
// looks and there is no LFS server behind an oci:// remote to ask later. So
// this is the one thing a v2 fetch leaves behind, deliberately: a pack carries
// the pointer, and a pointer without its object is a working tree full of
// hundred-byte stubs that fsck is perfectly happy with.
func (h *Helper) stagePackfile(ctx context.Context, sha string, manifest *ocispec.Manifest, st *staging) error {
	if manifest == nil {
		return nil
	}
	defer h.timer.phase("stage packfiles")()
	stream, err := h.ociClient.FetchPackfileStream(ctx, manifest)
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()

	cmd := stagingGit(ctx, st.env, "index-pack", "--stdin", "--fix-thin")
	cmd.Stdin = stream
	cmd.Stdout = io.Discard
	// Keep what index-pack says, as buildPackForWants does for pack-objects.
	// A packfile that fails to index is a registry serving something wrong,
	// and index-pack's one line about it -- a bad object, a truncated
	// stream, a missing base -- is the only account there is. Bounded,
	// because its stderr is a subprocess's to fill.
	stderr := &boundedBuffer{limit: 4096}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("index-pack: %w: %s", err, msg)
		}
		return fmt.Errorf("index-pack: %w", err)
	}

	return h.downloadLFSObjects(ctx, sha, manifest, st.filter)
}

// boundedBuffer keeps the first limit bytes written to it and drops the rest,
// so that a subprocess's diagnostics can be captured without letting the
// subprocess decide how much memory that takes.
type boundedBuffer struct {
	buf   []byte
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - len(b.buf); room > 0 {
		if len(p) > room {
			b.buf = append(b.buf, p[:room]...)
		} else {
			b.buf = append(b.buf, p...)
		}
	}
	// Reported as fully written: a short write would make the subprocess
	// see a broken pipe and stop, and the point is that it does not.
	return len(p), nil
}

func (b *boundedBuffer) String() string { return string(b.buf) }

// stagingParent picks where the staging object store is created.
//
// Inside $GIT_DIR rather than the system temp directory, because the staging
// area holds a copy of as much history as the fetch touches — for a clone, all
// of it. /tmp is a size-limited tmpfs on most systems, so a large clone would
// run it out of memory while the filesystem the objects are destined for has
// room to spare. It is also the same filesystem, which keeps the eventual write
// off the cross-device path.
//
// Falls back to the system default if the git directory cannot be located: a
// staging area somewhere is better than no fetch.
func (h *Helper) stagingParent() string {
	gitDir, ok := git.GitDir()
	if !ok {
		return ""
	}
	// A dedicated subdirectory keeps the temporary trees together and out of
	// the way of anything walking $GIT_DIR's top level.
	parent := filepath.Join(gitDir, "git-remote-oci-tmp")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return ""
	}
	return parent
}

// repositoryObjectDir locates the repository's own object store, which the
// staging area lists as an alternate so that `have` lines resolve.
func (h *Helper) repositoryObjectDir() (string, error) {
	objects, ok := git.ObjectsDir()
	if !ok {
		return "", fmt.Errorf("failed to locate the repository object store")
	}
	return objects, nil
}

// mustRefs returns the published refs, or nothing if they cannot be read.
func mustRefs(ctx context.Context, h *Helper) map[string]oci.RefEntry {
	refs, err := h.v2RemoteRefs(ctx)
	if err != nil {
		return nil
	}
	return refs
}

// refNamesByObject maps every published object id to a ref that points at it,
// including the tag object of an annotated tag.
func (h *Helper) refNamesByObject(ctx context.Context) map[string]string {
	refs, err := h.v2RemoteRefs(ctx)
	if err != nil {
		return nil
	}
	out := make(map[string]string, len(refs)*2)
	for name, entry := range refs {
		if entry.SHA != "" {
			out[entry.SHA] = name
		}
		// The tag object is what git asks for when it wants an annotated tag,
		// and it is never a manifest tag of its own.
		if entry.TagObject != "" {
			out[entry.TagObject] = name
		}
	}
	return out
}

// hasCapability reports whether the client declared a capability, allowing for
// the `name=value` form.
func hasCapability(caps []string, name string) bool {
	for _, c := range caps {
		if c == name || strings.HasPrefix(c, name+"=") {
			return true
		}
	}
	return false
}
