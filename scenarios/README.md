# Scenario packs

This directory holds scenario packs you keep in version control. `pikopod
scenario run` looks for packs in **`./scenarios`** (relative to where you run
it) and in **`<data_dir>/scenarios`**. Generated packs are written to
`<data_dir>/scenarios` — copy one here to check it in.

A scenario is a failure story run against a sandbox: send these requests, arm
these faults, assert these things about the responses, and tell me whether my
integration survives.

## Where packs come from

```bash
pikopod scenario list <sandbox>                    # which archetypes bind to YOUR api
pikopod scenario run <sandbox> declines timeouts   # run them
pikopod scenario create <sandbox> "timeout after the charge succeeds"
pikopod scenario from-drift fp_385153d1776c        # a real drift becomes a test
pikopod scenario from-recordings <upstream>        # recorded traffic becomes a pack
```

`scenario run` takes flags: `--persist` (run against the sandbox's real store
instead of an ephemeral copy), `--seed`, `--input name=value`, `--bind
role=operationId`, and `--target` / `--target-header` for a real endpoint.
`scenario create` takes `--yes` and `--model`; `from-drift` takes `--sandbox`;
`from-recordings` takes `--last` and `--name`. All are repeatable where the
value is a list.

## Archetypes: start here

You usually do not write a pack by hand. pikopod ships **eleven**
provider-agnostic failure archetypes that **bind themselves to your API** from
its imported specification:

`happy_path` · `unauthorized` · `invalid_request` · `duplicate_delivery` ·
`rate_limit_backoff` · `state_transition_sequence` · `retry_storm` ·
`declines` · `timeouts` · `partial_failure` · `downtime_recovery`

An archetype declares *roles* — `op`, `createOp`, `updateOp`, `emittedEvent` —
and binding finds operations in your API that can fill them.

```bash
pikopod scenario list examplepay
```

**Binding uses explicit and confirmed facts only.** A candidate resting solely
on inferred spec facts is rejected, because a test resting on a guess fails for
reasons that have nothing to do with your code.

### "archetype does not apply" / "no candidate binding grounds"

This is a real answer, not a failure. `archetype does not apply` means your API
has no operations that can fill the archetype's roles — a read-only API
genuinely has no `duplicate_delivery` story. `no candidate binding grounds`
means roles *could* be filled, but no candidate expanded into a definition that
validates against the pinned API.

When you disagree, you have two options:

```bash
pikopod scenario list examplepay                  # see what bound, and why the rest did not
pikopod scenario run examplepay declines --bind op=createCharge
```

`--bind role=operationId` overrides one role. The role names are the ones the
archetype declares (above); the value is an **operation ID from the imported
spec** (`operationId` in your OpenAPI document). If an override names something
the API does not have, the expansion fails to ground rather than running a test
that cannot mean anything.

## Writing a pack by hand

A pack is YAML: `name`, `provider`, optional `description`, and a `definition`
block. The full grammar is
[`schema/scenario-pack.schema.json`](../schema/scenario-pack.schema.json) — the
validator is strict on purpose (unknown keys are rejected), because a pack that
references a missing operation, an unresolvable variable, or an impossible
assertion is a broken test, and a broken test is worse than no test.

```yaml
name: charge-declines-cleanly
provider: examplepay
description: A charge is created, read back, then declines while the fault is armed.
definition:
  inputs:
    - name: amount
      type: number
      required: false
      default: 1250
  steps:
    - key: seed
      type: SEED_STATE
      config:
        resources:
          - type: /charges
            resourceKey: chg_probe
            attributes: { status: pending }
    - key: create
      type: REQUEST
      description: create a charge
      config:
        method: POST
        path: /charges
        body: { amount: "{{amount}}", currency: NGN }
      capture:
        chargeId: response.body$.id
      assertions:
        - target: response.status
          op: equals
          expected: 201
    - key: read-back
      type: REQUEST
      config:
        method: GET
        path: /charges/{{chargeId}}
      assertions:
        - target: response.status
          op: equals
          expected: 200
        - target: response.body
          path: $.status
          op: isOneOf
          expected: [pending, succeeded]
    - key: arm-decline
      type: INJECT_FAULT
      config: { method: POST, path: /charges, kind: error, status: 400, times: 1 }
    - key: declined
      type: REQUEST
      config:
        method: POST
        path: /charges
        body: { amount: "{{amount}}", currency: NGN }
      assertions:
        - target: response.status
          op: equals
          expected: 400
        - target: execution.faultApplied
          op: equals
          expected: true
```

Top level: `name` (required), `provider` (required — metadata; `scenario run`
targets whatever sandbox you name on the command line), `description`,
`contractVersion` (set by `from-drift`), `definition` (required).

`definition` holds `steps` (required, at least one) and optionally `inputs`,
`defaults`, and `requiresFidelity` (`L0`–`L3`).

### Steps

Every step is `{key, type, config}` plus optional `description`, `assertions`,
`capture`, `continueOnFailure`, `timeoutMs`. `key` must be unique and match
`[A-Za-z0-9_.-]+`. The per-step options live at the top of the step; everything
type-specific lives under `config`:

| `type` | `config` keys |
| --- | --- |
| `REQUEST` | `method`, `path` (sandbox-relative), `headers`, `query`, `body` |
| `WAIT` | `durationMs` (virtual time) |
| `EXPECT_WEBHOOK` | `match`, `timeoutMs` |
| `INJECT_FAULT` | `kind`, `method`, `path`, `status`, `delayMs`, `probability`, `target`, `wallclock`, `times`, `per`, `delayDistribution` |
| `CLEAR_FAULT` | `method`, `path` |
| `VERIFY_REQUESTS` | `path`, `method` |
| `VERIFY_SEQUENCE` | `requests` (ordered matchers) |
| `EMIT_WEBHOOK` | `event`, `data` |
| `ASSERT_STATE` | `resourceType`, `resourceId` |
| `SEED_STATE` | `resources` |
| `SNAPSHOT` | `label` |
| `NOTE` | `text` |

Fault `kind` is one of `error`, `latency`, `hang`, `slow_body`,
`connection_reset`, `malformed_response`, `wrong_content_length`, `rate_limit`,
`duplicate_webhook`, `drop_webhook`, `reorder_webhook`, `delay_webhook`. Every
one of them is also armable on a running sandbox with `pikopod chaos`; the
webhook kinds match on the event (`--event`, default any) rather than on a
method and path. `rate_limit` is sugar: the engine arms it as a 429 `error`.
`times: N` fires the fault for the first N matching requests and then recovers
deterministically; `per` scopes that window to `global`, `idempotency-key`, or
`resource`.

### VERIFY_SEQUENCE

`VERIFY_REQUESTS` answers "how many, and what was the last one". `VERIFY_SEQUENCE`
answers "in what order, and how far apart" — the retry-storm and double-charge
questions.

```yaml
- key: retried-with-one-key
  type: VERIFY_SEQUENCE
  config:
    requests:
      - { method: POST, path: /charges, headers: { idempotency-key: "<tokenized>" } }
      - { method: POST, path: /charges, minGapMs: 1000 }
```

It is an ordered **subsequence**: unrelated requests between matches are fine,
order is not. `minGapMs`/`maxGapMs` measure VIRTUAL time since the previous
match, so a backoff claim is reproducible. The matchers are the assertion, so
the step needs none of its own.

It fails closed. An evicted journal cannot prove an ordered claim, and a matcher
touching headers or query the journal had to truncate is reported as unprovable
rather than quietly not matching.

Journaled headers and query values are redacted before storage, so an
identifier arrives as a deterministic, collision-distinct token. That is what
makes "both retries used the SAME key" provable without the key being readable.

### EMIT_WEBHOOK

Some events follow no API call: money landing in a collection account, a
chargeback, a KYC decision. Declare them in the spec with
`x-pikopod-emit-only: true` and fire them on demand, from a scenario, from the
control plane (`POST /_pikopod/sandboxes/<name>/webhooks/emit`), or by hand:

```bash
pikopod webhook emit examplepay transaction.created --data @transaction.json
```

Only **declared** events can be emitted, and the sandbox never emits an event
the spec does not declare. An invented event is indistinguishable at your
handler from a real delivery, which makes it worse than silence. `data` is
overlaid onto the documented payload shape, so the values you pass reach a
nested payload rather than being replaced by synthesis.

### Webhook envelope

A delivery is only useful if your handler accepts it, and handlers verify the
provider's signature over the provider's wire shape. Declare that shape once
and every delivery to `--webhook-url` is wrapped and signed the way the
provider documents, so the same handler serves pikopod and production.

Put it in the spec as a top-level `x-pikopod-webhook-envelope`, or, for a spec
you do not control, in a sidecar file passed as `--webhooks`:

```yaml
# examplepay-webhooks.yaml
wrap:
  timestamp: "{{now_rfc3339}}"
  payload: "{{json_string body}}"
signature:
  algorithm: hmac-sha256        # or hmac-sha512
  content: "{{timestamp}}{{payload}}"
  keyEnv: EXAMPLEPAY_WEBHOOK_KEY
  keyEncoding: base64           # raw (default), base64 or hex
  output: base64                # base64 (default) or hex
  in: body                      # body or header
  name: signature
```

```bash
export EXAMPLEPAY_WEBHOOK_KEY=<the key the provider issued>
pikopod import examplepay --spec examplepay.yaml --webhooks examplepay-webhooks.yaml --webhook-url http://localhost:3000/hooks/examplepay
```

`wrap` builds the body: each field is a template, and a field that is exactly
`{{body}}` embeds the documented payload as JSON. Without `wrap` the body is
the payload itself. `headers` adds request headers the same way. `signature`
signs the rendered `content` with the key in `$keyEnv` and places the result in
a body field or a header; `format` (for example `v1={{signature}}`) shapes the
value.

Templates are a closed set: `{{body}}`, `{{json_string body}}`, `{{event}}`,
`{{id}}`, `{{timestamp}}` (unix seconds), `{{timestamp_ms}}`, `{{now_rfc3339}}`,
`{{uuid}}`, plus the `wrap` field names inside `headers` and `content`. Time is
the sandbox's virtual clock and `{{uuid}}` derives from the seed, so a run
replays byte for byte. Anything else is refused at import, naming the field.

The key is never stored: `pikopod up` reads it from the named variable and
refuses to start without it when a sink is configured. The signing secret
printed at import and the `x-pikopod-webhook-*` headers belong to the default
format and are not sent once an envelope is declared. The outbox, `EXPECT_WEBHOOK`
and `webhook.delivery` assertions keep seeing the documented payload.

### Assertions

An assertion is `{target, op}` plus optional `subject` (`SANDBOX` — the default
— `CLIENT`, or `PRODUCTION`), `path` (JSONPath into the target, starting `$.`),
`key` (for `response.headers`), `resourceType`, `resourceId`, `match`,
`expected`, `schemaRef`, and `soft`.

- Targets: `response.status`, `response.headers`, `response.body`,
  `response.latencyMs`, `state.resource`, `state.resourceCount`,
  `webhook.delivery`, `webhook.count`, `execution.faultApplied`,
  `sandbox.requestCount`, `sandbox.request`.
- Ops: `equals`, `notEquals`, `contains`, `notContains`, `in`, `matches`,
  `matchesSchema`, `exists`, `absent`, `gt`, `gte`, `lt`, `lte`, `countEquals`,
  `lengthEquals`, `isOneOf`.

`in` and `isOneOf` are the same check; so are `countEquals` and `lengthEquals`.
`matches` rejects an uncompilable or catastrophic regex at validation time, and
`matchesSchema` requires `schemaRef`.

### Captures and templates

`capture` maps a variable name to `source$jsonpath`, where source is one of
`response.body`, `response.headers`, `webhook.delivery`, `state.resource` —
for example `chargeId: response.body$.id`. A capture whose path does not
resolve fails the step rather than binding silently.

Reference variables as `{{name}}`; precedence is inputs, then captures, then
`defaults`. A variable must be declared, captured by an **earlier** step, or a
generator — `{{seq()}}`, `{{randomString(n)}}`, `{{now()}}` (the virtual clock,
never the wall clock). `{{any:string|number|boolean|iso8601|uuid}}` stays
verbatim in requests and is interpreted as a matcher in assertions.

### Seed data

`SEED_STATE` seeds resources into the sandbox before the traffic that reads
them:

```yaml
- key: seed
  type: SEED_STATE
  config:
    resources:
      - type: /charges
        resourceKey: probe_1
        attributes: { status: pending }
```

`resourceKey` must not be empty — **omit `resourceKey` to autogenerate one**.
An empty key is unreachable through list endpoints (keyset pagination treats it
as the first-page sentinel) yet still counts against quotas, so pikopod rejects
it rather than creating a resource you can never fetch. `attributes` is
required.

### Limits

Packs are bounded: **1 MiB per file**, 200 steps, 50 assertions per step, 1000
assertions total, 1000 seeded resources, 50 inputs, and 90 virtual days of
accumulated `WAIT`/`EXPECT_WEBHOOK` time. If you hit `scenario pack is too
large`, **split the scenario, or trim seed data** — a pack with thousands of
seeded resources is usually a load test wearing a scenario's clothes, and the
sandbox has quotas for that reason.

## "pack does not ground against this sandbox's API"

The pack references operations that the sandbox you pointed it at does not
have. This usually means one of:

- The pack was written for a different provider.
- The sandbox was re-imported from a spec that dropped the operation.
- The operation ID changed upstream — which is itself a drift worth looking at.

Run `pikopod scenario list <sandbox>` to see what the sandbox actually exposes,
and check the `method` + `path` of the step named in the error: a `REQUEST`
path must match an endpoint in the pinned API version, and it must be
sandbox-relative, never an absolute URL.

`unknown scenario` is the neighbouring error: the name you passed is neither an
archetype ID nor a pack in either scenarios directory. Packs resolve by their
`name` field, or by a path ending in `.yaml` / `.yml`.

## Running against a real endpoint

`pikopod scenario run <sandbox> <pack> --target https://api.example.com` drives
the pack at a real base URL instead of the sandbox. Only `REQUEST`, `NOTE`, and
`SNAPSHOT` steps are allowed there: a real endpoint has no fault arming, seeded
state, virtual clock, webhook outbox, or request journal to consult, so any
other step type is refused before anything runs. Trim the pack to those steps
for the remote check, or run it against the local sandbox.

## Packs generated from drift

`pikopod scenario from-drift <fingerprint>` turns a detected change into a
runnable pack that **pins the contract you integrated against** via
`contractVersion`. It passes while your sandbox still honours that baseline,
and fails the moment you re-import a spec that adopts the provider's change —
so the break happens here rather than in production.

Some drift kinds cannot be pinned deterministically (a field inside an array, a
multi-code status baseline, nullability and error-shape changes, and endpoints
with non-trailing or multiple path parameters). For those, write the scenario
by hand from the pack grammar above, using the response your integration
actually depends on.

These packs are ordinary YAML. Keep them in version control; they are the
regression suite for every provider change you have survived.
