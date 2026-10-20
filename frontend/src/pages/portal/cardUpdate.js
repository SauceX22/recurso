export const CARD_UPDATE_VIA_INVOICE =
  "To update your card, pay your next open invoice with the new card.";

// cardUpdateViaInvoice reports whether the customer's saved card belongs to a
// gateway that only saves a card during a payment. The portal can't start a
// card setup for those (a Stripe setup would move renewals to Stripe), so the
// card is replaced by paying an open invoice.
export function cardUpdateViaInvoice(profile) {
  if (typeof profile?.card_update_supported === "boolean") {
    return !profile.card_update_supported;
  }
  return profile?.payment_method_provider === "moyasar";
}
