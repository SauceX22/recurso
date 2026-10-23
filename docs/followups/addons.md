# Follow-ups: add-ons

Back to the [index](README.md).

How add-ons work at HEAD: an add-on is an ordinary plan attached to a
subscription with a quantity (`subscription_addons`, migration
`000063_subscription_addons.up.sql`). The subscription keeps one `plan_id`.
Each add-on becomes an extra line on the **next** recurring invoice
(`internal/service/invoice.go:367-404`). Nothing is prorated when an add-on is
added or removed, and the quantity can't be changed in place.

## Open

### ADDON-1: An add-on's billing period is ignored

- **Status:** open
- **Severity:** high
- **What's wrong:** nothing compares the add-on plan's interval with the
  subscription's. The add-on line is `Prices[0].Amount × quantity` on every
  invoice of the base plan's cycle.
- **Evidence:** `internal/service/subscription_addon.go:17-60` (`AddAddon`
  checks quantity, ownership and currency, never `IntervalUnit`/`IntervalCount`);
  `internal/service/invoice.go:379-390` (amount taken as-is); the period only
  advances on the base plan's interval (`invoice.go:1009`).
- **Consequence:** a yearly add-on on a monthly subscription is billed its
  full yearly amount every month (12× overbilling). A monthly add-on on a
  yearly subscription is billed one month per year.
- **Proposed fix:** refuse the attach when the intervals differ, now. The
  real fix is ADDON-4 (an add-on has one price per currency and period; attach
  picks the matching one or refuses).
- **Source:** add-ons analysis (founder, 2026-10).

### ADDON-2: Add-ons (and base plans) use `Prices[0]`, which is an arbitrary row

- **Status:** open
- **Severity:** medium
- **What's wrong:** a plan can have several prices, one per currency
  (`UNIQUE(plan_id, currency)`, `000001_init_schema.up.sql:35-43`). Add-on code
  reads only `Prices[0]`, and the price query has no `ORDER BY`, so which
  currency is "first" is up to Postgres.
- **Evidence:** `internal/service/subscription_addon.go:43` (attach refuses
  unless `Prices[0].Currency` matches); `internal/service/invoice.go:379-385`
  (renewal skips the add-on with a WARN when `Prices[0]` is another currency);
  `internal/adapter/db/plan_repository.go:83` (no `ORDER BY`). The base plan
  does the same: `subscription.go:974` (subscription currency),
  `invoice.go:276,620,942`.
- **Consequence:** a multi-currency add-on whose first row isn't the
  subscription's currency is refused at attach (`ErrAddonCurrencyMismatch`)
  even though a matching price exists, or, if attached, is silently dropped
  from the invoice (only a log line). A multi-currency base plan can flip the
  subscription's currency between reads.
- **Proposed fix:** pick the price by currency (`priceFor(plan, currency)`),
  never by index. Add `ORDER BY created_at, id` to the price queries as a
  floor. Part of ADDON-4.
- **Source:** add-ons analysis.

### ADDON-3: The add-on picker lists every plan, in every currency

- **Status:** open
- **Severity:** low
- **What's wrong:** the picker filters out only the base plan and plans
  already attached. Base plans and plans with no price in the subscription's
  currency are offered too.
- **Evidence:** `frontend/src/pages/SubscriptionPage.jsx:1084-1096`.
- **Consequence:** the operator picks a USD plan for a SAR subscription and
  gets a currency-mismatch error, or attaches a base plan as an add-on.
- **Proposed fix:** filter by plan kind `addon` and by a price in the
  subscription's currency (needs ADDON-4's plan kind).
- **Source:** add-ons analysis.

### ADDON-4: Add-on catalog, "Option A" (deferred feature, decided)

- **Status:** open
- **Severity:** medium
- **What's missing:** a real add-on model on top of the current
  single-`plan_id` subscription, the way Stripe and Chargebee present it:
  1. Plan kind `base | addon`; pickers filtered by kind.
  2. One add-on, many prices (currency + period). Attach picks the price that
     matches the subscription's currency and period, or refuses. Fixes ADDON-1
     and ADDON-2.
  3. Optional "compatible add-ons" per base plan.
  4. Change an add-on's quantity in place (today only
     `POST`/`GET`/`DELETE /v1/subscriptions/:id/addons`,
     `cmd/api/routes_v1.go:133-135`).
  5. Mid-cycle proration on add, remove and quantity change, through the
     existing plan-change proration engine
     (`internal/service/subscription_upgrade.go:26,78`). It posts ledger
     entries, so it must pass the invariant harness (ADR-002).
  6. Dashboard: plan form gets type, prices and compatible add-ons; the
     subscription page gets the filtered picker and editable quantities.
- **Consequence today:** add-ons start at the next renewal only, nothing is
  prorated, and ADDON-1/2/3 stay possible.
- **Estimate:** 3-5 days plus tests. Low ledger risk except item 5.
- **Rejected for now, Option B:** full Stripe-style subscription items that
  replace `plan_id`. Weeks of work: `plan_id` is read across ~30 service
  files, and every gateway's subscription sync would change.
- **Platform note:** this is a generic catalog capability, not a
  client-specific feature.
- **Source:** add-ons analysis.

## Done

None yet.
