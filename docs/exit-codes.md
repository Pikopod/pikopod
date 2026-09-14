# Exit codes

Exit codes are API. Script against them.

| Code | Meaning | Who scripts against it |
|------|---------|------------------------|
| `0` | Clean — no drift found | CI passes |
| `1` | The check ran and failed: drift found, assertions failed, or the provider violated its own spec (`pikopod replay --ci`, `pikopod spec-diff`, `pikopod scenario run`, `pikopod scenario from-drift`, and `pikopod conformance` **with `--strict`**) | CI fails the build on provider drift |
| `2` | pikopod or configuration error (bad YAML, refused bind, missing config, a result pikopod cannot verify) | Distinguishes "provider changed" from "tool misconfigured" — never conflate these in CI |

The separation between `1` and `2` is the point. A build that fails because
your provider shipped a breaking change needs a different response from a build
that fails because a token expired, and a CI gate that collapses them teaches
people to ignore both.

`2` also covers results pikopod cannot stand behind. `pikopod fix` exits `2`
when its impact scan finds nothing, because a literal scan cannot distinguish
"this field is unused" from "this codebase spells it differently" — see the
[fix limitations](config-reference.md#fix).

## Readers vs gates

`pikopod incidents` is a **reader**, not a gate: it exits `0` whether or not it
found anything, and exits `2` only when it cannot read the event log. Nothing
about "I found incidents" is an exit-code signal — script against
`--format json` instead, where `total_matching` and `truncated` are always
present so a shortened list can never be mistaken for a clean one.

`pikopod scenario reproduce` exits `0` when it writes a pack and `2` when it
refuses — when the incident's recording has aged out of retention, or when the
pack ran and the failure did **not** recur, which means pikopod cannot stand
behind it as a reproduction. It never fabricates a request to avoid refusing,
because a scenario built from invented data passes and means nothing.

There is no `1`, and the contrast with its sibling is deliberate.
`pikopod scenario from-drift` exits `1` on a failed replay because its pack
*pins the old contract*, so failing confirms the drift. `reproduce` is
inverted: its pack *recreates a failure*, so failing means the recreation did
not work. One is a finding; the other is an unverifiable result.

## Schema versions

`schema/drift-event.schema.json` is the other stable surface. Its
`schema_version` field is the protocol number, and it is pinned in the schema
and in the producer at the same time — `e2e/schema_contract_test.go` fails the
build if they diverge.

| Version | Change |
|---|---|
| `1` | Eight drift kinds: a successful response whose shape changed. |
| `2` | Adds four **incident** kinds — `upstream_error`, `upstream_unreachable`, `rate_limited`, `client_error` — for exchanges that failed rather than changed shape. |

### What moves the version, and what does not

v2 also settled the rule, because this will come up again:

- **New `kind` values do NOT bump the version.** The enum lists what pikopod
  emits today, not a closed set for all time. Treat a kind you do not recognise
  as informational and carry on.
- **New fields do NOT bump the version.** Ignore the ones you do not know.
- **Structural changes DO bump it**: a field removed, a type changed, or an
  existing field's meaning changed.

That is why v1 → v2 happened here. v1 declared `kind` a closed enum, so widening
it was a contract change and had to be signalled. With the rule above in place,
the next new kind will not need a bump — and when the number *does* move, it
means something specific.

If you validate strictly against v1's enum, incident events will fail until you
widen. If you check `schema_version == "1"` for equality, switch to a minimum
check, or you will reject ordinary drift events too.

Incidents carry the concrete status in `after` (`"503"`) and, like every event,
a path **template** in `endpoint` — never a concrete path.
