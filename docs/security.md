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
- **Free text and unknown shapes** are dropped. The classifier fails closed on
  anything it cannot place.

Detection is generic — key names, value shape, entropy — never a list of
provider-specific prefixes, so it does not silently stop working when you add a
provider nobody anticipated.

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

## What leaves the machine

pikopod initiates no network traffic on its own. Everything outbound is
something you configured, using your own credentials:

| Destination | Why | Carries |
|---|---|---|
| Your upstream targets | The proxy forwarding your traffic | Your requests, unmodified |
| Your Slack webhook | Alerts | Finding structure and field paths |
| GitHub / GitLab | `pikopod pr` | The report you asked it to post |
| Your LLM provider | `scenario create`, `fix` | Only with your own key configured |
| A `spec_source` URL | The declared-drift watcher | Nothing — it fetches |

There is no telemetry, there are no accounts, and there is no license check.

## Third-party specs are untrusted input

A fetched specification is bytes from someone else's server. Parsing is bounded
on size, depth, and reference expansion, runs panic-isolated, and a document
that fails to normalize is reported as an error — it never silently retires the
watcher.
