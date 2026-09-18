package oci

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	opencontainers "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// A packfile's object index: what it contains, published beside it.
//
// Every other annotation in this format is about commits — what a manifest's
// parents are, what its packfile was cut against, which ref it belongs to.
// None of them answer the question a partial clone asks, which is "which
// packfile holds this blob". Without an answer the only way to find out is to
// download packfiles and index them until the object turns up, which costs the
// repository to serve one object.
//
// So each packfile layer gets a sibling listing what is in it, and how large
// each one is. A reader fetches the index — tens of bytes per object, against
// the object itself — and can then both skip packs that cannot help and answer
// "how big is this object" without fetching anything at all.

const (
	// MediaTypeGitPackIndex is the original layer: object ids only.
	//
	// Still read, never written. A repository pushed by an older build has
	// these, and they answer the "is it worth fetching this pack" question
	// perfectly well; they simply cannot answer the size question.
	MediaTypeGitPackIndex = "application/vnd.git.repository.packindex.v1"

	// MediaTypeGitPackIndexV2 is the current layer: object ids and sizes.
	//
	// A separate media type rather than a wider v1, so that a reader can tell
	// from the *manifest* whether sizes are available without downloading the
	// blob to find out. That is what makes `object-info` answerable in one
	// round trip: the alternative is fetching an index only to discover it
	// predates sizes.
	MediaTypeGitPackIndexV2 = "application/vnd.git.repository.packindex.v2"

	// packIndexSizeDigits is the width of the size field in a v2 line.
	//
	// Fixed, and hex, because the uniform stride is the whole design: a reader
	// binary-searches the blob by seeking to a multiple of the line length.
	// Sixteen digits holds any uint64, so no object can ever fail to be
	// representable and force a ragged line.
	packIndexSizeDigits = 16
)

// PackIndexEntry is one object in a packfile.
type PackIndexEntry struct {
	OID  string
	Size int64
}

// packIndexLayout is what the stride of a blob tells a reader: how wide the ids
// are, and whether sizes follow them.
type packIndexLayout struct {
	stride   int
	oidWidth int
	hasSizes bool
}

// packIndexStride works out the layout from the first line.
//
// Four widths are meaningful, and nothing else is: an id and a newline for
// either hash algorithm, or an id, a space, sixteen hex digits and a newline.
// The algorithm is derived from the blob rather than configured, which is the
// rule the rest of the format follows — the ids are the record, so nothing can
// disagree with them.
func packIndexStride(data []byte) (packIndexLayout, bool) {
	i := bytes.IndexByte(data, '\n')
	switch i {
	case 40, 64:
		return packIndexLayout{stride: i + 1, oidWidth: i}, true
	case 40 + 1 + packIndexSizeDigits, 64 + 1 + packIndexSizeDigits:
		return packIndexLayout{stride: i + 1, oidWidth: i - 1 - packIndexSizeDigits, hasSizes: true}, true
	}
	return packIndexLayout{}, false
}

// oidAt returns the object id on line i.
func (l packIndexLayout) oidAt(index []byte, i int) string {
	return string(index[i*l.stride : i*l.stride+l.oidWidth])
}

// sizeAt returns the size on line i, and whether this layout records one.
func (l packIndexLayout) sizeAt(index []byte, i int) (int64, bool) {
	if !l.hasSizes {
		return 0, false
	}
	start := i*l.stride + l.oidWidth + 1
	size, err := strconv.ParseUint(string(index[start:start+packIndexSizeDigits]), 16, 64)
	if err != nil {
		return 0, false
	}
	// The field is wide enough for any uint64, and this blob came from a
	// registry, so it can hold a number no int64 can. Wrapping it would hand
	// back a negative size, which a caller reports to git as an object length.
	// An unrepresentable size is not a size: say so instead.
	if size > math.MaxInt64 {
		return 0, false
	}
	return int64(size), true
}

// EncodePackIndex renders sorted entries as an index blob.
//
// Fixed-width lines, so a reader can seek to the middle of the blob and
// binary-search without parsing what came before it. Hex rather than raw bytes
// because the width then says which hash algorithm this repository uses, and
// because an index that can be read with `less` is worth a few bytes.
func EncodePackIndex(entries []PackIndexEntry) []byte {
	// Normalise first, sort second. Sorting the input and lowercasing on the
	// way out looks equivalent and is not: 'A' sorts before 'a', so a mixed-case
	// input comes out in an order the reader's binary search does not expect,
	// and the search then reports objects absent that are sitting in the blob.
	// A false absence is the one answer this must never give.
	normalised := make([]PackIndexEntry, 0, len(entries))
	for _, entry := range entries {
		// Hex, not merely the right length. A forty-byte string that happens to
		// contain a newline would put the line break somewhere other than the
		// stride and make the whole blob unreadable -- which a reader has to
		// treat as "nothing known", silently costing the download this exists
		// to avoid. Dropping an entry is harmless by comparison: an absent id
		// only means a pack fetched that need not have been.
		if !isObjectID(entry.OID) {
			continue
		}
		// A negative size cannot be written in the fixed field and cannot be
		// true of an object. Recording zero would be a lie a reader would act
		// on, so the entry goes.
		if entry.Size < 0 {
			continue
		}
		normalised = append(normalised, PackIndexEntry{
			OID:  strings.ToLower(entry.OID),
			Size: entry.Size,
		})
	}
	sort.Slice(normalised, func(i, j int) bool { return normalised[i].OID < normalised[j].OID })

	// One line per object. A packfile holds an object once, so a repeated id is
	// a caller's mistake -- and left in, it makes the blob answer differently
	// depending on which copy a binary search happens to land on.
	deduped := normalised[:0]
	for i, entry := range normalised {
		if i > 0 && entry.OID == normalised[i-1].OID {
			continue
		}
		deduped = append(deduped, entry)
	}
	normalised = deduped

	// One width, or none at all. The stride is what makes the blob searchable
	// and it is read from the first line, so a SHA-1 id and a SHA-256 id in the
	// same index produce a ragged blob that is misread from the second entry
	// on. A repository is one algorithm throughout (FORMAT.md §10), so ids of
	// two widths are not a repository -- and publishing nothing leaves a reader
	// with "unknown", which it already knows how to handle, rather than an
	// index that answers confidently and wrongly.
	for _, entry := range normalised {
		if len(entry.OID) != len(normalised[0].OID) {
			return nil
		}
	}

	var buf bytes.Buffer
	for _, entry := range normalised {
		buf.WriteString(entry.OID)
		buf.WriteByte(' ')
		//nolint:gosec // G115: entries with a negative size are dropped above.
		fmt.Fprintf(&buf, "%0*x", packIndexSizeDigits, uint64(entry.Size))
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// searchPackIndex finds the line holding oid, if it is there.
func searchPackIndex(index []byte, layout packIndexLayout, oid string) (int, bool) {
	want := strings.ToLower(oid)
	if len(want) != layout.oidWidth {
		// A different width to this index: a SHA-256 id cannot be in a SHA-1
		// repository's pack.
		return 0, false
	}
	n := len(index) / layout.stride
	i := sort.Search(n, func(i int) bool { return layout.oidAt(index, i) >= want })
	if i < n && layout.oidAt(index, i) == want {
		return i, true
	}
	return 0, false
}

// PackIndexContains reports whether an index blob lists any of the given ids.
//
// Any, not all: the caller is deciding whether a packfile is worth fetching,
// and one wanted object in it is reason enough.
//
// A blob it cannot make sense of reports true. The index is an optimisation,
// and the safe direction to fail in is fetching a pack that turns out not to
// help — the alternative is skipping the pack that had the object and telling
// the client its object does not exist.
func PackIndexContains(index []byte, oids []string) bool {
	layout, ok := packIndexStride(index)
	if !ok {
		// Unreadable and not empty: nothing is known about this pack, so it
		// cannot be ruled out. An empty index is different -- it was written,
		// and it says the pack holds nothing.
		return len(index) != 0
	}
	for _, oid := range oids {
		if _, found := searchPackIndex(index, layout, oid); found {
			return true
		}
	}
	return false
}

// PackIndexSize reports the recorded size of an object.
//
// ok is false when the object is not listed, or when the index predates sizes
// (§4.4) and so records none. Both mean the same to a caller: this index cannot
// answer, ask something that can.
//
// There is deliberately no "unknown means zero" here. A size is either known or
// it is not, and a caller that acted on a fabricated zero would report an empty
// object rather than fetch a real one.
func PackIndexSize(index []byte, oid string) (int64, bool) {
	layout, ok := packIndexStride(index)
	if !ok || !layout.hasSizes {
		return 0, false
	}
	i, found := searchPackIndex(index, layout, oid)
	if !found {
		return 0, false
	}
	return layout.sizeAt(index, i)
}

// PushPackIndex uploads an index blob and returns its descriptor, for the
// caller to attach to the manifest as a layer.
func (c *Client) PushPackIndex(ctx context.Context, entries []PackIndexEntry) (ocispec.Descriptor, error) {
	data := EncodePackIndex(entries)
	if len(data) == 0 {
		return ocispec.Descriptor{}, nil
	}

	desc := ocispec.Descriptor{
		MediaType: MediaTypeGitPackIndexV2,
		Digest:    opencontainers.FromBytes(data),
		Size:      int64(len(data)),
	}
	if err := c.pushBlobOnce(ctx, desc, data); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("failed to push the pack index: %w", err)
	}
	return desc, nil
}

// PackIndexLayer returns a manifest's pack index layer, if it has one.
//
// Either version: a v1 index still answers "is this pack worth fetching",
// which is what most callers want. Manifests written before either existed have
// neither, and a reader must treat that as "unknown" rather than "empty" — see
// FetchPackIndex.
func PackIndexLayer(manifest *ocispec.Manifest) (ocispec.Descriptor, bool) {
	if manifest == nil {
		return ocispec.Descriptor{}, false
	}
	for _, layer := range manifest.Layers {
		if layer.MediaType == MediaTypeGitPackIndexV2 || layer.MediaType == MediaTypeGitPackIndex {
			return layer, true
		}
	}
	return ocispec.Descriptor{}, false
}

// PackIndexRecordsSizes reports whether a manifest's index can answer size
// questions, without fetching it.
//
// This is the point of giving v2 its own media type. A caller answering
// `object-info` can see from the manifest it already holds whether the index is
// worth a request, instead of spending the request to find out it is a v1.
func PackIndexRecordsSizes(manifest *ocispec.Manifest) bool {
	if manifest == nil {
		return false
	}
	for _, layer := range manifest.Layers {
		if layer.MediaType == MediaTypeGitPackIndexV2 {
			return true
		}
	}
	return false
}

// maxPackIndexBytes bounds a pack index read into memory.
//
// It is its own limit rather than maxMetadataBytes because an index is
// proportional to the *objects* in a packfile, not to the refs in the
// repository: at 58 bytes a line, a gigabyte is about eighteen million objects
// in one pack, which is past the largest packs in existence. As with the
// metadata limit it is a backstop against a descriptor lying about its size,
// not a quota.
const maxPackIndexBytes = 1 << 30

// FetchPackIndex downloads a manifest's pack index.
//
// ok is false when the manifest has no index or it could not be read, which
// are the same thing to a caller: nothing is known about what that packfile
// holds, so it cannot be ruled out. A repository pushed by an older build has
// no indexes at all and must keep working, just without the shortcut.
//
// The blob is verified against the descriptor's digest before it is used. An
// index that does not hash to what the manifest claims is not an index a
// reader can act on: acting on it is how a pack that held the object gets
// skipped, and that is the one answer this must never give.
func (c *Client) FetchPackIndex(ctx context.Context, manifest *ocispec.Manifest) ([]byte, bool) {
	desc, found := PackIndexLayer(manifest)
	if !found {
		return nil, false
	}
	data, err := c.fetchBlobBytes(ctx, desc, maxPackIndexBytes, "a pack index")
	if err != nil {
		return nil, false
	}
	return data, true
}
