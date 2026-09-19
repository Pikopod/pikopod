// Pikopod extension archetypes (declines, timeouts, partial failure, downtime
// recovery). Provider-agnostic; outside catalogue.go because that is golden-gated.
package archetype

import "encoding/json"

const extensionsJSON = `[
  {
    "id": "retry_storm",
    "archetypeVersion": 1,
    "title": "Retry storm with recovery",
    "description": "The provider fails the first two attempts under one idempotency key, then recovers on the third — the shape client retry logic must survive.",
    "expects": ["SANDBOX", "CLIENT"],
    "requires": [{ "role": "createOp", "bind": "operation", "match": { "crud": "CREATE", "hasSuccessResponse": true } }],
    "requiresFidelity": "L1",
    "expands": [
      {
        "key": "arm", "type": "INJECT_FAULT",
        "config": { "method": "<<createOp.method>>", "path": "<<createOp.collectionPath>>", "kind": "error", "status": 503, "times": 2, "per": "idempotency-key" }
      },
      {
        "key": "attempt1", "type": "REQUEST",
        "config": { "method": "<<createOp.method>>", "path": "<<createOp.collectionPath>>", "body": {}, "headers": { "Idempotency-Key": "retry-storm-1" } },
        "assertions": [{ "subject": "SANDBOX", "target": "response.status", "op": "equals", "expected": 503 }]
      },
      {
        "key": "attempt2", "type": "REQUEST",
        "config": { "method": "<<createOp.method>>", "path": "<<createOp.collectionPath>>", "body": {}, "headers": { "Idempotency-Key": "retry-storm-1" } },
        "assertions": [{ "subject": "SANDBOX", "target": "response.status", "op": "equals", "expected": 503 }]
      },
      {
        "key": "attempt3", "type": "REQUEST",
        "config": { "method": "<<createOp.method>>", "path": "<<createOp.collectionPath>>", "body": {}, "headers": { "Idempotency-Key": "retry-storm-1" } },
        "assertions": [{ "subject": "SANDBOX", "target": "response.status", "op": "gte", "expected": 200 }, { "subject": "SANDBOX", "target": "response.status", "op": "lt", "expected": 300 }]
      }
    ]
  },
  {
    "id": "declines",
    "archetypeVersion": 1,
    "title": "Declines",
    "description": "A create that succeeded starts declining with a 400; after the condition clears, service recovers.",
    "expects": ["SANDBOX"],
    "requires": [{ "role": "op", "bind": "operation", "match": { "crud": "CREATE", "hasSuccessResponse": true } }],
    "requiresFidelity": "L1",
    "expands": [
      {
        "key": "arm-decline", "type": "INJECT_FAULT",
        "config": { "method": "<<op.method>>", "path": "<<op.collectionPath>>", "kind": "error", "status": 400 }
      },
      {
        "key": "declined", "type": "REQUEST",
        "config": { "method": "<<op.method>>", "path": "<<op.collectionPath>>", "body": {} },
        "assertions": [
          { "target": "response.status", "op": "equals", "expected": 400 },
          { "target": "execution.faultApplied", "op": "equals", "expected": true }
        ]
      },
      {
        "key": "clear", "type": "CLEAR_FAULT",
        "config": { "method": "<<op.method>>", "path": "<<op.collectionPath>>" }
      },
      {
        "key": "recovered", "type": "REQUEST",
        "config": { "method": "<<op.method>>", "path": "<<op.collectionPath>>", "body": {} },
        "assertions": [
          { "target": "execution.faultApplied", "op": "absent" },
          { "target": "response.status", "op": "lt", "expected": 500 }
        ]
      }
    ]
  },
  {
    "id": "timeouts",
    "archetypeVersion": 1,
    "title": "Timeouts",
    "description": "A read suffers 30 seconds of provider latency (virtualized) but still succeeds; your timeout budget is the assertion.",
    "expects": ["SANDBOX"],
    "requires": [{ "role": "op", "bind": "operation", "match": { "crud": "LIST", "hasSuccessResponse": true } }],
    "requiresFidelity": "L1",
    "expands": [
      {
        "key": "arm-latency", "type": "INJECT_FAULT",
        "config": { "method": "<<op.method>>", "path": "<<op.collectionPath>>", "kind": "latency", "delayMs": 30000 }
      },
      {
        "key": "slow-read", "type": "REQUEST",
        "config": { "method": "<<op.method>>", "path": "<<op.collectionPath>>" },
        "assertions": [
          { "target": "response.latencyMs", "op": "gte", "expected": 30000 },
          { "target": "response.status", "op": "gte", "expected": 200 },
          { "target": "response.status", "op": "lt", "expected": 300 }
        ]
      },
      {
        "key": "clear", "type": "CLEAR_FAULT",
        "config": { "method": "<<op.method>>", "path": "<<op.collectionPath>>" }
      },
      {
        "key": "fast-again", "type": "REQUEST",
        "config": { "method": "<<op.method>>", "path": "<<op.collectionPath>>" },
        "assertions": [
          { "target": "execution.faultApplied", "op": "absent" },
          { "target": "response.status", "op": "gte", "expected": 200 },
          { "target": "response.status", "op": "lt", "expected": 300 }
        ]
      }
    ]
  },
  {
    "id": "partial_failure",
    "archetypeVersion": 1,
    "title": "Partial failure",
    "description": "Reads fail mid-sequence with 503, but state written before the outage stays consistent and reads recover.",
    "expects": ["SANDBOX"],
    "requires": [{ "role": "op", "bind": "operation", "match": { "crud": "LIST", "hasSuccessResponse": true } }],
    "requiresFidelity": "L2",
    "expands": [
      {
        "key": "seed", "type": "SEED_STATE",
        "config": { "resources": [{ "type": "<<op.collectionPath>>", "resourceKey": "partial_failure_probe", "attributes": { "pikopod_probe": "intact" } }] }
      },
      {
        "key": "arm-outage", "type": "INJECT_FAULT",
        "config": { "method": "<<op.method>>", "path": "<<op.collectionPath>>", "kind": "error", "status": 503 }
      },
      {
        "key": "read-fails", "type": "REQUEST",
        "config": { "method": "<<op.method>>", "path": "<<op.collectionPath>>" },
        "assertions": [
          { "target": "response.status", "op": "equals", "expected": 503 },
          { "target": "execution.faultApplied", "op": "equals", "expected": true }
        ]
      },
      {
        "key": "state-intact", "type": "ASSERT_STATE",
        "config": { "resourceType": "<<op.collectionPath>>", "resourceId": "partial_failure_probe" },
        "assertions": [{ "target": "state.resource", "path": "$.pikopod_probe", "op": "equals", "expected": "intact" }]
      },
      {
        "key": "clear", "type": "CLEAR_FAULT",
        "config": { "method": "<<op.method>>", "path": "<<op.collectionPath>>" }
      },
      {
        "key": "read-recovers", "type": "REQUEST",
        "config": { "method": "<<op.method>>", "path": "<<op.collectionPath>>" },
        "assertions": [
          { "target": "response.status", "op": "gte", "expected": 200 },
          { "target": "response.status", "op": "lt", "expected": 300 }
        ]
      }
    ]
  },
  {
    "id": "downtime_recovery",
    "archetypeVersion": 1,
    "title": "Downtime and recovery",
    "description": "The provider goes fully down with 503s, stays down five virtual minutes, then recovers.",
    "expects": ["SANDBOX"],
    "requires": [{ "role": "op", "bind": "operation", "match": { "crud": "LIST", "hasSuccessResponse": true } }],
    "requiresFidelity": "L1",
    "expands": [
      {
        "key": "arm-outage", "type": "INJECT_FAULT",
        "config": { "method": "<<op.method>>", "path": "<<op.collectionPath>>", "kind": "error", "status": 503 }
      },
      {
        "key": "down", "type": "REQUEST",
        "config": { "method": "<<op.method>>", "path": "<<op.collectionPath>>" },
        "assertions": [
          { "target": "response.status", "op": "equals", "expected": 503 },
          { "target": "execution.faultApplied", "op": "equals", "expected": true }
        ]
      },
      {
        "key": "still-down", "type": "REQUEST",
        "config": { "method": "<<op.method>>", "path": "<<op.collectionPath>>" },
        "assertions": [
          { "target": "response.status", "op": "equals", "expected": 503 },
          { "target": "execution.faultApplied", "op": "equals", "expected": true }
        ]
      },
      {
        "key": "outage-window", "type": "WAIT",
        "config": { "durationMs": 300000 }
      },
      {
        "key": "clear", "type": "CLEAR_FAULT",
        "config": { "method": "<<op.method>>", "path": "<<op.collectionPath>>" }
      },
      {
        "key": "recovered", "type": "REQUEST",
        "config": { "method": "<<op.method>>", "path": "<<op.collectionPath>>" },
        "assertions": [
          { "target": "response.status", "op": "gte", "expected": 200 },
          { "target": "response.status", "op": "lt", "expected": 300 }
        ]
      },
      {
        "key": "outage-shape", "type": "VERIFY_SEQUENCE",
        "config": { "requests": [
          { "method": "<<op.method>>", "path": "<<op.collectionPath>>" },
          { "method": "<<op.method>>", "path": "<<op.collectionPath>>" },
          { "method": "<<op.method>>", "path": "<<op.collectionPath>>", "minGapMs": 300000 }
        ] }
      }
    ]
  }
]`

// Extensions is the pikopod-native archetype set, parsed at load like the
// catalogue.
var Extensions = func() []Archetype {
	var out []Archetype
	if err := json.Unmarshal([]byte(extensionsJSON), &out); err != nil {
		panic("archetype extensions literal is invalid: " + err.Error())
	}
	return out
}()

// All returns the catalogue plus the pikopod extensions — what the CLI and
// the NL inventory offer. Parity tests pin Catalogue alone.
func All() []Archetype {
	out := make([]Archetype, 0, len(Catalogue)+len(Extensions))
	out = append(out, Catalogue...)
	out = append(out, Extensions...)
	return out
}
