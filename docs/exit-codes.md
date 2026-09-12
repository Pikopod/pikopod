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
