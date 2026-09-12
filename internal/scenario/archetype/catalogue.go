// The shipped archetype catalogue — data, not code. The raw JSON keeps the
// expander's output structurally identical to the expansion goldens.
package archetype

import "encoding/json"

const catalogueJSON = `[
  {
    "id": "happy_path",
    "archetypeVersion": 1,
    "title": "Happy path",
    "description": "A documented collection read returns a success response.",
    "expects": ["SANDBOX"],
    "requires": [{ "role": "op", "bind": "operation", "match": { "crud": "LIST", "hasSuccessResponse": true } }],
    "requiresFidelity": "L1",
    "expands": [
      {
        "key": "call", "type": "REQUEST",
        "config": { "method": "<<op.method>>", "path": "<<op.collectionPath>>" },
        "assertions": [
          { "target": "response.status", "op": "gte", "expected": 200 },
          { "target": "response.status", "op": "lt", "expected": 300 }
        ]
      }
    ]
  },
  {
    "id": "unauthorized",
    "archetypeVersion": 1,
    "title": "Unauthorized",
    "description": "A secured operation rejects a request with no credential.",
    "expects": ["SANDBOX"],
    "requires": [{ "role": "op", "bind": "operation", "match": { "requiresAuth": true } }],
    "requiresFidelity": "L1",
    "expands": [
      {
        "key": "unauth", "type": "REQUEST",
        "config": { "method": "<<op.method>>", "path": "<<op.collectionPath>>" },
        "assertions": [{ "target": "response.status", "op": "equals", "expected": 401 }]
      }
    ]
  },
  {
    "id": "invalid_request",
    "archetypeVersion": 1,
    "title": "Invalid request",
    "description": "A create with a missing required field is rejected with a 4xx.",
    "expects": ["SANDBOX"],
    "requires": [{ "role": "op", "bind": "operation", "match": { "crud": "CREATE", "hasErrorResponseClass": "4XX" } }],
    "requiresFidelity": "L1",
    "expands": [
      {
        "key": "invalid", "type": "REQUEST",
        "config": { "method": "<<op.method>>", "path": "<<op.collectionPath>>", "body": {} },
        "assertions": [
          { "target": "response.status", "op": "gte", "expected": 400 },
          { "target": "response.status", "op": "lt", "expected": 500 }
        ]
      }
    ]
  },
  {
    "id": "duplicate_delivery",
    "archetypeVersion": 1,
    "title": "Duplicate delivery",
    "description": "A webhook delivered twice — does the client dedupe?",
    "expects": ["CLIENT"],
    "requires": [
      { "role": "createOp", "bind": "operation", "match": { "crud": "CREATE", "hasSuccessResponse": true } },
      { "role": "emittedEvent", "bind": "webhookEvent", "match": {} }
    ],
    "requiresFidelity": "L1",
    "expands": [
      {
        "key": "create", "type": "REQUEST",
        "config": { "method": "<<createOp.method>>", "path": "<<createOp.collectionPath>>", "body": {} },
        "capture": { "resourceId": "response.body$.id" },
        "assertions": [{ "target": "response.status", "op": "gte", "expected": 200 }]
      },
      { "key": "await1", "type": "EXPECT_WEBHOOK", "config": { "match": { "eventType": "<<emittedEvent>>" }, "timeoutMs": 30000 } },
      { "key": "duplicate", "type": "INJECT_FAULT", "config": { "kind": "duplicate_webhook", "target": "<<emittedEvent>>" } },
      {
        "key": "await2", "type": "EXPECT_WEBHOOK", "config": { "match": { "eventType": "<<emittedEvent>>" }, "timeoutMs": 30000 },
        "assertions": [{ "subject": "CLIENT", "target": "webhook.count", "match": { "eventType": "<<emittedEvent>>" }, "op": "countEquals", "expected": 2 }]
      }
    ]
  },
  {
    "id": "rate_limit_backoff",
    "archetypeVersion": 1,
    "title": "Rate limit and backoff",
    "description": "Under rate limiting the sandbox emits 429; the client must back off.",
    "expects": ["SANDBOX", "CLIENT"],
    "requires": [{ "role": "op", "bind": "operation", "match": { "hasSuccessResponse": true } }],
    "requiresFidelity": "L1",
    "expands": [
      { "key": "arm", "type": "INJECT_FAULT", "config": { "method": "<<op.method>>", "path": "<<op.collectionPath>>", "kind": "rate_limit" } },
      {
        "key": "call", "type": "REQUEST",
        "config": { "method": "<<op.method>>", "path": "<<op.collectionPath>>" },
        "assertions": [{ "subject": "SANDBOX", "target": "response.status", "op": "equals", "expected": 429 }]
      }
    ]
  },
  {
    "id": "state_transition_sequence",
    "archetypeVersion": 1,
    "title": "State transition sequence",
    "description": "A resource is created then transitioned; the state must follow.",
    "expects": ["SANDBOX"],
    "requires": [
      { "role": "createOp", "bind": "operation", "match": { "crud": "CREATE", "hasSuccessResponse": true } },
      { "role": "updateOp", "bind": "operation", "match": { "crud": "UPDATE", "sameResourceAs": "createOp", "hasEnumField": true } }
    ],
    "requiresFidelity": "L2",
    "expands": [
      {
        "key": "create", "type": "REQUEST",
        "config": { "method": "<<createOp.method>>", "path": "<<createOp.collectionPath>>", "body": {} },
        "capture": { "rid": "response.body$.id" },
        "assertions": [{ "target": "response.status", "op": "gte", "expected": 200 }]
      },
      {
        "key": "transition", "type": "REQUEST",
        "config": { "method": "<<updateOp.method>>", "path": "<<updateOp.collectionPath>>/{{rid}}", "body": { "status": "active" } },
        "assertions": [{ "target": "response.status", "op": "gte", "expected": 200 }]
      },
      {
        "key": "verify", "type": "ASSERT_STATE",
        "config": { "resourceType": "<<updateOp.collectionPath>>", "resourceId": "{{rid}}" },
        "assertions": [{ "target": "state.resource", "path": "$.status", "op": "equals", "expected": "active" }]
      }
    ]
  }
]`

// Catalogue is the shipped archetype catalogue, parsed at load (panics on a
// malformed literal — a build-time bug, not a runtime condition).
var Catalogue = func() []Archetype {
	var out []Archetype
	if err := json.Unmarshal([]byte(catalogueJSON), &out); err != nil {
		panic("archetype catalogue literal is invalid: " + err.Error())
	}
	return out
}()

// Find returns the archetype with the given id, or nil.
func Find(id string) *Archetype {
	for i := range Catalogue {
		if Catalogue[i].ID == id {
			return &Catalogue[i]
		}
	}
	return nil
}
