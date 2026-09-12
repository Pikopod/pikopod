#!/usr/bin/env python3
"""Generate the SYNTHETIC importer fixtures.

These replace two fixtures pikopod had no license to redistribute (a
provider's proprietary Postman collection; a private Swagger 2.0 spec).
Everything here is invented, deterministic, and Apache-2.0 like the repo.
Each synthetic fixture preserves the STRUCTURAL properties the tests
exercise, without any third party's content:

synthetic-payments.postman.json (Postman collection v2.0):
  - >50 requests across nested folders (the real-world scale gate)
  - collection-level bearer auth (the auth-carrying gate)
  - documented 4xx response examples (the error-survival gate)
  - an HTML description whose "<body>" lands in the first 512 bytes —
    regression for the Detect misclassification this shape once caused
  - {{variable}} hosts, :param path segments, malformed example bodies,
    and duplicate (method, path) documentation entries

synthetic-sms.swagger2.json (Swagger 2.0):
  - basic-auth securityDefinitions (the enforceable http-basic scheme)
  - host/basePath/schemes -> servers conversion material
  - GET+POST on the same path, bulk variants, $ref'd definitions

Regenerate: python3 tools/gen-synthetic-fixtures.py
Changing a fixture invalidates its committed parity golden under
testdata/parity/importer/; goldens are maintainer-regenerated, so open an
issue rather than hand-editing one.
"""
import json
import os

OUT = os.path.join(os.path.dirname(__file__), "..", "testdata", "parity", "importer", "specs")

# ---------------------------------------------------------------- postman

DESCRIPTION_HTML = (
    "<html><head></head><body><p>Synthetic Payments API — an INVENTED "
    "fixture for pikopod's importer tests (Apache-2.0, no real provider). "
    "The html body tag in this description is deliberate: it reproduces the "
    "shape that once made format detection misread a JSON collection as an "
    "HTML page.</p></body></html>"
)

RESOURCES = [
    ("charges", ["create", "get", "list", "capture", "refund"]),
    ("transfers", ["create", "get", "list", "cancel"]),
    ("virtual-accounts", ["create", "get", "list", "credit"]),
    ("payouts", ["create", "get", "list"]),
    ("customers", ["create", "get", "list", "update"]),
    ("balances", ["get", "list"]),
    ("disputes", ["get", "list", "accept", "reject"]),
    ("payment-links", ["create", "get", "list"]),
    ("settlements", ["get", "list"]),
    ("refunds", ["create", "get", "list"]),
    ("beneficiaries", ["create", "get", "list", "delete"]),
    ("webhook-endpoints", ["create", "get", "list", "delete"]),
    ("cards", ["create", "get", "list", "freeze", "unfreeze", "delete"]),
    ("exchange-rates", ["get", "list"]),
    ("kyc-checks", ["create", "get", "list"]),
    ("mandates", ["create", "get", "list", "cancel"]),
]

def request_for(resource, action, idx):
    base = "{{baseUrl}}/api/v1/" + resource
    body = None
    if action == "create":
        method, url = "POST", base
        body = json.dumps({"amount": 1000 + idx, "currency": "NGN", "reference": f"syn-{resource}-{idx}"})
    elif action == "update":
        method, url = "PATCH", base + "/:reference"
    elif action == "list":
        method, url = "GET", base + "?page=1&perPage=20"
    elif action == "get":
        method, url = "GET", base + "/:reference"
    elif action == "delete":
        method, url = "DELETE", base + "/:reference"
    else:  # action verbs: capture/refund/cancel/credit/accept/reject
        method, url = "POST", base + "/:reference/" + action
        body = json.dumps({"note": f"synthetic {action}"})

    ok_body = json.dumps({
        "status": True, "message": f"{resource} {action} ok",
        "data": {"id": f"syn_{resource}_{idx}", "reference": f"syn-{resource}-{idx}",
                 "amount": 1000 + idx, "currency": "NGN", "state": "pending"},
    })
    responses = [
        {"name": "success", "code": 200, "body": ok_body},
        {"name": "bad request", "code": 400,
         "body": json.dumps({"status": False, "message": "validation failed", "errors": [{"field": "amount"}]})},
        {"name": "unauthorized", "code": 401,
         "body": json.dumps({"status": False, "message": "invalid key"})},
    ]
    req = {"method": method, "url": url}
    if body is not None:
        req["body"] = {"mode": "raw", "raw": body}
    return {"name": f"{action} {resource}", "request": req, "response": responses}

def build_postman():
    items, idx = [], 0
    for resource, actions in RESOURCES:
        folder = {"name": resource.replace("-", " ").title(), "item": []}
        for action in actions:
            idx += 1
            folder["item"].append(request_for(resource, action, idx))
        items.append(folder)

    # Real-world warts the importer must tolerate, all invented:
    items.append({"name": "Edge cases", "item": [
        {"name": "malformed example", "request": {
            "method": "POST", "url": "{{baseUrl}}/api/v1/charges/:reference/void",
            "body": {"mode": "raw", "raw": "{not json"},
        }, "response": [{"name": "accepted", "code": 202, "body": "also { not json"}]},
        {"name": "duplicate doc of create charge", "request": {
            "method": "POST", "url": "{{baseUrl}}/api/v1/charges",
            "body": {"mode": "raw", "raw": json.dumps({"amount": 5})},
        }, "response": [{"name": "conflict variant", "code": 409,
                         "body": json.dumps({"status": False, "message": "duplicate reference"})}]},
    ]})

    return {
        "info": {
            "_postman_id": "00000000-0000-4000-8000-00000000c0de",
            "name": "Synthetic Payments API (test fixture)",
            "description": DESCRIPTION_HTML,
            "schema": "https://schema.getpostman.com/json/collection/v2.0.0/collection.json",
        },
        "auth": {"type": "bearer", "bearer": [{"key": "token", "value": "{{secretKey}}"}]},
        "item": items,
    }

# ---------------------------------------------------------------- swagger2

def build_swagger2():
    message = {"type": "object", "properties": {
        "to": {"type": "string"}, "from": {"type": "string"}, "text": {"type": "string"},
    }}
    ticket = {"type": "object", "properties": {
        "ticketId": {"type": "string"}, "status": {"type": "string"},
        "price": {"type": "number", "format": "double"}, "parts": {"type": "integer"},
    }}
    dlr = {"type": "object", "properties": {
        "ticketId": {"type": "string"},
        "state": {"type": "string", "enum": ["delivered", "failed", "buffered"]},
        "doneAt": {"type": "string", "format": "date-time"},
    }}
    account = {"type": "object", "properties": {
        "balance": {"type": "number", "format": "double"},
        "currency": {"type": "string"}, "callbackUrl": {"type": "string"},
    }}
    def op(opid, summary, ok_ref, body_ref=None):
        o = {"operationId": opid, "summary": summary, "produces": ["application/json"],
             "responses": {"200": {"description": "OK", "schema": {"$ref": f"#/definitions/{ok_ref}"}},
                           "400": {"description": "Bad Request"}},
             "security": [{"basic": []}]}
        if body_ref:
            o["consumes"] = ["application/json"]
            o["parameters"] = [{"name": "body", "in": "body", "required": True,
                                "schema": {"$ref": f"#/definitions/{body_ref}"}}]
        return o

    return {
        "swagger": "2.0",
        "info": {"title": "Synthetic SMS API (test fixture)",
                 "description": "Invented Swagger 2.0 fixture — no real provider. Apache-2.0.",
                 "version": "2.0", "license": {"name": "Apache-2.0"}},
        "host": "sms.synthetic.test", "basePath": "/rest", "schemes": ["https"],
        "securityDefinitions": {"basic": {"type": "basic"}},
        "paths": {
            "/account/info": {"get": op("accountInfo", "Account info", "Account")},
            "/account/update": {"post": op("accountUpdate", "Update account", "Account", "Account")},
            "/sms/submit": {
                "get": op("submitGet", "Submit via query", "Ticket"),
                "post": op("submitPost", "Submit a message", "Ticket", "Message"),
            },
            "/sms/submit/bulk": {"post": op("submitBulk", "Bulk submit", "Ticket", "Message")},
            "/sms/submit/long": {"post": op("submitLong", "Long message", "Ticket", "Message")},
            "/sms/dlr": {
                "get": op("dlrGet", "Poll delivery reports", "DeliveryReport"),
                "post": op("dlrPost", "Query delivery reports", "DeliveryReport", "Message"),
            },
            "/sms/ticket/{ticketId}": {"get": {
                **op("ticketStatus", "Ticket status", "Ticket"),
                "parameters": [{"name": "ticketId", "in": "path", "required": True, "type": "string"}],
            }},
        },
        "definitions": {"Message": message, "Ticket": ticket,
                        "DeliveryReport": dlr, "Account": account},
    }

def main():
    os.makedirs(OUT, exist_ok=True)
    with open(os.path.join(OUT, "synthetic-payments.postman.json"), "w") as f:
        json.dump(build_postman(), f, separators=(",", ":"))
        f.write("\n")
    with open(os.path.join(OUT, "synthetic-sms.swagger2.json"), "w") as f:
        json.dump(build_swagger2(), f, indent=1)
        f.write("\n")
    print("wrote synthetic-payments.postman.json, synthetic-sms.swagger2.json")

if __name__ == "__main__":
    main()
