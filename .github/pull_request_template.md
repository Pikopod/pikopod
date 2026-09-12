**What this changes**

<!-- One or two sentences. Link the issue if there is one. -->

**Why**

<!-- The problem, not the patch. If it changes behaviour a user can observe,
say what they will see differently. -->

**Checklist**

- [ ] `go test ./... -count=1` passes
- [ ] `gofmt -l cmd internal` is empty and `go vet ./...` is clean
- [ ] No new cgo dependency (the release artifact is a static binary on every platform)
- [ ] Error message strings are unchanged, or the change is deliberate and called out above
- [ ] New test fixtures have an entry in `testdata/parity/importer/specs/SOURCES.md`
- [ ] No provider payloads, keys, tokens, or customer data in the diff or in test fixtures
