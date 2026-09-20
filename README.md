# pikopod

**Their sandbox only knows how to succeed.** It has never declined a charge in
a way you didn't ask for, never timed out halfway through, never delivered the
same webhook twice. So the first time your retry path runs for real, it runs
against real money.

pikopod builds a sandbox from your provider's own spec, makes it fail on
purpose, and — when something goes wrong in production anyway — replays that
exact failure back into it.

One Go binary. Runs locally. No accounts, no telemetry, no cloud.

![pikopod: which failure modes does my integration have?](docs/demo/demo.gif)

The same thing as text, so you can copy it:

```
$ pikopod scenario list examplepay
archetypes vs examplepay (4 endpoints):
  ✓ declines                   Declines  (1 candidate binding(s))
  ✓ timeouts                   Timeouts  (1 candidate binding(s))
  ✓ retry_storm                Retry storm with recovery  (1 candidate binding(s))
  ✓ rate_limit_backoff         Rate limit and backoff  (4 candidate binding(s))
  ✗ duplicate_delivery         Duplicate delivery
      no webhookEvent matching {} for role 'emittedEvent'

$ pikopod scenario run examplepay declines retry_storm
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

No proxy, no account, nothing in your request path — and nothing to author. The
failure stories bind themselves to *your* API from its specification, and the
one that cannot bind says which fact was missing rather than guessing a test
into existence.

## Install

```bash
go install github.com/pikopod/pikopod/cmd/pikopod@latest
```

Or with Homebrew:

```bash
brew trust pikopod/tap
brew install pikopod/tap/pikopod
```

Recent Homebrew requires third-party taps to be trusted explicitly. Without
that first line it refuses to load the formula and reports it as
`Invalid formula` — the formula is fine, the tap simply is not trusted yet.

Or download a signed binary from
[Releases](https://github.com/Pikopod/pikopod/releases) — macOS and Linux,
amd64 and arm64, static, zero dependencies.

Every release ships `SHA256SUMS`, cosign-signed, with SLSA provenance. The
verification command is in
[docs/security.md](docs/security.md#verifying-a-release) — one copy, because a
cosign command that loses its anchor still runs and still passes.

## Try it in ten seconds

```bash
pikopod demo
```

Zero config. It stands up a fake provider, sends traffic, silently changes the
provider's responses, and prints the alerts. Runs in about a second.

## One loop, not three tools

Detecting the change is the easy part. The rest is being able to *reproduce* it,
fix it, and keep it fixed. pikopod is one cycle, and each stage feeds the next:

| | Stage | Command |
|---|---|---|
| 0 | **Gate** — fail the build when a spec changes shape. No proxy, no account. | `pikopod spec-diff` |
| 1 | **Integrate** — a spec becomes a stateful sandbox | `pikopod import` |
| 2 | **Rehearse** — every failure production will throw, not just the happy path | `pikopod scenario list` · `run` · `chaos` |
| 3 | **Ship** | — |
| 4 | **Observe** — a proxy watches your traffic for failures and shape changes, while the spec watcher watches what they publish | `pikopod up` · `incidents` |
| 5 | **Reproduce** — the failure becomes a runnable scenario in that same sandbox | `pikopod scenario reproduce` |
| 6 | **Fix and prove** | `pikopod fix` · `scenario run` |
| 7 | **Regress forever** | `pikopod replay --ci` |

**Stage 5 is the one nothing else does.** You cannot ask a provider's sandbox to
return that exact 503, with that body, at that point in your state machine.
pikopod can, because the same tool recorded it and owns the sandbox. Mocking
tools have a sandbox and no observer; monitoring tools have an observer and no
sandbox.

You do not have to adopt all of it. **Stage 0 is the usual on-ramp** — one line
in CI, nothing installed in your request path — and the next section is exactly
that.

## Does it work on a real API?

Every example below uses a fictional `examplepay`, so here is a real one you can
reproduce yourself. Stripe publishes its OpenAPI document under MIT:

```bash
curl -fsSL -o stripe.json \
  https://raw.githubusercontent.com/stripe/openapi/master/openapi/spec3.json
pikopod init                                  # scaffolds pikopod.yaml
pikopod import stripe --spec ./stripe.json
pikopod scenario list stripe
```

419 paths and 7.7 MB become 594 endpoints in about 0.13 seconds. Eight of the
eleven archetypes bind. Three do not, and this is the part worth reading:

```
✗ invalid_request   no operation matching {"crud":"CREATE","hasErrorResponseClass":"4XX"} for role 'op'
✗ duplicate_delivery  no webhookEvent matching {} for role 'emittedEvent'
✗ state_transition_sequence  no operation matching {"crud":"UPDATE","hasEnumField":true,...}
```

The first one is true and checkable: **Stripe declares no 4xx response on any of
its 297 POST operations** — errors go through `default`. So the archetype that
needs a declared 4xx cannot bind, and pikopod says which fact was missing
instead of guessing a test into existence. That is the doctrine working on
somebody else's real spec, and you can verify it yourself with `jq`.

This is not a claim that pikopod "supports Stripe", and it is not an endorsement
by them. It is one reproducible run against a public document, shown because
`examplepay` proves nothing.

## Start here: catch a breaking change in CI

The cheapest thing pikopod does needs no proxy, no account, and no setup. It
diffs two versions of a spec and fails your build on a breaking change:

```bash
pikopod spec-diff git:origin/main:openapi.yaml openapi.yaml --fail-on ERR
```

It reads straight from git with no checkout, and exits `0` clean / `1` breaking
/ `2` tool error. One line in a pipeline:

```yaml
- run: |
    VERSION=0.1.0   # pin it; CI should not float on latest
    curl -fsSL "https://github.com/Pikopod/pikopod/releases/download/v${VERSION}/pikopod_${VERSION}_linux_amd64.tar.gz" | tar xz
    ./pikopod spec-diff git:origin/main:openapi.yaml openapi.yaml --fail-on ERR
```

Severity is **derived by law**: one function maps the shape of a change — its
effect, its direction, and any guards — onto `ERR`/`WARN`/`INFO`. No check
carries a hand-assigned severity, so "breaking" means the same thing on every
endpoint and on every provider, and changing the rule changes every verdict at
once.

## Then: watch a real provider

```bash
pikopod init                                    # scaffold pikopod.yaml
pikopod import examplepay --spec <spec-url>
pikopod up                                      # sandbox :4600 · agent :4700
```

`--spec` takes OpenAPI 3.0/3.1, Swagger 2.0, a Postman collection, or a GraphQL
schema, from a URL or a local path. A documentation *page* also works: pikopod
looks for an embedded or linked spec first, then the platform's well-known
spec paths, and only falls back to extracting one with a model if you have
configured your own key. See
[docs/security.md](docs/security.md#what-leaves-the-machine) for exactly what
that sends.

Pass `--emit-spec <path>` to keep the spec the import used. An extracted one
is written with an `x-pikopod-origin: llm-extracted` marker and a list of any
indexed pages that did not fit the extraction budget; review it, bind its
webhook events, commit it, and import from the file from then on, with no
model in the loop. It stays DRAFT until you delete the marker, which says the
facts are now yours.

That gives you two things.

**A sandbox at `:4600/examplepay`** built from the provider's spec — point
staging at it. It is deterministic: same seed, same bytes, every run. And
unlike the provider's own sandbox, you can make it misbehave:

```bash
pikopod scenario list examplepay      # which failure stories your API supports
pikopod scenario run examplepay declines timeouts partial_failure
pikopod chaos examplepay --kind error --status 503 --method POST --path /v1/charges
```

**An observing agent at `:4700/examplepay`** — point traffic at it. Start with
staging: drift needs a baseline, and letting it warm up somewhere low-stakes
means the first thing production sees is a tool that has already been quiet for
two days. Incidents fire either way, from the first request.

It forwards everything untouched and watches two different things:

- **Incidents** — the upstream answered 5xx, throttled you with a 429, or could
  not be reached at all. These are facts about one request, so they need no
  baseline and fire from the very first one.
- **Drift** — the shape of a successful response changed. This needs a baseline,
  so it stays quiet for the first 50 samples / 48 hours on purpose: a reference
  built from five responses hasn't seen your optional fields yet.

```bash
pikopod incidents                     # what has failed, newest first
pikopod incidents --only incidents --since 24h --format json
```

### It also crosses the two sides

A spec-diff tool reads what the provider *published*. Monitoring sees your
traffic but throws response bodies away. pikopod holds both, so it can say
things neither can:

> The spec removed this endpoint — and you're still sending it 120 requests a
> day.

Traffic evidence raises the severity of a declared change; a declared change
downgrades an observed one to "documented, not silent." Holding both sides is
*why* a reproduction is possible at all — it is the architecture, and stage 5 is
what the architecture is for.

Here is the smallest version of that. One letter changes, and your
`if status == "success"` quietly stops matching:

```
[ERR] pikopod drift — new value on GET /transaction/tx_{id} (examplepay)
status: value "succeeded" not in known set [success]
fingerprint fp_385153d1776c · first seen 2026-09-11T00:08:54Z · 3 occurrence(s)
replay it: pikopod scenario from-drift fp_385153d1776c
```

Nobody's changelog mentioned it. The fingerprint on that line is the handle you
feed back in.

## It cannot slow your traffic down

The sandbox is not in your request path at all — it is a local binary you point
staging at, so there is nothing to be unsafe about. The **observing agent** is a
reverse proxy in production, so it gets the scrutiny:

- **It serves first and observes afterwards.** Observation is asynchronous and
  bounded; every capture stage is panic-isolated and counted. If pikopod breaks
  internally, your traffic still flows.
- **It never retries.** An automatic retry in front of a payments API is a
  double-charge window.
- **It redacts before the disk, not after.** Credentials become placeholders,
  identifiers become format-preserving tokens, and unclassifiable *strings* are
  dropped. Numbers are kept unless their key names them as sensitive — a
  deliberate trade, spelled out in
  [docs/security.md](docs/security.md#redaction-happens-before-the-disk-not-after).
  Check it yourself with `pikopod inspect`.
- **It initiates no network traffic of its own.** Slack, your forge, and your
  LLM provider — all with your credentials, all because you configured them.

**Measured, not asserted.** `BenchmarkProxyServeObserverWedged` jams observation
completely — nothing drains the capture channel — and serves the same workload:

```
BenchmarkProxyServe                 82,041 ns/op
BenchmarkProxyServeObserverWedged   81,868 ns/op
```

Under 1% apart, run to run. That is the fail-open guarantee as a number rather
than a promise. (Both figures are a full loopback round trip on one machine, so
read the *difference*, not the absolute.)

**If the process dies**, traffic stops — pikopod is in the path, so run it as
you would any sidecar: a supervisor, and `/healthz` as your liveness probe. The
sandbox and the agent are separate listeners and fail independently; the sandbox
going down does not touch production traffic. Do not point two instances at one
`data_dir` — baselines are single-writer.

See [docs/security.md](docs/security.md) for how redaction works and how to
verify it.

## Stage 5: reproduce it locally

```bash
pikopod incidents                              # find the fingerprint
pikopod scenario reproduce fp_14835fa32dfb     # a failure becomes a runnable scenario
pikopod scenario run examplepay incident-14835fa32dfb
```

`reproduce` arms the same failure in your sandbox and replays the recorded
request at it, so your retry logic fails on your laptop instead of in
production. The generated pack is an ordinary scenario — commit it, and it
guards that path forever.

For a **shape change** rather than a failure, `from-drift` pins the old contract
instead:

```bash
pikopod scenario from-drift fp_385153d1776c   # the change becomes a test
pikopod replay --ci                           # gate builds on recorded traffic
pikopod fix fp_385153d1776c --check "go build ./..." --pr
```

`pikopod fix` scans your repository for affected code and opens a PR with a
patch, reverted in full if your check fails. **It is off unless you configure
your own model key**, and it sends excerpts of the matched source to that
provider — so it is opt-in twice over, and the least mature thing here. Read
[its limitations](docs/config-reference.md#fix) first.

> Reproduced requests are rebuilt from **redacted** recordings: identifiers are
> format-preserving tokens and anything the sanitizer could not classify was
> dropped before it reached disk. Every generated pack says so. For a 5xx or a
> timeout that changes nothing — the fault is armed on method and path. For a
> 4xx that your own payload caused, the body matters, so check the pack against
> what your code actually sends.

## Use it from a coding agent

`pikopod mcp` serves the same checks over the Model Context Protocol, so an
agent that just wrote or patched an integration can verify it against what the
provider actually sends before opening a pull request:

```json
{ "mcpServers": { "pikopod": { "command": "pikopod", "args": ["mcp"] } } }
```

Every answer carries a verdict, and `UNVERIFIABLE` is never dressed up as
`CLEAN`: a gate with no recordings, an upstream still warming up, or a check
whose evidence was redacted says so, with the reason. See
[docs/config-reference.md](docs/config-reference.md#mcp) for the tool list.

## Documentation

**Using pikopod**

- [Overview](docs/OVERVIEW.md) — how the pieces fit, and what pikopod refuses to do
- [Configuration reference](docs/config-reference.md) — every key, every default
- [Exit codes](docs/exit-codes.md) — they're API; script against them
- [Security notes](docs/security.md) — redaction, the salt, listener safety
- [Scenario packs](scenarios/README.md) — archetypes, bindings, and the pack format

**Contributing**

- [Contributing](CONTRIBUTING.md) — how to propose a change
- [Development](DEVELOPMENT.md) — build, test, project layout, release
- [Security policy](SECURITY.md) — reporting a vulnerability
- [Code of conduct](CODE_OF_CONDUCT.md)

## License

Apache-2.0. See [LICENSE](LICENSE).
