export const CARD_UPDATE_VIA_INVOICE =
  "To update your card, pay your open renewal invoice with the new card. The new card replaces the one on file.";

export const CARD_UPDATE_NO_INVOICE =
  "There's no renewal invoice to pay yet, so your card can't be updated right now. When the next one is issued you'll get a payment link by email. To change your card sooner, contact the business that bills you.";

// cardUpdateViaInvoice reports whether the customer's saved card belongs to a
// gateway that only saves a card during a payment. The portal can't start a
// card setup for those (a Stripe setup would move renewals to Stripe), so the
// card is replaced by paying an open invoice.
export function cardUpdateViaInvoice(profile) {
  return profile?.card_update_supported === false;
}

// cardUpdateInvoice picks the invoice whose payment will save the new card,
// as { id, invoiceNumber }, or null when there is none to pay. The server
// names it in card_update_invoice_id (null: nothing to pay), because only an
// invoice of an auto-renewing subscription saves the card. A server that
// doesn't send the field gets the earliest open invoice, as before.
export function cardUpdateInvoice(profile, invoices = []) {
  const byId = (id) => invoices.find((inv) => inv.id === id);
  if (profile && "card_update_invoice_id" in profile) {
    const id = profile.card_update_invoice_id;
    if (typeof id !== "string" || !id) return null;
    return { id, invoiceNumber: byId(id)?.invoice_number };
  }
  const next = invoices
    .filter((inv) => inv.status === "open" || inv.status === "past_due")
    .sort((a, b) => String(a.due_date || "").localeCompare(String(b.due_date || "")))[0];
  return next ? { id: next.id, invoiceNumber: next.invoice_number } : null;
}
