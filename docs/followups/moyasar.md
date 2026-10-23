# Follow-ups: Moyasar gateway

Back to the [index](README.md).

The gateway is env-configured and still marked experimental
(`cmd/api/main.go:305`). Design and runbook: [../spec_moyasar.md](../spec_moyasar.md).
Refund follow-ups are in [dunning-refunds.md](dunning-refunds.md).

## Open

### MOY-1: Sandbox verification with real keys (founder-gated)

- **Status:** blocked (needs the founder's Moyasar test account)
- **Severity:** high
- **What's missing:** the gateway has only been tested against a stub and the
  real Moyasar form. Nothing has run against a live Moyasar sandbox. In
  particular, no Moyasar page confirms a saved **Mada** token can be charged
  off-session.
- **Evidence:** `docs/spec_moyasar.md:293-303` (checklist);
  `cmd/api/main.go:305` (EXPERIMENTAL log line).
- **Consequence:** renewals on Mada cards may fail or demand 3DS (`failed` or
  `initiated`; the invoice falls to dunning, nothing double-charges). Until
  verified, auto-renewal on Moyasar is unproven.
- **Proposed fix:** run the spec checklist: Tokenization enabled, webhook
  registered; pay a renewing invoice with Visa `4111…1111` and Mada
  `4201…1010`; force a renewal on both; decline cards `…0508` (stops) and
  `…1101` (retries); webhook redelivery, a credit-note refund, Arabic
  checkout. Then drop the EXPERIMENTAL line.
- **Source:** `docs/spec_moyasar.md` "Founder-gated"; fork PR #2 "Before
  dropping experimental".

### MOY-2: A stuck or voided old-card attempt blocks a replayed charge forever

- **Status:** open
- **Severity:** medium
- **What's wrong:** on an idempotency-key replay, the gateway re-keys onto the
  current card only when the old attempt is `failed`. Any other non-paid
  status (`initiated`, `authorized`, `voided`) returns "still <status>" as an
  error, i.e. no answer.
- **Evidence:** `internal/adapter/gateway/moyasar.go:331-337` (the
  `!= "paid" && != "failed"` case); `internal/service/wallet.go:545-549`
  (an error is "no answer": keep the key, back off, replay); backoff caps at
  24h (`wallet.go:618-627`); no-answer failures never count toward the decline
  cap or send an email.
- **Consequence:** a wallet whose old-card attempt sits in `initiated` (e.g. a
  token charge that wanted 3DS and was abandoned) replays daily forever
  without charging the new card and without telling anyone. `voided` is
  terminal yet blocks the same way. Invoice renewals under the same key are
  affected the same way.
- **Proposed fix:** treat `voided` as terminal like `failed` (re-key). For
  `initiated` older than a bound (e.g. 1h, past Moyasar's 3DS window), treat
  it as abandoned and re-key, or stop after N replays and alert. Add a test
  for both.
- **Source:** R4b report (round-4 fix review).

### MOY-3: False "possible duplicate charge" warning when verify and webhook interleave

- **Status:** open
- **Severity:** low
- **What's wrong:** settlement marks the invoice paid, then claims the payment
  id. If settler A (checkout verify) transitions the invoice and settler B
  (webhook, same payment) runs `MarkInvoicePaid` and claims before A does, B
  sees `!transitioned && wasEmpty`.
- **Evidence:** `internal/service/gateway_settlement.go:316-318`.
- **Consequence:** a WARN asks the operator to review a legitimate single
  payment for refund. The browser return and the webhook commonly arrive
  together. The card is still saved (A's claim succeeds).
- **Proposed fix:** claim before `MarkInvoicePaid`, or don't warn when the
  invoice was paid by a gateway path and the claim recorded this payment.
- **Source:** `review4.md` L1.

### MOY-4: A buyer who paid a just-voided invoice isn't told they were charged

- **Status:** open
- **Severity:** low
- **What's wrong:** buyer pays on Moyasar, invoice is voided before the return,
  verify answers 409, the webhook logs "review for refund". The page then shows
  the generic void screen.
- **Evidence:** `frontend/src/pages/Checkout.jsx:378-386`
  (`notPayableTitle`/`notPayableBody` regardless of a gateway return).
- **Consequence:** the customer sees "cancelled, contact the business" with no
  mention of the charge: support tickets and chargebacks.
- **Proposed fix:** when a gateway return (`id`/`status` params) is present on
  a void invoice, show "your payment was received; the invoice was cancelled;
  a refund will follow", or have verify return a distinct status.
- **Source:** `review4.md` L3.

### MOY-5: Moyasar charges are missing from the payments log

- **Status:** open
- **Severity:** medium
- **What's wrong:** the only `PaymentAttempt` writer is the Stripe webhook.
  Moyasar settlement only claims `gateway_payment_id`, and the invoice page
  doesn't show that id.
- **Evidence:** `internal/adapter/handler/webhook_stripe.go:234` (sole
  `paymentAttempts.Create`); `internal/service/gateway_settlement.go:286-320`;
  `frontend/src/pages/InvoicePage.jsx:119-126` (attempts panel, no
  `gateway_payment_id`).
- **Consequence:** Payments and the invoice's "Payment attempts" panel stay
  empty for Moyasar; the operator can't cross-reference the Moyasar dashboard.
- **Proposed fix:** show `gateway_payment_id` on InvoicePage now; write an
  attempt row from settlement and from saved-card charges (success and
  decline) through the gateway-agnostic settlement service.
- **Source:** `docs/spec_moyasar.md:284`; `review_sar_ux.md` #5; fork PR #2
  section D.

### MOY-6: BYO Moyasar isn't connectable; no "Moyasar configured" status

- **Status:** open (decision pending: is BYO Moyasar wanted?)
- **Severity:** low
- **What's missing:** Moyasar is env-only, so every tenant collects into the
  platform's account. The gateways page doesn't show that Moyasar is
  configured at all.
- **Evidence:** `internal/core/domain/gateway_connection.go:20-31`
  (`ValidGatewayProvider` excludes moyasar); `frontend/src/components/PaymentGateways.jsx`
  (no moyasar entry); `internal/adapter/gateway/resolver.go:174-178`
  (`platformOnlyGateways`).
- **Consequence:** a merchant can't take SAR into their own Moyasar account;
  an operator can't see from the UI that Moyasar is live or in test mode.
- **Proposed fix:** first, a read-only "Moyasar configured (test/live)" card
  on the gateways page from `/version`'s gateway mode. Later, a `moyasar`
  connection type through the vault (secret + publishable key + webhook
  secret) on the same path as Stripe/Razorpay.
- **Source:** `docs/spec_moyasar.md:279`; fork PR #2 section D.

### MOY-7: Apple Pay

- **Status:** open
- **Severity:** low
- **What's missing:** the form offers cards only.
- **Evidence:** `frontend/src/components/checkout/MoyasarForm.jsx:17`
  (`DEFAULT_METHODS = ["creditcard"]`).
- **Consequence:** no Apple Pay for Saudi buyers, where it is common.
- **Proposed fix:** merchant domain validation with Moyasar, then add
  `applepay` to methods; check Apple Pay token reuse for renewals.
- **Source:** `docs/spec_moyasar.md:280`.

### MOY-8: Card update is a stopgap (pay an invoice with the new card)

- **Status:** open
- **Severity:** low
- **What's missing:** a real card-update flow (Moyasar token creation with
  3DS). Today the portal tells the customer to pay their open renewal invoice
  with the new card.
- **Evidence:** `frontend/src/pages/portal/cardUpdate.js:1-13`
  (`CARD_UPDATE_VIA_INVOICE`, `card_update_supported === false`).
- **Consequence:** with no open invoice the customer can't change their card
  before the next renewal fails.
- **Proposed fix:** a portal page that creates a Moyasar token with 3DS
  verification and saves it as the default method.
- **Source:** `docs/spec_moyasar.md:281`.

### MOY-9: No Content-Security-Policy, checkout included

- **Status:** open
- **Severity:** medium
- **What's missing:** nginx sets no CSP; the checkout loads the Moyasar form
  from jsDelivr and talks to Moyasar.
- **Evidence:** `frontend/nginx.conf:18-20` (CSP "deliberately not set yet").
- **Consequence:** a payment page with no CSP; an injected script would run
  unrestricted.
- **Proposed fix:** a CSP for the SPA, at least on `/checkout`, allowing
  `cdn.jsdelivr.net`, `*.moyasar.com` (script, frame, connect), Stripe and
  Razorpay origins as configured; roll out as `Report-Only` first.
- **Source:** `docs/spec_moyasar.md:283`.

### MOY-10: Settlement re-saves the card on every settled token payment

- **Status:** open
- **Severity:** low
- **What's wrong:** `saveCard` runs whenever this payment is the settling one,
  including a re-claim of the same id on a webhook redelivery and renewals
  paid with the saved token. Nothing checks that the source was a new card.
- **Evidence:** `internal/service/gateway_settlement.go:272-273` (save on a
  settling payment), `:323-343` (`FetchToken`, then `SetGatewayPaymentMethod`).
- **Consequence:** an extra Moyasar call per renewal webhook. A late
  redelivery for an old payment can overwrite a newer card saved meanwhile.
- **Proposed fix:** skip when the payment source is `token`, or when its token
  equals the stored method ref.
- **Source:** `review2_money_ops.md` B17.

### MOY-11: Webhook log noise when unconfigured; no secret rotation

- **Status:** open
- **Severity:** low
- **What's wrong:** an unconfigured Moyasar webhook logs at Error on every hit;
  the secret compare accepts exactly one secret.
- **Evidence:** `internal/adapter/handler/webhook_moyasar.go:62`, `:73-74`.
- **Consequence:** scanner traffic raises Error logs; rotating the secret
  gives 401s (and Moyasar retries) until the env change is redeployed.
- **Proposed fix:** 404 or Info-level when unconfigured; accept
  `MOYASAR_WEBHOOK_SECRET=new,old`.
- **Source:** `review_ops.md` L1/L2; `review2_money_ops.md` B15.

### MOY-12: Anyone with the checkout link can rate-limit the real buyer out

- **Status:** open
- **Severity:** low
- **What's wrong:** `/pay` (10/min) and verify have per-invoice buckets shared
  by all callers.
- **Evidence:** `cmd/api/routes_public.go:168-169` (`payPerInvoice`),
  `:177-179` (`verifyPerInvoice`).
- **Consequence:** junk requests get the buyer a 429 on Pay or on verify after
  3DS. The webhook still settles, so it is confusing, not lost money.
- **Proposed fix:** count only requests that reach the gateway, or key on IP +
  invoice with a higher invoice-wide ceiling.
- **Source:** `review3_money_security.md` L3 (partly addressed in `42664906`).

### MOY-13: Returning from 3DS, a failed invoice fetch shows "Invoice not found"

- **Status:** open
- **Severity:** low
- **What's wrong:** the not-found screen renders on any fetch error, even with
  a gateway return in the URL.
- **Evidence:** `frontend/src/pages/Checkout.jsx:366` (`if (error && !invoice)`).
- **Consequence:** a buyer who just paid is told the invoice doesn't exist.
- **Proposed fix:** with a gateway return present, skip the not-found screen
  and run the verify flow (with a retry).
- **Source:** `review2_frontend.md` m11.

## Done

- **Unknown-refund allocation booked the wrong note; double resolution**
  (`review4.md` M1, M2): `712f8754`.
- **BYO-Stripe tenants' Moyasar refunds routed to their Stripe**
  (`review4.md` M3): `fdbac8d5` (refund by payment origin,
  `smart_router.go:219`).
- **Wallet decline cap per saved method** (`review4.md` M4): `550ea5dc`.
- **Verify/pay limiters keyed on the canonical id, per-invoice verify cap**
  (`review4.md` M5): `42664906`.
- **Stripe restricted test keys classed as test** (`review4.md` L2): `c6bb2bbb`.
- **Re-key only after the old attempt failed** (`review4.md` L4): `a9f210e9`
  (its remaining gap is MOY-2).
