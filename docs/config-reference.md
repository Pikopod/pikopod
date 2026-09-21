# Configuration reference

pikopod reads `pikopod.yaml` from the working directory unless `--config` says
otherwise. `--config` is accepted by every command. Run `pikopod init` to
scaffold one.

**Unknown keys are startup errors, not silent no-ops.** A typo'd or removed
knob must never let you believe something is configured. If pikopod starts,
every key you wrote is a key it understands.

Environment variables override the file. The full set:

| Variable | Overrides |
|---|---|
| `PIKOPOD_LISTEN` | `listen` |
| `PIKOPOD_DATA_DIR` | `data_dir` |
| `PIKOPOD_TOKEN` | the listener token (never passed as an argument — argv is visible in `ps`) |
| `PIKOPOD_LLM_KEY` | `llm.api_key` (provider-neutral) |
| `OPENROUTER_API_KEY` | OpenRouter's provider-native key environment variable |
| `OPENAI_API_KEY` | OpenAI's provider-native key environment variable |
| `PIKOPOD_OPENROUTER_KEY` | deprecated OpenRouter-only key alias |
| `PIKOPOD_OPENROUTER_MODEL` / `OPENROUTER_MODEL` | `llm.model` for OpenRouter |
| `PIKOPOD_LLM_BASE` | configured LLM provider base URL override (testing/staging) |
| `PIKOPOD_OPENROUTER_BASE` | deprecated OpenRouter-only base URL override |
| `PIKOPOD_DEBUG` | verbose diagnostics |

A minimal working file:

```yaml
data_dir: ./pikopod-data
upstreams:
  examplepay:
    listen: /examplepay
    target: https://api.examplepay.com
    spec_source: https://api.examplepay.com/openapi.json
slack:
  webhook_url: https://hooks.slack.com/services/...
```

---

## upstreams

The one concept the whole tool turns on. Each named upstream has a route on the
agent and a target it forwards to, and **the target alone decides posture** —
a real provider means watch mode, a vendor sandbox means staging mode, the
internal sandbox means scenario mode.

```yaml
upstreams:
  examplepay:
    listen: /examplepay
    target: https://api.examplepay.com
    spec_source: https://api.examplepay.com/openapi.json
    volatile_fields: [request_id, timestamp]
    mute: ["/health"]
    incidents:
      client_errors: false
      client_error_rate: 0.05
```

| Key | Meaning |
|---|---|
| `listen` | Route prefix on the agent, e.g. `/examplepay`. |
| `target` | Absolute base URL this upstream forwards to. Must include scheme and host. |
| `volatile_fields` | Fields whose **values** are allowed to churn. Suppression is value-only: the field is still learned, and its absence, a type change or a null still alert. A bare name (`status`) matches the leaf segment at any depth; a `parent/child` suffix (`meta/status`, `items[]/status`) scopes it. Case-insensitive. Applies to baseline learning, drift diffing and the replay CI gate; replay tier-1 hashing additionally strips a curated request-field list (idempotency keys, trace ids, signatures, timestamps). Anything else is a configuration error (exit 2). |
| `mute` | Endpoint templates whose alerts are suppressed. |
| `spec_source` | Arms the declared-drift watcher. See [spec_watch](#spec_watch). |
| `incidents` | Tunes failed-exchange capture. See [incidents](#incidents). |

Use `pikopod volatile suggest <upstream>` to find noisy fields rather than
guessing. It also reports configured entries that are **dead** (match nothing
learned), **stable** (the field never changed value, so nothing is silenced) or
**over-broad** (a bare name also covers a field that never churned); `pikopod
up` prints the same at startup.

## ref-policy

Which `$ref`s a spec may carry, wherever pikopod reads one (`import`,
`spec-diff`, `spec_watch`):

| `$ref` | Resolved? | Why |
|---|---|---|
| `#/components/…` (local) | always | The document is already in hand. |
| `./schemas/x.yaml#/X`, `../common.yaml` (relative, same repository) | when the source is a local file or `git:<ref>:<path>` | Read from the same directory tree, or from git object storage at the **same ref**, never the working tree. Bounded: at most 64 files and 32 MiB across them; cycles across files are refused. |
| `/etc/…`, `../../…` escaping the root | never | The reference must stay inside the document's own tree. |
| `https://…`, `//host/…`, `file://…` | never | pikopod runs in CI on pull requests from forks; a fetched `$ref` is a server-side request forgery waiting to happen. Vendor the document instead. |
| any relative `$ref` when the source is a URL | never | There is no tree to resolve it in. |

A refused `$ref` is a typed error naming the pointer (`SPEC_REF_UNRESOLVABLE`),
exit `2`. In a CI container where the checkout is owned by another user, `git`
refuses to read it; pikopod says so and names the fix
(`git config --global --add safe.directory "$GITHUB_WORKSPACE"`).

## incidents

An **incident** is an exchange that failed, as opposed to drift, which is a
successful response whose shape changed. Incidents are facts about one request,
so they need no baseline and fire from the first request — they are never
subject to the warmup window.

| Kind | Fires when | Level |
|---|---|---|
| `upstream_error` | The upstream answered 5xx. | ERR |
| `upstream_unreachable` | pikopod could not reach the upstream and returned its own 502. Never retried. | ERR |
| `rate_limited` | The upstream answered 429. | WARN |
| `client_error` | The upstream answered 4xx. **Opt-in.** | WARN |

`upstream_error`, `upstream_unreachable` and `rate_limited` are always captured
and cannot be switched off. They are never normal.

```yaml
upstreams:
  examplepay:
    incidents:
      client_errors: true       # default false
      client_error_rate: 0.05   # default 0.05
```

| Key | Meaning |
|---|---|
| `client_errors` | Capture 4xx as incidents. Off by default. |
| `client_error_rate` | Share of requests to one endpoint family that must be 4xx before one is reported. Default `0.05`. |

**Why 4xx is opt-in.** A 4xx is usually the caller's own bug, which is exactly
why an integration tool should be able to catch it — but an endpoint where a 401
is the normal answer would page you all day. Below **20 requests** to an endpoint
family no rate is claimed at all, because the first 4xx is a rate of 1.0 and
means nothing.

Every incident is reproducible: see `pikopod incidents` and
`pikopod scenario reproduce <fingerprint>`.

## listen

```yaml
listen: 127.0.0.1
```

Bind address for both servers. Defaults to loopback.

**Binding a non-loopback address requires a token.** The drift agent sits in a
production request path; an unauthenticated listener on `0.0.0.0` is a way to
lose data, so pikopod refuses to start rather than let it happen. Supply the
token via `PIKOPOD_TOKEN` or [`token_file`](#storage) — never on the command
line, where `ps` can read it. Tokenless listeners additionally reject requests
carrying a foreign `Host` header, which blocks DNS rebinding.

## ports

```yaml
agent_port: 4700     # drift agent
sandbox_port: 4600   # sandbox
```

`pikopod up` serves both. The sandbox answers at `/<provider>/`, the agent at
the `listen` prefix you configured for each upstream.

## data_dir

```yaml
data_dir: ./pikopod-data
```

Everything pikopod persists lives here: recordings, baselines, alert state,
imported IRs, scenario packs, and the drift event log. Defaults to
`./pikopod-data`. Override with `PIKOPOD_DATA_DIR`.

Back it up like state, not like a cache — baselines represent days of learning,
and losing them restarts every warmup window.

## storage

How much pikopod keeps on disk, and the files it guards.

```yaml
token_file: /etc/pikopod/token   # top-level key, not nested under `storage`
sampling:
  rate: 0.25
retention:
  max_age_hours: 168
```

### token_file and the salt

`token_file` holds the listener token for non-loopback binds. It must not be
group- or world-readable.

The tokenization salt is `<data_dir>/.salt` and must be mode `0600` — see
[the salt section in the security notes](security.md#salt). pikopod refuses to
start if it is readable by other local users, because that would let them
correlate your tokens.

Writes are atomic, and the CLI and daemon coordinate through file locks where
they share state.

### sampling

`sampling.rate` thins what recordings **persist** — never what pikopod
**learns** from. Every record still feeds the learner and differ; the rate only
gates the disk write. Error responses, drift-bearing records, and pre-warmup
traffic are always kept regardless. The value is a fraction between `0` and `1`;
`rate: 0` (keep guaranteed classes only) is distinguishable from unset (`1.0`).

### retention

`retention.max_age_hours` ages recordings and the event log out of disk. A
record is kept at least that long and deleted no later than roughly twice that
age. `0` (default) keeps size-based rotation only.

Note that `pikopod scenario from-drift` reads the event log, so fingerprints
older than the retention window can no longer be pinned. Saved packs are
unaffected.

## tls

```yaml
tls:
  cert_file: /etc/pikopod/cert.pem
  key_file: /etc/pikopod/key.pem
```

Serves both ports over HTTPS. **Both files or neither** — half a TLS config is
a misconfiguration, not a default, and pikopod treats it as an error.
Self-signed certificates are fine; pikopod's own CLI clients trust the
configured certificate file directly.

## slack

```yaml
slack:
  webhook_url: https://hooks.slack.com/services/...
  min_level: WARN
  digest_hours: 24
```

| Key | Meaning |
|---|---|
| `webhook_url` | A plain incoming webhook. |
| `min_level` | Delivery floor: `INFO` (default, deliver everything), `WARN`, or `ERR`. |
| `digest_hours` | Periodic digest of new findings by severity. `0` disables it. |

`min_level` floors **the channel, not the record**. Muted alerts still appear
in the local event log, in `pikopod status`, and in the digest.

## alerts

Alert behavior is not configured by key — it is a fixed contract, documented
here because error messages point at it.

- **One alert per fingerprint, forever.** A given structural change notifies
  once, however many requests carry it.
- **N occurrences before the first alert** (default 3, inside a 15-minute
  window), so a single anomalous response never pages anyone.
- **Dedupe and acknowledgement are persisted**, so restarts do not re-alert.
- **A delivery ceiling** bounds messages per hour.
- **The fingerprint cap evicts rather than going dark** — pikopod would rather
  forget the least valuable state than stop tracking new drift entirely, and it
  tells you when it saturates.
- **Latency is never alerted.** pikopod only reports changes it can prove from
  the bytes. See [OVERVIEW](OVERVIEW.md#what-pikopod-refuses-to-do).

Acknowledge with `pikopod ack <fingerprint>`; accept a change as the new normal
with `pikopod accept`.

## baselines

```yaml
warmup:
  min_samples: 50
  min_hours: 48
```

Per-endpoint learning gates. pikopod watches an endpoint until it has seen
`min_samples` responses across at least `min_hours`, then **freezes** a
reference and compares against that frozen copy from then on. Freezing is what
stops a slow drift from quietly becoming the new normal.

Both are overridable for evaluation. Lowering them shortens the blind window
and raises false positives — a baseline built from 5 samples has not seen your
optional fields yet. `min_hours: 0` is explicitly distinguishable from unset.

Inspect progress with `pikopod status` and `pikopod report`.

## spec_watch

```yaml
spec_watch:
  interval_minutes: 60
```

Armed per upstream by `spec_source`. pikopod re-fetches the provider's
published specification and diffs it against your pinned import, so you learn
about changes the provider *declared* as well as changes observed on the wire.

`spec_source` accepts an `http(s)` URL, a local file path, or `git:<ref>:<path>`.

Fetches are ETag-gated, and the pin never advances on its own — accepting a
declared change is an explicit `pikopod import <name> --update`.

Severity is derived by law from the shape of the change (how the admitted
payload set moved, on which side, in a guaranteed or optional part), never
hand-assigned. Withdrawing a presence guarantee on a response field, whether by
removing the field or by making it optional, is `WARN`; traffic evidence that
consumers receive the field today raises it to `ERR`.

## refine

```yaml
refine:
  enabled: false
  prefer_spec: false
```

Off by default. When enabled, observed traffic accumulates an overlay beside
the spec-derived contract, and matured observations join the effective
contract. `prefer_spec` flips type-conflict precedence back to spec-wins; the
default is traffic-wins once the sustain gates clear.

Inspect the result with `pikopod contract <sandbox>`.

## behaviour

```yaml
behaviour:
  enabled: false
```

Off by default. When enabled, the agent learns the provider's **observed state
machine** from traffic: for each resource it sees more than once (a path with
an identifier segment, such as `/charges/{id}`), every low-cardinality string
field whose value changed between two recordings records an edge, `pending →
succeeded`, with a count. Nothing in a specification can say this; only
traffic can.

`pikopod contract <sandbox>` renders the graph (and `--format json` emits it):

```
observed state machine — examplepay

  GET /charges/{id} · status          (412 transitions over 9 days)
    pending      → succeeded      380
    pending      → failed          31
    succeeded    → refunded         1     ← seen once
    never observed: failed → pending, succeeded → pending
```

`never observed` is an absence in recorded traffic, listed only once the field
has cleared both warmup gates, and never a claim that the provider cannot make
the transition. **Nothing here is enforced**: the sandbox is untouched.

What is stored is the aggregate graph only: field paths, value pairs, counts
and timestamps under `data_dir/apis/<upstream>.behaviour.json`. The previous
value per resource lives in a bounded in-memory cache and is never written,
so a restart loses in-flight edges rather than persisting per-resource data.
Fields the sanitizer tokenized or dropped are invisible; `volatile_fields`
and `mute` apply here as everywhere else.

## quotas

Sandbox resource limits, fixed rather than configured:

| Limit | Default |
|---|---|
| Stored resources per sandbox | 10,000 |
| Total storage | 100 MiB |
| Single resource | 256 KiB |

Exceeding them returns `413` or `507` rather than degrading silently. Reset a
sandbox's state with `pikopod sandbox reset <name>`.

## recordings-tier

Recorded traffic is the sandbox's final resolution tier, enabled per sandbox
with `pikopod import <name> --recordings-fallback`.

Matching is hierarchical, because one clever hash would miss on real payment
traffic where every request carries different amounts and references:

1. **exact** — method, path, and normalized body hash
2. **shape** — method, path template, and body field set (values ignored)
3. **sequence** — the next unserved recording for that method and template

Every served response names the tier it came from in `X-Pikopod-Replay-Tier`,
and each degradation explains which fields missed.

## llm

```yaml
llm:
  provider: openrouter          # openrouter (default) or openai
  api_key: sk-or-...            # provider-neutral key
  model: openai/gpt-4o-mini     # optional; this is the OpenRouter default
  # openrouter_key: sk-or-...   # deprecated alias, still honoured
```

Bring your own key. pikopod never ships a key and never proxies your requests
through anyone else. The registered providers are `openrouter` and `openai`.
OpenRouter defaults to `openai/gpt-4o-mini`; OpenAI defaults to `gpt-4o-mini`.
The `Provider` boundary is the extension point for additional providers.

Adding one is two edits: a name and its key environment variables in
`internal/llmprovider`, which is what this file's validation reads, and a
`Provider` implementation registered against that name in
`internal/scenario/nl`. A name in the table with no implementation fails the
test suite rather than reaching a user.

Key resolution is deterministic and stops at the first value found:

1. `llm.api_key`
2. `PIKOPOD_LLM_KEY`
3. the provider's standard environment variable (`OPENROUTER_API_KEY` or `OPENAI_API_KEY`)
4. for OpenRouter only, deprecated `PIKOPOD_OPENROUTER_KEY` or
   `llm.openrouter_key`

When a key is stored in `pikopod.yaml`, the file must be private (`0600`).
Environment-provided keys do not make the configuration file secret.

For testing or staging, `PIKOPOD_LLM_BASE` overrides the configured provider's
API base URL. `PIKOPOD_OPENROUTER_BASE` remains supported as a deprecated
OpenRouter-only fallback when the generic override is unset.

The key is optional. Three things use it:

- plain-English scenario authoring (`pikopod scenario create`)
- [`pikopod fix`](#fix)
- importing from a documentation URL, as the last resort after the
  deterministic rungs (an embedded spec, a linked spec) fail. A contract
  extracted this way is marked `LLM_EXTRACTED` and imports as a draft.

Everything else — importing a spec, the sandbox, drift detection, spec diffing,
deterministic scenario packs, the CI gate — works with no key at all.

Where a model is used, it is fenced: for scenario authoring the model never
emits steps, only a schema-constrained intent validated against operations that
actually exist in your imported API. It cannot invent an endpoint.

## scenarios

Scenario packs live under `<data_dir>/scenarios`, and the repository's
[`scenarios/`](../scenarios/README.md) directory documents the format.

Eleven provider-agnostic failure archetypes bind themselves to your API from
its specification — run `pikopod scenario list <sandbox>` to see which of them
your API can support. Binding uses explicit and confirmed facts only, and
**zero candidates is a first-class result with a reason**, not an error and not
a guess.

```bash
pikopod scenario list examplepay
pikopod scenario run examplepay declines timeouts
pikopod scenario create examplepay "timeout after the charge succeeds"   # needs llm
pikopod scenario from-drift fp_6d540d187d44
```

## pr

`pikopod pr` posts findings to a pull request or opens one carrying a fix.
Supports GitHub and GitLab.

Authenticate with a token in the environment — `GITHUB_TOKEN` (or `GH_TOKEN`,
or a logged-in `gh` CLI) for GitHub, `GITLAB_TOKEN` for GitLab. pikopod posts
one marker-tagged comment per finding source and updates it in place rather
than adding a new comment per run.

```bash
pikopod pr comment --handoff report.json
pikopod pr open --handoff report.json
```

## fix

```bash
pikopod fix <fingerprint> --dir . --check "go build ./..." --pr
```

Turns a drift event into a code change: a deterministic impact scan of your
repository, then a bounded patch from your own model, verified by `--check` and
reverted in full if that check fails.

| Flag | Meaning |
|---|---|
| `--dir` | Repository to scan. Defaults to the working directory. |
| `--check` | Command that must pass for the patch to be kept. |
| `--dry-run` | Print the proposed edits, change nothing. |
| `--pr` | Open a pull request with the result. |

Requires an [`llm`](#llm) key. Without one the impact scan still runs — it is
the deterministic half.

**Known limitation, stated plainly.** The impact scan matches source text
literally and case-sensitively. A client that spells the field differently
(`accountNumber` for `account_number`), indexes it dynamically, or forwards the
payload untouched is invisible to it. When the scan finds nothing, pikopod
reports `UNVERIFIABLE` and exits non-zero rather than reporting a clean result —
it will not hand your CI a green gate on a silent miss. Treat a zero-impact
answer as "look yourself," not as proof the field is unused.

## mcp

`pikopod mcp` serves pikopod to a coding agent over the Model Context Protocol
on stdin/stdout. It reads the same `pikopod.yaml` (`--config`), starts no
listener and no daemon, and exits 2 when the configuration is missing or
invalid.

```json
{ "mcpServers": { "pikopod": { "command": "pikopod", "args": ["mcp", "--config", "/path/to/pikopod.yaml"] } } }
```

Every tool returns one verdict from a closed set, and **`CLEAN` and
`UNVERIFIABLE` never collapse**: an agent proceeds on `CLEAN`, and
`UNVERIFIABLE` always carries a reason saying what could not be established.

| Verdict | Meaning |
|---|---|
| `CLEAN` | The check ran and found nothing. |
| `FINDINGS` | The check ran and found something; findings attached. |
| `UNVERIFIABLE` | The check could not determine an answer; `reason` says why (no recordings, warmup incomplete, evidence redacted, nothing binds). |
| `ERROR` | pikopod or its configuration is broken; `error` carries what/why/fix/docs. |

The readers are the checks an agent cannot make by reading files:

| Tool | Answers |
|---|---|
| `spec_diff` | What two spec versions declare differently, with a severity per finding. |
| `drift_events` | What the agent observed change in traffic that passed warmup. Before warmup it is `UNVERIFIABLE` with the samples seen and the gate, never an empty `CLEAN`; every result carries a `warmup` block. |
| `replay_ci` | The CI gate: recordings against frozen baselines. |
| `conformance` | Whether the provider's recorded responses obey its own spec; redacted evidence counts as unverifiable, never as a pass. |
| `reproduce` | A recorded incident turned into a pack and run locally (writes one pack file under `data_dir/scenarios`). |
| `scenario_list`, `scenario_run` | Which archetypes bind to a sandbox, with the reason when one does not, and the verdict of running them against a throwaway copy. |
| `get_requests` | What the caller's own code actually sent to the running sandbox. |

The controls put a **running** sandbox (`pikopod up`) into a state the
caller's own tests then meet; they act on a local fake and never on a
provider: `set_mode`, `clear_mode`, `arm_fault`, `clear_faults`,
`emit_webhook`.

Deliberately absent: `fix` (a model editing code with no human in the loop),
`import`, `chaos`, `ack`, `accept`, and every reset. They stay human-operated
commands.
