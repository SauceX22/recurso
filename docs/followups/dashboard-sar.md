# Follow-ups: dashboard for a SAR operator

Back to the [index](README.md).

What's left from the SAR/Moyasar UX audit (`review_sar_ux.md`). The blocking
and misleading items landed in `eb0c2676..9fb54b26`, `f3269da0` and
`b4690011`. Tax-related items (#2, #10, #15, #16) are in [tax-vat.md](tax-vat.md);
the payments log (#5) is in [moyasar.md](moyasar.md).

## Open

### SAR-1: Reconciliation run details show SAR amounts as `$`

- **Status:** open
- **Severity:** low
- **What's wrong:** the run page hard-codes its currency.
- **Evidence:** `frontend/src/pages/ReconciliationRunPage.jsx:36`
  (`const RUN_CURRENCY = "USD"`), used at `:221,224`.
- **Consequence:** expected/found amounts of a SAR reconciliation read as
  dollars.
- **Proposed fix:** use the report's `reporting_currency`, or persist one per
  run.
- **Source:** `review_sar_ux.md` #14.

### SAR-2: Referral rewards are USD-only

- **Status:** open
- **Severity:** low
- **What's wrong:** the referral form has no currency picker and the backend's
  auto-referral reward is hard-coded.
- **Evidence:** `frontend/src/pages/Referrals.jsx:43,85` (`currency: "USD"`);
  `internal/service/referral.go:105` (`Currency: "USD"`, 5.00).
- **Consequence:** a SAR merchant's referral credits are booked in USD and
  never offset a SAR invoice (credits apply per currency).
- **Proposed fix:** currency select fed by `BILLING_CURRENCIES`; auto-referral
  reward amount and currency from tenant settings.
- **Source:** `review_sar_ux.md` #17.

### SAR-3: Plan form defaults to USD and shows an India HSN/SAC field on every plan

- **Status:** open
- **Severity:** low
- **What's wrong:** the create form starts on USD; the plan page always shows
  an HSN/SAC row.
- **Evidence:** `frontend/src/pages/CreatePlan.jsx:37` (`currency: "USD"`);
  `frontend/src/pages/PlanPage.jsx:127` (`label: "HSN/SAC"`).
- **Consequence:** an easy USD plan by mistake; India-only field noise.
- **Proposed fix:** default to the tenant's reporting/billing currency; show
  HSN/SAC only when the seller regime is GST.
- **Source:** `review_sar_ux.md` #18.

### SAR-4: The dashboard falls back to USD whenever a currency is missing

- **Status:** open
- **Severity:** low
- **What's wrong:** `formatCurrency`/`fromMinorUnits`/`toMinorUnits` default to
  USD, and ~29 page sites use `x.currency || "USD"`.
- **Evidence:** `frontend/src/lib/utils.js:17,32,40,53,67`;
  `frontend/src/pages/WalletPage.jsx:106` (`wallet?.currency || "USD"`); also
  `AccountPage.jsx:72`, `Payments.jsx:104`, the finance reports'
  `reporting_currency || "USD"`.
- **Consequence:** while a query is loading, or if a field is ever absent, SAR
  amounts render as `$` and USD decimals are assumed (fine for SAR's 2, wrong
  for a 0- or 3-decimal currency).
- **Proposed fix:** one tenant default currency (from `/version` or tenant
  settings, i.e. `REPORTING_CURRENCY`) used as the fallback everywhere.
- **Source:** `review_sar_ux.md` (follow-up list).

### SAR-5: Virtual accounts are INR/Razorpay-only but shown to every tenant

- **Status:** open
- **Severity:** low
- **What's wrong:** the virtual-account form on Offline Payments is labelled
  "Expected amount (INR)" and the feature routes to Razorpay only. Moyasar
  returns "not supported".
- **Evidence:** `frontend/src/pages/OfflinePayments.jsx:536`;
  `internal/adapter/gateway/smart_router.go:231-232` (always Razorpay);
  `internal/adapter/gateway/moyasar.go:561`.
- **Consequence:** a SAR operator sees an India-only feature that fails when
  used.
- **Proposed fix:** hide the virtual-account section unless a Razorpay gateway
  is configured.
- **Source:** `review_sar_ux.md` #13 (remaining part).

## Done

- **Customers in Saudi Arabia, no silent US default** (SAR UX #1, #19): `eb0c2676`.
- **Refund a paid invoice from the dashboard; credit note currency picker**
  (SAR UX #3, #4; PR #2 section D "no refund action"): `222160c3`.
- **Copy an invoice's payment link** (SAR UX #6): `4525b9af`.
- **No placeholder seller/bank details on PDFs** (SAR UX #7): `f3269da0`.
- **Saudi Arabia in the business-country list** (SAR UX #8): `eb0c2676`.
- **Honest wallet-close copy, wallet currency picker** (SAR UX #9, #13 wallets): `3a74d719`.
- **Onboarding gateway step completes** (SAR UX #11): `a8204b06`.
- **Plans currency filter includes SAR** (SAR UX #12): `1d22a31c`.
- **Offline payment currency picker** (SAR UX #13 offline payments): `9fb54b26`.
- **Pre-charge reminder names the saved card** (SAR UX #20): `b4690011`.
- **Spec no longer says SAR plans need the API** (SAR UX #21; PR #2 section D plan form): `aad27cbf`, `e1b11252`.
