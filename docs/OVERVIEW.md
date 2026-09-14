# Overview

How pikopod fits together, and why it is shaped this way.

## The problem

You built against someone else's API. Two things go wrong, and neither is your
fault.

**You cannot rehearse failure.** The provider's sandbox is happy-path only. You
cannot make a charge decline in an unusual way, or time out halfway through, so
you discover how your code handles it in production with real money.

**They change things without telling you.** A field is renamed. A status value
that was always `success` starts arriving as `succeeded`. The changelog is
silent, or nobody read it.

## Three tenses of one question

Everything pikopod does answers *"what is this API about to do to me?"* — asked
about three different times. Each part covers what the others structurally
cannot.

| | Sandbox | Drift agent | Replay |
|---|---|---|---|
| **Tense** | Future / conditional | Present | Past |
| **Answers** | What happens when they ship the new spec? When they decline? When they time out? | Did it actually happen, and is it on my wire now? | What exactly happened — prove the fix against it. |
| **Needs** | A spec. Works on day one. | Nothing for an incident — a failed request is a fact about itself. 48 hours of traffic before drift, which is a claim about what is *normal*. | Recordings, plus a frozen baseline — it gates on what traffic already taught it, so it inherits the 48-hour warmup. |

The sandbox is where you rehearse. The drift agent is the smoke detector.
Replay is the evidence.

These are not three tools but one loop: rehearse in the sandbox, ship, observe,
and when something fails, **reproduce it back in the sandbox** to fix it and
keep it fixed.

## How the parts fit

One config file, one `data_dir`, one command. `pikopod up` starts **two
listeners in one process**: the sandbox on `:4600` and the drift agent on
`:4700`. They share the data directory and fail independently: if the sandbox
listener dies, the message is loud and the drift agent keeps serving. Your
production traffic does not depend on the sandbox being up.

`up` always starts both listeners — there is no flag to run the agent alone —
so the sandbox is present whether or not you registered one. The spec watcher is
also part of `up`, not a separate command, and arms per-upstream whenever
`spec_source` is set. Recordings made by the agent feed the sandbox's replay
tier only when you ask for it with `--recordings-fallback` on `import` or
`sandbox add`, never silently.

**If the process dies, traffic stops.** That is the direction that matters: one
process is in your request path, so a crash, an OOM, or a bad deploy takes the
proxy with it. Run it as you would any sidecar — a supervisor that restarts it,
and `/healthz` as the liveness probe. `/healthz` is public for liveness and
needs the token for the full payload, so a load balancer can poll it without
being handed your upstream inventory. Baselines are single-writer: do not point
two instances at one `data_dir`.

The parts below are a **cycle**, not a menu. Import feeds the sandbox; the
sandbox is what a reproduction replays into; the agent's recordings are what a
reproduction is built from; replay is what keeps the fix. You can enter anywhere,
and **import and the sandbox need no proxy at all** — that is where most people
start.

## The parts

### Import

`pikopod import` turns a provider's OpenAPI 3.x, Swagger 2.0, Postman
collection, or GraphQL schema into a normalized internal representation.

A documentation **page** also works, and it is worth knowing what that does. It
climbs three deterministic rungs first: links out of the page to anything
machine-readable, then the page's own embedded document, then one hop into the
site's reference index. Only if all three miss does it reach the fourth rung,
where a model writes a spec from the site's prose — and that rung requires
**your own key**. With no key configured it stops and tells you so rather than
degrading quietly. The fetching is broader than one page; see
[security notes](security.md#importing-from-a-documentation-url-fetches-more-than-one-page).

Every field carries **provenance**: `EXPLICIT` (stated in the source), `DERIVED`
(a deterministic transformation of it), `INFERRED` (a heuristic, with a stated
rule), or `LLM_EXTRACTED` (model output). The levels never merge, because
inferred behavior must not be presented as documented behavior.

**A model-written contract is simulated, not refused** — and that distinction is
deliberate rather than an oversight. The sandbox declines to *enforce* only
`INFERRED` fields, because a heuristic guess about auth or validation produces a
test that fails for reasons unrelated to your code. Model-extracted fields are
simulated so the sandbox is useful at all for a provider that ships no spec, but
they are marked `DRAFT` in `pikopod sandbox list`, capped at
`POTENTIALLY_BREAKING` in any spec diff, and lose to observed traffic whenever
the two disagree. The guess is carried, labelled, and outranked — not trusted.

### Sandbox

The imported contract becomes a stateful simulator: route matching, declared
auth enforcement, request validation, a seeded synthesiser, a virtual clock,
signed webhooks, and a resource store. Same seed, same bytes, every run.

`pikopod chaos` arms failures the real provider will not perform on request —
errors, latency, hangs, slow bodies, rate limits.

**It is honest about its limits.** Action endpoints (`POST /charges/{id}/capture`)
return empty bodies; rate limiting is injectable but not modelled; pagination
understands `limit`/`cursor` only. It is built to rehearse shape changes, not to
be a faithful clone.

### Scenarios

Eleven provider-agnostic failure stories — declines, timeouts, duplicate
delivery, partial failure, downtime recovery, and others — that **bind
themselves to your API** from its specification.

`pikopod scenario list <sandbox>` answers *"which failure modes does my
integration actually have?"* with no authoring at all. Binding uses explicit and
confirmed facts only, and refuses to bind on a guess. Zero candidates is a
first-class answer with a reason attached. See [scenarios/](../scenarios/README.md).

Plain-English authoring is available with your own model key. The model never
writes steps — it emits a constrained intent that is validated against
operations which actually exist in your imported API, then expanded
deterministically. It cannot invent an endpoint, and it must declare what your
request asked for that it failed to capture.

### Drift agent

A fail-open reverse proxy. It forwards your traffic untouched and observes
asynchronously.

Learning runs **LEARN → FREEZE → DIFF**. For each endpoint and status class it
accumulates field statistics until **both** warmup gates clear (50 samples AND 48
hours by default), then **freezes** a reference. Later traffic is compared
against the frozen copy, never against a slowly moving average — which is what
stops a gradual change from quietly becoming the new normal.

Eight structural finding kinds, exactly:

- `field_added` — a field appeared
- `field_removed` — a field that was always present is gone
- `type_changed` — a field's type changed
- `enum_value_new` — a value outside the known set
- `field_nullable` — a never-null field arrived null
- `status_code_changed` — the exact code moved within a known class
- `status_new` — a status class this endpoint never returned
- `error_shape_changed` — a 4xx/5xx body was restructured

### Incidents

Drift is about a **successful** response whose shape changed, and it needs a
baseline to be meaningful. A request that simply **failed** needs no baseline at
all, so incidents fire from the very first request and are never subject to the
warmup window.

Four incident kinds: the upstream answered 5xx, it throttled you with a 429, it
could not be reached at all, or it rejected your requests with 4xx above a
configured rate. The first three are always on. The 4xx case is opt-in, because
a 4xx is usually your own bug — which is exactly why it is worth catching, and
also why an endpoint that answers 401 all day must not page you.

An incident forces its recording to disk regardless of the sampling rate. That
is deliberate: the recording **is** the reproduction, and sampling it away would
leave you with an alert pointing at something you cannot run.

### Reproduction

`pikopod scenario reproduce <fingerprint>` turns an incident into a runnable
scenario: it arms the same failure in your sandbox and replays the recorded
request at it. The break happens on your laptop instead of in production, and
the generated pack is an ordinary scenario you can commit.

This is the payoff, and the reason the sandbox and the observer are one tool
rather than two. You cannot ask a provider's sandbox to
return that exact 503, with that body, at that point in your state machine.

The honest limit: requests are rebuilt from **redacted** recordings. Identifiers
are format-preserving tokens and anything the sanitizer could not classify was
dropped before it reached disk, so the body is not byte-identical. Every
generated pack says so. For a 5xx or a timeout it changes nothing, because the
fault is armed on method and path. For a 4xx your own payload caused, it matters.

For a shape change rather than a failure, `scenario from-drift` pins the old
contract instead.

### Spec watcher and the join

Separately, pikopod re-fetches the provider's published spec and diffs it
against your pinned import. Severity is **derived by law** — one function maps a
change's shape onto ERR/WARN/INFO, so no check carries a hand-assigned severity
and changing the rule changes every verdict at once.

Then the two signals **cross**. Traffic evidence raises the severity of a
declared change; a declared change downgrades an observed one to *documented,
not silent*.

Holding both sides is *why* one tool can do the reproduction above — everyone
else has one side. A
spec-diff tool knows what was published. Monitoring knows what arrived. Only
something holding both can say *"the spec removed this endpoint, and you are
still sending it 120 requests a day."*

### Replay and the CI gate

Recorded traffic becomes fixtures, matched hierarchically — exact, then shape,
then sequence — with every served response naming the tier it came from.
`pikopod replay --ci` gates builds offline.

### Fix

A drift event becomes a code change: a deterministic impact scan, a bounded
patch from your own model confined to that impact set, verified by a check
command and reverted in full on failure, optionally opened as a PR.

This is the least mature part of pikopod. Read
[its limitations](config-reference.md#fix) before relying on it.

## What pikopod refuses to do

The refusals are the design, not gaps in it.

**It never alerts on latency.** Latency is noisy, environment-dependent, and
rarely provable from the bytes. Adding "the p99 moved" would make every other
alert less trustworthy.

The boundary, since incidents include "could not be reached": **a request that
failed is an incident; a request that was merely slow is nothing.** pikopod sets
no upstream timeout of its own — deliberately, so a legitimate slow or streaming
response is never cut short — so an incident fires when *your caller* gives up
and the connection dies, not when a stopwatch says so. Duration is recorded on
every exchange and available to `pikopod export`; it is never a finding.

**It never enforces a guess.** Heuristically inferred fields are not validated
as though they were documented, and checks whose evidence the sanitizer removed
are counted as unverifiable rather than scored either way.

**It never advances your pin on its own.** A provider publishing a new spec
generates findings; accepting them is an explicit `import --update`.

**It never lets internal failure touch your traffic.** Observation is bounded
and panic-isolated, and the proxy does not retry.

**It never reports clean when it cannot tell.** `UNVERIFIABLE` is a real
outcome, distinct from a pass. It is not a fourth exit code — the contract is
`0`/`1`/`2` and stays that way — it maps onto the existing ones by what it
means. `conformance` counts checks whose evidence the sanitizer removed and
reports them as neither pass nor violation, leaving the exit code alone;
`fix` and `scenario reproduce` exit `2`, because a result pikopod cannot stand
behind is a tool outcome, never a finding. See
[exit codes](exit-codes.md).

## Where the data lives

Everything is under `data_dir`: recordings, baselines, alert state, imported
contracts, scenario packs, the event log. Nothing is uploaded. There is no
account and no telemetry.

Recordings are redacted before they are written, and `pikopod inspect` shows you
exactly what was kept. See [security notes](security.md).
