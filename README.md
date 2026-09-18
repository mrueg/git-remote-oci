# git-remote-oci

![git-remote-oci Logo](logo.png)


`git-remote-oci` is a Git remote helper that stores a whole Git repository inside an OCI (Open
Container Initiative) container registry. Clone, fetch and push against an `oci://` URL and the
objects live as registry blobs and manifests — no Git server involved.

Each push publishes an OCI image manifest for the tip of every ref it updates, tagged with both the
commit id and the encoded ref name. The commits in between travel inside the packfile; they are not
tagged individually. The layout is specified in [FORMAT.md](FORMAT.md).

It is developed against the CNCF `registry:3` reference implementation and tested against
[zot](https://zotregistry.dev) as well. Hosted registries should
work — the manifests are written to be spec-conformant, and authentication is tested against a
password-protected registry — but see [Limitations](#limitations) before depending on one.

---

## What it is for

The registry is already inside your trust boundary. The forge usually is not.

- **A production or customer environment that must not reach your forge.** A cluster, an appliance,
  or a build farm already has registry credentials and a network path to the registry. Giving it
  access to an internal GitHub or GitLab means a new account, a new egress rule, and a token that can
  usually reach a great deal more than the one repository you meant. A pull secret that already
  exists reaches exactly one repository and nothing else.
- **No new infrastructure to run.** If you ship containers you already operate a registry, with its
  authentication, storage, replication, retention and backups decided. A Git remote out of it costs
  nothing further to run: no server, no database, no separate thing to patch at 2am.
- **Air-gapped and cross-domain transfer.** Moving artifacts across a boundary is usually a solved
  and *approved* problem — `oras`, `skopeo` and `crane` mirror registries, and the review process for
  doing so exists. A repository stored this way travels that path, rather than needing a second one
  agreed for Git.
- **Code beside the artifact it produced.** GitOps manifests, Helm values, the Dockerfile that built
  the image: same registry namespace, same credentials, same lifecycle policy as the image itself.
- **A mirror on infrastructure you already have.** `git push --mirror` to an `oci://` URL leaves a
  working remote, not an archive that has to be restored before anyone can use it — useful when the
  forge is down, being migrated, or being left.

**When not to.** If you can reach a Git server, use one. A registry cannot negotiate, run a hook,
check reachability on push, or update several refs atomically, and no amount of work on this tool
will change that — the reasoning is in [Limitations](#limitations). This is for the places a Git
server cannot go.

---

## ⚠️ Status: Experimental

This project works for everyday push/fetch/clone against the registries it has been tested on, but it
is **not production-ready**. Please read [Limitations](#limitations) before relying on it.

In particular:

- The **on-registry format is unstable** and carries **no compatibility path**. It is specified in
  [FORMAT.md](FORMAT.md) and versioned (`io.git-remote-oci.format-version`, pinned at `1` for all of
  0.x), but a reader that meets a version it does not implement refuses the repository outright
  rather than guessing.
- It depends on **pre-release libraries** (`go-git/v6` and `go-billy/v6`, both `v6.0.0-alpha`).
- Several remote-helper options are accepted but not yet honoured. See
  [Git Remote Helper Options](#git-remote-helper-options) for exactly which.

Bug reports and pull requests are welcome.

---

## Features

- 📦 **Commits as OCI Images**: Each pushed ref publishes an OCI Image manifest for its tip commit, tagged with both the commit SHA and the encoded ref name (`refs/heads/*`, `refs/tags/*`). Commits between one push and the next are carried inside the packfile rather than tagged individually.
- ⚡ **Thin, Incremental Packfiles**: A push omits objects the registry already serves *and* stores what remains as deltas against them, so a small change to a large file costs the change rather than the file — measured at 1.3 KB for a seven-byte edit to a 512 KB file, against 316 KB without.
- 🧭 **Recorded Pack Bases**: That is safe only because each push records exactly which commits it was cut against, in `io.git-remote-oci.pack-bases`. Fetch follows that list, imports the bases first, and fails loudly if one is unavailable, rather than producing a repository that is quietly missing objects.
- 🪶 **Optional cheap `--depth 1` clones**: with `ociremote.shallowSnapshot` enabled, each push also publishes a self-contained snapshot of the ref tip, so a shallow clone over the simple path (`ociremote.protocolV2=false`) fetches that one packfile instead of the history behind it — 0.5 MB against 4.4 MB on the benchmark fixture. Off by default, because it costs a full copy of the tip on every push; the default [protocol v2](#protocol-v2) path does not read snapshots and applies the depth when it builds the pack instead.
- 🧹 **Compaction that happens by itself**: every push publishes a commit manifest and a commit tag that nothing can remove until the history is repacked, so a busy repository gets slower to clone forever. Once `ociremote.compactAfter` commits (50) have accumulated, the push that crosses the line repacks each ref into one self-contained packfile and prunes what is no longer needed. It runs after the push has reported its result and never fails it.
- ⏸️ **Resumable pushes**: a packfile over 32 MB is uploaded in chunks, so a connection dropped at 90% of a multi-gigabyte push costs one chunk rather than the whole transfer — the registry is asked how much it already holds and the upload continues from there. Registries that do not support chunked uploads fall back to a single request before any content is sent, so a push can never fail for having tried.
- ⛓️ **One round trip for the pack graph**: pack bases form a chain — each push cut against the one before it — so discovering it from the manifests was one sequential request per push before any packfile moved. The whole graph is published on the `_refs` index that every operation reads anyway, so a clone resolves it in a single parallel wave. It stays advisory: each manifest's own `pack-bases` is still read, so a stale or absent chain costs round trips and never correctness.
- 🗂️ **Published pack indexes**: each push also publishes the object ids its packfile contains *and their sizes*, so a partial clone's lazy fetch can rule a ref out by reading a few kilobytes instead of downloading and indexing its history, and `object-info` can answer "how big is this blob" without transferring it. Additive and optional: a repository pushed without them still clones, just more expensively.
- 🚀 **Parallel transfers**: `errgroup` worker pools overlap Git LFS uploads and downloads, commit fetching, and multi-ref pushes, which matters most on high-latency links. The pool sizes default to 12 for fetch and LFS and 64 for refs within one push, and are configurable per repository or per remote — see [Configuration](#configuration).
- 🎨 **Standard OCI annotations**: Manifests carry `org.opencontainers.image.title`, `.authors`, `.created`, `.description`, `.vendor` and `.documentation`, which registry web UIs generally surface. How any particular one renders them has not been verified.
- 📋 **Standard OCI Image Index (`_index`)**: Groups all repository branches and tags under a standard OCI Image Index manifest (`application/vnd.oci.image.index.v1+json`), making repository references discoverable by standard OCI clients (`oras`, `crane`, `skopeo`).
- 🏷️ **Annotated Tag Metadata (`git tag -a`)**: Records annotated tag metadata (tagger, tag message, GPG/SSH tag signature, tag object SHA) in OCI manifest annotations.
- 🔢 **SHA-256 repositories**: Object ids of either width are accepted, the `object-format` capability is advertised, and `list` reports the repository's algorithm when git asks. The algorithm is derived from the published ids rather than stored, so the two cannot disagree. A repository holds one algorithm and there is no conversion between them, as in git.
- 🔗 **Injective ref-name → tag mapping**: Every ref maps to a distinct OCI tag. Refs longer than 128 encoded bytes get a hashed tag that stays unique but cannot be decoded back, so they are listed from the `_refs` index rather than by enumerating tags — see [FORMAT.md](FORMAT.md).
- ⚡ **Optional Packfile Compression**: `gzip` or multi-threaded `zstd` (`klauspost/compress`, `runtime.NumCPU()`, level `SpeedFastest`) via `OCI_COMPRESSION`, plus tuned HTTP transport connection pooling.
- 🔒 **Advisory Ref Locking**: a push takes a `_lock_<ref>` OCI tag for the ref it is updating and releases it afterwards, and updates to the `_refs` index detect a writer that slipped past the lock and retry against fresh state. This narrows the window for concurrent pushes to clobber each other. **Advisory only**, because registries offer no compare-and-swap — see [Limitations](#limitations).
- ⚡ **`_refs` Index**: Fast reference lookup and listing via a consolidated `_refs` manifest tag, avoiding expensive registry tag enumeration.
- 🛠️ **Remote Helper Options**: `followtags`, `atomic`, `cas` (`--force-with-lease`), `dry-run`, `verbosity`, `progress`. See the [options table](#git-remote-helper-options) for what is honoured and what is merely accepted.
- 🔬 **Wire protocol v2**: the helper serves git's protocol v2 over `stateless-connect`, which is what makes **partial clone** (`--filter=blob:none`) and genuinely cheap `--depth n` possible — neither can be expressed through the simple helper interface at all. On by default; `ociremote.protocolV2=false` returns to the simple path. See [Protocol v2](#protocol-v2).
- 🧰 **Maintenance subcommands**: `gc` compacts a repository into one self-contained packfile per ref, `fsck` checks every published ref is still fetchable without downloading anything, that `_refs` agrees with the ref tags (`--repair` rebuilds it when it does not) and that the `_index` mirror still agrees with `_refs`, `set-head` shows or changes the default branch a clone checks out, `break-lock` releases a ref lock left behind by a client that died mid-push, and `lfs-lock`/`lfs-locks`/`lfs-unlock` coordinate Git LFS file locks.
- 🚀 **Pure Go**: `go-git/v6` for packfiles and `oras-go/v2` for the registry API. No cgo. It shells out to `git` only where go-git cannot do the job — `pack-objects` (go-git's encoder cannot delta against a base it was told to exclude, which is the whole of the thin-pack saving), `index-pack`/`unpack-objects` to complete one, and `git config` for scope precedence and `includeIf`. Object lookups, path discovery and history walks are go-git. `git` must be on `PATH`.

---

## Limitations

Things that do not work, or do not work the way you might expect.

A Git server is a program. It parses your `want`/`have` lines, walks the object graph, builds a pack
tailored to that request, runs hooks, and updates refs in a transaction. **A registry is a filestore
with three verbs**: put a blob, put a manifest, move a tag. It runs no code on your behalf and offers
no atomicity. Almost everything below follows from that one difference.

What *does* work is under [Features](#features), and the format is specified in
[FORMAT.md](FORMAT.md).

| Area | Consequence |
| :--- | :--- |
| **`want`/`have` negotiation** | There is nobody to negotiate with. The *pusher* has to guess what a future fetcher will already have, which is what `io.git-remote-oci.pack-bases` records, and what crosses the wire is whole packfiles as they were cut at push time. Over [protocol v2](#protocol-v2) git is still handed a pack built for its request — the helper stages those packfiles locally and cuts the slice itself — so the negotiation happens on your machine, after the download, rather than being saved by it. |
| **Partial clone** (`--filter=blob:none`, `blob:limit=<n>`) | Not a storage problem: a blob-less pack could be published beside the full one for a few hundred bytes, and git would reject it. A remote helper's `fetch` is *defined* as transferring a complete object graph and git verifies that. It needs wire protocol v2, which a helper can only speak through `stateless-connect` — so it works, and is on by default. With [`ociremote.protocolV2`](#protocol-v2) turned off, `--filter` merely skips automatic Git LFS downloads. |
| **Shallow clone** (`--depth <n>`) | Cutting a pack at a boundary the client names needs server-side compute. A registry can only serve a shape prepared in advance, which is why `--depth 1` can be cheap on the simple path — the tip snapshot is published at push time, if `ociremote.shallowSnapshot` is on — and no other depth can. [Protocol v2](#protocol-v2), which is on by default, lifts this: there the depth is applied when the pack is built. See [Shallow clones](#5-shallow-clones). |
| **Reachability checks on push** | `git receive-pack` refuses a push whose objects do not connect. A registry accepts any blob you upload and validates nothing, which is exactly why a reader must treat a missing pack base as a hard error rather than a warning. |
| **`--atomic`** | No transactions. Ref tags are written independently, so the closest achievable is to write `_refs` once at the end and re-point the tags on failure. The visible state does not move, but that is a compensating action: a reader between the two steps sees the intermediate state, and uploaded manifests and blobs stay behind as garbage. |
| **`--force-with-lease`, ref locking** | Both are compare-and-swap, and the distribution API has none — no `If-Match` on a tag PUT. Check and write are separate requests, so another client can slip between them. A digest check on `_refs` catches the interleaving that actually loses data; locks are advisory, and a client that dies mid-push blocks a ref until the 10-minute TTL expires. When two pushes come out of that window both believing they hold the `_refs` lock, the one whose lock was overwritten reads the published index back after its write and either confirms its refs are there or redoes the update; it never reports a ref as pushed on the strength of the lock alone. |
| **Reflogs** | A remote reflog is a server-side append-only record of ref transitions. There is nothing to append to, and no transaction boundary to append at. |
| **Hooks, push certificates, CI triggers** | These all need code running on push. A registry executes nothing, so there is no point at which a policy could accept or reject one. A push certificate exists for a *server* to verify; with no verifier, storing it proves nothing, so nothing is stored. Anything hook-shaped has to run in the client, where the person being restricted controls it. |
| **Server-side `gc`, and scaling** | Nothing repacks on the far end, so compaction is work a client has to do. Each push adds one OCI tag and one packfile, and a clone runs `git index-pack` once per push generation. That is handled rather than left to you — a push repacks the repository once `ociremote.compactAfter` commits have accumulated — but it is still a client paying for it, and consolidation has to precede pruning because commit-SHA tags are load-bearing: they are the pack bases later pushes were cut against. `git-remote-oci gc` runs the same thing deliberately, from anywhere, with or without a clone. |
| **Arbitrary ref names** | An OCI tag must match `[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}`: no `/`, and a hard length limit. Ref names are encoded injectively ([FORMAT.md §3](FORMAT.md#3-ref-names-to-tags)), but one whose encoding exceeds 128 bytes is stored under a hashed tag that cannot be decoded back, so it is discoverable only through the `_refs` index. |
| **Symrefs and the default branch** | A tag points at a manifest, never at another tag, so `HEAD` cannot be a symref; the format records its target in an annotation. Nothing in the remote-helper protocol tells a helper what a remote's default *should* be, so a push never sets it — it is adopted from the first branch pushed. Use `git-remote-oci set-head` to change it afterwards. |
| **Ref namespaces**, alternates, object sharing | All assume a server-side object store several repositories can address. Each OCI repository here is self-contained. |
| **Ref deletion** | Whether a tag can be removed is registry policy, and several hosted registries forbid it — the distribution registries need `REGISTRY_STORAGE_DELETE_ENABLED=true`. Where deletion is refused the ref tag is overwritten with a tombstone, so the ref stops being listed and is not resurrected by tag enumeration, but the tag remains and its blobs are never reclaimed. Registries also collect unreferenced blobs on their own schedule, which the client neither controls nor observes. |
| **Registry tag immutability** | Some registries can be configured to forbid overwriting a tag. A Git ref is mutable by definition, so on such a repository pushing an update to an existing branch fails outright. |
| **`git lfs lock`** | Not reachable through git-lfs itself: locking is an HTTP API served by an LFS server, and an `oci://` remote has none. Drive it by hand with `lfs-lock`, `lfs-locks` and `lfs-unlock`, which share the same `_lfs_locks` record. Advisory either way — nothing blocks a push to a locked path. |
| **Registry compatibility** | Verified on every change against two implementations that share no code — the CNCF `registry:3` reference implementation (Distribution v3) and [**zot**](https://zotregistry.dev), which stores the OCI image layout natively — and against **GHCR** on pushes to `main` and weekly. ECR, Docker Hub, Quay, Harbor and Artifact Registry are untested; the manifests are written to be spec-conformant, and two independent readers accepting them is evidence of that rather than proof. Point the suite at another with `E2E_REGISTRY_IMAGE`, and `E2E_REGISTRY_ARGS` for one not configured the distribution way. |

---

## Installation

### Prerequisites
- Go 1.27.1 or later (see the `go` directive in `go.mod`)
- `git` on your `PATH` — the helper shells out to it for `pack-objects` when pushing,
  `index-pack` and `unpack-objects` when fetching, and `git config` to read its settings

### With `go install`

```bash
go install github.com/mrueg/git-remote-oci@latest
```

### From a release

Tarballs for linux and darwin, `amd64` and `arm64`, are attached to each
[release](https://github.com/mrueg/git-remote-oci/releases). There is no Windows build. Each tarball
ships an SPDX SBOM alongside it, the checksum file is signed, and every tarball carries a build
provenance attestation — see [Verifying a release](#verifying-a-release).

```bash
tar xzf git-remote-oci_linux_amd64.tar.gz
install git-remote-oci ~/.local/bin/
```

### As a container image

```bash
docker run --rm ghcr.io/mrueg/git-remote-oci:latest version
```

The image carries `git` as well as the helper, because the helper shells out to
it, and puts the binary on `PATH` so `git clone oci://…` works inside the
container. It is a minimal base: enough to run git, not a shell environment.

```bash
docker run --rm -v "$PWD:/work" -w /work ghcr.io/mrueg/git-remote-oci:latest \
  clone oci://ghcr.io/your-username/my-repo
```

Built with [ko](https://ko.build) from the same GoReleaser build as the
tarballs, so the binary in the image and the one in a release cannot drift
apart. An SPDX SBOM is attached to each image.

### From source

```bash
git clone https://github.com/mrueg/git-remote-oci.git
cd git-remote-oci
make build
install git-remote-oci ~/.local/bin/
```

However you install it, the binary must be named `git-remote-oci` and be on your `PATH`: that is how
Git finds a helper for `oci://` URLs.

### When Git says `aborted session`

```
fatal: remote helper 'oci' aborted session
```

This is the only thing Git says when a helper fails before answering, whatever the reason, so it is
worth knowing what it hides. Nearly always it is one of three, and none of them are about the
registry:

- **The binary is not called `git-remote-oci`.** Git derives the helper's name from the URL scheme
  and execs exactly that. A binary of any other name is simply not found, whatever it is otherwise
  capable of.
- **It is not on the `PATH` Git sees**, which is not always the one your shell sees — `sudo`, a
  desktop Git client, or an editor's integrated terminal may all have a different one.
- **It was built for another platform.** The helper is a native binary; one built elsewhere fails at
  exec time. This bites where a checkout is shared across machines — a repository mounted into a
  Linux container from a macOS host, say.

`git-remote-oci version` is the quickest discriminator: if that runs, the binary exists, is named
right and matches this machine, and the problem is Git not finding it.

If the helper is starting but failing for some other reason, `-v -v` reaches it — Git turns that into
`option verbosity 3` and the helper reports to stderr, which Git passes through:

```bash
git clone -v -v oci://registry.example.com/org/repo
```

### Verifying a release

Everything is signed keylessly: the signing identity is the GitHub Actions workflow that built the
release, recorded in a Sigstore certificate. There is no public key to fetch and no key for anyone to
steal — verification checks *who built it*, not *who holds a secret*.

Only `checksums.txt` is signed. It names every other artifact by digest, so one signature plus a
hash check covers the whole release:

```bash
cosign verify-blob checksums.txt --bundle checksums.txt.bundle \
  --certificate-identity-regexp 'https://github.com/mrueg/git-remote-oci/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

sha256sum --check --ignore-missing checksums.txt
```

The container image is signed directly:

```bash
cosign verify ghcr.io/mrueg/git-remote-oci:latest \
  --certificate-identity-regexp 'https://github.com/mrueg/git-remote-oci/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Build provenance is separate from the signature and answers a different question. The signature says
this release came from this workflow; the attestation records *how* — which commit, which workflow
run, which inputs:

```bash
gh attestation verify git-remote-oci_linux_amd64.tar.gz --repo mrueg/git-remote-oci
```

---

## Usage

### 1. Push to an OCI Registry

Add an `oci://` remote and push your branch or tags:

```bash
# Add an OCI remote endpoint
git remote add origin oci://ghcr.io/your-username/my-repo

# Push main branch
git push origin main

# Push with reachable tags automatically
git push -o followtags=true origin main

# Push Git tags
git push origin v1.0.0
# or push all tags
git push origin --tags
```

For local or insecure test registries (such as `localhost:5000`), set `OCI_INSECURE=1`:

```bash
OCI_INSECURE=1 git push oci://localhost:5000/my-repo main
```

### 2. Clone & Fetch

Clone a repository or fetch updates directly from an OCI registry:

```bash
# Clone
git clone oci://ghcr.io/your-username/my-repo

# Fetch updates
git fetch origin
```

> Over [protocol v2](#protocol-v2), the default, `--depth n` and `--filter` are applied when the
> pack is built. With `ociremote.protocolV2=false`, `--depth` is honoured in what git shows you —
> `--depth 3` gives exactly three commits — but the full history is still transferred, and
> `--filter` only skips Git LFS blobs. See [Limitations](#limitations).

### 3. Maintenance

Each push writes its own packfile and its own commit-SHA tag, and nothing removes
them, so a long-lived repository accumulates one of each per push. Cloning it
then means fetching every packfile and running `git index-pack` once per pack.

This happens on its own: once a repository has published `ociremote.compactAfter`
commits — 50 by default — the next push repacks it. The command below is for
running it deliberately, on a schedule, or with the automatic trigger turned off
(`ociremote.compactAfter 0`).

`gc` rewrites every ref as a single self-contained packfile and prunes the
commit manifests that are no longer needed, along with released or expired
locks:

```bash
git-remote-oci gc --dry-run oci://ghcr.io/your-username/my-repo
git-remote-oci gc oci://ghcr.io/your-username/my-repo
```

It does not need a clone. Objects it cannot find locally are fetched from the
registry into a scratch store and thrown away afterwards, so this runs anywhere
— including outside a git repository, which is what makes it usable as a
scheduled job next to the registry rather than a chore for whoever happens to
have the whole history checked out. Running it from a clone that already holds
the history just saves the download. The scratch store needs room for as much
history as it fetches; `$TMPDIR` redirects it.

Indicative, from a repository built by 20 incremental pushes plus a branch, a
SHA-named branch and an annotated tag. These figures predate thin packfiles and
are not re-measured on every change, so treat them as the shape of the win
rather than a promise:

| | registry tags | clone requests | packfiles |
| :--- | ---: | ---: | ---: |
| before `gc` | 28 | 136 | 30 |
| after `gc` | 11 | 22 | 4 |

The end-to-end suite checks the reduction on every run against a real registry,
and currently reports 24 tags before and 12 after for its own fixture.

On a registry that permits manifest deletion, `gc` reclaims the tags it prunes. On one that refuses —
GHCR, ECR and Docker Hub all do, to varying degrees — the consolidation still happens and the tags it
could not remove are reported, rather than the whole run failing and throwing the consolidation away.

Two more subcommands help when something has gone wrong:

```bash
# Check every published ref is fetchable, without downloading packfiles.
# Follows io.git-remote-oci.pack-bases exactly as a fetch does, and compares
# the _refs index against the ref tags and the _index mirror against _refs.
git-remote-oci fsck oci://ghcr.io/your-username/my-repo

# The same, then rewrite _refs to agree with the ref tags.
git-remote-oci fsck --repair oci://ghcr.io/your-username/my-repo

# Release an advisory ref lock left behind by a client that died mid-push.
git-remote-oci break-lock --force oci://ghcr.io/your-username/my-repo refs/heads/main

# Show, then change, the branch a fresh clone checks out.
git-remote-oci set-head oci://ghcr.io/your-username/my-repo
git-remote-oci set-head oci://ghcr.io/your-username/my-repo main
```

`set-head` is how a repository's default branch changes. A push cannot say what
the default should be — nothing in the remote-helper protocol carries it, so the
only safe reading of pushing a branch is "publish this", not "make it the
default". A repository therefore adopts its default from the first branch ever
pushed to it and keeps it; this is the deliberate way to move it afterwards,
which is what renaming `master` to `main` needs.

`fsck` exists because a registry validates nothing: it accepts any blob and any
manifest, and has no idea a packfile is a packfile. There is no server-side
reachability check, so this is the only way to find out that a repository has
become unclonable short of cloning it.

It also compares `_refs` against the ref tags. The tags are authoritative: a
push writes a ref's tag before it rewrites the index, and a deletion removes the
tag before it drops the entry, so a push or deletion that died between the two
leaves the index lagging — a branch nobody can see, a tip older than the one
that was pushed, or a deleted ref still listed — and nothing else ever notices,
because every ordinary read lists refs from `_refs` and never looks at a tag.
`fsck` lists each disagreement as the change a repair would make;
`fsck --repair` rewrites the index to agree with the tags, under the same lock
and compare-and-swap a push uses, keeping the recorded `HEAD`, the annotated-tag
metadata and every entry it does not touch as they were. Ref names come from
each manifest's annotation, so a ref whose tag was truncated is recovered under
its full name. What cannot come back is the commit author, timestamp and message
of an entry it rebuilds: the tag does not carry them, and they are optional.
Reach for it after an interrupted push or deletion, or when `git ls-remote`
disagrees with what was pushed; a plain `fsck` first says whether it is needed.

It compares `_index` against `_refs` as well. The two are written together and
`_index` stands in for `_refs` when `_refs` cannot be read, so a mirror left
behind by a half-completed push serves an outdated ref list to whoever reaches
it. Any push rewrites it, and so does `fsck --repair`.

> Because git invokes the helper as `git-remote-oci <remote> <url>`, a git remote
> cannot be named `gc`, `fsck`, `break-lock`, `set-head`, `lfs-lock`,
> `lfs-locks`, `lfs-unlock`, `version` or `help`. A remote with one of those names is refused
> by name — git exports `GIT_DIR` into the helper's environment, which is what
> tells a colliding remote apart from you running the subcommand by hand. If you
> ever need to run one from a shell that does export `GIT_DIR`, set
> `GIT_REMOTE_OCI_SUBCOMMAND=1`. Run `git-remote-oci help` for the current list.

### 4. Git LFS

Git LFS payloads referenced by pushed commits are uploaded as additional OCI layers and restored on
fetch. No `git lfs` configuration is required for this, and no LFS server is involved.

Two outcomes are treated differently on push, identically on the ordinary and the `--atomic` path:

| Situation | Result |
| :--- | :--- |
| The object is not in the local LFS store | A warning. Normal after a partial checkout, and the push continues without that blob. |
| The upload fails, or the pointer's OID is malformed | The ref fails. Publishing it would leave a ref referencing a blob the registry does not have. |

The whole pushed commit range is scanned, not just the tip tree, so an object introduced mid-range
and deleted by the tip is still uploaded.

File locking is available, though not through `git lfs lock` — see below:

```bash
git-remote-oci lfs-lock   oci://ghcr.io/your-username/my-repo art/hero.psd
git-remote-oci lfs-locks  oci://ghcr.io/your-username/my-repo
git-remote-oci lfs-unlock oci://ghcr.io/your-username/my-repo art/hero.psd
```

`git lfs lock` itself cannot work here. Locking in Git LFS is an HTTP API — `POST
/locks`, `GET /locks`, `POST /locks/:id/unlock` — that `git-lfs` calls on an LFS
server it discovers from the remote URL. An `oci://` remote has no such endpoint,
and a remote helper is not an HTTP server: git speaks to it over a pipe, only for
fetch and push. Serving that API would mean running a daemon beside git.

These subcommands put the same locks in the same place (`_lfs_locks`), so they
interoperate with anything else using this tool. Locking is **advisory** —
nothing prevents a push to a locked path; a lock records an intent others can
read.

### 5. Shallow clones

This section is about the *simple* path, `ociremote.protocolV2=false`. Over
[protocol v2](#protocol-v2), which is the default, the depth is applied when the pack is built and the
snapshot described here plays no part; see [Protocol v2](#protocol-v2) for what that path costs
instead.

On the simple path `git clone --depth 1` shows one commit but transfers the whole history. The depth
is honoured for what git displays; it saves no bandwidth.

That is not laziness, it is the shape of the storage. A shallow clone needs the boundary commit's
**complete tree**, and the stored packfiles are incremental — a file untouched since the first commit
lives in the first packfile — so the walk cannot stop early without handing git a commit whose
content is missing. `gc` does not help either: it collapses a ref's chain into one packfile, but that
packfile still contains every commit, so a depth-1 clone of a compacted repository costs what a full
clone costs.

Enabling the snapshot fixes it:

```bash
git config ociremote.shallowSnapshot true
```

Each push then also publishes a self-contained packfile holding exactly the objects reachable from
the tip, with no ancestry, and `--depth 1` fetches that alone.

**It is off by default because the cost lands on every push.** The snapshot is a second, complete
copy of the tip's tree and deltas against nothing — that is what "self-contained" means — so it
undoes the thin-pack saving for the push carrying it.

`make bench` measures both sides on a fixture with 61 commits, a 4.4 MB history and a 512 KB tip:

| | snapshot off | snapshot on |
| :--- | ---: | ---: |
| `clone --depth 1` | 4.43 MB | **0.53 MB** |
| `clone` (full) | 4.42 MB | 4.36 MB |
| one more commit, pushed | **64 KB** | 577 KB |

Read the last row before enabling it: the extra 513 KB is the tip tree, paid again on every push.
Most repositories are pushed to far more often than they are cloned shallowly, so most should decline
the trade; a repository that exists mainly to be checked out by CI should take it.

Two things worth knowing:

- The key controls **publishing** only. Reading a snapshot is unconditional, because a fresh clone
  has no configuration to consult and using one that exists is free.
- A repository pushed with the setting mixed is fine. A fetch uses snapshots when every requested ref
  has one and walks the packfiles otherwise, and turning the key off later leaves everything already
  published readable.

`--depth 2` and deeper always transfer the full history over the *simple* path: the snapshot is
depth-1, and a registry cannot produce the depth-*n* equivalent on demand. [Protocol v2](#protocol-v2)
changes that, and is what you get unless you turn it off — there the depth is applied when the pack is
built, so any depth transfers only the commits it covers.

### 6. Version

```bash
git-remote-oci version
```

---

## Git Remote Helper Options

Options Git sends to the helper, and what each one actually does today.

These describe the *simple* helper interface — what you get with
[`ociremote.protocolV2`](#protocol-v2) turned off. On, which is the default, `filter` and `depth`
stop being helper options at all and become arguments to protocol-v2 commands, where they mean what
they mean everywhere else in git. The rows below say so where it matters.

### Honoured

| Option | Values | Description |
| :--- | :--- | :--- |
| `followtags` | `true`, `false` | Automatically push reachable tags alongside branch updates. |
| `cas` | `<ref>:<old-sha>` | Compare-And-Swap (`--force-with-lease`) ref protection against concurrent updates. |
| `dry-run` | `true`, `false` | Validate packfile generation and registry connection without modifying registry state. Covers ref deletion and the `_refs` index as well as ref updates: nothing is written. |
| `verbosity` | `<n>` | Log verbosity on stderr. `0` is quiet; `1` is the default; `2` and above add detail. |
| `progress` | `true`, `false` | Enable or disable progress meters. |
| `object-format` | `true`, `false` | Ask for the remote's hash algorithm to be reported on `list`. Required before git will fetch from a SHA-256 repository. |

### Partially honoured

| Option | Values | Description |
| :--- | :--- | :--- |
| `atomic` | `true`, `false` | The `_refs` index is updated all-or-nothing and the ref tags are restored on a mid-batch failure, so the visible state does not move. The manifests and blobs already uploaded stay behind as garbage. |
| `filter` | `blob:none`, `blob:limit=<size>` | Only skips automatic **Git LFS** blob downloads; Git objects are always transferred in full. Sizes may use `k`/`m`/`g` suffixes and are compared against the LFS object's own size. This is the behaviour with [protocol v2](#protocol-v2) turned off; on, which is the default, `--filter` is a real partial clone. |
| `depth` / `deepen` | `<n>` | The boundary is always honoured, so `--depth n` shows exactly n commits. Bandwidth is only saved at `--depth 1`, and only when the pushing side enabled `ociremote.shallowSnapshot`; otherwise the full history is transferred. See [Shallow clones](#5-shallow-clones), or [protocol v2](#protocol-v2) to have the depth applied when the pack is built. |

### Accepted but not implemented

Answering `ok` to these keeps Git from erroring out, but they currently change nothing.

| Option | Values | Description |
| :--- | :--- | :--- |
| `pushcert` | `true`, `false`, `if-asked` | No push certificate is read or verified. |

`push-option` is handled but not in this category: `followtags=<bool>` is acted upon and everything
else is answered `unsupported`, so git is never told an option was accepted when it was dropped.

Any other option is answered `unsupported`, per `gitremote-helpers(7)`.

Again: the above is the simple interface. Unless you have turned [protocol v2](#protocol-v2) off,
`filter` and `depth` are not taking these paths.

---

## Protocol v2

On by default. The helper does not answer `fetch <sha> <name>`; it serves git's wire protocol
version 2 directly, the way `git-remote-http` does.

```bash
# to go back to the simple fetch/push commands
git config ociremote.protocolV2 false
```

Turning it off costs the features in the table below and nothing else. `stateless-connect` is
advertised either way and declines with `fallback`, which is a documented reply that leaves the simple
protocol running on the same connection — so it is a real escape hatch if a registry or a client turns
out to have trouble with the v2 path.

**Why it exists.** The simple remote-helper interface has no vocabulary for the things below. They are
not helper capabilities but arguments to protocol-v2 commands, so a helper is never asked about them:

| | Simple interface | Protocol v2 |
| :--- | :--- | :--- |
| `--filter` (partial clone) | Impossible. `fetch` is defined as delivering a complete object graph, and git verifies it. | Served. The filter is applied while the pack is built, and the lazy fetches afterwards are answered too. |
| `object-info` (how big is this object?) | No such question exists. | Advertised and served, and answered from the sizes published in each packfile's index — a few kilobytes read, no history transferred. A repository pushed before sizes existed falls back to fetching the packfile that holds the object, which is never more work than the fetch it replaces. No released git sends this yet; the client-side series is still upstream. |
| `--depth`, `--deepen`, `--shallow-since`, `--shallow-exclude` | Only `--depth`, and then only as a recorded boundary: the whole history is transferred (unless `shallowSnapshot` covers `--depth 1`). | Applied when the pack is built — by generation, by committer date, or by an excluded ref. Deepening, relative deepening and `--unshallow` all work. |
| `ref-prefix` | Never sent. `list` always advertises every ref. | Honoured; the advertisement is narrowed to what was asked for. |
| Annotated tags | Advertised as the commit they peel to — the interface has no peel form. | Advertised properly, with `peeled:`. |

**How it works.** The objects live in registry packfiles, not in a database this process can pack
from, so serving a fetch stages them: the packfiles the wants depend on are imported into a temporary
object directory whose alternate is the real repository, and `git pack-objects` cuts the requested
slice out of the combined view. The staging directory is discarded afterwards, so a failed fetch
leaves nothing behind — unlike the simple path, which writes into the repository as it goes.

**What it costs.** A lazy fetch — git coming back for a blob a partial clone omitted — asks for object
ids that are not commits, so there is no pack-base graph to follow. Refs are searched instead, most
likely first, stopping as soon as the wanted objects turn up. Each ref is checked against the object
index its push published, so ruling one out reads a few kilobytes rather than downloading and
indexing its packfiles: an object on the branch being checked out costs one ref, and one that lives
only on the last ref tried costs one packfile plus an index per ref skipped. Repositories pushed
before the index existed (§4.4 of [FORMAT.md](FORMAT.md)) fall back to staging each ref and looking,
which is correct but pays the full cost.

**Git LFS** works the same as on the simple path. A packfile carries only the *pointer*, so the
objects are downloaded into `.git/lfs/objects` as the packfiles they belong to are staged — there is
no LFS server behind an `oci://` remote to fetch them from later. `--filter` suppresses that download
exactly as it does elsewhere.

**What is not served.**

- **Push**, which is not a protocol-v2 thing to serve. v2 defines `ls-refs`, `fetch` and `object-info`; `git push` speaks the older protocol whatever `protocol.version` says. So `stateless-connect` is declined for `git-receive-pack` and the simple path handles pushing, as it would anyway.
- **Negotiation rounds.** The response is single-round: the server answers `ready` and sends the pack
  immediately rather than trading `have` lines. This is not the compromise it looks like — exclusion
  is transitive, so `^<have>` on the client's most recent commits removes all the shared history
  behind them, and the pack is minimal anyway. A deepening fetch is the exception: there the haves are
  ignored and the whole depth-limited slice is re-sent, because a shallow client's haves cannot be
  safely excluded without knowing which of them are complete.

**Why it is on by default.** `--filter` and a real `--depth n` are the two things people most often
find missing, and neither can be expressed through the simple interface at any price — so leaving v2
behind a switch meant the better implementation was the one nobody got unless they read far enough to
find it. `stateless-connect` is still described by `gitremote-helpers(7)` as "experimental; for
internal use only", which is the argument for keeping the way back rather than for not taking the way
forward: `ociremote.protocolV2=false` returns to the simple path per repository or per remote,
declining with the documented `fallback` reply, and everything already published stays readable either
way.

---

## Environment Variables

| Variable | Description |
| :--- | :--- |
| `OCI_USERNAME` | Username for registry authentication. |
| `OCI_PASSWORD` | Password or personal access token for registry authentication. |
| `OCI_BEARER_TOKEN` / `OCI_TOKEN` | Bearer token for registry authorization. |
| `OCI_COMPRESSION` | Layer compression algorithm: `gzip`, `zstd`, or `none` (`raw` and the empty string are accepted as aliases for `none`). Defaults to `none`, since a packfile is already compressed per object. Anything else is an error. |
| `OCI_INSECURE` | Set to `1` or `true` to allow plain HTTP connections for local development registries (automatically enabled for `localhost:` and `127.0.0.1:`). |
| `GIT_REMOTE_OCI_TIMING` | Set to any value other than `0` or `false` to print a per-phase breakdown to stderr when the helper exits: how long went on resolving the pack graph, reading pack indexes, transferring packfiles, staging them locally, and building and uploading on push. Phases run concurrently, so the totals sum to more than the wall time; that is what makes them useful for "which phase is the work in". |
| `GIT_REMOTE_OCI_FULL_PACK` | Set to any non-empty value to make every push write a self-contained packfile instead of an incremental one. Larger uploads, but the result depends on nothing else on the registry. An escape hatch for a repository whose history you suspect. |
| `USER` | Names the owner of ref locks and Git LFS locks as `$USER@$HOSTNAME`, so a blocked push can say who holds the lock. Falls back to `git-user@localhost`. |
| `GIT_REMOTE_OCI_SUBCOMMAND` | Set to any non-empty value to run a subcommand even though `GIT_DIR` is set. A subcommand taking one URL has the same arguments as git invoking the helper for a remote of that name, and `GIT_DIR` is what tells them apart; this overrides that check for the rare shell that exports `GIT_DIR` itself. |
| `GIT_DIR` | Path to the git directory, used verbatim. Git sets this when it invokes the helper, which is also how a colliding remote name is detected. |
| `GIT_WORK_TREE` | Working tree to pair with `GIT_DIR`. Git sets it where relevant; without it a `GIT_DIR` ending in `.git` uses its parent, and anything else is treated as bare — the same assumption git makes. |
| `GIT_OBJECT_DIRECTORY` | Where objects are read and written, overriding `$GIT_DIR/objects`. Git sets it for a quarantined receive and for some worktree layouts, and it is honoured rather than assumed away — a helper that wrote to `$GIT_DIR/objects` regardless would put objects somewhere git is not looking. |

## Configuration

Tunables are read from `git config`, which is where a git user already looks and can be set per
repository or per remote without exporting anything. Two scopes are consulted, most specific first:

```bash
# Applies to every oci:// remote in this repository.
git config ociremote.concurrency 4

# Applies to one remote. Pushing to a local registry and to a hosted one from
# the same clone are different situations.
git config remote.origin.ociPushLockTTL 30m
```

| Key | Default | Description |
| :--- | :--- | :--- |
| `compression` | `none` | Layer compression: `none`, `gzip` or `zstd`. `OCI_COMPRESSION` overrides it. |
| `shallowSnapshot` | `false` | Publish a self-contained snapshot of each ref tip, so `git clone --depth 1` fetches only that. Off because it costs a full copy of the tip on every push — see [Shallow clones](#5-shallow-clones). Reading a snapshot is unconditional; this key only decides whether you publish them. |
| `protocolV2` | `true` | Serve git's wire protocol v2, which is what makes `--filter` (partial clone) and a real `--depth n` possible. Set it `false` to fall back to the simple fetch/push commands — see [Protocol v2](#protocol-v2). |
| `compactAfter` | `50` | Repack the repository once it has published this many commits, as part of the push that crosses the threshold. Every push adds one commit manifest and one tag that nothing can remove until the history is repacked, so left alone the count grows forever and a clone pays it. `0` disables it, for anyone scheduling [`gc`](#3-maintenance) themselves. |
| `chunkSize` | `32m` | Send blobs larger than this in chunks of this size, so a failed upload resumes instead of restarting. Accepts `k`/`m`/`g`. `0` sends every blob in one request. A registry that will not take chunked uploads falls back automatically. |
| `concurrency` | `12` | Workers fetching manifests and packfiles, and uploading LFS objects. Lower it for a registry that rate-limits, or a slow link. |
| `blobConcurrency` | `64` | Workers for the wide blob fan-out when pushing many refs at once. Larger because those requests mostly wait on the registry. |
| `pushLockTTL` | `10m` | How long one ref's push may hold its lock. Must exceed the time to generate and upload the packfile, or another client can legitimately take the lock mid-push. |
| `indexLockTTL` | `5m` | How long the `_refs` index lock is held. Covers fetching the index, listing refs and pushing four objects. |
| `lfsIndexLockTTL` | `15s` | How long the `_lfs_locks` index lock is held. A shorter critical section: one read-modify-write of a single blob. |

Durations accept a Go duration (`90s`, `10m`, `1h30m`) or a bare integer, read as seconds. A value
that cannot be parsed, or is not positive, falls back to the default rather than failing: a typo in
a tunable should cost a slower transfer, not a push you cannot make. Names are case-insensitive, so
`ociRemote.pushLockTTL` and `ociremote.pushlockttl` are the same key.

Everything else remains an environment variable, and the environment wins over `git config` where
both apply.

---

## OCI Repository Structure

The layout is specified in **[FORMAT.md](FORMAT.md)** — tags, manifests, annotations, the ref-name
encoding, and the rules a reader has to follow. The sketch below is orientation only; FORMAT.md is
the reference, and the code is the truth.

```
oci://registry.example.com/org/repo
├── _refs                                     ref index, format version, whole pack-base graph
├── _index                                    OCI image index, for oras/crane/skopeo
├── _lfs_locks                                Git LFS locks (not reachable from git)
├── _lock_main                                advisory ref lock
├── 4b825dc642cb6eb9a060e54bf8d69288fbee4904  commit manifest: packfile + what it depends on
├── main                                      ref manifest for refs/heads/main
└── _t_v1.0.0                                 ref manifest for refs/tags/v1.0.0
```

A commit manifest carries its packfile plus, optionally, an index of the object ids in it
([FORMAT.md §4.4](FORMAT.md#44-pack-index-layer-optional)) so a partial clone can tell whether a pack
is worth downloading; `_refs` carries the same pack-base edges gathered into one blob
([§6.1](FORMAT.md#61-pack-chain-layer-optional)) so a clone resolves the graph in one round trip
instead of one per push. Both are advisory: a reader that ignores them is slower and never wrong.

The one thing worth knowing before reading further: `io.git-remote-oci.pack-bases` records which
commits a packfile was cut against, and is what fetch follows. It is **not** the same as
`io.git-remote-oci.parents`, which is the commit graph and is metadata. A push carrying several
commits publishes a manifest only for its tip, so the tip's parent usually has no manifest while the
base it was packed against does. See [FORMAT.md §4.2](FORMAT.md#42-pack-bases).

The `application/vnd.git.*` media types are specific to this project and are not registered with
IANA.

---

## Authentication & Credentials

When the registry rejects a request, the error names which of these the credentials came from, since
the resolution order is internal and otherwise invisible. It also distinguishes two cases that a bare
`401 Unauthorized` does not:

- **Never accepted** — nothing this client sent has ever been served, so the credential is wrong,
  missing, or lacks access to the repository.
- **Accepted earlier, now rejected** — the registry was serving this client and has stopped, so
  something expired part-way through. A session obtained from a username and password can be
  re-established by authenticating again; a static `OCI_BEARER_TOKEN` cannot be renewed by anything,
  so the error says to reissue it.

That distinction matters most on a long push, where a token issued with a short lifetime can expire
between the first blob and the last.

`git-remote-oci` automatically resolves authentication credentials in order of priority:

1. **Environment Variables**: `OCI_BEARER_TOKEN`, `OCI_USERNAME`, and `OCI_PASSWORD`.
2. **Docker Config & Credential Helpers**: Reads `~/.docker/config.json`, including the native credential helpers it names under `credsStore` and `credHelpers` (such as `docker-credential-gcloud`, `docker-credential-ecr-login`, `docker-credential-desktop`, `docker-credential-pass`). This is oras-go's credential store, not code of this project's, and the test suite covers the config file but does not exercise a helper.
3. **Anonymous Fallback**: Unauthenticated access for public registries.

---

## Development

The `Makefile` is the single source of truth for build and test commands. CI runs the same work, though it invokes the individual targets rather than `make check`.

```bash
make help       # list all targets

make build      # build the binary
make test       # unit tests with the race detector
make cover      # unit tests with a coverage profile
make fuzz       # a short run of every fuzz target
make lint       # golangci-lint, at the version pinned in the Makefile (config: .golangci.yml)
make check      # everything CI runs on a pull request: fmt-check, tidy-check, vet, lint, test, vulncheck

make vulncheck  # govulncheck: vulnerabilities reachable from this code
make e2e        # end-to-end tests against a real registry:3 container (needs Docker)
make e2e-ghcr   # end-to-end tests against a real hosted registry (needs credentials)
make bench      # large-scale performance suite (needs Docker; slow)
```

`make e2e` and `make bench` start a throwaway `registry:3` container and skip cleanly when no Docker
daemon is available.

`make e2e-ghcr` runs against a real hosted registry, which is the only way to cover the things
`registry:3` is too permissive to catch: the token exchange, manifests being schema-validated, and a
registry that refuses manifest deletion. It needs a target and credentials, and skips without them:

```bash
E2E_GHCR_REPO=ghcr.io/<owner>/<name>/e2e \
OCI_USERNAME=<user> OCI_PASSWORD=<token-with-write:packages> \
make e2e-ghcr
```

Each run namespaces its refs with `E2E_GHCR_REF_SUFFIX` and deletes them afterwards, so repeated and
concurrent runs do not collide. CI runs it against `ghcr.io/mrueg/git-remote-oci/e2e` on pushes to
`main` and weekly; it is skipped on pull requests because a fork's token cannot write packages.

See [AGENTS.md](AGENTS.md) for the architecture, the remote-helper protocol rules, and the
conventions this repository follows.

---

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the full guide and
[SECURITY.md](SECURITY.md) for reporting a vulnerability. In short:

- Keep commits in [Conventional Commits](https://www.conventionalcommits.org/) form
  (`feat(oci): …`, `fix(helper): …`) — the release changelog is generated from them.
- Run `make check` before opening a pull request.
- Do not add a feature to the README until it is implemented and covered by a test. If the README
  and the code disagree, the code is the truth.
- A change to the on-registry layout is a format change: update [FORMAT.md](FORMAT.md) in the same
  commit. Bump `oci.FormatVersion` only when a reader that does not know about the change would
  misread a repository — [FORMAT.md §11](FORMAT.md#11-changing-the-format) says when.

---

## License

Apache License 2.0. See [LICENSE](LICENSE).
