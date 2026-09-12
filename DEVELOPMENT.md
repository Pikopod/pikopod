# Developing pikopod

Everything you need to build, test, and release pikopod from source.

For contribution flow (forking, pull requests, commit style), see
[CONTRIBUTING.md](./CONTRIBUTING.md).

## Prerequisites

- **Go 1.22 or newer.** That is the only hard requirement.
- **No cgo.** pikopod builds static binaries with `CGO_ENABLED=0`, including
  its SQLite layer. If a change needs cgo, it needs a discussion first.
- **Python 3** (optional) only if you regenerate synthetic test fixtures.

Any platform Go targets works. CI builds and tests on Linux and macOS.

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
| `specdiff` | Typed spec-to-spec diff. Severity derived by law, never hand-assigned. |
| `specupdate` | Format-preserving, additive-only spec patches by YAML AST surgery. |
| `conformance` | Does the provider obey its own documentation? |
| `contract` | The traffic overlay layered beside the spec-derived contract. |

### Import and simulation

| Package | Role |
| --- | --- |
| `ir` | The normalized contract. Every field carries provenance. |
| `importer` | OpenAPI, Swagger 2.0, Postman, GraphQL to IR. |
| `docimport` | Documentation URL to spec, by deterministic rungs. |
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

## Parity goldens

`testdata/parity/` holds committed golden files, and several suites diff
against them.

**They are maintainer-regenerated. Do not hand-edit them.**

If your change legitimately alters a golden, say so in the pull request and
leave the golden alone. A pull request that edits a golden to make a test pass
is indistinguishable from one that breaks the behavior the golden protects, and
it will be sent back.

Golden-backed suites: `internal/importer`, `internal/sanitize`,
`internal/sandbox`, `internal/scenario/archetype`, `internal/scenario/nl`.

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
binaries for macOS and Linux on amd64 and arm64, a `SHA256SUMS` file signed
with cosign, SLSA provenance, deb and rpm packages, a container image on GHCR,
and a Homebrew formula.

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

Verify a release the way a user would:

```bash
sha256sum -c SHA256SUMS --ignore-missing
cosign verify-blob --certificate SHA256SUMS.pem --signature SHA256SUMS.sig SHA256SUMS \
  --certificate-identity-regexp 'github.com/pikopod/pikopod' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

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

**Refuse rather than guess.** If a check cannot be proven, report that it could
not be proven. See how `conformance` counts unverifiable checks, how archetype
binding treats zero candidates as a first-class answer with a reason, and how
`fix` refuses on an empty impact scan.

**Errors are a contract.** Use `errfmt.New(what, why, fix, docs)` rather than
`fmt.Errorf`, and make sure the doc anchor you cite exists.

**Exit codes are API.** `0` clean, `1` drift found, `2` tool or configuration
error. Never conflate `1` and `2`. See [docs/exit-codes.md](./docs/exit-codes.md).

**Comments are two lines maximum.** Say why, not what. Anything longer belongs
in `docs/`, where users can find it.
