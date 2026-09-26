# Changelog

All notable changes to pikopod. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and releases are tagged `vX.Y.Z` on GitHub.

## [Unreleased]

### Changed

- Every comment was removed from the Go source; the token stream of every file is unchanged. The house rule in `DEVELOPMENT.md` is now "no comments in Go code". (#62)

## [0.1.2] - 2026-09-24

### Added

- `pikopod mcp`: the checks and sandbox controls served to a coding agent over the Model Context Protocol, with `CLEAN`, `FINDINGS`, `UNVERIFIABLE` and `ERROR` verdicts. (#51)
- `pikopod spec-diff --format githubactions`: findings appear inline on the pull request diff, and same-repository relative `$ref`s resolve for file and `git:` sources. (#56)
- Documentation-URL import: `--emit-spec` writes the spec the import used, the crawl is budgeted by page group, and a `--webhooks` sidecar binds events to the calls that fire them. (#44)
- Webhook deliveries in the provider's declared envelope, wrapped and signed the way the provider documents, with the key read from the environment. (#39, #40)
- Emit-only webhook events, `pikopod webhook emit`, and the rule that only declared events are ever delivered. (#38)
- Spec examples serve responses, and create, read and modify responses follow the spec's envelope. (#42)
- `--bind role=operationId` grounds an archetype on a docs import by asserting the missing fact. (#41)
- Request journal records headers, query and virtual time; `VERIFY_SEQUENCE` proves order and spacing. (#36)
- `pikopod mode set|show|clear`: a scenario's standing state on the served sandbox, so your own tests meet the failure. (#35)
- The observed state machine (`behaviour.enabled`), rendered by `pikopod contract`. (#58)
- `pikopod incidents export` and incident bundles that `scenario reproduce` and `fix` accept on any machine. (#59)
- Spec-declared enum values survive redaction when the value is one of the declared members. (#45)

### Changed

- Severity in `spec-diff` is derived by one law over the admitted-payload set; removing a required response field is `WARN`, raised to `ERR` by traffic evidence. (#57)
- `volatile_fields` suppression is value-only and path-scoped; malformed entries are refused, and dead, stable and over-broad entries are reported. (#54)
- Dead IR fields and the unproduced trust level were deleted. (#55)
- The README was shortened and now points at docs.pikopod.com. (#60)

### Fixed

- The fail-open proxy test snapshots its counters before the request, removing an intermittent CI failure. (#43)
- Orderly teardown: the proxy, recorder and sandbox engines drain and close cleanly on shutdown. (#59)

## [0.1.1] - 2026-09-15

### Added

- Failed exchanges are captured as incidents (`upstream_error`, `upstream_unreachable`, `rate_limited`, and opt-in `client_error`) and reproduced in the sandbox with `pikopod scenario reproduce`. (#29)
- Benchmarks for the sanitize and join hot paths. (#24)

### Fixed

- `--version` reports the module version when `go install` embeds no VCS information, and the commit and build date otherwise. (#16, #19)
- Upstream failures that happen mid-response are counted. (#23)
- The sandbox no longer issues provider-shaped webhook signing secrets. (#25)
- The Homebrew `brew trust` step and the cosign identity in the README. (#22)
- Canary fixtures use reserved example domains. (#15, #18)

## [0.1.0] - 2026-09-13

### Added

- First release: a deterministic sandbox built from an OpenAPI, Swagger 2.0, Postman or GraphQL spec; eleven failure archetypes that bind to it; `pikopod chaos`; the fail-open observing agent with redaction before disk, warmup, baselines and drift alerts; `pikopod spec-diff`; `pikopod replay --ci`; `pikopod demo`.
- Release pipeline: static binaries for macOS, Linux and Windows on amd64 and arm64, an SBOM per archive, cosign-signed `SHA256SUMS` with SLSA provenance, deb and rpm packages, and a Homebrew tap.

[Unreleased]: https://github.com/Pikopod/pikopod/compare/v0.1.2...HEAD
[0.1.2]: https://github.com/Pikopod/pikopod/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/Pikopod/pikopod/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/Pikopod/pikopod/releases/tag/v0.1.0
