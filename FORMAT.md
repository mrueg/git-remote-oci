# Git Repositories as OCI Artifacts

**Specification version 1**, recorded as `io.git-remote-oci.format-version` on the `_refs` index
(§6) and the `_index` image index (§7).

This specification describes how a Git repository is laid out inside an OCI registry, so that the
layout can be inspected, reimplemented, or argued with independently of the Go code that happens to
write it today. It is written for someone building a second implementation.

> **Status: not stable.** This is pre-1.0 software. There is no compatibility path and no
> negotiation: a reader that meets a version it does not implement **refuses the repository** rather
> than guessing.
>
> The version is a tripwire against a layout this build would misread, not a release counter. It
> moves when — and only when — a change would make an older reader wrong; see §11. This document is
> the changelog.
>
> Where this document *describes* what the implementation does and the two disagree, the code is
> the truth and this document is a bug. Where it states a *requirement* the implementation does
> not meet, the implementation is the bug. The distinction matters now that the requirements
> below are normative: a spec that yields to the code cannot be conformed to.

---

## Table of Contents

- [Notational Conventions](#notational-conventions)
- [Terminology](#terminology)
- [1. Overview](#1-overview)
- [2. Object model](#2-object-model)
  - [2.1 Media types](#21-media-types)
- [3. Ref names to tags](#3-ref-names-to-tags)
  - [3.1 Over-long refs](#31-over-long-refs)
  - [3.2 Refs that look like commit ids](#32-refs-that-look-like-commit-ids)
- [4. Commit manifests](#4-commit-manifests)
  - [4.1 The packfile layer](#41-the-packfile-layer)
  - [4.2 Pack bases](#42-pack-bases)
  - [4.3 Shallow snapshot layer (optional)](#43-shallow-snapshot-layer-optional)
  - [4.4 Pack index layer (optional)](#44-pack-index-layer-optional)
  - [4.5 LFS layers](#45-lfs-layers)
- [5. Ref manifests](#5-ref-manifests)
- [6. `_refs` — the ref index](#6-_refs--the-ref-index)
  - [6.1 Pack chain layer (optional)](#61-pack-chain-layer-optional)
  - [6.2 `io.git-remote-oci.head`](#62-iogit-remote-ocihead)
- [7. `_index` — the OCI image index](#7-_index--the-oci-image-index)
- [8. Deleting a ref](#8-deleting-a-ref)
- [9. Locks](#9-locks)
  - [9.1 `_lfs_locks` — Git LFS file locks](#91-_lfs_locks--git-lfs-file-locks)
- [10. What the format does not do](#10-what-the-format-does-not-do)
- [11. Changing the format](#11-changing-the-format)
- [Conformance](#conformance)
- [Appendix A: Annotation registry](#appendix-a-annotation-registry)
- [Appendix B: Media type registry](#appendix-b-media-type-registry)
- [Appendix C: References](#appendix-c-references)

---

## Notational Conventions

The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT", "SHOULD", "SHOULD NOT",
"RECOMMENDED", "NOT RECOMMENDED", "MAY", and "OPTIONAL" in this document are to be interpreted as
described in [RFC 2119](https://www.rfc-editor.org/rfc/rfc2119) and
[RFC 8174](https://www.rfc-editor.org/rfc/rfc8174) when, and only when, they appear in all capitals.

Paragraphs introduced by ***Rationale*** are **non-normative**. They explain why a requirement is
what it is, and an implementation is not judged on them. Everything else in a numbered section is
normative.

Requirements are addressed to one of two roles, defined in [Terminology](#terminology): a **writer**
publishes a repository, a **reader** consumes one. A requirement that names neither applies to both.

Byte strings are written in the form `"literal"`. Object ids are written lowercase throughout; see
§2 for the case rule.

---

## Terminology

**Repository** — one OCI repository, addressed as `oci://<registry>/<namespace>/<name>`, holding one
Git repository. Repositories are self-contained: nothing in this format refers across them.

**Writer** — an implementation that publishes to a repository: pushing refs, updating indexes,
deleting refs, compacting.

**Reader** — an implementation that consumes a repository: listing refs, fetching objects.

**Commit manifest** — an OCI image manifest tagged with a commit id, carrying that push's packfile.
See §4.

**Ref manifest** — an OCI image manifest tagged with an encoded ref name, recording where a ref
points. See §5.

**Pack base** — a commit whose packfile a later packfile was cut against, and without which that
later packfile is incomplete. See §4.2.

**Object id** — a Git object name: 40 lowercase hex characters for SHA-1, 64 for SHA-256.

**Tombstone** — a manifest that records the absence of the thing its tag names, used where a
registry refuses deletion. See §8 for refs and §9 for locks.

---

## 1. Overview

A registry is not a filesystem. It offers three things: content-addressed blobs, manifests that
reference blobs, and mutable tags that name manifests. It offers no directories, no transactions,
and no compare-and-swap. Everything below is a consequence.

A repository is stored as a set of OCI image manifests under tags in a single OCI repository. Git
objects travel as standard packfiles in manifest layers. Where a ref points, and what a packfile
depends on, are recorded in manifest annotations and in two index tags.

***Rationale.*** Two constraints shape the design more than anything else:

- **A tag is the only mutable name**, so every mutable thing — a ref, an index, a lock — is a tag,
  and every update to one is a read-modify-write with no atomicity.
- **A packfile is opaque to the registry.** The registry cannot tell that one packfile depends on
  objects in another, so if a packfile omits objects, something in the format has to say what it
  omitted and where to find them. That is what §4.2 exists for.

---

## 2. Object model

A repository occupies one OCI repository. Everything lives under tags in that one repository.

| Tag | Holds | Mutable |
| :--- | :--- | :--- |
| `<commit id>` (40 or 64 hex) | commit manifest — a packfile and what it depends on | no, written once |
| `<encoded ref name>` | ref manifest — where a ref points, plus display metadata | yes |
| `_refs` | the ref index: the authoritative list of refs | yes |
| `_index` | an OCI image index over the same refs, for generic tooling | yes |
| `_lfs_locks` | Git LFS lock records | yes |
| `_lock_<encoded ref name>` | advisory ref lock | yes |

The tags `_refs`, `_index` and `_lfs_locks`, and every tag beginning with `_lock_`, are RESERVED. A
writer MUST NOT publish a ref manifest under a reserved tag, and everything it publishes that is
neither a commit manifest nor a ref manifest — the indexes, the locks — MUST live under a reserved
tag. Beginning with `_` is not by itself what makes a tag reserved: the ref-name encoding (§3) also
uses that namespace, for its `_t_`, `_r_`, `_x_` and `_h_` prefixes and for a branch whose own name
begins with `_`.

Object ids appearing in tags, annotations and index blobs MUST be lowercase hexadecimal. A reader
MAY accept uppercase on input; a writer MUST NOT emit it.

A repository MUST use one hash algorithm throughout. A reader derives the algorithm from the width
of the ids it finds, and MUST NOT rely on any recorded field, because there is none.

***Rationale.*** The ref-name encoding in §3 can never produce a bare `_` prefix followed by
anything other than `_`, `t_`, `r_`, `x_` or `h_`, so a ref can never collide with a reserved tag.
That is the entire protection, which is why everything mutable has to live inside the namespace it
guards. Locks were once tagged `lock-<encoded ref name>`, outside it — and `refs/heads/lock-main`
encodes to `lock-main`, exactly the lock tag for `refs/heads/main`. Pushing `main` overwrote that
branch's ref manifest with a lock, and a collector classifying tags by prefix would prune the branch
as a released lock.

Deriving the hash algorithm from the ids rather than recording it means the two cannot disagree.

### 2.1 Media types

| Media type | Used for |
| :--- | :--- |
| `application/vnd.git.repository.packfile.v1` | packfile layer, stored raw |
| `application/vnd.git.repository.packfile.v1+gzip` | packfile layer, gzip |
| `application/vnd.git.repository.packfile.v1+zstd` | packfile layer, zstd |
| `application/vnd.git.repository.packindex.v1` | a packfile layer's object ids (read-only; superseded) |
| `application/vnd.git.repository.packindex.v2` | a packfile layer's object ids and their sizes |
| `application/vnd.git.repository.packchain.v1+json` | the whole pack-base graph, on `_refs` |
| `application/vnd.git.repository.index.v1+json` | the `_refs` index blob |
| `application/vnd.git.lfs.v1+blob` | a Git LFS object |
| `application/vnd.oci.image.config.v1+json` | config blob on every manifest |
| `application/vnd.oci.empty.v1+json` | the placeholder layer on manifests with no payload |

The `vnd.git.*` types are specific to this project and are not registered with IANA.

A reader MUST select a decompressor from the layer's media type. A writer MUST label a layer with
the compression it actually applied; a manifest that labels a layer `+gzip` and stores zstd is
malformed.

***Rationale.*** This implementation's buffered read path additionally sniffs magic bytes, but
nothing should rely on that: a streaming reader cannot, and a conforming writer never makes it
necessary.

---

## 3. Ref names to tags

An OCI tag MUST match `[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}`. A Git ref name may contain `/` and much
else, and the same short name may exist as both a branch and a tag. The mapping MUST therefore be
**injective**: two distinct refs MUST NOT share a tag. §3.1 is the one qualified exception.

```
refs/heads/<name>  ->           escape(<name>)
refs/tags/<name>   ->  "_t_" +  escape(<name>)
refs/<rest>        ->  "_r_" +  escape(<rest>)
<anything else>    ->  "_x_" +  escape(<name>)
```

`escape()` passes `[a-zA-Z0-9.-]` through unchanged, doubles `_` to `__`, and renders every other
byte as `_` followed by two lowercase hex digits.

| Ref | Tag |
| :--- | :--- |
| `refs/heads/main` | `main` |
| `refs/heads/release-1.2` | `release-1.2` |
| `refs/heads/feature/login` | `feature_2flogin` |
| `refs/heads/my_branch` | `my__branch` |
| `refs/tags/v1.0.0` | `_t_v1.0.0` |
| `refs/notes/commits` | `_r_notes_2fcommits` |
| `HEAD` | `_x_HEAD` |

***Rationale.*** Sharing a tag means one ref silently overwrites the other's manifest. Because an
encoded branch name escapes a leading `_` to `__`, an encoded name can never be mistaken for the
`_t_` / `_r_` / `_x_` markers, and the common case stays readable: `refs/heads/main` is the tag
`main`.

### 3.1 Over-long refs

A ref whose encoding exceeds 128 bytes MUST be stored as:

```
"_h_" + <encoded prefix> + "-" + <first 8 hex digits of sha256(ref name)>
```

This is **lossy**, and it is the one place the mapping is not injective by construction: the tag
cannot be decoded back to a ref name, so such refs are discoverable only through `_refs` (§6), and
two distinct long refs receive distinct tags unless the first 32 bits of their digests collide. That
is collision-resistant — unique in practice — rather than guaranteed.

A reader enumerating tags MUST NOT attempt to decode a `_h_` tag to a ref name.

***Rationale.*** The `_h_` marker is load-bearing. A truncated tag ends in `-<hex>`, which is also
perfectly ordinary content, so without a reserved marker a truncated long ref and a short ref
spelled exactly like the truncation would produce the same tag. `escape()` can never emit `_h`,
because a lone `_` is always followed by `_` or two hex digits and `h` is not hex.

### 3.2 Refs that look like commit ids

A ref whose encoded tag is 40 or 64 hexadecimal characters, in either case, would collide with the
commit-manifest namespace. Such a ref manifest MUST be published under `ref-<tag>` instead.

---

## 4. Commit manifests

A commit manifest is tagged with the commit id. It MUST be written once and MUST NOT be rewritten
with different content, except by a writer performing consolidation (§10, §4.2).

A writer MUST publish a commit manifest for the tip of each pushed refspec. A writer MAY publish
manifests for no other commit, and this implementation does not: commits between one push and the
next have no manifest of their own and are carried inside the packfile. A reader MUST NOT assume an
arbitrary commit is addressable.

```jsonc
{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "config": { "mediaType": "application/vnd.oci.image.config.v1+json", ... },
  "layers": [
    { "mediaType": "application/vnd.git.repository.packfile.v1", ... },
    { "mediaType": "application/vnd.git.repository.packindex.v2", ... },  // optional, see 4.4
    { "mediaType": "application/vnd.git.lfs.v1+blob", ... }   // zero or more
  ],
  "annotations": {
    "org.opencontainers.image.revision": "<commit id>",
    "org.opencontainers.image.title":    "<commit id>",
    "org.opencontainers.image.vendor":   "git-remote-oci",
    "io.git-remote-oci.pack-bases":      "none",               // see 4.2
    "io.git-remote-oci.parents":         "<id>,<id>"           // optional, metadata
  }
}
```

The config blob MUST be a valid OCI image config with platform `unknown/unknown`. Its
`rootfs.diff_ids` MUST hold the digest of the **uncompressed** packfile, which differs from the
layer digest whenever compression is on.

***Rationale.*** The config blob exists so that generic registry tooling can render the manifest;
nothing in this format reads it back.

### 4.1 The packfile layer

Layer 0 MUST be the packfile: a standard Git packfile as produced by `git pack-objects`, optionally
compressed as a whole (§2.1). It is incremental — it contains the objects reachable from this commit
minus those reachable from the commits named in `pack-bases`.

The packfile MAY be **thin**: objects MAY be stored as deltas whose base is not in the pack, but in
one of the packs named by `pack-bases`.

***Rationale.*** Thin packs are only sound because §4.2 requires a reader to fetch and import the
bases first; by the time this pack is indexed, its bases are on disk and `git index-pack --fix-thin`
can complete it. A reader that skips the bases will not merely lose objects, it will fail to index
the pack at all — which is the loud failure the invariant is meant to produce.

### 4.2 Pack bases

The `io.git-remote-oci.pack-bases` annotation is REQUIRED on every commit manifest and every ref
manifest. Its value MUST be either the literal `none`, or a comma-separated list of object ids.

It names the commits the packfile was cut against. It is the **packfile dependency graph**, and it
is what a reader MUST follow.

It is *not* the same as `io.git-remote-oci.parents`, which is the **commit graph** and is metadata
only. A reader MUST NOT use `parents` to resolve objects.

The invariant a writer MUST maintain:

> A manifest, together with the transitive closure of the manifests named in its `pack-bases`,
> contains every object reachable from that manifest's commit.

A writer MAY name a base only if all three hold:

1. a ref on the registry points at it, so it is published rather than merely local;
2. it is an ancestor of the commit being pushed, so a reader walking back from that commit reaches
   it — this is why a force push that rewrites history MUST NOT name the replaced tip;
3. the registry actually serves a manifest for it.

The graph MUST be acyclic. A manifest MUST NOT name itself.

Requirements on readers:

- Absent, empty, or malformed `pack-bases` is an error. A reader MUST NOT treat it as `none`.
- A named base that cannot be fetched is an error. A reader MUST NOT warn and continue.
- A reader MUST fetch every base, and finish importing it, **before** importing the packfile that
  depends on it.
- A reader MUST detect a cycle and report it, and MUST NOT block on one. Resolving the graph before
  importing satisfies this.
- A reader MAY bound how large a graph it will resolve, and fail rather than run unbounded on
  registry-supplied data.
- A reader MUST validate that each entry is a well-formed object id before using it as a tag.

**Corollary:** commit-id tags are load-bearing. They are the pack bases later pushes were cut
against, and MUST NOT be pruned without first rewriting the dependent packfiles as self-contained
ones.

***Rationale.*** `pack-bases` and `parents` coincide only when a push carried exactly one commit.
Consider pushing `A`, then committing `B` and `C` and pushing again:

```
commits:   A ─── B ─── C          parents(C) = B
manifests: A             C        B has no manifest
packfiles: [everything]  [C,B minus A]     pack-bases(C) = A
```

A reader following `parents` looks for `B`, does not find it, and stops — never reaching `A`, whose
objects it needs. `git index-pack` accepts the truncated result, git updates the ref, and the
repository is quietly missing objects until a checkout fails. Following `pack-bases` reaches `A`
directly.

Acyclicity is implied by condition 2 — ancestry is a partial order — but is stated separately
because it is the property a reader has to enforce. A cycle has no valid import order: every
manifest in it waits for another, so a reader that follows the ordering rule without checking does
not fail, it hangs. This implementation stops at 50 000 manifests and tells the user to run
`git-remote-oci gc`, which is well beyond any repository that has been compacted even once.

### 4.3 Shallow snapshot layer (optional)

A manifest MAY carry one extra layer annotated `io.git-remote-oci.snapshot: "true"`. It is a
packfile — same media types as §4.1, compression included — holding exactly the objects reachable
from the manifest's commit, with **no ancestry**: the commit, its tree, its sub-trees and its blobs.
Unlike the packfile in §4.1 it MUST NOT be thin and MUST NOT depend on any base.

Requirements:

- It is OPTIONAL. A writer MAY omit it. A reader that does not find one MUST fall back to §4.2.
- It is **not** the ref's packfile. Because it carries a packfile media type, a reader looking for
  "the packfile" MUST exclude snapshot-annotated layers by annotation rather than relying on layer
  order.
- A reader MAY use it **only** when the client asked for depth 1. It contains one commit, so serving
  any other request from it would silently under-deliver.

***Rationale.*** It exists for `git clone --depth 1`. A shallow clone needs the boundary commit's
complete tree, and the §4.1 packfiles are incremental, so reconstructing that tree means walking the
whole `pack-bases` chain — the entire history, the thing `--depth` was asked to leave out.
Compaction does not help: it collapses the chain to one packfile that still contains every commit.

Because the layer is optional and additive, a reader that ignores the annotation entirely still
behaves correctly: it sees an extra layer it does not recognise and fetches the §4.1 packfile as
before. The cost to a writer is a second, undeltified copy of the tip's tree on every push that
publishes one. This implementation omits it unless `ociremote.shallowSnapshot` is set.

### 4.4 Pack index layer (optional)

A manifest MAY carry one layer listing the contents of its §4.1 packfile, in one of two versions:

| Media type | Line format | Records sizes |
| :--- | :--- | :--- |
| `application/vnd.git.repository.packindex.v2` | `<oid> <size>\n` | yes |
| `application/vnd.git.repository.packindex.v1` | `<oid>\n` | no |

A writer MUST publish v2. v1 is specified because repositories written before sizes existed carry
it, and a reader MUST still accept it.

The content MUST be plain text, sorted bytewise ascending by object id, with every line the same
width and `\n`-terminated. Object ids MUST be lowercase hex and MUST be unique: a writer MUST NOT
emit the same id twice. In v2 the id is followed by a single space and the object's uncompressed
size as **exactly 16 lowercase hex digits, zero-padded**.

The stride therefore takes one of four values, and a reader determines both the hash algorithm and
the version from it:

| Stride | Algorithm | Version |
| :--- | :--- | :--- |
| 41 | SHA-1 | v1 |
| 65 | SHA-256 | v1 |
| 58 | SHA-1 | v2 |
| 82 | SHA-256 | v2 |

A writer MUST NOT emit an index mixing id widths, or containing a size it cannot represent in the
fixed field; where either would occur, it MUST omit the layer entirely.

Requirements:

- It is OPTIONAL, and it is not the ref's packfile — it has its own media type, so a reader looking
  for "the packfile" is unaffected by it.
- Its ids are those of the objects the packfile **adds**, which for a thin packfile (§4.1) excludes
  the bases it deltas against. A reader resolving an object still needs the `pack-bases` chain to
  apply the pack; the index answers "is it worth fetching", not "is this pack self-sufficient".
- A missing or unreadable index means **unknown**, never **empty**. A reader MUST NOT treat an
  absent or unparseable index as evidence that an object is not in the packfile; it MUST fall back
  to fetching the packfile and looking.
- A reader MUST NOT treat the absence of a size as a size of zero. A v1 index records no sizes at
  all, and a caller that needs one MUST obtain it another way.
- The size field can hold values no signed 64-bit integer can. A reader MUST reject a size it cannot
  represent rather than wrapping it, and MUST report it as unknown.

***Rationale.*** It exists for partial clones. A promisor fetch asks for a **blob**, and no
annotation in this format can say which packfile holds one: `parents` and `pack-bases` describe
commits, and a blob belongs to every ref that reaches it. Without an index the only way to answer is
to download packfiles and index them until the object appears, so serving one blob can cost most of
the repository. With it, a reader fetches a few kilobytes per ref and downloads only the packfile
that actually has the object.

Sizes turn the v2 `object-info` command from something that has to fetch a packfile to measure an
object into an index read — which matters because the client asking is usually deciding whether it
wants the object at all, and fetching it to answer is precisely backwards.

The 16-digit field is wider than any real object, which is deliberate — a fixed field cannot be
allowed to overflow into a ragged line — but it means a registry can serve a number that does not
fit in an `int64`. Wrapping it would hand a caller a negative length to report to git, so it is
rejected instead. Uniqueness matters for the same class of reason: two lines for one id make the
blob answer differently depending on which copy a binary search lands on.

Sizes got a **new media type** rather than a wider v1 so that a reader can tell from the *manifest*
whether the index will answer a size question, without spending a request to discover that it is a
v1. Fixed-width hex rather than decimal keeps the stride uniform, which is what lets a reader
binary-search the blob — or a byte range of it — without parsing what comes before; sixteen digits
holds any `uint64`, so no object can ever force a ragged line. Mixed id widths would make the blob
ragged and misread from the second entry on, which is why a writer omits the layer rather than
emitting one.

A reader that ignores the layer entirely behaves correctly, just more expensively. The cost to a
writer is roughly 58 bytes per object per push for v2, against a packfile entry of the object
itself.

### 4.5 LFS layers

Git LFS objects referenced by the pushed tree are additional layers of media type
`application/vnd.git.lfs.v1+blob`, annotated:

| Annotation | Value |
| :--- | :--- |
| `org.git.lfs.oid` | the LFS object id, a 64-hex sha256 |
| `org.git.lfs.size` | size in bytes, decimal |

The layer content MUST be the object itself, not a pointer. A reader MUST verify that the content
hashes to the declared oid before storing it, and MUST validate the oid before using it to build a
path.

No LFS server is involved and there is no Batch API.

---

## 5. Ref manifests

A ref manifest is tagged with the encoded ref name (§3). It MUST carry the same layers as the commit
manifest it points at.

Additional annotations:

| Annotation | Meaning |
| :--- | :--- |
| `io.git-remote-oci.ref` | full ref name, e.g. `refs/heads/main` |
| `io.git-remote-oci.tagger` | annotated tag: tagger identity |
| `io.git-remote-oci.tag-message` | annotated tag: message |
| `io.git-remote-oci.tag-signature` | annotated tag: signature |
| `io.git-remote-oci.tag-object` | annotated tag: the tag object's own id |
| `io.git-remote-oci.deleted` | `"true"` on a tombstone; see §8 |
| `org.opencontainers.image.*` | title, authors, created, description, vendor, documentation |

For an annotated tag, `org.opencontainers.image.revision` MUST be the commit the tag resolves to,
and `io.git-remote-oci.tag-object` MUST be the tag object itself. The packfile MUST be built from
the tag object, so that the tag object is included.

A writer MUST NOT write `org.opencontainers.image.signature`.

***Rationale.*** Duplicating the layers means a reader that resolves a ref does not need the commit
manifest as well.

`org.opencontainers.image.signature` used to carry two unrelated things — an annotated tag's real
GPG/SSH signature, and the value of the `pushcert` push option, which is the string `"true"` and not
a signature at all — with no way for a reader to tell which it had. Publishing `"true"` under a
standard OCI key is worse than publishing nothing, because tooling that trusts the key trusts that.
A tag's signature is in `io.git-remote-oci.tag-signature`, which only ever means that; the
`pushcert` option is accepted so git does not error out, and recorded nowhere, since there is no
server here to verify a certificate against.

---

## 6. `_refs` — the ref index

`_refs` is the authoritative list of refs. It carries the format version, which `_index` (§7)
mirrors and is checked for in the same way.

Its `application/vnd.git.repository.index.v1+json` layer MUST be a JSON object mapping ref name to
entry:

```json
{
  "refs/heads/main": {
    "sha": "f0d5b61268be377529d6aa5585bd30226aab8d03",
    "author": "Alice <alice@example.com>",
    "timestamp": 1700000000,
    "message": "Initial commit"
  },
  "refs/tags/v1.0.0": {
    "sha": "f0d5b61268be377529d6aa5585bd30226aab8d03",
    "tagger": "Alice <alice@example.com>",
    "tag_message": "release 1.0.0",
    "tag_sig": "-----BEGIN PGP SIGNATURE-----\n…\n-----END PGP SIGNATURE-----\n",
    "tag_object": "9a1f2b3c4d5e6f708192a3b4c5d6e7f809a1b2c3"
  }
}
```

Every field except `sha` is OPTIONAL. An entry MUST be a JSON object; a reader MUST reject a bare
string.

`tagger`, `tag_message`, `tag_sig` and `tag_object` mirror the ref manifest's annotations (§5):
`tag_sig` is the annotated tag's signature block verbatim, the same value as
`io.git-remote-oci.tag-signature`, so a reader listing refs can tell which tags are signed without
resolving each manifest. The signature is recorded, not verified; there is no server here to verify
it against.

The manifest MUST carry `io.git-remote-oci.format-version`. A reader MUST check it before acting on
anything in the repository, and MUST refuse a value it does not implement.

The manifest MAY carry `io.git-remote-oci.head` (§6.2).

A reader listing refs MUST read `_refs`. If `_refs` is absent, a reader MAY fall back to `_index`
(§7), and then to enumerating tags and inspecting each manifest.

A writer updating `_refs` MUST do so under the `_refs_index_lock` (§9), and MUST treat the update as
a read-modify-write against the currently published index rather than against a snapshot taken
earlier.

***Rationale.*** `_index` carries and is checked for the same version, because it stands in when
`_refs` is missing and an unchecked stand-in is simply an unchecked way in.

Tag enumeration is a repair path for a damaged repository, not a normal one — it is O(tags) round
trips and cannot see refs whose tags were truncated (§3.1).

The read-modify-write requirement is not decorative: a writer that merges its own snapshot over the
published index will silently revert any ref another writer advanced in the meantime, and the
advancing writer has already been told its push succeeded.

### 6.1 Pack chain layer (optional)

The `_refs` manifest MAY carry a layer of media type
`application/vnd.git.repository.packchain.v1+json`: a JSON object mapping a commit id to the ids its
packfile was cut against — the same edges as `io.git-remote-oci.pack-bases` (§4.2), gathered in one
place.

```json
{
  "f0d5b61268be377529d6aa5585bd30226aab8d03": ["9a1f2b3c4d5e6f708192a3b4c5d6e7f809a1b2c3"],
  "9a1f2b3c4d5e6f708192a3b4c5d6e7f809a1b2c3": []
}
```

An empty array means the packfile is self-contained, the same thing `pack-bases: none` says. A
commit the object does not mention is a commit the chain says nothing about.

Requirements:

- It is OPTIONAL and **advisory**. A reader MUST still read each manifest's own `pack-bases` and
  MUST follow anything the chain did not mention. The chain MAY only add to what the reader was
  going to fetch; it MUST NOT authorise a reader to stop early.
- It has no separate consistency requirement, precisely because of the rule above.
- A reader MUST validate that each key and each element is a well-formed object id before using it
  as a tag, and MUST ignore any entry that is not.
- A writer that consolidates (§4.2, §10) MUST **replace** it rather than merge into it.

***Rationale.*** Pack bases form a **chain**: each push is cut against the one before it, so the
graph is one link wide and as deep as the number of pushes since the last compaction. A reader
discovering it from the annotations learns exactly one link per request and cannot ask for the next
until the current one arrives — there is nothing to overlap. Clone latency was therefore linear in
the push count before any packfile moved. With the chain, a reader knows the whole set up front and
fetches those manifests in one parallel wave.

Believing an incomplete chain would mean skipping a packfile and producing a repository quietly
missing objects, so a stale, truncated, or absent chain must cost round trips and nothing else.

Consolidation invalidates every edge: they name intermediate commit manifests that compaction
removes. That applies however compaction was reached — this implementation both offers it as
`git-remote-oci gc` and triggers it from a push. A run that repacks only some refs publishes a chain
describing only those, which is permitted: the refs it omits are walked the slow way.

The layer lives on `_refs` because every operation reads that manifest already; anywhere else would
cost the round trip it exists to remove.

### 6.2 `io.git-remote-oci.head`

Names the ref the remote's `HEAD` points at, e.g. `refs/heads/main`. Absent means none is recorded.

A push MUST NOT move it. A writer MAY set it when nothing is recorded yet; first writer wins.

A writer that changes it MUST republish the index with the refs otherwise unchanged, MUST refuse a
ref the repository does not publish, and MUST refuse a ref outside `refs/heads/`.

If the recorded ref is deleted, the annotation MUST be dropped rather than left dangling.

***Rationale.*** A tag points at a manifest, never at another tag, so there is nowhere to put a
symref; this annotation is the substitute. Without it a reader has to guess, and the guess — `main`,
else `master`, else the alphabetically first branch — is wrong for any repository whose default
branch is neither, which clones onto the wrong branch entirely.

Nothing in the remote-helper protocol tells a helper what the remote's default branch should be,
which is why a push never moves it. It is not write-once, though: `git-remote-oci set-head` rewrites
it, because first-writer-wins is the right rule for a push and the wrong rule for a repository — it
would leave whoever pushed first having chosen permanently. `HEAD` is a symbolic ref to a branch,
and a reader that finds it naming a tag, or nothing at all, has no branch to check out.

---

## 7. `_index` — the OCI image index

`_index` is a standard `application/vnd.oci.image.index.v1+json` listing the same refs, so that
`oras`, `crane`, `skopeo` and registry web UIs can discover what a repository contains.

A writer MUST write it alongside `_refs`. It MUST carry no information that `_refs` does not,
including the format version and the recorded `HEAD`, and a reader MUST honour both here exactly as
on `_refs`.

A writer that cannot write it after `_refs` has landed MUST report the failure. It MUST NOT fail the
operation — the refs are live, and reporting failure would be wrong in the other direction — and it
MUST NOT stay silent.

The index MUST be annotated `io.git-remote-oci.type: repository-index`.

Each entry MUST point at the corresponding ref manifest, MUST carry platform `unknown/unknown`, and
MUST repeat the ref metadata as descriptor annotations — including `io.git-remote-oci.ref` with the
full ref name and `org.opencontainers.image.ref.name` with the encoded tag. A reader MUST use
`io.git-remote-oci.ref`, and MUST NOT infer a ref name from the short name.

***Rationale.*** The `repository-index` annotation is how a tool tells this apart from an ordinary
multi-platform image index. Platform `unknown/unknown` is what makes an entry selectable at all by
generic tooling.

A mirror left behind by a half-completed write is not cosmetic: it stands in for `_refs` when `_refs`
cannot be read, so what it serves then is an outdated ref list — a resurrected deleted ref, or an old
commit id — with nothing to say so. No ordinary read compares the two, because every ordinary read
prefers `_refs`. `git-remote-oci fsck` compares them, which is what turns a silent drift into
something findable.

---

## 8. Deleting a ref

A writer SHOULD delete the ref manifest and then rewrite `_refs` without the ref.

Where a registry refuses manifest deletion, a writer MUST instead overwrite the ref tag with a
**tombstone**: a manifest with no packfile layer, carrying

```
"io.git-remote-oci.deleted": "true"
```

A reader MUST skip tombstones when enumerating tags.

A deletion that fails for any reason other than the registry refusing deletion MUST be reported as a
failure, and MUST NOT be turned into a tombstone.

***Rationale.*** Simply leaving the tag would be wrong: tag enumeration would rediscover it and the
ref would come back on the next push. The tag and its blobs are not reclaimed, which is inherent to
a registry that will not delete.

---

## 9. Locks

Registries offer no compare-and-swap, so all locking here is **advisory**. It narrows the window for
concurrent writers to clobber each other; it does not close it.

A lock is a manifest with no payload layer, tagged `_lock_` followed by the ref name encoded by the
§3 mapping **with a length budget of 122 bytes** (the 128-byte tag limit less the prefix). A ref
whose §3 encoding is 122 bytes or shorter therefore has the lock tag `_lock_` + its ref tag; a
longer one is truncated exactly as §3.1 describes, at the smaller budget, so that the whole tag fits
the limit and distinct refs still receive distinct locks. The two truncation thresholds differ, so a
ref may have an untruncated ref tag and a truncated lock tag; nothing ever derives one from the
other. The manifest carries:

| Annotation | Meaning |
| :--- | :--- |
| `org.git-remote-oci.lock.ref` | the ref being locked |
| `org.git-remote-oci.lock.owner` | who holds it, or `released` for a tombstone |
| `org.git-remote-oci.lock.expires_at` | RFC 3339 expiry |
| `org.git-remote-oci.lock.id` | unique id of this acquisition |

Acquisition MUST be: check, write, then read back and confirm the id is still ours.

A reader MUST honour expiry; an expired lock is not a lock.

Releasing MUST write a tombstone with owner `released` rather than deleting, and MUST verify that
the lock id matches before doing so. A published lock with no id MUST be treated as someone
else's: the id is the only evidence of ownership there is. When no live lock is published — none,
a tombstone, or one that has expired — a release MUST NOT write anything: a client whose own lock
has expired has nothing to release, and a tombstone written then would land on whatever lock
another client has legitimately taken since.

The prefix is inside the reserved namespace of §2, which the ref-name encoding cannot produce. A
writer MUST NOT use a lock namespace a ref could encode into.

The pseudo-ref `_refs_index_lock` serialises updates to `_refs` (§6).

***Compatibility.*** The 122-byte budget did not bump the format version (§11). Locks are never
enumerated to learn anything about a repository, and a writer consults only the lock for the ref it
is about to write; for every ref whose encoding fits the budget the tag is byte-for-byte what it
always was. A writer that did not apply the budget could not form the lock tag for a longer ref at
all — the registry rejects a tag over the limit — so its push of such a ref failed before this
change and fails the same way after it. Nothing is misread; nothing that used to work stops.

### 9.1 `_lfs_locks` — Git LFS file locks

`_lfs_locks` is a manifest with no payload layer whose **config blob** is the lock list. It is
annotated `org.git.lfs.locks.count` with the number of locks it holds, so a tool can see that
without fetching the blob.

```json
{
  "locks": [
    {
      "id": "b1946ac92492d2347c6235b4d2611184",
      "path": "assets/model.bin",
      "owner": { "name": "alice@workstation" },
      "locked_at": "2026-01-01T12:00:00Z"
    }
  ]
}
```

`id`, `path` and `owner.name` are REQUIRED; `locked_at` is RFC 3339. Updating the list is a
read-modify-write and MUST be done under the `_lfs_locks_index_lock` pseudo-ref (§9).

These locks are **not reachable from `git`**. Git LFS locking is an HTTP API served by an LFS
server, and an `oci://` remote has none, so `git lfs lock` cannot reach this. It is driven by the
`lfs-lock`, `lfs-locks` and `lfs-unlock` subcommands, which read and write this record directly.

Like ref locks, they are advisory: nothing prevents a push to a locked path.

***Rationale.*** Read-back-and-confirm catches the interleaving where another writer wrote between
our check and our read, but not the one where it writes after. Both writers can believe they hold
the lock, which is why every requirement elsewhere in this document that could rely on a lock also
requires a re-read of the thing being modified.

Expiry must be honoured or a client that died mid-push wedges the ref forever. Release writes a
tombstone because deletion may be refused.

---

## 10. What the format does not do

- **No garbage collection of its own.** A registry expires nothing and repacks nothing, so anything
  that shrinks a repository is a client rewriting it. Commit tags accumulate one per push and are
  load-bearing (§4.2), so pruning one is only safe after the packfiles that named it have been
  rewritten to stand alone — consolidation first, deletion second, never the other way round. This
  implementation does that in `git-remote-oci gc`, and runs it from a push once enough commits have
  accumulated, but nothing in the format obliges a writer to do either.
- **No mixing of hash algorithms.** A repository is either SHA-1 or SHA-256 throughout, as in git.
  Object ids are 40 or 64 hex characters accordingly; readers derive the algorithm from the ids they
  find rather than from a recorded field, so the two cannot disagree.
- **No sub-packfile addressing.** A packfile is fetched whole or not at all. The pack index (§4.4)
  lets a reader decide *which* packfiles it needs without downloading them, but there is no way to
  fetch part of one, and no representation of an object outside the pack that carries it.
- **No referrers.** No manifest sets `subject`, and the Referrers API is not used. Association is by
  tag alone.
- **No cross-packfile deduplication.** Identical blobs deduplicate by digest, but overlapping
  packfiles do not.
- **No atomicity.** There is no transaction spanning two tags. A reader may observe any intermediate
  state of a multi-tag write, and MUST NOT assume that `_refs`, `_index` and the ref manifests agree
  at the instant it reads them.

---

## 11. Changing the format

Any change to a manifest shape, an annotation, a media type, or the tag mapping is a format change.
It MUST be described in the section it belongs to, in the same commit that makes it: this document
specifies what is written now, not what used to be.

Whether it bumps `io.git-remote-oci.format-version` depends on what a reader that does not know
about the change would do with a repository that has it:

- A change a reader can **ignore** and still be correct — an optional layer, an advisory annotation,
  a new field it will not look at — MUST NOT bump the version. Bumping would refuse repositories to
  readers that would have handled them perfectly well. §4.3, §4.4 and §6.1 are the existing
  examples; an addition of this kind SHOULD be specified as such explicitly, stating what a reader
  that ignores it loses.
- A change that would make an existing reader **misread** a repository — a field whose meaning
  moves, a layer it would mistake for another, a tag mapping that resolves differently — MUST bump
  it. There is no negotiation and no compatibility path: a reader refuses a version it does not
  implement, and that refusal is the entire mechanism. Leaving the version alone through such a
  change is what turns "this build is too old" into a wrong answer nobody is told about.

So the version may move whenever a change needs it to, including during 0.x. It is not a release
counter and does not track this project's version; it moves only when the alternative is a reader
being quietly wrong. This document remains the changelog, and MUST be updated in the same commit
either way.

---

## Conformance

An implementation is **conformant** if it satisfies all the MUST, MUST NOT, REQUIRED, SHALL and
SHALL NOT requirements for the roles it implements.

A **conformant writer** MUST additionally:

1. maintain the §4.2 invariant on every push;
2. publish `_refs` and `_index` together, with the same format version and `HEAD`;
3. never publish a ref under a reserved tag (§2);
4. use an injective ref-name mapping (§3).

A **conformant reader** MUST additionally:

1. check the format version before acting on any repository content (§6);
2. resolve `pack-bases` transitively, in dependency order, and fail loudly on anything absent,
   malformed, or cyclic (§4.2);
3. validate every registry-supplied object id before using it as a tag (§4.2, §6.1);
4. treat every OPTIONAL layer as absent-safe: ignoring it MUST cost only performance.

A conformant reader MAY additionally reject a repository whose pack-base graph exceeds an
implementation-defined bound, provided it says so (§4.2).

***Rationale.*** The reader requirements are all cases where the failure mode is silence: a
repository that looks fetched but is missing objects, or an identifier from the registry used
unchecked. Each has a corresponding "MUST NOT warn and continue" elsewhere in the document.

---

## Appendix A: Annotation registry

Non-normative summary. The normative definition of each is in the section named.

| Annotation | On | Section |
| :--- | :--- | :--- |
| `io.git-remote-oci.format-version` | `_refs`, `_index` | §6, §7 |
| `io.git-remote-oci.pack-bases` | commit + ref manifests | §4.2 |
| `io.git-remote-oci.parents` | commit manifests | §4 |
| `io.git-remote-oci.snapshot` | snapshot layer | §4.3 |
| `io.git-remote-oci.ref` | ref manifests, `_index` entries | §5, §7 |
| `io.git-remote-oci.tagger` | ref manifests | §5 |
| `io.git-remote-oci.tag-message` | ref manifests | §5 |
| `io.git-remote-oci.tag-signature` | ref manifests | §5 |
| `io.git-remote-oci.tag-object` | ref manifests | §5 |
| `io.git-remote-oci.deleted` | ref tombstones | §8 |
| `io.git-remote-oci.head` | `_refs`, `_index` | §6.2 |
| `io.git-remote-oci.type` | `_index` | §7 |
| `org.git.lfs.oid` | LFS layers | §4.5 |
| `org.git.lfs.size` | LFS layers | §4.5 |
| `org.git-remote-oci.lock.ref` | lock manifests | §9 |
| `org.git-remote-oci.lock.owner` | lock manifests | §9 |
| `org.git-remote-oci.lock.expires_at` | lock manifests | §9 |
| `org.git-remote-oci.lock.id` | lock manifests | §9 |
| `org.git.lfs.locks.count` | `_lfs_locks` | §9.1 |
| `org.opencontainers.image.revision` | commit + ref manifests | §4, §5 |
| `org.opencontainers.image.ref.name` | `_index` entries | §7 |

`org.opencontainers.image.signature` is **not** written; see §5.

## Appendix B: Media type registry

Non-normative summary of §2.1. None of the `vnd.git.*` types is registered with IANA.

| Media type | Normative section |
| :--- | :--- |
| `application/vnd.git.repository.packfile.v1`(`+gzip`, `+zstd`) | §4.1 |
| `application/vnd.git.repository.packindex.v1` (read-only), `.v2` | §4.4 |
| `application/vnd.git.repository.packchain.v1+json` | §6.1 |
| `application/vnd.git.repository.index.v1+json` | §6 |
| `application/vnd.git.lfs.v1+blob` | §4.5 |

## Appendix C: References

- [OCI Image Format Specification v1.1](https://github.com/opencontainers/image-spec/blob/main/spec.md)
- [OCI Distribution Specification v1.1](https://github.com/opencontainers/distribution-spec/blob/main/spec.md)
- [RFC 2119](https://www.rfc-editor.org/rfc/rfc2119) — Key words for use in RFCs
- [RFC 8174](https://www.rfc-editor.org/rfc/rfc8174) — Ambiguity of uppercase vs lowercase
- [RFC 3339](https://www.rfc-editor.org/rfc/rfc3339) — Date and time on the Internet
- `gitformat-pack(5)` — the packfile format
- `gitprotocol-v2(5)` — the wire protocol this implementation serves over `stateless-connect`
