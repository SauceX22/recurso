# Spec: Moyasar Payment Gateway (SAR, Saudi Arabia)

> **Status: EXPERIMENTAL until sandbox-verified (founder-gated: needs Moyasar
> test-mode credentials).** Same rollout pattern as GoCardless/Adyen
> (`spec_integrations_payments.md`): boot-time log line, zero live or sandbox
> calls in CI.

## Objective

Moyasar is the main Saudi payment gateway (Mada, Visa, Mastercard, Amex,
Apple Pay). A deployment with `GATEWAY_CURRENCY_OVERRIDES="SAR=moyasar"` can:

1. take the first payment on the hosted checkout (`/checkout/{invoiceId}`)
   with Moyasar's embedded form, saving the card as a reusable token;
2. settle invoices from Moyasar webhooks;
3. auto-charge renewals and dunning retries against the saved token;
4. refund through Moyasar (credit notes already call the gateway's `Refund`).

The mechanism is generic. Saved payment methods become provider-neutral
(`customers.pm_provider`), the dunning worker learns to stop on hard declines
when a gateway reports one, and the smart router can verify payment IDs for any
registered extra gateway. Moyasar is the first gateway to use these.

## Moyasar API facts (verified 2026-10-21)

Sources: https://docs.moyasar.com/llms.txt and the pages linked below. The
payments create/fetch/list/refund references exist only as HTML
(`https://docs.moyasar.com/api/payments/0X-…`), not `.md` or llms-full.

| Fact | Value | Source |
|---|---|---|
| Base URL | `https://api.moyasar.com/v1`, HTTPS only | [auth](https://docs.moyasar.com/docs/api/authentication.md) |
| Auth | HTTP Basic, **API key as username, empty password** | [auth](https://docs.moyasar.com/docs/api/authentication.md) |
| Keys | `pk_test_`/`sk_test_`, `pk_live_`/`sk_live_`; the key alone selects test vs live, so no `MOYASAR_ENV` | [auth](https://docs.moyasar.com/docs/api/authentication.md) |
| Publishable key | browser-safe; may only create payments/tokens | [auth](https://docs.moyasar.com/docs/api/authentication.md) |
| Amounts | integer smallest unit (halalas for SAR; 100 = 1.00 SAR) | [create](https://docs.moyasar.com/api/payments/01-create-payment) |
| Create payment | `POST /payments` `{given_id, amount, currency, description, callback_url, source, metadata}` | [create](https://docs.moyasar.com/api/payments/01-create-payment) |
| Token source | `source: {type:"token", token:"token_…"}`; `callback_url` **required** for token sources; active tokens default to `3ds:false` | [tokenized cards](https://docs.moyasar.com/docs/guides/tokenization/tokenized-cards.md) |
| Idempotency | **body field `given_id`** (client UUID, becomes the payment `id`); no header. Replays return the original result; a conflicting reuse returns 400 `Payment is already created.` Create only: refund/capture/void have no key | [idempotency](https://docs.moyasar.com/docs/api/idempotency.md) |
| Declines | **HTTP 201** with `status:"failed"`, reason in `source.message`, ISO-8583 code in `source.response_code` | [errors](https://docs.moyasar.com/docs/api/errors.md) |
| Errors | `{type, message, errors}`; types `invalid_request_error`, `authentication_error`, `rate_limit_error`, `api_error`, … | [errors](https://docs.moyasar.com/docs/api/errors.md) |
| Statuses | `initiated`, `paid`, `failed`, `authorized`, `captured`, `refunded`, `voided`, `verified` | [status](https://docs.moyasar.com/docs/api/payments/payment-status-reference.md) |
| Fetch | `GET /payments/{id}` | [fetch](https://docs.moyasar.com/api/payments/02-fetch-payment) |
| Refund | `POST /payments/{id}/refund` `{amount}` (optional; partial allowed); response is the payment object with `refunded` | [refund](https://docs.moyasar.com/api/payments/05-refund-payment), [operations](https://docs.moyasar.com/docs/guides/payment-operations/index.md) |
| Payment id | UUID, no prefix | [create](https://docs.moyasar.com/api/payments/01-create-payment) |
| Token id | `token_…`; statuses `initiated`, `active`, `inactive`(, `expired`) | [token object](https://docs.moyasar.com/docs/api/other/tokens/token-object.md) |
| Save card in form | `credit_card: {save_card: true}`; token returned in `payment.source.token`. **Tokenization must be enabled on the merchant account** | [overview](https://docs.moyasar.com/docs/guides/tokenization/overview.md), [form config](https://docs.moyasar.com/docs/guides/references/form-configuration/index.md) |
| Form assets | `https://cdn.jsdelivr.net/npm/moyasar-payment-form@2.3.0/dist/moyasar.umd.min.js` + `…/dist/moyasar.css` (docs render the latest version; pinned here) | [basic integration](https://docs.moyasar.com/docs/guides/card-payments/basic-integration.md) |
| Form redirect | `callback_url?id=<payment_id>&status=<status>&message=<msg>`; must be verified server-side | [basic integration](https://docs.moyasar.com/docs/guides/card-payments/basic-integration.md) |
| Webhook events | `payment_paid`, `payment_failed`, `payment_refunded`, `payment_voided`, `payment_authorized`, `payment_captured`, `payment_abandoned`, `payment_verified`, … (two pages misspell `payment_faild`) | [available](https://docs.moyasar.com/docs/api/other/webhooks/available-webhooks.md) |
| Webhook envelope | `{id, type, created_at, secret_token, account_name, live, data:<payment>}` | [reference](https://docs.moyasar.com/docs/api/other/webhooks/webhook-reference.md) |
| Webhook retries | 5 retries (1m, 10m, 30m, 1h, 2h) without a 2xx | [reference](https://docs.moyasar.com/docs/api/other/webhooks/webhook-reference.md) |

### Webhook authenticity: what the docs say

No page documents an HMAC or signature header. The only mechanism is the
**shared secret** set when the webhook is registered (`shared_secret` in
[create webhook](https://docs.moyasar.com/docs/api/other/webhooks/create-webhook.md),
"Secret Token" in the
[dashboard](https://docs.moyasar.com/docs/guides/dashboard/setting-up-webhooks.md)).
Moyasar echoes it in the JSON body as `secret_token`.

Recurso therefore:

1. fails closed when `MOYASAR_WEBHOOK_SECRET` is unset (503);
2. compares `secret_token` in constant time (401 on mismatch);
3. **never acts on `data`**. It re-fetches the payment with the secret key and
   settles only on that read-back.

A leaked secret then lets an attacker replay real payment IDs, which is
harmless because settlement is idempotent and bound to the payment's own
metadata. It cannot be used to forge a payment.

## Design

### Port mapping (`internal/adapter/gateway/moyasar.go`)

| Port method | Moyasar |
|---|---|
| `CreateOrder` | **No API call.** The embedded form creates the payment in the browser with the publishable key. Returns `PaymentOrder{ID: invoiceID, Gateway: "moyasar", Amount, Currency, Receipt}` |
| `VerifyPayment(orderID=invoiceID, paymentID, _)` | `GET /payments/{paymentID}`; nil only if `status=="paid"` and `metadata.invoice_id == orderID` |
| `ChargeSavedPaymentMethod` (`SavedCardCharger`) | `POST /payments` with token source and a deterministic `given_id` (see Idempotency) |
| `RetryPayment` | No API call. Returns `PaymentResult{Success:false, ErrorCode:"no_saved_payment_method", Permanent:true}`, nil. Without a saved token there is nothing to charge off-session, so the customer pays via the checkout link instead of being retried every tick |
| `Refund` | `POST /payments/{id}/refund {amount}` → `RefundResult{RefundID: payment id, Status: "refunded"}` |
| `CreateSubscription` | error `"gateway-managed subscriptions are not supported by moyasar (Recurso owns the cycle)"`. The caller logs and continues. **Must never return an ID**: a non-empty ID is stored as `stripe_subscription_id`, and the renewal claim skips such rows (`subscription_repository.go`, `COALESCE(stripe_subscription_id,'') <> ''`) |
| `CancelSubscription` | nil (nothing gateway-side) |
| `CreateMandate`, `ExecuteMandateDebit`, `RevokeMandate`, `CreateVirtualAccount` | "not supported by moyasar" errors |
| extra: `FetchPayment(ctx, id) (*port.GatewayPayment, error)` | `GET /payments/{id}` mapped to `port.GatewayPayment` |
| extra: `OwnsPaymentID(id string) bool` | true for a UUID-shaped id (router's verify fallback) |

### Idempotency

Moyasar's key is `given_id`, a UUID that becomes the payment id. The adapter
derives it deterministically: `uuid.NewSHA1(moyasarNamespace,
[]byte(idempotencyKey))`. The renewal key `renewal-<invoice>` and the retry
key `retry-<invoice>-<n>` therefore map to the same payment id on every replay,
and Moyasar dedupes. If Moyasar answers 400 `Payment is already created.`, the
adapter fetches `GET /payments/{given_id}` and maps that payment. It never
creates a second one. An empty `idempotencyKey` is refused, so the adapter
never charges blind. Refund has no key; the adapter makes exactly one call and
does not retry.

### Decline mapping (`source.response_code`, ISO 8583)

| Result | ErrorCode | Permanent |
|---|---|---|
| `paid` | — (Success) | — |
| `failed`, code in hard set `04 07 14 15 33 34 35 36 37 38 41 43 54 56 57 59 62 63 67 79 93` | `card_declined_<code>` | **true** |
| `failed`, any other code (`51` insufficient funds, `05` do-not-honour, `91` issuer unavailable, …) | `card_declined_<code>` | false |
| `failed`, null code (3DS/pre-acquirer) | `card_declined` | false |
| `initiated` (issuer wants 3DS: token charge needs the customer) | `authentication_required` | false |
| HTTP/network error | `gateway_error` (+ returned error) | false |

The hard set follows Moyasar's
[gateway response codes](https://docs.moyasar.com/docs/guides/references/gateway-response-codes.md).
Moyasar does not classify codes itself; this grouping (lost, stolen, expired,
invalid, fraud, restricted) is Recurso's.

### Routing (`smart_router.go`)

- `GATEWAY_CURRENCY_OVERRIDES="SAR=moyasar"` plus `RegisterGateway("moyasar", …)`
  (already generic). `gatewayFor` handles orders, retries and refunds by
  currency.
- **`VerifyPayment` fix**: keep the `order_` (Razorpay) and `pi_` (Stripe)
  prefixes exactly as they are. For any other id, ask the registered extra
  gateways (sorted by name, deterministic) that implement
  `OwnsPaymentID(string) bool`. The first that claims the id verifies it.
  Otherwise return the old "unknown gateway" error.
- `Refund`: Moyasar ids are bare UUIDs, so SAR refunds fall through the
  `pi_`/`ch_`/`pay_` prefix checks to currency routing and reach Moyasar.
  A router test proves it. (Code note: `Refund` has no GoCardless `PM` case,
  despite older docs; GoCardless refunds also route by currency.)

### Saved-method model (provider-neutral, additive)

Constraint: **no existing signature or test changes**. Stripe's saved-card
path (`GetSavedPaymentMethod` 4-tuple, `SavedCardGatewayRouter.ChargerFor`,
`SetSavedMethodCharging`) stays byte-identical. Provider routing is a new,
nil-safe, opt-in layer consulted first.

Migration `000176_customer_pm_provider` adds `customers.pm_provider TEXT NULL`
(the down migration drops it). NULL means Stripe, which covers every existing
row. The existing generic columns are reused: `default_payment_method` holds
the Moyasar `token_…` and `card_brand`/`card_last4` the display. Moyasar has
no customer object, so `stripe_customer_id` is untouched.

- `domain.SavedPaymentMethod{Provider, CustomerRef, PaymentMethodRef, GatewayConnectionID}`
  (landed in Phase 1).
- **New** `CustomerRepository.GetSavedGatewayMethod(ctx, customerID) (domain.SavedPaymentMethod, error)`.
- **New** `CustomerRepository.SetGatewayPaymentMethod(ctx, tenantID, customerID, provider, ref, brand, last4)`:
  sets `default_payment_method`, the display fields and `pm_provider`, and
  clears `pm_gateway_connection_id`. Filters by `tenant_id`.
- `SetDefaultPaymentMethod` (Stripe portal) additionally sets
  `pm_provider = NULL`, so a customer who later saves a Stripe card routes back
  to Stripe. Signature unchanged.
- **New** `service.ProviderSavedMethods` (in `saved_card_gateway.go`):
  `NewProviderSavedMethods(lookup)`, `Register(provider, charger)`,
  `Resolve(ctx, customerID) (charger SavedCardCharger, ref string, ok bool)`.
  `ok` is true only when the customer's `pm_provider` is a registered
  non-Stripe provider and a ref is stored. Every other case returns ok=false
  and the caller continues down the untouched Stripe path.
- **New** nil-safe setters `SetProviderSavedMethods(p)` on `RenewalService`,
  `WalletService` and `RetryWorker`. When `Resolve` returns ok, the caller
  charges that charger (`customerRef = ""`, `paymentMethodID = ref`) with the
  same amount and idempotency key it already computes. This runs **before**
  the Stripe nil-charger early return, so a Moyasar-only deployment (Stripe
  mocked) still autopays.

### Renewal and dunning

- Renewal charges `amountDue` with the existing key `renewal-<invoice>`. On
  success it calls `MarkInvoicePaid` as today.
- Pre-existing bug, fixed in a **separate PR** per CONTRIBUTING ("separate
  unrelated fixes"): the claim sweep's context carries no tenant, so
  settlement fails with "tenant_id missing from context". Stripe renewals
  settle via the webhook instead. Moyasar does not depend on the fix, because
  `payment_paid` fires for token charges and settles the same way.
- Dunning (`internal/adapter/worker/retry_worker.go`): when a failed result
  has `Permanent: true`, the worker stops auto-retrying. The invoice stays
  `past_due` with `NextRetryAt = nil` and `LastPaymentError = <code>`, and
  dunning emails and the checkout link remain the recovery path. Stripe never
  sets `Permanent`, so its behavior is unchanged.
- Settlement posts ledger legs through `MarkInvoicePaid` (ADR-002). No new
  invoice-creating flow is introduced.

### Webhook (`POST /webhooks/moyasar`)

| Event | Action |
|---|---|
| `payment_paid` | fetch payment → `status=="paid"` and bound to invoice (metadata, currency, amount ≥ due) → `MarkInvoicePaid` (tenant ctx), `SetGatewayPaymentID`, save card token |
| `payment_failed` | ack, log (renewal/dunning record failures synchronously; checkout failures leave the invoice open) |
| `payment_refunded` | ack, log (refunds originate from Recurso credit notes, which already post the reversal) |
| other | ack, ignore |

Dedup uses the existing inbound-webhook store with provider `moyasar` and the
event `id`. The event is marked processed only after it is handled, so
redelivery is safe because `MarkInvoicePaid` is idempotent. If the dedup store
is down, the handler returns 503 and Moyasar redelivers.

### Hosted checkout

- `POST /checkout/:id/pay` returns `gateway:"moyasar"` plus
  `moyasar_publishable_key` (503 if the key is missing), `amount` = amount
  due, `currency`, `description` (`Invoice <number>`), and `metadata:
  {invoice_id}`.
- The frontend loads the pinned CSS and JS from jsDelivr and mounts
  `Moyasar.init({element:'.mysr-form', amount, currency, description,
  publishable_api_key, callback_url: <origin>/checkout/<id>?gateway=moyasar,
  methods:['creditcard'], supported_networks:['mada','visa','mastercard'],
  metadata:{invoice_id}, credit_card:{save_card:true}})` in the page, with no
  redirect to a hosted page. (3DS may redirect to the issuer, which returns to
  `callback_url`.)
- On return (`?id=…&status=…`) the page calls
  `POST /checkout/:id/moyasar/verify {payment_id}`. The server fetches the
  payment, checks `status=="paid"` and the invoice binding, settles, records
  the gateway payment id and saves the token. **The browser alone can never
  mark an invoice paid.**

## Interface contract (fixed before parallel work)

```go
// internal/adapter/gateway/moyasar.go
type MoyasarGateway struct{ secretKey, checkoutBaseURL, baseURL string; httpClient *http.Client }
func NewMoyasarGateway(secretKey, checkoutBaseURL string) *MoyasarGateway // baseURL = https://api.moyasar.com/v1
func (g *MoyasarGateway) FetchPayment(ctx context.Context, paymentID string) (*port.GatewayPayment, error)
func (g *MoyasarGateway) ChargeSavedPaymentMethod(ctx context.Context, customerRef, token string, amount int64, currency, invoiceID, idempotencyKey string) (*port.PaymentResult, error)
func (g *MoyasarGateway) OwnsPaymentID(id string) bool
// callback_url for token charges = checkoutBaseURL + "/checkout/" + invoiceID

// internal/core/port: PaymentResult.Permanent, port.GatewayPayment   (landed in Phase 1)
// internal/core/domain: SavedPaymentMethod                            (landed in Phase 1)
// internal/adapter/handler/moyasar_common.go: moyasarPaymentFetcher,
//   gatewayCardSaver, checkMoyasarPaymentForInvoice, saveMoyasarCard     (landed in Phase 1)

// internal/adapter/handler (webhook)
func (h *WebhookHandler) SetMoyasar(fetcher moyasarPaymentFetcher, saver gatewayCardSaver, secret string)
func (h *WebhookHandler) HandleMoyasar(c *gin.Context)                // POST /webhooks/moyasar
// internal/adapter/handler (checkout)
func (h *CheckoutHandler) SetMoyasar(fetcher moyasarPaymentFetcher, saver gatewayCardSaver, publishableKey string)
func (h *CheckoutHandler) MoyasarVerify(c *gin.Context)               // POST /checkout/:id/moyasar/verify

// internal/adapter/db (new methods; existing ones keep their signatures)
func (r *CustomerRepository) GetSavedGatewayMethod(ctx context.Context, customerID uuid.UUID) (domain.SavedPaymentMethod, error)
func (r *CustomerRepository) SetGatewayPaymentMethod(ctx context.Context, tenantID, customerID uuid.UUID, provider, paymentMethodRef, brand, last4 string) error
// internal/service (saved_card_gateway.go, new)
type ProviderSavedMethods struct{ ... }
func NewProviderSavedMethods(lookup interface{ GetSavedGatewayMethod(context.Context, uuid.UUID) (domain.SavedPaymentMethod, error) }) *ProviderSavedMethods
func (p *ProviderSavedMethods) Register(provider string, c SavedCardCharger)
func (p *ProviderSavedMethods) Resolve(ctx context.Context, customerID uuid.UUID) (SavedCardCharger, string, bool)
func (s *RenewalService) SetProviderSavedMethods(p *ProviderSavedMethods) // + WalletService, worker.RetryWorker (worker declares a local interface)
```

## Env vars

| Var | Purpose |
|---|---|
| `MOYASAR_SECRET_KEY` | server API key (`sk_test_…`/`sk_live_…`); enables the gateway |
| `MOYASAR_PUBLISHABLE_KEY` | browser key for the embedded form |
| `MOYASAR_WEBHOOK_SECRET` | shared secret configured on the Moyasar webhook |
| `GATEWAY_CURRENCY_OVERRIDES` | must include `SAR=moyasar` |

The token-charge `callback_url` uses the existing `DASHBOARD_URL` (falls back
to `BASE_URL`). No `MOYASAR_ENV`: the key prefix selects test or live.

## Residency

A payment gateway is operator-configured egress, the same class as
Stripe/Razorpay/Adyen, so it is not residency-blocked and `internal/residency`
needs no change.

## Testing

- httptest suite (`moyasar_test.go`): Basic auth header (`sk:` with empty
  password), exact request JSON, deterministic `given_id`, the
  "already created" → fetch path, response mapping, decline → `PaymentResult{Success:false,
  ErrorCode, Permanent}` per hard/soft code, `initiated` → `authentication_required`,
  refund body, and the not-supported methods. **No network.**
- Router: SAR → moyasar via override; Moyasar UUID verify; Stripe/Razorpay
  prefixes unchanged; SAR UUID refund → moyasar.
- Existing tests are **not modified**. All new behavior gets new test
  functions or files.
- Renewal: a SAR subscription renews and charges the Moyasar token through
  `ProviderSavedMethods`. Settlement posts ledger legs (Postgres-backed).
  `CreateSubscription`
  returns an error and no id, so the renewal claim still picks the subscription up.
- Dunning: soft decline reschedules; `Permanent` stops.
- Webhook: bad/missing secret rejected; duplicate delivery processed once;
  `payment_paid` settles; `payment_failed` does not; a forged body whose
  re-fetched payment is not paid or not bound does not settle.
- Checkout: backend returns the moyasar shape; verify settles only on a
  bound, paid read-back. Frontend: the form mounts with `save_card`, and the
  return path calls verify.
- Browser (manual, local): the form renders from the CDN against a local stack.
  With test keys, a full sandbox pass using the test cards.

## Mada and merchant-initiated recurring: UNCONFIRMED

Mada cards can be tokenized (token `brand` enum includes `mada`), but **no
Moyasar page confirms that a saved Mada token can be charged
merchant-initiated without the customer present**. The
[WooCommerce subscriptions guide](https://docs.moyasar.com/docs/ecommerce/woocommerce/subscriptions.md)
lists only "Credit Card and Apple Pay", and the API has no MIT/recurring
indicator field. If the issuer rejects or demands 3DS, the token charge returns
`failed` or `initiated`. Renewal then leaves the invoice open, dunning takes
over, and the customer pays via the checkout link. Nothing double-charges or
hangs.

## Out of scope / follow-ups

- **KSA 15% VAT**: the tax factory currently gives SA 0% (via the India GST
  export path). That belongs in a separate PR.
- **BYO (per-tenant) Moyasar connections**: they need a provider CHECK-constraint
  migration plus resolver, connection validation, checkout key lookup, webhook
  secret lookup and dashboard list changes. This PR is env-configured
  (single-tenant) only.
- Dashboard plan form offers only USD/INR/EUR/GBP (`CreatePlan.jsx`); SAR plans
  are created via the API.
- Apple Pay through the Moyasar form (needs merchant domain validation).
- Recording the gateway payment id on synchronous renewal success; today
  the `payment_paid` webhook records it.

## Founder-gated: sandbox verification before dropping EXPERIMENTAL

1. Test-mode account with **Tokenization enabled**; set the three `MOYASAR_*`
   vars plus `SAR=moyasar`.
2. Checkout with Visa `4111111111111111` and Mada `4201320111111010`: the
   invoice is paid and the token saved.
3. Force a renewal: the token is charged off-session, with no 3DS, for **both**
   Visa and Mada. This settles the Mada MIT question.
4. Decline cards (`…0508` lost → stops, `…1101` insufficient → retried).
5. Webhook delivery and redelivery, and a refund from a credit note.

Until then `main.go` logs `Moyasar gateway configured (EXPERIMENTAL — sandbox verification pending)`.
