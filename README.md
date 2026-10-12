<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/pikopod-mark-dark.svg">
    <img src="docs/assets/pikopod-mark-light.svg" width="96" height="96" alt="pikopod">
  </picture>
</p>

<h1 align="center">pikopod</h1>

<p align="center">
  Every failure a third-party API throws at production becomes a test on your laptop the same day,<br>
  and never reaches production twice. A sandbox built from the provider's spec, corrected by what the provider actually sends you.
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

**Their sandbox only knows how to succeed.** So the first time your retry path runs for real, it runs against real money.

pikopod builds a sandbox from your provider's spec, makes it fail on purpose, learns from what the provider really sends, and replays the exact failure production hit, on your laptop, as a test you keep.

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

### Shell completion

Run the one-liner for your shell to enable completion in the current session:

Bash:

```bash
source <(pikopod completion bash)
```

Zsh (run `autoload -Uz compinit && compinit` first if completion is not initialized):

```zsh
source <(pikopod completion zsh)
```

Fish:

```fish
pikopod completion fish | source
```

PowerShell:

```powershell
pikopod completion powershell | Out-String | Invoke-Expression
```

To enable completion in future sessions, add the line to `~/.bashrc`,
`~/.zshrc`, `~/.config/fish/config.fish`, or your PowerShell `$PROFILE`, respectively.

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
| **Observe** | A fail-open proxy in front of the real provider. It records failures from the first request, reports what the provider did that the sandbox would not have, and shape changes once it knows what normal is. Every finding carries a fingerprint. | [Observe](https://docs.pikopod.com/observe/overview) · [Divergence](https://docs.pikopod.com/observe/divergence) |
| **Reproduce** | The fingerprint becomes a scenario that replays the production failure against the sandbox. Commit it, and the path is guarded forever. The recordings also answer the sandbox before the spec, and a number says how much of the provider's real traffic it reproduces. | [Reproduce](https://docs.pikopod.com/reproduce/reproduce) · [Truthfulness](https://docs.pikopod.com/observe/truthfulness) |

You cannot ask a provider's sandbox to return that exact 503, with that body,
at that point in your state machine. pikopod can, because the same tool
recorded it and owns the sandbox. See [The loop](https://docs.pikopod.com/getting-started/the-loop).

## Start here: turn last week's provider incident into a test

With `pikopod up` in front of the real provider in staging or production,
every failure the provider throws is recorded, redacted, and listed with a
handle:

```bash
pikopod agent incidents
```

```
[ERR] incident upstream_error         POST /charges (examplepay) · 4 occurrence(s) · last 2026-09-19T10:00:00Z
  fp_14835fa32dfb
  reproduce: pikopod reproduce fp_14835fa32dfb
  export: pikopod agent incidents export fp_14835fa32dfb
```

```bash
pikopod reproduce fp_14835fa32dfb
```

```
reproduced fp_14835fa32dfb (examplepay answered 503 on POST /charges) as pikopod-data/scenarios/incident-14835fa32dfb.yaml
PASSED — 1 assertion(s) passed; 0 not evaluated
the failure now happens locally — fix it, then re-run: pikopod scenario check examplepay incident-14835fa32dfb
```

The generated pack is an ordinary scenario: commit it and it guards that path
on every build. When the agent runs on another host,
`pikopod agent incidents export <fp>` writes a bundle that `reproduce` accepts
anywhere. See [Incidents](https://docs.pikopod.com/observe/incidents) and
[Reproduce](https://docs.pikopod.com/reproduce/reproduce).

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
pikopod scenario check examplepay declines retry_storm
```

```
✓ declines — PASSED (4 assertion(s) passed; 0 not evaluated)
    NOT_EVALUATED  arm-decline      armed error on POST /charges
    PASSED         declined         POST /charges → 400
    NOT_EVALUATED  clear            cleared matching faults
    PASSED         recovered        POST /charges → 201
✓ retry_storm — PASSED (4 assertion(s) passed; 3 matcher(s) matched in order; 0 not evaluated)
    NOT_EVALUATED  arm              armed error on POST /charges
    PASSED         attempt1         POST /charges → 503
    PASSED         attempt2         POST /charges → 503
    PASSED         attempt3         POST /charges → 201
    PASSED         storm-shape      3 matcher(s) matched in order
```

That proves the sandbox fails the way the story says. To prove your own code
survives it, serve the sandbox with `pikopod up`, put it into the story's
standing state, run your tests against `:4600/examplepay`, then ask:

```bash
pikopod mode set examplepay retry_storm
pikopod mode verify examplepay
```

```
✓ retry_storm — PASSED (3 matcher(s) matched in order)
    PASSED         storm-shape      3 matcher(s) matched in order
```

`verify` reads what your client actually sent and exits `1` when it fell
short, so it can sit in CI next to your test suite. `pikopod chaos` arms one
fault directly. See [Modes](https://docs.pikopod.com/sandbox/modes) and
[Faults](https://docs.pikopod.com/sandbox/faults).

## Observe: what the provider did that the sandbox would not have

`pikopod up` also starts the observing agent on `:4700/examplepay`. Point your
app's provider base URL at it, keeping your real credentials; it forwards
everything untouched and watches. Incidents fire from the first request. So
does divergence: every production answer is compared with what the sandbox
would have said, and a 422 on a duplicate the spec never mentions becomes an
event with a fingerprint and a reproduce command. Drift waits 50 samples and
48 hours per endpoint, because a baseline built from five responses has not
seen your optional fields yet.

The recordings then answer the sandbox before the spec, so the sandbox learns
from production, and `pikopod agent truthfulness examplepay` says how much of
what the provider really sent it reproduces. `pikopod agent replay --ci` gates
every build on recorded traffic, with no network and no provider account. See
[Divergence](https://docs.pikopod.com/observe/divergence),
[Precedence](https://docs.pikopod.com/sandbox/precedence),
[Truthfulness](https://docs.pikopod.com/observe/truthfulness) and
[Replay gate](https://docs.pikopod.com/observe/replay-gate).

## Also: gate CI on a breaking spec change

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

Exit `0` clean, `1` breaking, `2` tool error. In GitHub Actions it is two
lines, with the release verified by cosign before it runs and every finding
inline on the pull request diff:

```yaml
- uses: Pikopod/spec-diff-action@v1
  with: { old: "git:origin/main:openapi.yaml", new: openapi.yaml }
```

See [Spec diff](https://docs.pikopod.com/gate/spec-diff) and
[CI integration](https://docs.pikopod.com/gate/ci-integration).

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
