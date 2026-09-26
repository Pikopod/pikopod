<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/pikopod-mark-dark.svg">
    <img src="docs/assets/pikopod-mark-light.svg" width="96" height="96" alt="pikopod">
  </picture>
</p>

<h1 align="center">pikopod</h1>

<p align="center">
  A sandbox for the third-party APIs you depend on. Built from the provider's spec,<br>
  it fails on purpose, and it replays the exact failure production hit.
</p>

<p align="center">
  <a href="https://github.com/Pikopod/pikopod/actions/workflows/ci.yml"><img src="https://github.com/Pikopod/pikopod/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://github.com/Pikopod/pikopod/releases"><img src="https://img.shields.io/github/v/release/Pikopod/pikopod" alt="Release"></a>
  <a href="https://pkg.go.dev/github.com/pikopod/pikopod"><img src="https://pkg.go.dev/badge/github.com/pikopod/pikopod.svg" alt="Go Reference"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-blue.svg" alt="License: Apache-2.0"></a>
</p>

<p align="center">
  <a href="https://docs.pikopod.com">Docs</a> ·
  <a href="https://docs.pikopod.com/getting-started/quickstart">Quickstart</a> ·
  <a href="https://pikopod.com">pikopod.com</a>
</p>

**Their sandbox only knows how to succeed.** It has never declined a charge in
a way you didn't ask for, never timed out halfway through, never delivered the
same webhook twice. So the first time your retry path runs for real, it runs
against real money. pikopod builds a sandbox from your provider's own spec,
makes it fail on purpose, and when production fails anyway, replays that exact
failure back into it so you fix it on a laptop and keep the fix as a test.

One Go binary. Runs locally. No accounts, no telemetry, no cloud.

![pikopod: import a spec, see which failures bind, make it fail, and reproduce a production incident](docs/demo/demo.gif)

## Install

```bash
go install github.com/pikopod/pikopod/cmd/pikopod@latest
```

Or with Homebrew:

```bash
brew trust pikopod/tap
brew install pikopod/tap/pikopod
```

Recent Homebrew requires third-party taps to be trusted explicitly; without the
first line it reports `Invalid formula`. Or download a signed binary from
[Releases](https://github.com/Pikopod/pikopod/releases): macOS and Linux, amd64
and arm64, static, zero dependencies. Every release ships `SHA256SUMS`, cosign-signed,
with SLSA provenance; the verification command is in [Installation](https://docs.pikopod.com/getting-started/installation).

## Try it in ten seconds

```bash
pikopod demo
```

Zero config. It stands up a fake provider, sends traffic, silently changes the
provider's responses, and prints the alerts. Runs in about a second.

## Three things, one tool

| | What it does | Docs |
|---|---|---|
| **Rehearse** | A deterministic sandbox built from the provider's spec. Eleven failure stories bind to it with nothing authored: declines, timeouts, retry storms, duplicate webhooks. | [Sandbox](https://docs.pikopod.com/sandbox/overview) · [Scenarios](https://docs.pikopod.com/scenarios/overview) |
| **Observe** | A fail-open proxy in front of the real provider. It records failures from the first request and shape changes once it knows what normal is, then hands you a fingerprint. | [Observe](https://docs.pikopod.com/observe/overview) |
| **Reproduce** | The fingerprint becomes a scenario that replays the production failure against the sandbox. Commit it, and the path is guarded forever. | [Reproduce](https://docs.pikopod.com/reproduce/reproduce) |

You cannot ask a provider's sandbox to return that exact 503, with that body,
at that point in your state machine. pikopod can, because the same tool
recorded it and owns the sandbox. See [The loop](https://docs.pikopod.com/getting-started/the-loop).

## Start here: catch a breaking change in CI

No proxy, no account, no config file. It reads two versions of a spec straight
from git with no checkout and fails the build on a breaking change:

```bash
pikopod spec-diff git:origin/main:openapi.yaml openapi.yaml --fail-on ERR
```

```
1 change(s): 1 ERR, 0 WARN, 0 INFO

ERR  GET    /charges/{id}                            endpoint-removed
     endpoint removed from the spec  [fp_bcc85ba9a094]

breaking declared drift at/above ERR — failing the gate (exit 1)
```

Exit `0` clean, `1` breaking, `2` tool error. Add `--format githubactions` and
every finding lands inline on the pull request diff. Severity comes from one
fixed rule, so "breaking" means the same thing on every endpoint and every
provider. See [Spec diff](https://docs.pikopod.com/gate/spec-diff) and
[CI integration](https://docs.pikopod.com/gate/ci-integration).

## Rehearse: make the sandbox fail

```bash
curl -fsSL -o examplepay.spec.json https://raw.githubusercontent.com/pikopod/pikopod/main/docs/demo/examplepay.spec.json
pikopod init
pikopod import examplepay --spec ./examplepay.spec.json
pikopod scenario list examplepay
```

`--spec` also takes your provider's spec URL, or its documentation page.

```
archetypes vs examplepay (4 endpoints):
  ✓ happy_path                 Happy path  (1 candidate binding(s))
  ✓ unauthorized               Unauthorized  (4 candidate binding(s))
  ✓ invalid_request            Invalid request  (1 candidate binding(s))
  ✗ duplicate_delivery         Duplicate delivery
      no webhookEvent matching {} for role 'emittedEvent'
  ✓ rate_limit_backoff         Rate limit and backoff  (4 candidate binding(s))
  ✓ state_transition_sequence  State transition sequence  (1 candidate binding(s))
  ✓ retry_storm                Retry storm with recovery  (1 candidate binding(s))
  ✓ declines                   Declines  (1 candidate binding(s))
  ✓ timeouts                   Timeouts  (1 candidate binding(s))
  ✓ partial_failure            Partial failure  (1 candidate binding(s))
  ✓ downtime_recovery          Downtime and recovery  (1 candidate binding(s))
```

Ten stories bound to four endpoints with nothing authored. The one that did
not says which fact the spec is missing.

```bash
pikopod scenario run examplepay declines retry_storm
```

```
✓ declines — PASSED (4 assertion(s) passed; 0 not evaluated)
    NOT_EVALUATED  arm-decline      armed error on POST /charges
    PASSED         declined         POST /charges → 400
    NOT_EVALUATED  clear            cleared matching faults
    PASSED         recovered        POST /charges → 201
✓ retry_storm — PASSED (4 assertion(s) passed; 0 not evaluated)
    NOT_EVALUATED  arm              armed error on POST /charges
    PASSED         attempt1         POST /charges → 503
    PASSED         attempt2         POST /charges → 503
    PASSED         attempt3         POST /charges → 201
```

`pikopod scenario list examplepay` shows every story that bound and, for any
that did not, which fact in the spec was missing. `pikopod up` serves the
sandbox on `:4600/examplepay` so your own tests meet the same failures, and
`pikopod chaos` arms one fault directly. See [Faults](https://docs.pikopod.com/sandbox/faults).

## Observe and reproduce

`pikopod up` also starts the observing agent on `:4700/examplepay`. Point your
app's provider base URL at it, keeping your real credentials; it forwards
everything untouched and watches. Incidents fire from the first request. Drift
waits 50 samples and 48 hours per endpoint, because a baseline built from five
responses has not seen your optional fields yet. `pikopod incidents` lists
what failed, newest first, each with a fingerprint:

```bash
pikopod scenario reproduce fp_14835fa32dfb
```

```
reproduced fp_14835fa32dfb (examplepay answered 503 on POST /charges) as pikopod-data/scenarios/incident-14835fa32dfb.yaml
PASSED — 1 assertion(s) passed; 0 not evaluated
the failure now happens locally — fix it, then re-run: pikopod scenario run examplepay incident-14835fa32dfb
```

The generated pack is an ordinary scenario: commit it and it guards that path
forever. `pikopod replay --ci` then gates every build on recorded traffic, with
no network and no provider account. For a shape change rather than a failure,
`pikopod scenario from-drift <fp>` pins the old contract instead. See
[Drift](https://docs.pikopod.com/observe/drift) and [Replay gate](https://docs.pikopod.com/observe/replay-gate).

## It cannot slow your traffic down

The agent serves first and observes afterwards: observation is asynchronous,
bounded, and panic-isolated, so if pikopod breaks internally your traffic still
flows. It never retries, because a retry in front of a payments API is a
double-charge window. It redacts before anything touches disk: credentials
become placeholders, identifiers become format-preserving tokens, and
unclassifiable strings are dropped. See [Data plane safety](https://docs.pikopod.com/observe/data-plane-safety)
and [Redaction](https://docs.pikopod.com/observe/redaction).

## Use it from a coding agent

`pikopod mcp` serves the same checks over the Model Context Protocol, and
`UNVERIFIABLE` is never dressed up as `CLEAN`. See [Coding agents](https://docs.pikopod.com/getting-started/coding-agents).

```json
{ "mcpServers": { "pikopod": { "command": "pikopod", "args": ["mcp"] } } }
```

## Documentation

[docs.pikopod.com](https://docs.pikopod.com): [Quickstart](https://docs.pikopod.com/getting-started/quickstart) ·
[Configuration](https://docs.pikopod.com/operations/configuration) · [Exit codes](https://docs.pikopod.com/operations/exit-codes) ·
[Security](https://docs.pikopod.com/operations/security) · [CLI reference](https://docs.pikopod.com/reference/cli/overview)

Contributing: [CONTRIBUTING.md](CONTRIBUTING.md) · [DEVELOPMENT.md](DEVELOPMENT.md) ·
[SECURITY.md](SECURITY.md) · [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) ·
[RELEASE.md](RELEASE.md)

## License

Apache-2.0. See [LICENSE](LICENSE).
