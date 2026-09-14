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
| **Needs** | A spec. Works on day one. | Nothing for an incident — a failed request is a fact about itself. 48 hours of traffic before drift, which is a claim about what is *normal*. | It to have already broken. |

The sandbox is where you rehearse. The drift agent is the smoke detector.
Replay is the evidence.

These are not three tools but one loop: rehearse in the sandbox, ship, observe,
and when something fails, **reproduce it back in the sandbox** to fix it and
keep it fixed.

## The parts

### Import

`pikopod import` turns a provider's OpenAPI 3.x, Swagger 2.0, Postman
collection, or GraphQL schema — or a documentation page that embeds one — into
a normalized internal representation.

Every field carries **provenance**: stated explicitly in the source, derived
deterministically from it, inferred heuristically, or extracted by a model. The
levels never merge, because inferred behavior must never be presented as
documented behavior. The sandbox refuses to enforce a guess as though it were
a contract.

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
accumulates field statistics until the warmup gates clear (50 samples across 48
hours by default), then **freezes** a reference. Later traffic is compared
against the frozen copy, never against a slowly moving average — which is what
stops a gradual change from quietly becoming the new normal.

Eight structural finding kinds: a field added or removed, a type changed, a new
enum value, nullability, an exact status code, a status class, a restructured
error body.

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

This is the thing no one else can do, and the reason the sandbox and the
observer are one tool rather than two. You cannot ask a provider's sandbox to
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
against your pinned import. Severity is derived by law from the shape of the
change rather than hand-assigned.

Then the two signals **cross**. Traffic evidence raises the severity of a
declared change; a declared change downgrades an observed one to *documented,
not silent*.

This is the part no one else can do, because everyone else has one side. A
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
rarely provable from the bytes. An alert here means the *structure* changed —
something you can act on and verify. Adding "the p99 moved" would make every
other alert less trustworthy.

**It never enforces a guess.** Heuristically inferred fields are not validated
as though they were documented, and checks whose evidence the sanitizer removed
are counted as unverifiable rather than scored either way.

**It never advances your pin on its own.** A provider publishing a new spec
generates findings; accepting them is an explicit `import --update`.

**It never lets internal failure touch your traffic.** Observation is bounded
and panic-isolated, and the proxy does not retry.

**It never reports clean when it cannot tell.** `UNVERIFIABLE` is a real
outcome with its own exit code.

## Where the data lives

Everything is under `data_dir`: recordings, baselines, alert state, imported
contracts, scenario packs, the event log. Nothing is uploaded. There is no
account and no telemetry.

Recordings are redacted before they are written, and `pikopod inspect` shows you
exactly what was kept. See [security notes](security.md).
