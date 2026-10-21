#!/usr/bin/env python3
"""Test-only stand-in for the Moyasar API, used by scripts/e2e_test.sh.

The API container reaches it through MOYASAR_API_URL (never set in
production; boot refuses it with a live key). It keeps payments in memory and
answers only what Recurso calls:

  POST /v1/payments                            token charge (renewal, dunning retry)
  GET  /v1/payments/{id}                       read-back (verify, webhook, refund)
  GET  /v1/payments?metadata[invoice_id]=&status=  already-paid lookup
  POST /v1/payments/{id}/refund {amount}       refund; returns the payment
  GET  /v1/tokens/{id}                         saved-card expiry

Token charges succeed unless the token id names a decline: "decline_hard"
fails with ISO 8583 code 54 (expired card, never retried), "decline_soft"
with 51 (insufficient funds). A reused given_id is refused with a 400, as
Moyasar does; the adapter then reads the payment back by that id.

Test control (no auth): PUT /_stub/payments/{id} with a Moyasar payment object
creates or replaces it.

Usage: moyasar_stub.py [--host 0.0.0.0] [--port 8091]
"""

import argparse
import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

PAYMENTS = {}
LOCK = threading.Lock()

# Token-name -> (ISO 8583 response code, source.message) for declined charges.
DECLINES = {
    "decline_hard": ("54", "Expired Card"),
    "decline_soft": ("51", "Insufficient Funds"),
}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):  # keep the e2e output readable
        pass

    def reply(self, status, body):
        raw = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def not_found(self):
        self.reply(404, {"type": "invalid_request_error", "message": "Object not found"})

    def body(self):
        n = int(self.headers.get("Content-Length") or 0)
        return json.loads(self.rfile.read(n) or b"{}")

    def authorized(self):
        # Moyasar authenticates with HTTP Basic, secret key as the username.
        if not self.headers.get("Authorization", "").startswith("Basic "):
            self.reply(401, {"type": "authentication_error", "message": "Invalid authorization credentials"})
            return False
        return True

    def do_GET(self):
        url = urlparse(self.path)
        parts = url.path.strip("/").split("/")
        if not self.authorized():
            return
        if parts == ["v1", "payments"]:
            q = parse_qs(url.query)
            invoice = q.get("metadata[invoice_id]", [None])[0]
            status = q.get("status", [None])[0]
            with LOCK:
                found = [
                    p for p in PAYMENTS.values()
                    if (invoice is None or p.get("metadata", {}).get("invoice_id") == invoice)
                    and (status is None or p.get("status") == status)
                ]
            return self.reply(200, {"payments": found, "meta": {"total_count": len(found)}})
        if len(parts) == 3 and parts[:2] == ["v1", "payments"]:
            with LOCK:
                p = PAYMENTS.get(parts[2])
            return self.reply(200, p) if p else self.not_found()
        if len(parts) == 3 and parts[:2] == ["v1", "tokens"]:
            return self.reply(200, {"id": parts[2], "status": "active", "month": 12, "year": 2030})
        self.not_found()

    def do_POST(self):
        parts = urlparse(self.path).path.strip("/").split("/")
        if not self.authorized():
            return
        if parts == ["v1", "payments"]:
            return self.charge_token(self.body())
        if len(parts) == 4 and parts[:2] == ["v1", "payments"] and parts[3] == "refund":
            amount = int(self.body().get("amount", 0))
            with LOCK:
                p = PAYMENTS.get(parts[2])
                if p is None:
                    return self.not_found()
                if amount <= 0 or p.get("refunded", 0) + amount > p["amount"]:
                    return self.reply(400, {"type": "invalid_request_error", "message": "Invalid refund amount"})
                p["refunded"] = p.get("refunded", 0) + amount
                if p["refunded"] == p["amount"]:
                    p["status"] = "refunded"
                out = dict(p)
            return self.reply(200, out)
        self.not_found()

    def charge_token(self, req):
        source = req.get("source") or {}
        missing = [f for f in ("given_id", "amount", "currency", "callback_url") if not req.get(f)]
        if source.get("type") != "token" or not source.get("token"):
            missing.append("source")
        if missing:
            return self.reply(400, {"type": "invalid_request_error", "message": "Validation Failed",
                                    "errors": {f: ["can't be blank"] for f in missing}})
        token = source["token"]
        code, message = next(((c, m) for name, (c, m) in DECLINES.items() if name in token), (None, "APPROVED"))
        p = {
            "id": req["given_id"],
            "status": "failed" if code else "paid",
            "amount": int(req["amount"]),
            "refunded": 0,
            "currency": req["currency"],
            "description": req.get("description", ""),
            "callback_url": req["callback_url"],
            "metadata": req.get("metadata") or {},
            "source": {"type": "creditcard", "company": "visa", "number": "4111-11XX-XXXX-1111",
                       "token": token, "message": message, "response_code": code or "00"},
        }
        with LOCK:
            if p["id"] in PAYMENTS:
                return self.reply(400, {"type": "invalid_request_error", "message": "Validation Failed",
                                        "errors": {"given_id": ["has already been taken"]}})
            PAYMENTS[p["id"]] = p
        self.reply(201, p)

    def do_PUT(self):
        parts = urlparse(self.path).path.strip("/").split("/")
        if len(parts) == 3 and parts[:2] == ["_stub", "payments"]:
            p = self.body()
            p["id"] = parts[2]
            p.setdefault("refunded", 0)
            with LOCK:
                PAYMENTS[parts[2]] = p
            return self.reply(200, p)
        self.not_found()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", default="0.0.0.0")
    ap.add_argument("--port", type=int, default=8091)
    args = ap.parse_args()
    ThreadingHTTPServer((args.host, args.port), Handler).serve_forever()


if __name__ == "__main__":
    main()
