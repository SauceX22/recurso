import { describe, expect, it } from "vitest";

import { cardUpdateInvoice, cardUpdateViaInvoice } from "../cardUpdate";

describe("cardUpdateViaInvoice", () => {
  it.each([
    [{ card_update_supported: false, payment_method_provider: "stripe" }, true],
    [{ card_update_supported: true, payment_method_provider: "moyasar" }, false],
    // The server's flag decides; the provider name never does.
    [{ payment_method_provider: "moyasar" }, false],
    [{}, false],
    [null, false],
  ])("%j → %s", (profile, want) => {
    expect(cardUpdateViaInvoice(profile)).toBe(want);
  });
});

describe("cardUpdateInvoice", () => {
  const invoices = [
    { id: "a", status: "open", due_date: "2026-12-01", invoice_number: "INV-A" },
    { id: "b", status: "past_due", due_date: "2026-11-01", invoice_number: "INV-B" },
    { id: "c", status: "paid", due_date: "2026-10-01", invoice_number: "INV-C" },
  ];

  it("uses the server's invoice, numbered from the list when it's there", () => {
    expect(cardUpdateInvoice({ card_update_invoice_id: "a" }, invoices)).toEqual({ id: "a", invoiceNumber: "INV-A" });
    expect(cardUpdateInvoice({ card_update_invoice_id: "zz" }, invoices)).toEqual({ id: "zz", invoiceNumber: undefined });
  });

  it.each([null, "", 42])("treats card_update_invoice_id %j as nothing to pay", (id) => {
    expect(cardUpdateInvoice({ card_update_invoice_id: id }, invoices)).toBeNull();
  });

  it("falls back to the earliest open or past-due invoice when the field is absent", () => {
    expect(cardUpdateInvoice({}, invoices)).toEqual({ id: "b", invoiceNumber: "INV-B" });
    expect(cardUpdateInvoice(null, [invoices[2]])).toBeNull();
    expect(cardUpdateInvoice({})).toBeNull();
  });
});
