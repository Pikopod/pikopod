# Exit codes

Exit codes are API. Script against them.

| Code | Meaning | Who scripts against it |
|------|---------|------------------------|
| `0` | Clean — no drift found | CI passes |
| `1` | The check ran and failed: drift found, assertions failed, or the provider violated its own spec (`pikopod replay --ci`, `pikopod spec-diff`, `pikopod scenario run`, `pikopod conformance`) | CI fails the build on provider drift |
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
refuses — for example when the incident's recording has aged out of retention.
It never fabricates a request to avoid refusing, because a scenario built from
invented data passes and means nothing. There is no `1`: the pack it generates
is what carries a pass/fail verdict, via `pikopod scenario run`.
