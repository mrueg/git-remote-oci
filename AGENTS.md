# AGENTS.md

Guidance for AI agents and new contributors working on `git-remote-oci`.

`git-remote-oci` is a Git **remote helper** that stores Git repositories inside OCI registries.
Git invokes it as `git-remote-oci <remote> <url>` for `oci://` URLs and talks to it over
stdin/stdout using the protocol in `gitremote-helpers(7)`.

Status: **experimental**. The on-registry format is versioned — `oci.FormatVersion`, currently `1` —
and a reader refuses anything else. Versioned is not the same as stable: there is no compatibility
path and no read path for a superseded version. The number is a tripwire against a layout this build
would misread, not a release counter. See [FORMAT.md §11](FORMAT.md#11-changing-the-format), which
is the authority on this.

---

## The one rule that matters most

**stdout *is* the wire protocol.** Every byte written to stdout is parsed by Git as a protocol
response. A stray `fmt.Println`, a debug print, or an interleaved write from a goroutine corrupts
the session — usually as a confusing Git-side error far from the cause.

- Write protocol responses **only** through `Helper.printfOut` / `Helper.printlnOut`
  (`pkg/helper/helper.go`).
- Write diagnostics **only** to stderr, and only through `logInfo` / `logVerbose` / `logWarn`, which
  respect `option verbosity`. Do not call `fmt.Fprintf(os.Stderr, …)` directly.
- Protocol output from concurrent goroutines must be serialised. Both the fetch and push paths run
  `errgroup` worker pools.

Before changing anything in `Run`, `handleList`, `handleFetchBatch`, or `handlePushBatch`, read
`man 7 gitremote-helpers`. In particular:

- `capabilities` output must list **real** capability names, terminated by a blank line. The
  mandatory marker is `*`, not `+`. Option names (`filter`, `deepen`, …) are not capabilities.
- `list` output is `<value> <name> [<attr>…]`, where `<value>` is a hex object id, `@<dest>` for a
  symref, `:<keyword> <value>`, or `?`. There is no `^`-prefixed peel form.
- `fetch` and `push` arrive in batches terminated by a blank line, and each batch's responses must
  also be terminated by a blank line — including on failure. Report per-ref problems as
  `error <ref> <why>`, not by returning an error from the batch handler.
- A `lock <file>` line must be the **absolute** path of a `.keep` file under `$GIT_DIR/objects/pack`.
  Git unlinks exactly that path afterwards.

---

## Layout

| Package | Responsibility |
|---|---|
| `main.go` | Argument handling, signal context, wiring only. Keep it thin. |
| `pkg/cli` | Subcommand dispatch: `gc`, `fsck`, `set-head`, `break-lock`, `lfs-*`, `version`, `help`. Add a subcommand to the `subcommands` table in `cli.go` and nowhere else — dispatch, the reserved-name set and the usage text are all derived from it. A two-argument call is ambiguous with git's `<remote> <url>` invocation; `GIT_DIR` being set decides it, and a colliding remote name is refused rather than dispatched. |
| `pkg/helper` | The protocol state machine. The only package that may touch stdout. Serving protocol v2 stages packfiles into a scratch object store and shells out to `git pack-objects` and `git index-pack` against it. |
| `pkg/gc` | Compaction: repack each ref self-contained, then prune what that makes safe to remove. |
| `pkg/git` | go-git wrapper: open repo, resolve refs, build/import packfiles, tag metadata. Shells out to `git pack-objects` when pushing (the snapshot too) and `git index-pack` / `git unpack-objects` when fetching; everything else — revision walks, object lookups, sizes — is go-git. |
| `pkg/oci` | ORAS registry client: manifests, blobs, the `_refs`/`_index`/`_lfs_locks` tags, ref and LFS locking, retry transport, compression. |
| `pkg/lfs` | Git LFS pointer parsing and local object storage. |
| `pkg/config` | Reads tunables from `git config`: `ociremote.<key>` and the per-remote `remote.<name>.oci<key>`. One `git config --list -z` exec, so scope precedence and `includeIf` are git's. A leaf; never fails, only falls back to defaults. |
| `internal/registrytest` | Shared in-process OCI registry and a seeded repository, for tests. Use it rather than writing another mock: the copies it replaced had drifted, and one could not serve a manifest by digest, so its deletion assertions passed without a DELETE ever being issued. `Observe` hooks each request, `Intercept` answers one in the registry's place (a 500 on one tag, say), `Requests` is the log of what was served, and the `Set*` helpers mutate stored state directly — between them they are how a test reproduces an interleaving, which is the only way to exercise a race against something with no transactions. Its blob upload verifies the digest the way a real registry does, so a fixture that stores bytes under the wrong digest fails here rather than only in production. |
| `test/` | End-to-end tests against a real `registry:3` container, and against zot in CI — `E2E_REGISTRY_IMAGE` and `E2E_REGISTRY_AUTH` point them at either, or another — plus GHCR when credentials are present. How a registry is *administered* is outside the distribution spec, so anything depending on it is declared per registry and then verified to have taken effect, rather than assumed; see the authentication test. |
| `benchmark/` | Large-scale end-to-end performance suite. |

Dependencies point one way, from the entry points inwards:

```
main.go → cli → {helper, gc, git, oci, lfs, config}
                 helper → {gc, git, oci, lfs, config}
                 gc     → {git, oci, lfs, config}
                 git    → {lfs, config}
                 oci    → {lfs, config}
```

Nothing may import `pkg/helper` except `pkg/cli`; `pkg/oci` must not import `pkg/git`. `pkg/lfs` and
`pkg/config` are the leaves and import nothing internal, which is what lets everything above depend
on them without a cycle.

Those are the only git subcommands the binary runs: `pack-objects`, `index-pack`, `unpack-objects`
and `config`. If a change adds another, say so here and in the README's requirements list.

`pkg/helper` imports `pkg/gc` so a push can compact the repository once it has accumulated enough
commits (`ociremote.compactAfter`). The direction is one-way — `pkg/gc` knows nothing about the
protocol — so it neither forms a cycle nor lets compaction reach stdout.

`pkg/config` is read at the entry points and the resolved values are handed downwards —
`oci.Client.ApplyConfig`, the pool sizes on `Helper`. The registry client is *told* its settings; it
must never go looking for a git repository, because the subcommands can be run outside one.

## On-registry format

**[FORMAT.md](FORMAT.md) is the specification.** Read it before touching anything that writes to a
registry. The points that catch people out:

- One OCI image manifest per pushed *tip*, tagged with that commit's id. Commits between one push
  and the next are inside the packfile and have no manifest of their own — never assume an arbitrary
  commit is addressable.
- `io.git-remote-oci.pack-bases` is the packfile dependency graph and is what fetch walks;
  `io.git-remote-oci.parents` is the *commit* graph and is metadata. They coincide only when a push
  carried a single commit, and confusing the two is what made multi-commit pushes unclonable. The
  annotation is mandatory: absent, empty or malformed is an error, and a declared base that cannot be
  fetched is an error. Never downgrade either to a warning.
- Commit-id tags are load-bearing — they are the bases later packfiles were cut against — so they
  cannot be pruned without first rewriting the dependents as self-contained.
- A **snapshot layer is not the packfile.** It carries a packfile media type but holds a
  self-contained copy of the tip for `--depth 1`, so anything selecting "the packfile" must skip
  layers annotated `io.git-remote-oci.snapshot` rather than take the first match. Publishing one is
  opt-in (`ociremote.shallowSnapshot`); *reading* one is unconditional, because a fresh clone has no
  configuration to consult.
- `oci.EncodeRefTag` is injective and is the only ref→tag mapping. There is no second scheme to fall
  back to.
- The **pack index** (§4.4) and the **pack chain** (§6.1) are optional and advisory. They make a
  partial clone and a clone cheap respectively, and a reader that ignores either must still be
  *correct* — only slower. This is the pattern to copy for anything new: say what a reader that
  ignores it loses, and never let it authorise skipping a fetch.
- A repository whose `io.git-remote-oci.format-version` is not `oci.FormatVersion` is refused,
  loudly. That is about the *version*, not about every older shape: a manifest without a pack index,
  or with a v1 index that records no sizes, is read perfectly well. Absent-and-optional is the
  cheap way to evolve, because it costs no version bump; changing what an existing field means is
  the expensive way, and has to bump.

Any change to manifest shape, annotations, media types, or the tag mapping is a format change.
Describe it in FORMAT.md **in the same commit**, in the section it belongs to, and say so in the
commit message.

Whether to bump `oci.FormatVersion` turns on one question: *what would a reader that does not know
about this change do?*

- It can ignore the change and still be correct — an optional layer, an advisory annotation — then
  **do not bump**. Bumping refuses repositories to readers that would have coped.
- It would **misread** the repository — a field whose meaning moves, a layer it mistakes for
  another — then **bump**. A reader refuses a version it does not implement, and that refusal is the
  only thing standing between an old build and a wrong answer.

The version may move whenever a change needs it to, 0.x included. It is not a release counter.

## Concurrency

Both fetch and push fan out over `errgroup` pools, and `pkg/oci.Client` holds caches shared across
those goroutines. When touching these paths:

- Assume go-git storage is **not** goroutine-safe.
- Never lazily initialise shared `Helper` fields from inside a worker.
- Run `make test` (`go test -race ./pkg/...`) — CI does, and these paths have had real races.

## Commands

Use the `Makefile`; it is the single source of truth and CI calls the same targets.

```
make build      # build the binary
make fmt        # gofmt -w
make fmt-check  # fail if anything is not gofmt-clean
make tidy-check # fail if go.mod/go.sum are not tidy
make vet        # go vet ./...
make lint       # golangci-lint run, at GOLANGCI_LINT_VERSION (config in .golangci.yml)
make test       # go test -race ./pkg/...
make cover      # coverage profile, summary, and a floor (COVER_MIN)
make fuzz       # every fuzz target for FUZZTIME (default 15s, a smoke run)
make e2e        # end-to-end tests; requires a running Docker daemon
make e2e-ghcr   # end-to-end against a real hosted registry; needs credentials
make vulncheck  # govulncheck, at GOVULNCHECK_VERSION
make bench      # large benchmark suite; requires Docker; slow
make check      # fmt-check + tidy-check + vet + lint + test + vulncheck
```

The tool versions are pinned in the Makefile and CI reads them from there, so a bump is one edit.

`make e2e` and `make bench` start a real `registry:3` container and skip cleanly when Docker is not
available.

## Conventions

- **Conventional Commits** (`feat(helper): …`, `fix(oci): …`, `perf(git): …`, `docs:`, `test:`,
  `ci:`). `.goreleaser.yaml` filters the changelog on these prefixes, so the format is load-bearing.
- Code must be `gofmt`-clean and pass `golangci-lint run`; CI enforces both.
- Do not add a feature to the README until it is implemented and covered by a test. If a README
  claim and the code disagree, the code is the truth — fix the README in the same change. FORMAT.md
  is different: it is normative, so a requirement the code does not meet is a bug in the code.
- Errors that indicate data loss (a failed object import, a failed blob upload, a failed lock
  release) must be propagated, never swallowed. Several past bugs were exactly this.
- Content fetched from a registry is untrusted. Validate identifiers before using them in
  filesystem paths, and verify digests before storing content.
