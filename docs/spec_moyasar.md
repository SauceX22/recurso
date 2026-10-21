# Spec: Moyasar Payment Gateway (SAR, Saudi Arabia)

> **Status: EXPERIMENTAL until sandbox-verified (founder-gated: needs Moyasar
> test-mode credentials).** Same rollout as GoCardless/Adyen
> (`spec_integrations_payments.md`): a boot log line, no live or sandbox calls
> in CI.

## Objective

With `GATEWAY_CURRENCY_OVERRIDES="SAR=moyasar"`, SAR invoices are paid through
Moyasar (Mada, Visa, Mastercard): embedded form on the hosted checkout, webhook
settlement, off-session charges of the saved token (renewal, dunning, wallet),
and credit-note refunds. The mechanism is generic: a saved method records its
gateway (`customers.pm_provider`) and is charged only there, and dunning stops
on hard declines from any gateway that reports them.

## Moyasar API facts (verified 2026-10-21)

| Fact | Value | Source |
|---|---|---|
| Auth | `https://api.moyasar.com/v1`, HTTP Basic: secret key as username, empty password; `sk_test_`/`sk_live_` select the mode | [auth](https://docs.moyasar.com/docs/api/authentication.md) |
| Idempotency | body field `given_id` (UUID, becomes the payment `id`); replay returns the original, conflicting reuse is 400. Creation only: refunds have no key | [idempotency](https://docs.moyasar.com/docs/api/idempotency.md) |
| Declines | **HTTP 201**, `status:"failed"`, reason in `source.message`, ISO 8583 code in `source.response_code` (null before the acquirer) | [errors](https://docs.moyasar.com/docs/api/errors.md), [codes](https://docs.moyasar.com/docs/guides/references/gateway-response-codes.md) |
| Token charge | `POST /payments`, `source:{type:"token", token}`; `callback_url` required; active tokens skip 3DS by default; no MIT/recurring flag | [tokenized cards](https://docs.moyasar.com/docs/guides/tokenization/tokenized-cards.md), [create](https://docs.moyasar.com/api/payments/01-create-payment) |
| List, refund | `GET /payments?metadata[key]=…&status=…`; `POST /payments/{id}/refund {amount}` returns the payment with its `refunded` total | [list](https://docs.moyasar.com/api/payments/03-list-payments), [refund](https://docs.moyasar.com/api/payments/05-refund-payment) |
| Webhook auth | no HMAC/signature; the shared secret set at registration is echoed in the body as `secret_token`; 5 retries without a 2xx | [reference](https://docs.moyasar.com/docs/api/other/webhooks/webhook-reference.md), [create](https://docs.moyasar.com/docs/api/other/webhooks/create-webhook.md) |
| Form | `moyasar-payment-form` on jsDelivr (pinned 2.3.0, SRI); `credit_card:{save_card:true}` puts the token in `payment.source.token` (Tokenization must be enabled); returns to `callback_url?id=&status=&message=`, verify server-side | [basic](https://docs.moyasar.com/docs/guides/card-payments/basic-integration.md), [config](https://docs.moyasar.com/docs/guides/references/form-configuration/index.md) |
| Statuses | `initiated`, `paid`, `failed`, `authorized`, `captured`, `refunded`, `voided`, `verified` | [status](https://docs.moyasar.com/docs/api/payments/payment-status-reference.md) |

## Design

### Adapter (`internal/adapter/gateway/moyasar.go`)

| Port method | Moyasar |
|---|---|
| `CreateOrder` | no API call (the form creates the payment); `PaymentOrder{ID: invoiceID, Gateway: "moyasar"}` |
| `ChargeSavedPaymentMethod` | paid-payment lookup, then `POST /payments` with the token and a derived `given_id` |
| `RetryPayment` | no API call; `ErrorCode: no_saved_payment_method` |
| `Refund` | `POST /payments/{id}/refund`, with read-back |
| `CreateSubscription` | error, **never an id** (a stored id makes the renewal claim skip it) |
| `VerifyPayment`, mandates, VAs / `CancelSubscription` | errors (verify = `FetchPayment` read-back) / nil |
| extra `FetchPayment`, `FetchToken` | `GET /payments/{id}` → `port.GatewayPayment`; `GET /tokens/{id}` → card expiry |

**Idempotency.** `given_id = uuid.NewSHA1(fixed namespace, key)`, so replays of
`renewal-<inv>`, `retry-<inv>-<n>` or `wallet-recharge-<wallet>-<attempt>`
hit the same payment; an empty key is refused. Before charging an invoice the
adapter lists `metadata[invoice_id]=<inv>&status=paid` and returns a paid
payment in the same currency covering the amount (net of refunds) as success
without charging, catching a payment whose response was lost under an earlier
attempt's key. On a 400 it reads back `GET /payments/{given_id}`: a found
payment is mapped as the result (`processing_error` if amount/currency
differ); a 404 means a real rejection. A 4xx (except 401/403/429) whose
`errors` name the token or source, or whose message names the token, is the
card: `invalid_payment_method`, Permanent. Any other 4xx is our request or
account (bad `callback_url`, amount, `given_id`, account not live): an error
and an ERROR log, so callers back off rather than treat it as a decline.
Wallet top-ups carry `metadata.wallet_id`, never `invoice_id`.

**Declines** (Moyasar doesn't classify codes; the grouping is Recurso's):

| Result | ErrorCode | Permanent |
|---|---|---|
| `failed` `51` `61` `65` | `insufficient_funds` | no |
| `failed` `33` `54` / `41` `43` | `expired_card` / `lost_or_stolen` | yes |
| `failed` `04 07 14 15 34–38 56 57 59 62 63 67 79 93` | `card_declined` | yes |
| `failed`, other or null code | `card_declined` | no |
| `initiated` (issuer wants 3DS) / other status | `authentication_required` / `processing_error` | no |

**Refund read-back.** No refund key, so exactly one call, never retried; after
a transport error or 5xx the payment is re-read (with a detached context) and
the refund succeeds if `refunded` grew by the amount. `RefundID` =
`<payment id>:<refunded total>`. If the read-back fails or doesn't show it yet,
the refund is `pending` (`<payment id>:unknown:<uuid>`), never failed: it holds
the refundable amount so it can't be re-issued, and nothing is booked (no
ledger refund, tax reversal or rev-rec unwind) while the outcome is unknown.
The `payment_refunded` webhook resolves it from the payment's `refunded` total
(`CreditNoteService.ResolveUnknownRefunds`): oldest first, what that total
covers beyond the processed refunds becomes `processed` and is booked then; the
rest becomes `refund_failed`. Refunds of one payment are serialized within a
process.

### Saved methods and routing

Migration `000176` adds `customers.pm_provider` (NULL = Stripe; CHECK
`stripe|moyasar`; the down clears non-Stripe methods first). The `token_…` sits
in `default_payment_method`, brand/last4/expiry in the card columns.
`SetGatewayPaymentMethod` writes them; Stripe's setter resets `pm_provider`.

Renewal, dunning retry and wallet auto-recharge charge through
`SavedCardGatewayRouter.ChargerFor` (`service/saved_card_gateway.go`), with
Moyasar added via `RegisterProvider`. A method the router can never charge as
configured returns `ErrSavedMethodNotChargeable`; any other router or lookup
error is transient. **Currency guard** (`SetCurrencyRouting`):
a method charges only when its provider is `SmartRouter.ProviderFor(currency)`
(override, then INR→Razorpay, else Stripe). Exception: a Stripe method still
charges a currency whose gateway has no saved-card charger (INR on Razorpay,
EUR on GoCardless), as before. A refusal leaves the invoice to dunning.

### Settlement (`service/gateway_settlement.go`)

Checkout verify and the webhook share `Settle`: `BindPaymentToInvoice`
(metadata invoice id, currency, amount net of refunds ≥ `AmountDue`) →
`MarkInvoicePaid` under the invoice's tenant (ledger legs, ADR-002) →
`RecordSettlingPayment` → save the card → on the paid transition, close the
dunning campaign and reward the retry bandit. A void or draft invoice is never
settled: the payment is logged for refund review (`InvoicePayable`: open,
past_due, uncollectible). Renewal and retry-worker successes settle
synchronously and use the same `RecordSettlingPayment`: it always attempts the
conditional `ClaimGatewayPaymentID` and logs a possible duplicate charge only
when the claim loses to another payment, or when the invoice was settled
another way (offline) with no payment recorded; finding the same payment
already recorded (the webhook settled it first) is not a duplicate, and only a
non-duplicate saves its card.

**Card consent.** `SavesCard` is true only for a subscription that will renew
(not canceled, not `cancel_at_period_end`); one-off invoices never save.
Checkout sends it as `save_card` and shows the consent text before payment.
The expiry (`FetchToken`) feeds the card-expiry notice.

**Dunning handoff** (`worker/retry_worker.go`). A `Permanent` decline, or
`no_saved_payment_method` from any gateway's `RetryPayment` (Moyasar, and
Stripe for a card-less invoice — before, Stripe kept such invoices on worker
retries), stops worker retries: `past_due`, `NextRetryAt = nil`,
`DunningManagedBy = "scheduler"`; the scheduler's emails and pay link take
over. A hard decline records the declined method (`declined_method_ref`) and
doesn't count as a retry (the scheduler's email counter stays in step); when
the scheduler hands the invoice back, that method is not charged again until
the customer saves another. A lookup or router failure is not "no method": it
takes the infrastructure retry. Infrastructure errors back off from 5 minutes
(doubling, 6h cap) and hand the invoice to email dunning after 6 in a row.
Every post-attempt write is conditional on the invoice still being open or
past_due, so a payment that lands during the charge is never reverted.

**Wallet auto-recharge.** The attempt number in the key advances only when the
gateway answers (paid or declined), so a lost response replays the same charge
whatever else changed on the wallet. Failed attempts back off 1h, 6h, then
daily; a hard decline stops auto-recharge until the customer's saved method
changes.

### Webhook (`POST /webhooks/moyasar`)

1 MB cap; 503 (fail closed) without secret, fetcher or settler; constant-time
`secret_token` compare (401). Dedup by event `id` in the inbound store, marked
only after a 2xx (store down → 503). Only `data.id` is read: the payment is
re-fetched with the secret key and the invoice found by its metadata.
`payment_paid` settles a bound `paid` read-back (still `initiated` → 503 for
redelivery); a void or draft invoice is acked as `invoice not payable`.
`payment_refunded` resolves refunds of unknown outcome (above);
`payment_refunded`/`payment_voided` on a paid invoice log a WARN to confirm a
credit note exists. `payment_failed` and others are acked.

### Hosted checkout

`GET /checkout/:id` shows `amount_due` from the database only (no gateway
call). `POST /checkout/:id/pay` answers 409 for a void or draft invoice on every
gateway, is limited per invoice as well as per IP, creates every gateway's
order for `AmountDue`, and for SAR returns `gateway:"moyasar"`,
`save_card`, `card_on_file` (`{merchant, amount, currency, interval,
interval_count}`, the consent terms, when the card is saved), the publishable
key, `description` and `metadata:{invoice_id}`. Before handing out the form,
`SettleExistingPayment` asks Moyasar for a paid payment bound to the
invoice and settles it, so a renewal whose answer was lost is never paid twice
(`/pay` fails closed with 502 if it can't check). `MoyasarForm.jsx` loads the pinned,
SRI-checked assets (`moyasarAssets.js`) and mounts `Moyasar.init` (mada, visa,
mastercard; `callback_url` `/checkout/<id>?gateway=moyasar`). The return calls
`POST /checkout/:id/moyasar/verify {payment_id}`, which reads back, binds and
settles only `paid`; void and draft invoices answer 409, and the route is
limited per invoice and payment id (so other callers' verifies cannot lock the
buyer out) as well as per IP. The browser never marks an invoice paid.

**Arabic/RTL.** `lib/checkoutCopy.js` has English and Arabic copy, picked from
the browser language; Arabic sets `dir="rtl"` and the form's `language: "ar"`.

### Portal card update (stopgap)

No Moyasar card-setup flow exists (a Stripe setup would move renewals to
Stripe), so `PaymentMethodUpdatePolicy` marks Moyasar not self-serve: the
portal returns 409 `card_update_unsupported`, and the profile's
`card_update_invoice_id` names the open invoice of a renewing subscription whose
payment saves the new card (null when none would). Dunning emails link to the
dunned invoice's checkout, card-expiry emails to that invoice.

## Env vars and boot validation

| Var | Purpose |
|---|---|
| `MOYASAR_SECRET_KEY` | enables the gateway; `sk_test_`/`sk_live_` |
| `MOYASAR_PUBLISHABLE_KEY` | browser key; `pk_test_`/`pk_live_`, same mode |
| `MOYASAR_WEBHOOK_SECRET` | shared secret on the Moyasar webhook |
| `GATEWAY_CURRENCY_OVERRIDES` | includes `SAR=moyasar` |
| `MOYASAR_API_URL` | test only: replaces the API base (the e2e stub); refused with `sk_live_` |

`validateMoyasarConfig` (`cmd/api/gateway_config.go`): fatal on a bad key
prefix, mixed test/live keys, a `DASHBOARD_URL` (else `BASE_URL`) that isn't an
absolute URL (public https with a live key; it is token charges'
`callback_url`), or `MOYASAR_API_URL` with a live key; warns on a missing
publishable key (SAR checkout 503) or webhook secret. An override naming an
unregistered gateway is fatal; `DEMO_MODE` skips Moyasar. In
`gatewayModeFromEnv` Stripe/Razorpay set the server's live/test mode when
configured; Moyasar (`sk_live_`), GoCardless and Adyen set it only without
them, and a live/test mix is a boot warning.

## Self-hosting runbook

1. **Don't run the repo `docker-compose.yml` as production.** It is the e2e
   stack: it defaults `MOYASAR_API_URL` to the e2e stub, uses `sk_test_e2e`
   keys and runs the renewal sweep every 5s. Set your own env, and leave
   `MOYASAR_API_URL` unset.
2. **Moyasar account:** the **Tokenization** feature must be enabled for the
   merchant (without it no card is saved and renewals never auto-charge).
   Register the webhook `https://<api host>/webhooks/moyasar` (Settings →
   Webhooks) with `MOYASAR_WEBHOOK_SECRET` as its shared secret, for at least
   `payment_paid`, `payment_failed` and `payment_refunded`.
3. **Env:** `MOYASAR_SECRET_KEY`, `MOYASAR_PUBLISHABLE_KEY`,
   `MOYASAR_WEBHOOK_SECRET`, `GATEWAY_CURRENCY_OVERRIDES=SAR=moyasar`, and
   `DASHBOARD_URL` = the public https origin buyers open checkout links on.
4. **Serve the dashboard and API on one origin** through `frontend/nginx.conf`:
   `/checkout/:id` is both the SPA page and the invoice API, split on Accept.
5. **SAR plans** are created through the API (`POST /v1/plans`); the plan form
   offers USD/INR/EUR/GBP only.
6. **Refunds** are `POST /v1/credit-notes` with `type: "refund"` and the
   `invoice_id`; the dashboard has no refund action for any gateway.
7. **VAT:** 15% KSA VAT needs the KSA VAT change and the tenant's legal entity
   country set to `SA`; without it SAR invoices carry no VAT.

## Residency

Operator-configured gateway egress, like Stripe/Adyen: not residency-blocked.

## Testing

No network in CI. httptest adapter suite (auth, request JSON, `given_id`, paid
lookup, 400 read-back, declines, refund read-back); router, saved-card guard,
settlement (unit + Postgres), renewal, wallet, retry-worker, webhook, checkout,
portal and boot-validation tests; frontend checkout, copy and portal tests.

## Mada merchant-initiated charges: UNCONFIRMED

Mada cards tokenize, but no Moyasar page confirms a saved Mada token can be
charged without the customer present (the
[WooCommerce subscriptions guide](https://docs.moyasar.com/docs/ecommerce/woocommerce/subscriptions.md)
names only cards and Apple Pay). A refusal or 3DS demand returns `failed` or
`initiated`; the invoice stays open for dunning. Nothing double-charges.

## Known limitations and follow-ups

- BYO Moyasar: env-configured only; `moyasar` is not a connectable provider.
- Apple Pay through the form (needs merchant domain validation).
- Full card-update flow (token creation with 3DS) to replace the stopgap.
- Moyasar-dashboard refunds only log a webhook WARN; nothing posts a reversal.
- CSP: none set yet; it must allow jsDelivr and Moyasar.
- Moyasar charges are not written to the tenant payments log.
- `CreatePlan.jsx` offers only USD/INR/EUR/GBP; SAR plans need the API.
- Refund serialization is per process; two instances refunding the same
  payment at once can still misread each other's amount.
- A refund of unknown outcome that Moyasar never applied gets no
  `payment_refunded` webhook, so it stays `pending` (unbooked, refundable
  amount held) until a later refund of the same payment triggers one or an
  operator resolves it; there is no operator action or reconciler yet.

## Founder-gated: sandbox verification before dropping EXPERIMENTAL

1. Test account with **Tokenization enabled**; env vars set; webhook registered.
2. Pay a renewing subscription's invoice with Visa `4111111111111111` and Mada
   `4201320111111010`: paid, token saved (a one-off invoice saves nothing).
3. Force a renewal on **both** cards: charged off-session without 3DS.
4. Decline cards: `…0508` (lost) stops retries; `…1101` (insufficient) retries.
5. Webhook delivery and redelivery; a credit-note refund; Arabic checkout.

Until then `main.go` logs `Moyasar gateway configured (EXPERIMENTAL — sandbox verification pending)`.
