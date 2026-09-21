# Developing pikopod

Everything you need to build, test, and release pikopod from source.

For contribution flow (forking, pull requests, commit style), see
[CONTRIBUTING.md](./CONTRIBUTING.md).

## Prerequisites

- **Go, at the version `go.mod` declares.** That is the only hard requirement,
  and `go.mod` is the source of truth — CI resolves it with
  `go-version-file: go.mod` rather than pinning a number, so this document
  cannot drift out of date the way a hardcoded version would.
- **No cgo.** pikopod builds static binaries with `CGO_ENABLED=0`, including its
  SQLite layer — that is `modernc.org/sqlite`, the pure-Go driver, not
  `mattn/go-sqlite3`. Swapping it for the cgo driver would break every
  cross-compiled release target, so if a change needs cgo it needs a discussion
  first.
- **Python 3** (optional) only if you regenerate synthetic test fixtures.

Any platform Go targets works. **CI runs tests on Linux only**
(`ubuntu-latest`). The darwin and windows entries in the release matrix are
cross-compiles — `go build`, never `go test` — so if you change anything
platform-sensitive, run the suite on your own machine and say so in the pull
request.

## Initial setup

```bash
git clone https://github.com/pikopod/pikopod.git
cd pikopod
go build ./...
```

There is no code generation step, no vendored submodule, and no toolchain to
install. If `go build` succeeds you are set up.

Confirm the binary works:

```bash
go run ./cmd/pikopod demo
```

`demo` is self-contained: it starts a fake provider in-process, sends traffic
through the agent, silently changes the provider's responses, and prints the
alerts. It takes about a second and needs no config.

## Where local state goes

Running pikopod from the repo writes into the repo. All of it is gitignored, so
you will not commit it by accident, but you should know it is there:

| Path | What |
| --- | --- |
| `./pikopod.yaml` | Config, written by `pikopod init` |
| `./pikopod-data/` | Everything else: `recordings/*.ndjson`, `baselines/`, `events.ndjson`, `alerts/state.json`, `scenarios/`, and `.salt` |

`.salt` is the per-install tokenization secret. Deleting it re-tokenizes
everything, so old recordings stop correlating with new ones — that is the
intended behaviour, not a bug, but it will surprise you mid-debug.

To keep an experiment out of the way, point the binary somewhere else:

```bash
go run ./cmd/pikopod --config /tmp/scratch/pikopod.yaml up
```

`--config` is a persistent flag and works on every command. `data_dir` inside
that file decides where the rest lands.

The drift-event schema everything scripts against is
[`schema/drift-event.schema.json`](./schema/drift-event.schema.json), and
`e2e/schema_contract_test.go` fails the build if it and the Go struct diverge.

## Build and test loop

```bash
go build ./...                  # build
go test ./... -race -count=1    # the suite, as CI runs it
go vet ./... && gofmt -l .      # must both be clean
```

All packages pass with `-race`. Keep it that way. A red suite is not a starting
point for review.

### Individual targets

| Command | What it does |
| --- | --- |
| `go build ./...` | Build every package |
| `CGO_ENABLED=0 go build ./cmd/pikopod` | Build the static binary CI ships |
| `go test ./... -race -count=1` | Full suite with the race detector |
| `go test ./internal/sandbox/ -run TestParity -v` | One package, one test |
| `go test ./internal/proxy/ -bench . -benchtime 300x` | Proxy hot-path benchmarks |
| `go vet ./...` | Vet |
| `gofmt -l .` | List unformatted files (must be empty) |
| `go run ./cmd/pikopod demo` | End-to-end smoke test |

The e2e suite lives in `e2e/` and runs as part of `go test ./...`.

## Project structure

`cmd/pikopod` is thin cobra wiring. Everything else lives in `internal/`.

### The data plane

| Package | Role |
| --- | --- |
| `proxy` | Fail-open reverse proxy. No internal error may alter, delay, or drop proxied traffic. |
| `sanitize` | Redact before disk. Classification, format-preserving tokens, fail-closed drops. |
| `store` | Atomic writes, file locks, the per-install salt, NDJSON logs with rotation. |
| `agent` | Composes the data plane: proxy to recorder to learner and differ to alerter. |

### Detection

| Package | Role |
| --- | --- |
| `baseline` | Learns per-endpoint normal, then freezes a reference. |
| `pathtmpl` | Collapses concrete paths into endpoint templates. |
| `drift` | Diffs traffic against the frozen reference. Eight structural finding kinds. |
| `alert` | Dedupe, N-in-window emission, delivery ceiling, persisted ack state, sinks. |
| `volatile` | Volatile-field suggestion and dead-entry linting. |

### Declared changes

| Package | Role |
| --- | --- |
| `specwatch` | Re-fetches the provider's published spec. ETag gated, never advances the pin. |
| `specdiff` | Typed spec-to-spec diff. One function maps change shape onto ERR/WARN/INFO; no check carries a hand-assigned severity. |
| `specupdate` | Format-preserving, additive-only spec patches by YAML AST surgery. |
| `conformance` | Does the provider obey its own documentation? |
| `contract` | The traffic overlay layered beside the spec-derived contract. |

### Import and simulation

| Package | Role |
| --- | --- |
| `ir` | The normalized contract. Every field carries provenance. |
| `importer` | OpenAPI, Swagger 2.0, Postman, GraphQL to IR. |
| `docimport` | Documentation URL to spec. Three deterministic rungs, then an opt-in model rung that needs your own key. |
| `sandbox` | The deterministic engine: routing, auth, validation, synthesis, faults, webhooks. |
| `replay` | Recorded traffic as the sandbox's final resolution tier, plus the CI gate. |
| `scenario` | Pack schema, validator, runner, archetype catalogue, NL authoring. |
| `bridge` | Drift to scenario, recordings to scenario. |

### Output

| Package | Role |
| --- | --- |
| `pr` | GitHub and GitLab forge layer. One marker-tagged comment, updated in place. |
| `fix` | Drift to code change: impact scan, bounded patch, check-gated apply. |
| `errfmt` | The error contract: what, why, fix, docs. |
| `config` | Loads pikopod.yaml. Unknown keys are startup errors. |
| `demo` | The zero-config first-run story. |

## The README recording

`docs/demo/demo.gif` is generated, not hand-made: every byte of program output
in it came from running the real binary. `docs/demo/README.md` has the
regeneration steps and the one rule that matters — regenerate when the output
changes, not on a schedule, because each one is a permanent ~113 KB blob in git
history.

## Parity goldens

`testdata/parity/` holds committed golden files, and several suites diff
against them.

**They are maintainer-regenerated. Do not hand-edit them.**

A pull request that edits a golden to make a test pass is indistinguishable from
one that breaks the behavior the golden protects, and it will be sent back.

### What to do when your change legitimately moves a golden

There is **no regeneration command yet**; regenerating is a manual maintainer
step. The process is:

1. **Leave the golden exactly as it is.** Do not edit it, do not delete it.
2. **Expect the parity job to be red**, and expect `go test ./...` to be red
   locally. That is correct for this kind of change and it is not a sign you
   have done something wrong.
3. **Say so in the pull request**: which golden moved, and why your change
   should move it. That sentence is what gets reviewed.
4. A maintainer regenerates on your branch, from the code, and pushes the
   result. Review then judges the regenerated golden on its own.

This is the one place the "a red suite is not a starting point" rule above does
not apply, and it applies to nothing else.

### Which suites use them

Five packages have `TestParity*` suites — the ones CI's parity job runs:
`internal/importer`, `internal/sanitize`, `internal/sandbox`,
`internal/scenario/archetype`, `internal/scenario/nl`.

Three more read fixtures out of `testdata/parity/` without a parity suite of
their own: `cmd/pikopod`, `internal/ir`, `internal/scenario`. If your change
moves a fixture, check those too — their failures will not look like parity
failures.

## Test fixtures and licensing

Fixtures under `testdata/parity/importer/specs/` are license-checked in CI by
`tools/check-fixture-licenses.sh`. Every file needs an entry in that
directory's `SOURCES.md`, and an unlisted spec fails the build.

No provider content lands without an explicit grant. "Publicly fetchable" is
not a grant. When a test needs a real-world shape you cannot license, generate
a synthetic fixture and name it `synthetic-*`:

```bash
python3 tools/gen-synthetic-fixtures.py
```

## Release

Releases are cut by tag and built by GoReleaser. Each release ships static
binaries for **macOS, Linux and Windows** on amd64 and arm64 (`.tar.gz`, `.zip`
on Windows), an SBOM per archive, a `SHA256SUMS` file signed with cosign, SLSA
provenance, deb and rpm packages, a container image on GHCR, and a Homebrew
formula.

Everything in that list is a published artifact someone may depend on, so it
all has to work — if we stop supporting one, remove it from `.goreleaser.yml`
rather than leaving it unadvertised.

```bash
git tag v0.2.0
git push origin v0.2.0
```

Publishing the Homebrew formula needs the tap repository
[`pikopod/homebrew-tap`](https://github.com/pikopod/homebrew-tap) to exist, and
a `HOMEBREW_TAP_GITHUB_TOKEN` repository secret holding a token with
`contents: write` on it. The default `GITHUB_TOKEN` is scoped to this
repository and cannot push across repositories. The formula is written on the
first tagged release; there is no formula before then.

Verify a release the way a user would. The command lives in exactly one place —
[docs/security.md](docs/security.md#verifying-a-release) — because an
unanchored or wrongly-cased copy silently verifies nothing, and a verification
command that always passes is worse than none.

Two things that command gets right and a hand-written one usually does not: the
organisation is `Pikopod`, capitalised, and the identity is **anchored** to the
release workflow and to `refs/tags/`. An unanchored regexp matches any workflow
in any repository whose identity URL happens to contain the substring.

## House rules

These are enforced in review because they are the reasons pikopod is safe to
run in a production request path.

**Nothing internal may alter the data plane.** The drift agent forwards live
traffic. It serves before it observes, never retries, and isolates every
capture stage. A change that can make a proxied request slower, different, or
absent does not merge, whatever it buys.

**Redaction happens before the disk.** If you touch `internal/sanitize` or
anything that writes records, the canary test in `internal/agent` must still
pass. It seeds sentinels into every position and sweeps every persisted byte.

**A field on the IR that nothing reads is deleted, not parked.**
`internal/ir`'s reader audit fails the build on a field no code outside the
producers reads; serialisation-only fields need a one-line reason in its
exemption list.

**Refuse rather than guess.** If a check cannot be proven, report that it could
not be proven. See how `conformance` counts unverifiable checks, how archetype
binding treats zero candidates as a first-class answer with a reason, and how
`fix` refuses on an empty impact scan.

**Errors are a contract.** Use `errfmt.New(what, why, fix, docs)` rather than
`fmt.Errorf`, and make sure the doc anchor you cite exists.

**Exit codes are API.** `0` clean, `1` the check ran and failed, `2` tool or
configuration error — including any result pikopod cannot stand behind. Never
conflate `1` and `2`. [docs/exit-codes.md](./docs/exit-codes.md) is the
definition; do not restate it in a third place.

**Every model call is opt-in, keyed by the user, and confined to three places.**
"It initiates no network traffic of its own" is a headline promise, so anything
that reaches a model goes through `internal/scenario/nl` and nowhere else.
Today exactly three commands can call one — `scenario create`, `fix`, and
`import` against a documentation URL — and each refuses with a typed error when
no key is configured rather than degrading quietly. Adding a fourth means
adding a row to
[docs/security.md](./docs/security.md#what-leaves-the-machine) naming what it
carries, in the same pull request.

**Comments are two lines maximum.** Say why, not what. Anything longer belongs
in `docs/`, where users can find it.
