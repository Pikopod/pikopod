# pikopod

**Your API provider changed something and didn't tell you.** pikopod watches
the APIs you depend on — the specs they publish *and* the bytes they actually
send — and tells you the moment those two disagree with what you built against.

One Go binary. Runs locally. No accounts, no telemetry, no cloud.

```
[ERR] pikopod drift — new value on GET /transaction/tx_{id} (examplepay)
status: value "succeeded" not in known set [success]
fingerprint fp_385153d1776c · first seen 2026-09-11T00:08:54Z · 3 occurrence(s)
replay it: pikopod scenario from-drift fp_385153d1776c
```

One letter. Your `if status == "success"` stops matching and payments start
looking unsettled. Nobody's changelog mentioned it.

> **v0.x.** Exit codes and the drift-event schema are stable and safe to script
> against. The CLI surface may still change between minor releases.

## Try it in ten seconds

```bash
pikopod demo
```

Zero config. It stands up a fake provider, sends traffic, silently changes the
provider's responses, and prints the alerts. Runs in about a second.

## Install

```bash
go install github.com/pikopod/pikopod/cmd/pikopod@latest
```

Or with Homebrew:

```bash
brew install pikopod/tap/pikopod
```

Or download a signed binary from
[Releases](https://github.com/pikopod/pikopod/releases) — macOS and Linux,
amd64 and arm64, static, zero dependencies.

Verify what you downloaded — every release ships `SHA256SUMS`, cosign-signed
with SLSA provenance:

```bash
sha256sum -c SHA256SUMS --ignore-missing
cosign verify-blob --certificate SHA256SUMS.pem --signature SHA256SUMS.sig SHA256SUMS \
  --certificate-identity-regexp 'github.com/pikopod/pikopod' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

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
    curl -sSL https://github.com/pikopod/pikopod/releases/latest/download/pikopod_linux_amd64.tar.gz | tar xz
    ./pikopod spec-diff git:origin/main:openapi.yaml openapi.yaml --fail-on ERR
```

Severity is derived by law from the shape of the change — effect, direction,
and guards — never hand-assigned, so "breaking" means the same thing on every
endpoint.

## Then: watch a real provider

```bash
pikopod init                                    # scaffold pikopod.yaml
pikopod import examplepay --spec <spec-or-docs-url>
pikopod up                                      # sandbox :4600 · drift agent :4700
```

That gives you two things.

**A sandbox at `:4600/examplepay`** built from the provider's spec — point
staging at it. It is deterministic: same seed, same bytes, every run. And
unlike the provider's own sandbox, you can make it misbehave:

```bash
pikopod scenario list examplepay      # which failure stories YOUR api supports
pikopod scenario run examplepay declines timeouts partial_failure
pikopod chaos examplepay --kind error --status 503 --method POST --path /v1/charges
```

**A drift agent at `:4700/examplepay`** — point production at it. It forwards
everything untouched, learns what normal looks like, then tells you when the
shape changes. It stays quiet for the first 50 samples / 48 hours on purpose,
because a baseline built from five responses hasn't seen your optional fields
yet.

## What makes it different

Most tools see one side. A spec-diff tool reads what the provider *published*.
Monitoring sees your traffic but throws response bodies away for privacy
reasons.

pikopod holds **both**, and crosses them. So it can say things neither can:

> The spec removed this endpoint — and you're still sending it 120 requests a
> day.

Traffic evidence raises the severity of a declared change. A declared change
downgrades an observed one to "documented, not silent." That join is the
product.

## Safe to put in front of money

The drift agent is a reverse proxy in your production request path, so:

- **It serves first and observes afterwards.** Observation is asynchronous and
  bounded; every capture stage is panic-isolated and counted. If pikopod breaks
  internally, your traffic still flows.
- **It never retries.** An automatic retry in front of a payments API is a
  double-charge window.
- **It redacts before the disk, not after.** Credentials become placeholders,
  identifiers become format-preserving tokens, unclassifiable values are
  dropped. Check it yourself with `pikopod inspect`.
- **It initiates no network traffic of its own.** Slack, your forge, and your
  LLM provider — all with your credentials, all because you configured them.

See [docs/security.md](docs/security.md) for how redaction works and how to
verify it.

## Closing the loop

```bash
pikopod scenario from-drift fp_385153d1776c   # the change becomes a test
pikopod replay --ci                           # gate builds on recorded traffic
pikopod fix fp_385153d1776c --check "go build ./..." --pr
```

`pikopod fix` scans your repository for affected code and opens a PR with a
patch from your own model, reverted in full if your check fails.

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

## Licence

Apache-2.0. See [LICENSE](LICENSE).
