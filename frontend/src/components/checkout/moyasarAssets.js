// Moyasar's embedded payment form, pinned. The form renders card inputs in our
// DOM, so both files carry SRI hashes: bumping the version without new hashes
// makes the browser refuse to load them. Regenerate with
//   curl -sL <url> | openssl dgst -sha384 -binary | openssl base64 -A
export const MOYASAR_FORM_VERSION = "2.3.0";
const BASE = `https://cdn.jsdelivr.net/npm/moyasar-payment-form@${MOYASAR_FORM_VERSION}/dist`;

export const MOYASAR_JS = `${BASE}/moyasar.umd.min.js`;
export const MOYASAR_JS_INTEGRITY =
  "sha384-1EtqiHfTdzVtlk1ChJ9HeyrZU7ScCc9S+vN8eiUDIeJmcv7kx5TJG0dWoxQOcVJB";

export const MOYASAR_CSS = `${BASE}/moyasar.css`;
export const MOYASAR_CSS_INTEGRITY =
  "sha384-kXA6TcBKVzqpjpsbaJciletoxOQ3kNhUKCCMPpZaXA2MOhJMTaqVspZdFxXo5iTA";

// The payment id Moyasar creates is kept per invoice, so the return from the
// form (or its 3DS redirect) is recognised by a value we stored rather than by
// the shape of the query string.
export const moyasarPaymentKey = (invoiceId) => `moyasar:${invoiceId}`;

export function rememberMoyasarPayment(invoiceId, paymentId) {
  try {
    sessionStorage.setItem(moyasarPaymentKey(invoiceId), paymentId);
  } catch {
    // Storage can be disabled; the ?gateway=moyasar marker still works.
  }
}

export function storedMoyasarPayment(invoiceId) {
  try {
    return sessionStorage.getItem(moyasarPaymentKey(invoiceId));
  } catch {
    return null;
  }
}

export function forgetMoyasarPayment(invoiceId) {
  try {
    sessionStorage.removeItem(moyasarPaymentKey(invoiceId));
  } catch {
    // nothing to clean up
  }
}
