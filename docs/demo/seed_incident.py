"""Seed one recorded incident into ./pikopod-data so the recording can show
`pikopod incidents` and `pikopod scenario reproduce` without a live provider.
The event and the recording are the redacted shapes the agent writes."""
import json
import os

data = "pikopod-data"
os.makedirs(os.path.join(data, "recordings"), exist_ok=True)

event = {
    "schema_version": "2",
    "fingerprint": "fp_14835fa32dfb",
    "upstream": "examplepay",
    "method": "POST",
    "endpoint": "/charges",
    "status_class": "5xx",
    "kind": "upstream_error",
    "after": "503",
    "first_seen": "2026-09-18T10:00:00Z",
    "last_seen": "2026-09-19T10:00:00Z",
    "occurrences": 4,
    "level": "ERR",
}
record = {
    "ts": "2026-09-19T10:00:00Z",
    "upstream": "examplepay",
    "method": "POST",
    "path": "/charges",
    "status": 503,
    "dur_ms": 84,
    "req_kind": "json",
    "resp_kind": "json",
    "req_body": {"amount": 1250, "currency": "NGN"},
    "resp_body": {"message": "upstream down"},
    "redactions": 0,
}
with open(os.path.join(data, "events.ndjson"), "a") as f:
    f.write(json.dumps(event) + "\n")
with open(os.path.join(data, "recordings", "examplepay.ndjson"), "a") as f:
    f.write(json.dumps(record) + "\n")
print("seeded incident fp_14835fa32dfb")
