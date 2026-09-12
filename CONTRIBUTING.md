# Contributing to pikopod

Thanks for your interest in pikopod. This document covers how to propose a
change. For building, testing, and the project layout, see
[DEVELOPMENT.md](./DEVELOPMENT.md).

## Getting help

For bugs and feature requests, please use [GitHub issues][issues] so the
discussion stays attached to the work. For broader design or usage questions,
start a [GitHub discussion][discussions].

## Code of Conduct

Please read and follow our [code of conduct](./CODE_OF_CONDUCT.md).

## How to contribute

The most valuable contribution right now is a bug report from a real
integration: a provider whose spec we import badly, a drift we miss, or a drift
we invent. pikopod is pre-v1 and every provider is a new edge case.

- Report bugs or request features with a [GitHub issue][issues].
- Improve docs, tests, examples, packaging, or platform support.
- Add support for a spec format or a provider shape we handle poorly.
- Pick up a [good first issue][good-first-issues] for a scoped starting point.

## Building and testing

See [DEVELOPMENT.md](./DEVELOPMENT.md) for prerequisites, the build and test
loop, the project layout, and the conventions enforced in review.

## Submitting a pull request

1. Fork the repository and create a focused branch.
2. Keep the change scoped to one bug fix, feature, or documentation improvement.
3. Add a test that fails before your change and passes after. If you are fixing
   a bug, we want to see the bug reproduced first.
4. Do not hand-edit files under `testdata/parity/`. They are maintainer
   regenerated. If your change alters a golden, say so in the pull request.
5. Run `go test ./... -race -count=1`, `go vet ./...`, and `gofmt -l .` before
   pushing.
6. Use [conventional commits][conventional-commits] for commit messages, and
   sign off with `git commit -s`. We use the [DCO][dco], not a CLA, so you keep
   your copyright.
7. Open the pull request with a clear summary and test plan.

If you are unsure which checks apply, say so in the pull request. We can help
narrow it down.

## Filing an issue

The most useful report includes the provider, the spec you imported, what
pikopod said, and what you expected instead. Redact freely: `pikopod inspect`
shows you what is safe to share.

For security issues, do not open an issue. See [SECURITY.md](./SECURITY.md).

[issues]: https://github.com/pikopod/pikopod/issues
[discussions]: https://github.com/pikopod/pikopod/discussions
[good-first-issues]: https://github.com/pikopod/pikopod/labels/good%20first%20issue
[conventional-commits]: https://www.conventionalcommits.org/en/v1.0.0/
[dco]: https://developercertificate.org/
