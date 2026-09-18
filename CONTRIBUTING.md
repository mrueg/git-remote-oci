# Contributing

Pull requests are welcome. This is an experimental, pre-1.0 project, so the bar
is "does it work and is it honest about what it does" rather than "is it
finished".

## Before you open a pull request

```bash
make check      # fmt, tidy, vet, lint, unit tests with -race, govulncheck
make e2e        # end-to-end against a throwaway registry:3 (needs Docker)
```

`make help` lists everything. CI runs the same work.

## The rules that actually matter

**stdout is the wire protocol.** Every byte written to stdout is parsed by git.
Use `Helper.printfOut` / `printlnOut` for protocol responses and the `logInfo` /
`logVerbose` / `logWarn` helpers for anything human-facing, which go to stderr.
A stray `fmt.Println` corrupts the session. See [AGENTS.md](AGENTS.md) for the
protocol details this depends on.

**A change to the on-registry layout is a format change.** Update
[FORMAT.md](FORMAT.md) *in the same commit*, in the section it belongs to.

Bump `oci.FormatVersion` only if a reader that does not know about the change
would *misread* a repository containing it. If it can ignore the change and
still be correct — an optional layer, an advisory annotation — leave the version
alone, because bumping refuses repositories to readers that would have coped. A
reader refuses a version it does not implement rather than guessing, so the
number is a tripwire, not a history, and not a release counter.

FORMAT.md is a specification, with RFC 2119 keywords and a conformance section.
Where it and the code disagree on a *requirement*, the code is the bug — the
opposite of the README rule below.

**Registry content is untrusted.** Validate every identifier before it becomes a
tag name or a filesystem path, and verify digests.

**Never downgrade a data-loss failure to a warning.** Nothing on the registry
side checks reachability, so a fetch that cannot reach a declared pack base must
fail loudly. Several real bugs here were warnings that `git fetch -q` then hid.

**Do not document a feature before it works.** If the README and the code
disagree, the code is the truth and the README is a bug. Options that are
accepted but not honoured belong in the Limitations table, not the feature list.

## Tests

New behaviour needs a test. For a bug fix, the useful question is *"does this
test fail without the fix?"* — please check, and say so in the pull request.
Several fixes here looked right and were verified only because someone ran the
test against the unfixed code.

Prefer the end-to-end suite for anything touching the protocol or the registry.
It drives real `git` against a real registry, and it has caught things unit
tests against the mock could not — a shallow clone that produced a repository
git refused, and an `fsck` that resolved annotated tags the wrong way.

## Commits

[Conventional Commits](https://www.conventionalcommits.org/) — `feat(oci): …`,
`fix(helper): …`. The release changelog is generated from them, and `!` marks a
compatibility break.

Explain *why* in the body. The diff already shows what changed.
