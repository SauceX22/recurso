# Follow-ups: tax and VAT (KSA)

Back to the [index](README.md).

Context: KSA VAT lives on fork PR #4, branch `feat/ksa-vat` (open, not merged
into `feat/moyasar-gateway`). It adds `KSAVATEngine` (15% domestic,
zero-rated exports) and routes SA sellers to it. **Decision:** keep PR #4 open
until TAX-2 is done.

## Open

### TAX-1: SAR invoices go through the India GST path

- **Status:** in progress (routing fixed on `feat/ksa-vat`, not merged)
- **Severity:** medium
- **What's wrong:** for an SA seller the factory returns `NoTaxEngine`, but
  the resolver only special-cases the US and EU engines, so SA falls into
  `resolveIndiaGST`. A non-INR invoice there is an India "export".
- **Evidence:** `internal/core/service/tax/factory.go:46` (`NewNoTaxEngine`);
  `internal/service/tax_resolver.go:200-208` (default branch calls
  `resolveIndiaGST`); `tax_resolver.go:337-348` (`TaxType:"export"`,
  HSN 998314, note "Zero-rated export (no LUT on file …)"); persisted at
  `internal/service/invoice.go:325,473`.
- **Consequence:** every SAR invoice is stored as `tax_type="export"` with HSN
  998314 on its lines and an India LUT note. The dashboard and PDF hide HSN
  for an SA seller, so it leaks through the API, the data and reports. No VAT
  is charged (correct for unregistered merchants, by accident).
- **Proposed fix:** merge the routing part of `feat/ksa-vat` (`46e083bb`:
  `resolveNoTax` for `NoTaxEngine`, `resolveKSAVAT` for SA). It is safe to
  land on its own, without the 15% engine switched on (see TAX-2).
- **Source:** `review_sar_ux.md` #2; fork PR #2 section D.

### TAX-2: PR #4 charges 15% VAT on every domestic supply; there is no "VAT registered" switch

- **Status:** blocked (gates merging PR #4)
- **Severity:** high
- **What's wrong:** once the seller country is SA, `KSAVATEngine` charges 15%
  on every domestic supply. Nothing asks whether the merchant is VAT
  registered. Most merchants on this platform are **not** registered and must
  charge no VAT.
- **Evidence:** `git show feat/ksa-vat:internal/core/service/tax/ksa_vat.go`
  (`CalculateTax`: export → 0%, else `ksaVATAmount`, no registration check);
  `git diff main...feat/ksa-vat -- internal/service/tax_resolver.go`
  (`case *tax.KSAVATEngine` always resolves VAT). No "registered" setting
  exists anywhere in the diff.
- **Consequence:** merging PR #4 as-is overcharges every unregistered
  merchant's customers by 15% and books tax liability they don't owe.
- **Proposed fix (required before merge):** a per-tenant (or per legal
  entity) "VAT registered" setting, default **off** = no VAT, plus a VAT
  number when on; a dashboard control in tax settings; tests covering
  off → 0% with no VAT note, on → 15% domestic, 0% export. This is a generic
  "seller is registered for tax in this jurisdiction" capability, not a
  KSA-only flag.
- **Source:** founder decision on PR #4 (2026-10).

### TAX-3: Saving any GSTIN turns the tenant into an Indian seller; GST nav is always shown

- **Status:** open
- **Severity:** medium
- **What's wrong:** the nav always shows Mandates, GST Returns and GST
  Configuration. Saving a GSTIN or a state code in GST Configuration makes the
  resolver treat the tenant as `IN`, overriding entity country and env.
- **Evidence:** `frontend/src/lib/navigation.js:73,88,125`;
  `internal/service/tax_resolver.go:271` (`c.GSTIN != "" || c.StateCode != ""`
  ⇒ `return "IN"`).
- **Consequence:** an SA operator who pokes at GST settings silently switches
  every SAR invoice to India GST "export" invoices.
- **Proposed fix:** hide the GST and Mandates entries unless the seller
  country is IN or a GST config already exists; warn on the GST page when the
  seller country isn't IN.
- **Source:** `review_sar_ux.md` #10.

### TAX-4: HSN 998314 on SAR downgrade credit notes

- **Status:** open
- **Severity:** low
- **What's wrong:** the downgrade proration credit copies the resolver's HSN,
  which is 998314 because of TAX-1.
- **Evidence:** `internal/service/subscription_upgrade.go:412`
  (`HSNCode: taxRes.HSN`); rendered at
  `internal/service/pdf_credit_note.go:231` ("Taxable value (HSN …)") and
  `frontend/src/pages/CreditNotePage.jsx:288`.
- **Consequence:** an India SAC code on a Saudi credit note (only when the
  subtotal is above 0).
- **Proposed fix:** mostly fixed by TAX-1's routing. Otherwise gate HSN on a
  GST tax type.
- **Source:** `review_sar_ux.md` #15.

### TAX-5: ZATCA e-invoicing and KSA document layout (later)

- **Status:** open
- **Severity:** medium
- **What's missing:** Saudi documents use generic labels and lack KSA invoice
  elements: seller label "Tax ID", tax line "Tax", title "INVOICE", and a QR
  code only for an India IRN. No Arabic layout, no ZATCA QR (TLV), no
  Phase 2 (Fatoora) integration.
- **Evidence:** `internal/service/pdf_invoice.go:283` (`DocTitle = "INVOICE"`),
  `:327` (`"Tax ID"`), `:650-661` (QR/IRN block India-only);
  `frontend/src/pages/InvoicePage.jsx` tax row says "Tax". `feat/ksa-vat`
  renames the tax line to VAT but keeps "Tax ID".
- **Consequence:** a VAT-registered Saudi merchant's invoices are not
  ZATCA-compliant tax invoices. Matters only once TAX-2 lets a merchant be
  registered.
- **Proposed fix:** after TAX-2: "VAT number" label and "Tax Invoice" title for
  registered sellers, simplified-invoice QR (TLV base64), bilingual
  Arabic/English PDF; Fatoora integration as a separate project.
- **Source:** `review_sar_ux.md` #16; fork PR #2 section D (ZATCA).

## Done

None yet.
