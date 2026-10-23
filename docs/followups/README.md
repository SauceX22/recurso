# Follow-ups tracker

A living list of things to take care of later on the self-hosted SAR/Moyasar
fork: bugs, gaps, deferred features and decisions pending. Each item was
checked against the code at `feat/moyasar-gateway` @ `814fc38f` when it was
written. Where an item and the code disagree, the code wins: fix the item.

## How to use it

- **One file per area.** Each item has a stable id (`ADDON-1`, `MOY-3`, …),
  title, status, severity, what's wrong or missing, evidence (file:line),
  consequence, proposed fix and source. Ids are never reused or renumbered.
- **Status values:** `open` · `in progress` · `blocked` (say on what) ·
  `done`.
- **Severity:** `high` (money, tax or data is wrong, or a deploy is unsafe) ·
  `medium` (wrong or missing behaviour with a workaround) · `low` (cosmetic,
  noisy, or edge-case).
- **Adding an item:** next free id in the area file, verified against current
  code (cite file:line), plus a row in the index below.
- **Finishing an item:** set `done`, move it to the area file's **Done**
  section with the commit or PR that fixed it (one line is enough), and update
  its index row. Don't delete it.
- **When code moves:** if you touch code an item cites, update the item's
  file:line in the same PR.

## Index

| Id | Title | Area | Severity | Status | File |
|---|---|---|---|---|---|
| ADDON-1 | An add-on's billing period is ignored | add-ons | high | open | [addons.md](addons.md) |
| ADDON-2 | Add-ons (and base plans) use `Prices[0]`, an arbitrary row | add-ons | medium | open | [addons.md](addons.md) |
| ADDON-3 | Add-on picker lists every plan, in every currency | add-ons | low | open | [addons.md](addons.md) |
| ADDON-4 | Add-on catalog "Option A" (kind, multi-price, quantity, proration) | add-ons | medium | open | [addons.md](addons.md) |
| TAX-1 | SAR invoices go through the India GST path | tax/VAT | medium | in progress | [tax-vat.md](tax-vat.md) |
| TAX-2 | PR #4 charges 15% VAT with no "VAT registered" switch | tax/VAT | high | blocked | [tax-vat.md](tax-vat.md) |
| TAX-3 | Saving a GSTIN turns the tenant Indian; GST nav always shown | tax/VAT | medium | open | [tax-vat.md](tax-vat.md) |
| TAX-4 | HSN 998314 on SAR downgrade credit notes | tax/VAT | low | open | [tax-vat.md](tax-vat.md) |
| TAX-5 | ZATCA e-invoicing and KSA document layout | tax/VAT | medium | open | [tax-vat.md](tax-vat.md) |
| SAR-1 | Reconciliation run shows SAR as `$` | dashboard (SAR) | low | open | [dashboard-sar.md](dashboard-sar.md) |
| SAR-2 | Referral rewards are USD-only | dashboard (SAR) | low | open | [dashboard-sar.md](dashboard-sar.md) |
| SAR-3 | Plan form defaults to USD; HSN/SAC on every plan | dashboard (SAR) | low | open | [dashboard-sar.md](dashboard-sar.md) |
| SAR-4 | Dashboard falls back to USD when a currency is missing | dashboard (SAR) | low | open | [dashboard-sar.md](dashboard-sar.md) |
| SAR-5 | Virtual accounts are INR/Razorpay-only but shown to all | dashboard (SAR) | low | open | [dashboard-sar.md](dashboard-sar.md) |
| MOY-1 | Sandbox verification with real keys (incl. Mada off-session) | Moyasar | high | blocked | [moyasar.md](moyasar.md) |
| MOY-2 | Stuck or voided old-card attempt blocks a replayed charge forever | Moyasar | medium | open | [moyasar.md](moyasar.md) |
| MOY-3 | False duplicate-charge warning on verify/webhook race | Moyasar | low | open | [moyasar.md](moyasar.md) |
| MOY-4 | Paid-then-voided buyer isn't told they were charged | Moyasar | low | open | [moyasar.md](moyasar.md) |
| MOY-5 | Moyasar charges missing from the payments log | Moyasar | medium | open | [moyasar.md](moyasar.md) |
| MOY-6 | BYO Moyasar not connectable; no "configured" status card | Moyasar | low | open | [moyasar.md](moyasar.md) |
| MOY-7 | Apple Pay | Moyasar | low | open | [moyasar.md](moyasar.md) |
| MOY-8 | Card update is a pay-an-invoice stopgap | Moyasar | low | open | [moyasar.md](moyasar.md) |
| MOY-9 | No Content-Security-Policy, checkout included | Moyasar | medium | open | [moyasar.md](moyasar.md) |
| MOY-10 | Settlement re-saves the card on every token payment | Moyasar | low | open | [moyasar.md](moyasar.md) |
| MOY-11 | Webhook log noise when unconfigured; no secret rotation | Moyasar | low | open | [moyasar.md](moyasar.md) |
| MOY-12 | Checkout-link holders can rate-limit the real buyer | Moyasar | low | open | [moyasar.md](moyasar.md) |
| MOY-13 | 3DS return with a failed fetch shows "Invoice not found" | Moyasar | low | open | [moyasar.md](moyasar.md) |
| REF-1 | Moyasar-dashboard refunds and voids never reach the books | dunning/refunds | medium | open | [dunning-refunds.md](dunning-refunds.md) |
| REF-2 | Unknown-outcome refunds need an operator action or reconciler | dunning/refunds | medium | open | [dunning-refunds.md](dunning-refunds.md) |
| REF-3 | Refund serialization is per process | dunning/refunds | low | open | [dunning-refunds.md](dunning-refunds.md) |
| REF-4 | Off-session 3DS demand retried like a soft decline | dunning/refunds | medium | open | [dunning-refunds.md](dunning-refunds.md) |
| REF-5 | Renewal network error: no quick retry, "failed" email | dunning/refunds | low | open | [dunning-refunds.md](dunning-refunds.md) |
| REF-6 | Stripe never sets `Permanent` | dunning/refunds | low | open | [dunning-refunds.md](dunning-refunds.md) |
| UP-1 | Coupon codes unique across all tenants | upstream | medium | open | [upstream-bugs.md](upstream-bugs.md) |
| UP-2 | Portal "login (dev only)" button doesn't log in | upstream | low | open | [upstream-bugs.md](upstream-bugs.md) |
| UP-3 | BYO-Stripe tenants' Adyen currencies go to platform Adyen | upstream | medium | open | [upstream-bugs.md](upstream-bugs.md) |
| OPS-1 | Mock gateways settle real invoices in production | deploy/ops | high | done (`2a5a4e36`) | [deploy-ops.md](deploy-ops.md) |
| OPS-2 | Image build no longer runs Go tests; fork has no CI runs | deploy/ops | high | open | [deploy-ops.md](deploy-ops.md) |
| OPS-3 | Prod compose publishes the frontend on 5168 | deploy/ops | low | open | [deploy-ops.md](deploy-ops.md) |
| OPS-4 | Frontend tests time out under load | deploy/ops | low | open | [deploy-ops.md](deploy-ops.md) |
| OPS-5 | Money anomalies only logged; no Moyasar health check | deploy/ops | medium | open | [deploy-ops.md](deploy-ops.md) |
| OPS-6 | Migration 000176 validates under lock; down deletes SAR cards | deploy/ops | low | open | [deploy-ops.md](deploy-ops.md) |
| OPS-7 | Renewal batch can outlive its lease (multi-replica) | deploy/ops | low | open | [deploy-ops.md](deploy-ops.md) |
| OPS-8 | Test hygiene left over from the reviews | deploy/ops | low | open | [deploy-ops.md](deploy-ops.md) |

Already-fixed items from the source reviews are listed in each area file's
**Done** section.

## Sources

- Founder add-ons analysis and KSA VAT decision (2026-10).
- Fork PRs: #2 (`feat/moyasar-gateway`, section "D. Found but not fixed"),
  #4 (`feat/ksa-vat`).
- [../spec_moyasar.md](../spec_moyasar.md) "Known limitations" and
  "Founder-gated".
- Review reports and fix plans from the Moyasar work, kept outside the repo
  in the maintainer's workspace archive (`archive/2026-10-moyasar/reviews/`
  and `plans/`): `review_sar_ux.md`, `review4.md`, `review*_*.md`,
  `fix_plan_r2.md`, `fix_plan_r3.md`.
