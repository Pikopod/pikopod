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
```

| Key | Meaning |
|---|---|
| `listen` | Route prefix on the agent, e.g. `/examplepay`. |
| `target` | Absolute base URL this upstream forwards to. Must include scheme and host. |
| `volatile_fields` | Field names excluded from baseline learning, drift diffing, the replay CI gate, and replay tier-1 hashing. Matched by name at any depth, case-insensitive. |
| `mute` | Endpoint templates whose alerts are suppressed. |
| `spec_source` | Arms the declared-drift watcher. See [spec_watch](#spec_watch). |

Use `pikopod volatile suggest <upstream>` to find noisy fields rather than
guessing.

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

Severity is derived by law from the shape of the change, never hand-assigned.

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
  provider: openrouter          # default when unset
  api_key: sk-or-...            # provider-neutral key
  model: openai/gpt-4o-mini     # OpenRouter default when unset
  # openrouter_key: sk-or-...   # deprecated alias, still honoured
```

Bring your own key. pikopod never ships a key and never proxies your requests
through anyone else. `openrouter` is currently the only registered provider;
the `Provider` boundary is the extension point for additional providers.

Adding one is two edits: a name and its key environment variables in
`internal/llmprovider`, which is what this file's validation reads, and a
`Provider` implementation registered against that name in
`internal/scenario/nl`. A name in the table with no implementation fails the
test suite rather than reaching a user.

Key resolution is deterministic and stops at the first value found:

1. `llm.api_key`
2. `PIKOPOD_LLM_KEY`
3. the provider's standard environment variable (`OPENROUTER_API_KEY` today)
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
