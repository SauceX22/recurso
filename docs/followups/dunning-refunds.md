# Follow-ups: dunning and refunds

Back to the [index](README.md).

## Open

### REF-1: Refunds and voids done in the Moyasar dashboard never reach the books

- **Status:** open
- **Severity:** medium
- **What's wrong:** a `payment_refunded`/`payment_voided` webhook for a refund
  Recurso didn't issue only resolves Recurso's own unknown-outcome refunds,
  then logs a WARN.
- **Evidence:** `internal/adapter/handler/webhook_moyasar.go:92` (routes both
  events), `:204-215` (resolve, then WARN "confirm a matching credit note
  exists").
- **Consequence:** the invoice stays paid with no credit note and no ledger
  reversal. MRR, rev-rec and tax stay overstated, and nothing compares the
  books with the gateway.
- **Proposed fix:** when the gateway's refunded total (or a void) exceeds the
  processed credit notes, create an "external refund" credit note in a
  needs-review state and alert; post the reversal once confirmed.
- **Source:** `docs/spec_moyasar.md:282`; `review_ops.md` H3;
  `review_money_security.md` L11.

### REF-2: Refunds of unknown outcome need an operator action or a reconciler

- **Status:** open
- **Severity:** medium
- **What's missing:** a refund whose answer was lost stays `pending`
  (unbooked, refundable amount held) until a later `payment_refunded` resolves
  it unambiguously. When the total is ambiguous, the only signal is an ERROR
  log.
- **Evidence:** `internal/service/credit_note.go:571-620` (resolve only when
  exactly one unknown note matches; otherwise ERROR at `:614`); no UI or
  endpoint to resolve a pending refund.
- **Consequence:** pending refunds can sit forever; the customer may have been
  refunded while the books say not, or the reverse.
- **Proposed fix:** a "Confirm refund outcome" action on the credit note page
  (applied / not applied, operator checks the gateway dashboard), plus a daily
  reconciler that fetches each pending refund's payment and alerts on any
  still ambiguous.
- **Source:** `docs/spec_moyasar.md:287-291`; fork PR #2 section D.

### REF-3: Refund serialization is per process

- **Status:** open
- **Severity:** low
- **What's wrong:** refunds of one payment are serialized by an in-memory
  striped mutex.
- **Evidence:** `internal/adapter/gateway/moyasar.go:37-40,501`
  (`refundLocks [64]sync.Mutex`).
- **Consequence:** with two or more API instances, two refunds of the same
  payment can run at once and misread each other's refunded amount.
  Single-instance self-hosting is not affected.
- **Proposed fix:** a Postgres advisory lock (or Redis lock) keyed on the
  payment id around the refund call.
- **Source:** `docs/spec_moyasar.md:285-286`; fork PR #2 section D.

### REF-4: An off-session charge that wants 3DS is retried like a soft decline

- **Status:** open
- **Severity:** medium
- **What's wrong:** an `initiated` result maps to `authentication_required`
  with no "needs the customer" signal. The retry worker keys each attempt on
  the retry count, so each retry creates a new `initiated` payment.
- **Evidence:** `internal/adapter/gateway/moyasar.go:436-444`;
  `internal/adapter/worker/retry_worker.go:135`
  (`retry-%s-%d` on `inv.RetryCount`).
- **Consequence:** every retry leaves another orphaned `initiated` payment and
  the customer isn't sent the pay link early. If Mada off-session charges need
  3DS (MOY-1), every Mada renewal goes this way.
- **Proposed fix:** treat `authentication_required` as "needs customer": stop
  worker retries and hand the invoice to email dunning with the checkout link
  at once.
- **Source:** `review_money_security.md` L7; `review_ops.md` M3.

### REF-5: A renewal network error gets no quick retry; the customer gets a "payment failed" email

- **Status:** open
- **Severity:** low
- **What's wrong:** on a gateway error (no answer) the renewal only logs and
  leaves the invoice to dunning. Only a `Permanent` decline is recorded.
- **Evidence:** `internal/service/renewal.go:231-237`.
- **Consequence:** a card that was never declined triggers a dunning email,
  and the first real retry is days later.
- **Proposed fix:** on an infra error schedule a short worker retry (same key)
  before the email ladder starts.
- **Source:** `review_ops.md` M5; `review_money_security.md` M4.

### REF-6: Stripe never sets `Permanent`

- **Status:** open
- **Severity:** low
- **What's wrong:** Moyasar classifies lost/stolen/closed cards as permanent so
  retries stop. The Stripe adapter never sets `Permanent`.
- **Evidence:** `internal/adapter/gateway/moyasar.go:363,432` set it;
  `internal/adapter/gateway/stripe.go` has no `Permanent`.
- **Consequence:** Stripe lost or stolen cards keep being retried by dunning,
  against card-network retry rules.
- **Proposed fix:** map Stripe `decline_code` `lost_card`, `stolen_card`,
  `pickup_card`, `restricted_card`, `invalid_account` to `Permanent: true`.
- **Source:** fork PR #2 section D.

## Done

- **No dashboard refund action** (PR #2 section D, SAR UX #3): `222160c3`
  (`frontend/src/pages/CreateCreditNote.jsx:30`, `type: "refund"`).
- **Refund a Moyasar payment on Moyasar whatever the currency routes to**:
  `fdbac8d5`.
