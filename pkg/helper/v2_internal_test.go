package helper

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// The end-to-end tests in v2_test.go prove that git accepts what this server
// sends, which is the thing that ultimately matters. What they cannot do is say
// *why* a byte stream was wrong when it is: a mistake in parsing a request
// surfaces as a hang or a missing object several steps later. These cover the
// decisions directly.

func TestReadV2RequestSplitsCapabilitiesFromArguments(t *testing.T) {
	// The delimiter is the whole of the distinction: lines before it describe
	// the client, lines after it are the command's arguments. A reader that
	// ignores it reads "want <oid>" as a capability and asks for nothing.
	var buf strings.Builder
	w := newPktWriter(&buf)
	for _, line := range []string{"command=fetch", "agent=git/2.43", "object-format=sha1"} {
		if err := w.WriteLine("%s", line); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := w.Delim(); err != nil {
		t.Fatalf("delim: %v", err)
	}
	for _, line := range []string{"thin-pack", "want abc123", "done"} {
		if err := w.WriteLine("%s", line); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	req, err := readV2Request(newPktReader(strings.NewReader(buf.String())))
	if err != nil {
		t.Fatalf("readV2Request: %v", err)
	}
	if req.command != "fetch" {
		t.Errorf("command = %q, want fetch", req.command)
	}
	if want := []string{"agent=git/2.43", "object-format=sha1"}; !reflect.DeepEqual(req.capabilities, want) {
		t.Errorf("capabilities = %q, want %q", req.capabilities, want)
	}
	if want := []string{"thin-pack", "want abc123", "done"}; !reflect.DeepEqual(req.args, want) {
		t.Errorf("args = %q, want %q", req.args, want)
	}
	if !req.has("done") {
		t.Error("done was not recognised, so the acknowledgments section would be sent when it must be omitted")
	}
	if got := req.values("want "); !reflect.DeepEqual(got, []string{"abc123"}) {
		t.Errorf("wants = %q, want [abc123]", got)
	}
}

func TestParseShallowArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want shallowArgs
	}{
		{
			name: "an ordinary fetch asks for no depth",
			args: []string{"want abc", "done"},
		},
		{
			// "deepen-since" begins with "deepen", and a prefix match that is
			// not anchored on the space would read it as a depth of 0 and serve
			// an unlimited fetch while the client recorded a boundary.
			name: "deepen is not confused by deepen-since",
			args: []string{"deepen-since 1234567", "want abc"},
			want: shallowArgs{since: time.Unix(1234567, 0)},
		},
		{
			name: "deepen-not names refs to exclude",
			args: []string{"deepen-not refs/tags/v1", "deepen-not refs/heads/old", "want abc"},
			want: shallowArgs{exclude: []string{"refs/tags/v1", "refs/heads/old"}},
		},
		{
			// The shallowest cut wins, so all three can arrive together.
			name: "a depth, a date and an exclusion together",
			args: []string{"deepen 3", "deepen-since 100", "deepen-not refs/heads/old", "want abc"},
			want: shallowArgs{deepen: 3, since: time.Unix(100, 0), exclude: []string{"refs/heads/old"}},
		},
		{
			// git has already parsed whatever the user typed, so an
			// unparseable timestamp is a bug rather than input — and ignoring
			// it would serve an unlimited history to someone who asked for a
			// slice.
			name: "an unparseable date is refused, not ignored",
			args: []string{"deepen-since not-a-number", "want abc"},
			want: shallowArgs{unsupported: "deepen-since"},
		},
		{
			// `git fetch --deepen=2`. The depth is the same number; what
			// changes is where it is counted from, which the flag records and
			// the boundary walk acts on.
			name: "deepen-relative is a depth counted from the client's boundary",
			args: []string{"deepen-relative", "deepen 2", "shallow aaa", "want abc"},
			want: shallowArgs{deepen: 2, relative: true, clientBoundary: []string{"aaa"}},
		},
		{
			name: "a depth and an existing boundary",
			args: []string{"shallow aaa", "shallow bbb", "deepen 3", "want abc"},
			want: shallowArgs{deepen: 3, clientBoundary: []string{"aaa", "bbb"}},
		},
		{
			// --unshallow. The value has to survive as an int rather than
			// overflow or be used to size anything.
			name: "unshallow asks for the maximum depth",
			args: []string{"shallow aaa", "deepen 2147483647", "want abc"},
			want: shallowArgs{deepen: 2147483647, clientBoundary: []string{"aaa"}},
		},
		{
			name: "a shallow client fetching without deepening",
			args: []string{"shallow aaa", "want abc", "have def"},
			want: shallowArgs{clientBoundary: []string{"aaa"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseShallowArgs(v2Request{command: "fetch", args: tc.args})
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseShallowArgs = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestDedupePreservesOrder(t *testing.T) {
	got := dedupe([]string{"b", "a", "b", "c", "a"})
	if want := []string{"b", "a", "c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("dedupe = %q, want %q", got, want)
	}
}

func TestMatchesAnyPrefix(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ref      string
		prefixes []string
		want     bool
	}{
		// No prefixes means git wants the lot, not that nothing is in scope.
		{"no prefixes matches everything", "refs/heads/main", nil, true},
		{"a matching prefix", "refs/heads/main", []string{"refs/heads/"}, true},
		{"a non-matching prefix", "refs/tags/v1", []string{"refs/heads/"}, false},
		{"one of several", "refs/tags/v1", []string{"refs/heads/", "refs/tags/"}, true},
		{"HEAD in scope", "HEAD", []string{"HEAD"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchesAnyPrefix(tc.ref, tc.prefixes); got != tc.want {
				t.Errorf("matchesAnyPrefix(%q, %q) = %v, want %v", tc.ref, tc.prefixes, got, tc.want)
			}
		})
	}
}

func TestHasCapabilityAcceptsTheValueForm(t *testing.T) {
	caps := []string{"agent=git/2.43", "thin-pack", "object-format=sha1"}
	for _, tc := range []struct {
		name string
		cap  string
		want bool
	}{
		{"a bare capability", "thin-pack", true},
		{"a capability carrying a value", "agent", true},
		{"one that was not sent", "sideband-all", false},
		// "object-form" is a prefix of "object-format", and matching on prefix
		// alone would report a capability the client never declared.
		{"a prefix of one that was sent", "object-form", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasCapability(caps, tc.cap); got != tc.want {
				t.Errorf("hasCapability(%q) = %v, want %v", tc.cap, got, tc.want)
			}
		})
	}
}

// TestWriteAcknowledgementsShape pins the section's grammar. NAK and ACK are
// mutually exclusive, and the closing "ready" is what tells git the packfile
// follows in this same response rather than after another round.
func TestWriteAcknowledgementsShape(t *testing.T) {
	for _, tc := range []struct {
		name   string
		common []string
		want   []string
	}{
		{"nothing in common", nil, []string{"acknowledgments", "NAK", "ready"}},
		{"one common commit", []string{"aaa"}, []string{"acknowledgments", "ACK aaa", "ready"}},
		{"several", []string{"aaa", "bbb"}, []string{"acknowledgments", "ACK aaa", "ACK bbb", "ready"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf strings.Builder
			if err := writeAcknowledgements(newPktWriter(&buf), tc.common); err != nil {
				t.Fatalf("writeAcknowledgements: %v", err)
			}
			got := readAllLines(t, buf.String())
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("section = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSendV2ErrorIsRecognisable pins the one packet git surfaces to the user
// verbatim. Without the "ERR " prefix the message is read as protocol content
// and the fetch fails with something unrelated.
func TestSendV2ErrorIsRecognisable(t *testing.T) {
	var buf strings.Builder
	if err := sendV2Error(newPktWriter(&buf), "no such object"); err != nil {
		t.Fatalf("sendV2Error: %v", err)
	}
	lines := readAllLines(t, buf.String())
	if len(lines) != 1 || lines[0] != "ERR no such object" {
		t.Fatalf("lines = %q, want [\"ERR no such object\"]", lines)
	}
	// The response still has to be terminated, or git waits for a packet that
	// never arrives instead of reporting the error.
	if !strings.HasSuffix(buf.String(), "00000002") {
		t.Errorf("error response does not end with a flush and response-end: %q", buf.String())
	}
}

// readAllLines decodes the data packets of a framed stream, ignoring the
// special lengths.
func readAllLines(t *testing.T, stream string) []string {
	t.Helper()
	r := newPktReader(strings.NewReader(stream))
	var out []string
	for {
		line, kind, err := r.ReadLine()
		if err != nil {
			return out
		}
		if kind == pktData {
			out = append(out, line)
		}
	}
}

// TestMissingObjects covers the check that stands between an unservable want
// and an infinite loop. git answers a well-formed empty pack by asking again on
// the next access, so a want the staging area cannot produce has to be found
// before the pack is built, not inferred from the pack afterwards.
func TestMissingObjects(t *testing.T) {
	repo := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "one")
	commit := run("rev-parse", "HEAD")
	blob := run("rev-parse", "HEAD:a.txt")

	// The object store is read directly, so the probe is pointed at the git
	// directory rather than run from inside it.
	gitDir := filepath.Join(repo, ".git")

	absent := "0000000000000000000000000000000000000000"
	for _, tc := range []struct {
		name string
		oids []string
		want []string
	}{
		{"everything present", []string{commit, blob}, nil},
		{"one absent", []string{commit, absent}, []string{absent}},
		// Which one is missing has to survive, not just how many: the caller
		// names it in the error the client is sent.
		{"absent first", []string{absent, commit}, []string{absent}},
		{"all absent", []string{absent}, []string{absent}},
		// A want that is not an object id at all cannot be served either, and
		// must not reach the object store as a lookup.
		{"not an object id", []string{"HEAD"}, []string{"HEAD"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := missingObjects(gitDir, tc.oids)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("missingObjects(%v) = %v, want %v", tc.oids, got, tc.want)
			}
		})
	}
}

// TestOrderRefsByLikelihood pins the search order a promisor fetch uses. It is
// only a heuristic — every ref is tried before the fetch gives up — but it is
// what decides whether serving one lazy blob costs one ref's history or the
// whole repository's, so getting the common case first is the entire point.
func TestOrderRefsByLikelihood(t *testing.T) {
	refs := map[string]oci.RefEntry{
		"refs/heads/main":    {SHA: "a"},
		"refs/heads/feature": {SHA: "b"},
		"refs/heads/old":     {SHA: "c"},
		"refs/tags/v1":       {SHA: "d"},
		"refs/tags/v2":       {SHA: "e"},
	}

	for _, tc := range []struct {
		name string
		head string
		want []string
	}{
		{
			// The branch being checked out is where a lazily fetched blob
			// almost always lives, so it is tried first.
			name: "HEAD leads, then branches, then everything else",
			head: "refs/heads/main",
			want: []string{
				"refs/heads/main", "refs/heads/feature", "refs/heads/old",
				"refs/tags/v1", "refs/tags/v2",
			},
		},
		{
			name: "a non-default HEAD still leads and is not repeated",
			head: "refs/heads/feature",
			want: []string{
				"refs/heads/feature", "refs/heads/main", "refs/heads/old",
				"refs/tags/v1", "refs/tags/v2",
			},
		},
		{
			// A repository with no branches left records no HEAD; the order is
			// then simply every ref, and none may be dropped.
			name: "no HEAD recorded",
			head: "",
			want: []string{
				"refs/heads/feature", "refs/heads/main", "refs/heads/old",
				"refs/tags/v1", "refs/tags/v2",
			},
		},
		{
			// A HEAD naming a ref that is not there must not add a phantom
			// entry, or the search would try to stage something absent.
			name: "HEAD names a ref that is gone",
			head: "refs/heads/deleted",
			want: []string{
				"refs/heads/feature", "refs/heads/main", "refs/heads/old",
				"refs/tags/v1", "refs/tags/v2",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := orderRefsByLikelihood(tc.head, refs)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("order = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSendSidebandErrorUsesBandThree covers the only way left to report a
// failure once the packfile section has begun.
//
// By then an ERR packet is no longer possible — the client is reading sideband
// frames, so "ERR ..." would be read as pack bytes and corrupt the stream it
// was trying to warn about. Band 3 is the channel git treats as fatal. This is
// error handling that only ever runs when something has already gone wrong,
// which is exactly the kind that goes unnoticed when it is broken.
func TestSendSidebandErrorUsesBandThree(t *testing.T) {
	var buf strings.Builder
	if err := sendSidebandError(newPktWriter(&buf), "pack-objects died"); err != nil {
		t.Fatalf("sendSidebandError: %v", err)
	}

	payload, kind, err := newPktReader(strings.NewReader(buf.String())).Read()
	if err != nil || kind != pktData {
		t.Fatalf("first packet is (%v, %v), want a data packet", kind, err)
	}
	if len(payload) == 0 {
		t.Fatal("empty payload; git would see a band byte of nothing")
	}
	if payload[0] != sidebandError {
		t.Errorf("band is %d, want %d — git only treats band 3 as fatal, and would take band 1 for pack bytes",
			payload[0], sidebandError)
	}
	if got := string(payload[1:]); got != "pack-objects died" {
		t.Errorf("message = %q, want it carried verbatim after the band byte", got)
	}

	// Terminated, or git waits for a packet that never comes instead of
	// reporting the failure it was just told about.
	if !strings.HasSuffix(buf.String(), "00000002") {
		t.Errorf("response does not end with a flush and response-end: %q", buf.String())
	}
}

// TestSendV2ErrorTruncatesAnOversizedMessage: a registry's error body can run
// to pages, and an ERR packet that cannot be framed is no message at all.
func TestSendV2ErrorTruncatesAnOversizedMessage(t *testing.T) {
	var buf strings.Builder
	if err := sendV2Error(newPktWriter(&buf), strings.Repeat("x", pktMaxPayload*2)); err != nil {
		t.Fatalf("an oversized message was not truncated but refused: %v", err)
	}
	payload, kind, err := newPktReader(strings.NewReader(buf.String())).Read()
	if err != nil || kind != pktData {
		t.Fatalf("first packet is (%v, %v), want a data packet", kind, err)
	}
	if len(payload) != pktMaxPayload {
		t.Errorf("payload is %d bytes, want it cut to the pkt-line maximum of %d", len(payload), pktMaxPayload)
	}
	if !strings.HasPrefix(string(payload), "ERR ") {
		t.Errorf("truncation lost the ERR marker: %q", payload[:8])
	}
	if !strings.HasSuffix(buf.String(), "00000002") {
		t.Errorf("error response does not end with a flush and response-end: %q", buf.String()[len(buf.String())-16:])
	}
}

// TestSendSidebandErrorTruncatesAnOversizedMessage: the band byte has to fit in
// the same packet as the text, so a message at the limit would otherwise be
// refused by the writer and the client would learn nothing at all.
func TestSendSidebandErrorTruncatesAnOversizedMessage(t *testing.T) {
	var buf strings.Builder
	if err := sendSidebandError(newPktWriter(&buf), strings.Repeat("x", pktMaxPayload*2)); err != nil {
		t.Fatalf("an oversized message was not truncated but refused: %v", err)
	}
	payload, kind, err := newPktReader(strings.NewReader(buf.String())).Read()
	if err != nil || kind != pktData {
		t.Fatalf("first packet is (%v, %v), want a data packet", kind, err)
	}
	if len(payload) != pktMaxPayload {
		t.Errorf("payload is %d bytes, want it cut to the pkt-line maximum of %d", len(payload), pktMaxPayload)
	}
	if payload[0] != sidebandError {
		t.Errorf("truncation lost the band byte: got %d", payload[0])
	}
}
