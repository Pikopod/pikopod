# Security notes

Operational security details for running pikopod. To report a vulnerability,
see [SECURITY.md](../SECURITY.md).

pikopod's drift agent is a reverse proxy that sits in a production request
path, and it writes what it observes to disk. Both facts drive everything
below.

## The data plane never fails closed

The proxy serves first and observes afterwards. Observation runs asynchronously
behind a bounded queue, every capture stage is panic-isolated *and* counted,
and the proxy never retries a request — an automatic retry in front of a
payments API is a double-charge window.

If pikopod's own machinery breaks, your traffic still flows. That is the
contract, and it is why observation failures increment a counter instead of
surfacing as an error.

## Redaction happens before the disk, not after

Values are classified and rewritten on the way in, not cleaned up later:

- **Credentials** become typed placeholders.
- **Identifiers** become deterministic, format-preserving tokens, so
  `tok_8f3a91` stays token-shaped and path templates still work.
- **Names and similar PII** are dropped.
- **Free text and unknown strings** are dropped. The classifier fails closed on
  any *string* it cannot place.

**The exception, stated plainly: numbers, booleans and nulls are kept unless
their key names them as sensitive.** A number carries no shape signal — `5000`
could be an amount, an account number or a timestamp — so key names are the only
evidence available, and an unrecognised key passes the value through. Keys that
look like secrets, identifiers, card data, phone numbers or expiry dates are
still substituted or tokenized whatever the JSON type, because a PAN sent as a
number must not ride through on its type. But `"amount": 245000` under a key
pikopod does not recognise is written verbatim.

This is a deliberate trade, not an oversight — dropping every unrecognised
number would discard most of what makes a baseline useful — and it is the one
place redaction is not fail-closed. If that matters for a field, name it in
`volatile_fields` or check it with `pikopod inspect` before you trust the
recording.

Detection is generic — key names, value shape, entropy — never a list of
provider-specific prefixes, so it does not silently stop working when you add a
provider nobody anticipated.

**One thing an imported contract changes, precisely.** When an upstream has an
imported spec, a response field whose schema declares an `enum` keeps a value
on disk **only if that value is one of the declared members**. `currency: NGN`
survives because the provider published `[NGN, USD]`; the same field carrying
`GHS` or a free-text error is classified exactly as it would be without the
spec. Only `EXPLICIT` and `DERIVED` enums count; an enum the model extracted
from prose or a heuristic inferred unlocks nothing, because relaxing redaction
on a guess is not recoverable. Request bodies, headers, paths and every other
field are untouched, and an upstream with no imported contract behaves exactly
as before. `pikopod up` prints how many fields this applies to.

Verify it yourself rather than trusting this page:

```bash
pikopod inspect
```

`inspect` prints stored records with tokenized fields visible, so you can
confirm for yourself what was kept before you trust it.


## salt

Tokenization uses an HMAC keyed by a per-install salt stored at
`<data_dir>/.salt`, generated on first run. The salt is what makes tokens
correlatable **within** your install and meaningless outside it.

The file must be mode `0600`. pikopod refuses to start if it is group- or
world-readable, because another local user who can read it can correlate every
token you have stored:

```bash
chmod 600 <data_dir>/.salt
```

Deleting the salt regenerates it. Existing tokens will no longer correlate with
new ones — baselines built on the old salt are effectively reset, so treat the
salt as state worth backing up alongside `data_dir`.

## Listener safety

Both servers bind `127.0.0.1` by default.

Binding any non-loopback address **requires a token**, supplied through
`PIKOPOD_TOKEN` or a `token_file` — never through a command-line argument,
which is visible to every user on the box via `ps`. Tokenless listeners also
reject requests carrying a foreign `Host` header, which blocks DNS rebinding
from a browser on the same machine.

Serve over HTTPS with [`tls`](config-reference.md#tls). Both certificate and
key, or neither.

## Verifying a release

The canonical verification command. The README links here; `DEVELOPMENT.md`
links here; there is deliberately no second copy, because a copy that drifts
out of case or loses its anchor still *runs* and still *passes*, which is worse
than having no command at all.

```bash
sha256sum -c SHA256SUMS --ignore-missing   # macOS without coreutils: shasum -a 256 -c
cosign verify-blob --certificate SHA256SUMS.pem --signature SHA256SUMS.sig SHA256SUMS \
  --certificate-identity-regexp '^https://github.com/Pikopod/pikopod/\.github/workflows/release\.yml@refs/tags/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Two details that are the whole point:

- **The organisation is `Pikopod`, capitalised.** cosign matches the identity
  case-sensitively. A lowercase regexp matches nothing, and `cosign` reports
  that as a verification failure rather than a typo — so you would think the
  artifact was bad.
- **The regexp is anchored** to the release workflow and to `refs/tags/`. An
  unanchored pattern like `'github.com/pikopod/pikopod'` matches any workflow,
  in any repository, whose identity URL contains that substring. It would pass
  on something we did not build.

The Go module path is `github.com/pikopod/pikopod`, lowercase, because that is
what `go.mod` declares. Both spellings are correct in their own context and
they are not interchangeable here.

## What leaves the machine

pikopod initiates no network traffic on its own. Everything outbound is
something you configured, using your own credentials:

| Destination | Why | Carries |
|---|---|---|
| Your upstream targets | The proxy forwarding your traffic | Your requests, unmodified |
| Your Slack webhook | Alerts | Finding structure and field paths |
| GitHub / GitLab | `pikopod pr` | The report you asked it to post |
| Your model provider (OpenRouter) | `scenario create` | Your prompt, plus the operation inventory of the imported spec |
| Your model provider (OpenRouter) | `fix` | The drift event, plus **excerpts of your source**: up to 5 matched regions per file, with 4 lines of context either side |
| Your model provider (OpenRouter) | `import --spec <docs-url>`, rung 4 only | Up to 240 KB of the **provider's** documentation prose, in 36 KB batches |
| A `spec_source` URL | The declared-drift watcher | Nothing — it fetches |
| A documentation page and what it links to | `import --spec <docs-url>`, rungs 1–3 | Nothing — it fetches. See below. |

Every model row requires **your own key** (`llm.api_key`). With no key
configured, none of those three paths runs at all: `fix` refuses, and a docs-URL
import that needs rung 4 returns an error telling you so. pikopod is a
bring-your-own-key tool and never proxies a request through infrastructure we
operate — but note that OpenRouter is a *broker* that routes to third-party
model vendors, so your provider relationship is with them, not with a single
model vendor.

There is no telemetry, there are no accounts, and there is no license check.

## Importing from a documentation URL fetches more than one page

If `--spec` points at an HTML page rather than a spec, pikopod climbs a ladder
(`internal/docimport`). You should know what that does before pointing it at an
internal documentation host:

1. **Rungs 1–2 — linked documents.** It follows `href`/`src` links from the page
   to anything that looks machine-readable. **There is deliberately no same-host
   restriction**, because documentation sites legitimately host their OpenAPI
   document on a different domain. That means a page you do not control can
   direct the fetcher at a host of its choosing. The risk is bounded by the size
   and content-type gates, and by the fact that anything fetched must parse as a
   spec to be used — but if your threat model includes SSRF from an untrusted
   docs page, do not point `--spec` at one. Fetch the spec yourself and pass a
   local path.
2. **Rung 3 — embedded documents.** The page itself, plus at most one hop into
   the site's own reference index. Same-host.
3. **Rung 4 — model extraction.** Only with your key. Assembles up to 240 KB of
   the site's prose (preferring its `llms.txt` index) and asks your model to
   write an OpenAPI document from it. The result is marked `LLM_EXTRACTED` and
   imported as a draft.

Rungs 1–3 are deterministic. Rung 4 is not, which is why its output carries
provenance that follows it through every later decision.

## Third-party specs are untrusted input

A fetched specification is bytes from someone else's server. Parsing is bounded
on size, depth, and reference expansion, runs panic-isolated, and a document
that fails to normalize is reported as an error — it never silently retires the
watcher.
