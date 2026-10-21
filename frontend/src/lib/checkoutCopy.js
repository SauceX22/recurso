// Buyer-facing copy for the public checkout page, per locale. English is the
// source; Arabic covers buyers whose browser prefers it. Gateways that render
// their own UI (Moyasar) receive the same locale.

import { formatCurrency } from "@/lib/utils";

export const RTL_LOCALES = new Set(["ar"]);

// detectCheckoutLocale picks the checkout language from the browser's
// preferred language. Unknown languages fall back to English.
export function detectCheckoutLocale(nav = typeof navigator !== "undefined" ? navigator : undefined) {
  const preferred = nav?.languages?.[0] || nav?.language || "";
  return preferred.toLowerCase().startsWith("ar") ? "ar" : "en";
}

// isolate wraps a left-to-right value (an amount, a Latin name) in Unicode
// first-strong isolates so it keeps its own direction inside Arabic text:
// "-SAR 20.00" must not render with the minus on the far side.
export const isolate = (s) => `⁨${s}⁩`;

const EN_UNITS = {
  day: ["day", "days"],
  week: ["week", "weeks"],
  month: ["month", "months"],
  year: ["year", "years"],
};

const AR_UNITS = {
  day: ["يوم", "يومين", "أيام", "يومًا"],
  week: ["أسبوع", "أسبوعين", "أسابيع", "أسبوعًا"],
  month: ["شهر", "شهرين", "أشهر", "شهرًا"],
  year: ["سنة", "سنتين", "سنوات", "سنة"],
};

const UNIT_ALIASES = { daily: "day", weekly: "week", monthly: "month", yearly: "year", annual: "year", annually: "year" };

const en = {
  loading: "Loading checkout",
  notFoundTitle: "Invoice not found",
  notFoundBody: "Check the payment link in your email, or contact the sender.",
  checkoutTitle: "Checkout",
  invoice: "Invoice",
  subtotal: "Subtotal",
  tax: "Tax",
  total: "Total",
  paidOrCredited: "Paid or credited",
  amountDue: "Amount due",
  dueDate: "Due date",
  pay: (amount) => `Pay ${amount}`,
  starting: "Starting...",
  processingButton: "Processing...",
  poweredBy: "Powered by Recurso",

  paidTitle: "Payment successful",
  paidBody: (n) => `Invoice ${n} has been paid.`,
  closePage: "You can close this page.",
  failedTitle: "Payment not completed",
  failedBody: (n) =>
    `Your payment for invoice ${n} was declined or cancelled. You have not been charged.`,
  tryAgain: "Try again",
  processingTitle: "Payment processing",
  processingBankBody: (n) =>
    `We've received your payment for invoice ${n}. Bank payments (like ACH) take a few business days to clear — you'll get a receipt once it settles. No further action is needed.`,
  unconfirmedTitle: "Payment not confirmed yet",
  initiatedBody: (n) =>
    `Your bank hasn't confirmed the payment for invoice ${n}. If you closed the verification step, try again.`,
  pendingBody: (n) =>
    `Your bank is still confirming the payment for invoice ${n}. Check again in a minute. Checking doesn't charge you again.`,
  checkAgain: "Check again",
  verifyingTitle: "Confirming your payment",
  verifyingBody: "This takes a few seconds. Keep this page open.",
  verifyErrorTitle: "We couldn't confirm your payment",

  errNetwork:
    "We couldn't reach the server to confirm your payment. Check your connection, then check again. Checking doesn't charge you again.",
  errMismatch: (n, ref) =>
    `This payment doesn't match invoice ${n}. If you were charged, contact the sender and quote payment reference ${ref}.`,
  errNotConfirmed: (n, ref) =>
    `We couldn't confirm this payment for invoice ${n}. If you were charged, contact the sender and quote payment reference ${ref}.`,
  errSettle:
    "Your payment went through, but we couldn't record it yet. Check again in a minute. You won't be charged twice.",
  errNotFound: "We couldn't find this invoice. Check the payment link in your email, or contact the sender.",
  errUnavailable:
    "Card payments aren't available for this invoice right now. Contact the sender to arrange payment.",
  errProvider:
    "We couldn't confirm the payment with the payment provider yet. Check again in a minute. Checking doesn't charge you again.",
  errStart: "We couldn't start the payment. Retry, or contact the sender if it keeps failing.",

  notConfigured:
    "This checkout isn't fully configured. Please contact the sender to arrange payment.",
  unsupportedCurrency: (c) =>
    `Self-serve checkout for ${c} isn't available here yet. Please contact the sender to arrange payment.`,
  razorpayNote:
    "A secure Razorpay window opens to complete your payment (UPI, cards, netbanking).",
  razorpayLoadError: "The payment window didn't load. Check your connection and retry.",
  paymentFailed: "The payment didn't go through. Try again or use another method.",
  stripeFailed: "Payment could not be completed. Please try another method.",
  stripeIncomplete: "Payment is not complete. Please try another method.",

  saveCardConsent:
    "By paying, you agree that your card will be saved and charged automatically for future renewals of this subscription.",
  cardOnFileConsent: ({ merchant, amount, period }) =>
    `By paying, you authorise ${merchant} to save this card and charge it ${amount} ${period} until you cancel. This card replaces any card already saved for this subscription. You can cancel at any time from your billing portal.`,
  every: (unit, count) => {
    const [one, many] = EN_UNITS[unit];
    return count === 1 ? `every ${one}` : `every ${count} ${many}`;
  },
  cardForm: "Card details",
  cardFormLoading: "Loading the card form",
  cardFormLoadError: "The card form didn't load. Check your connection and retry.",
  retry: "Retry",
  cardFailed: "The card payment didn't go through. Check the card details or use another card.",
};

const ar = {
  loading: "جارٍ تحميل صفحة الدفع",
  notFoundTitle: "لم يتم العثور على الفاتورة",
  notFoundBody: "تحقق من رابط الدفع في بريدك الإلكتروني، أو تواصل مع المرسل.",
  checkoutTitle: "الدفع",
  invoice: "الفاتورة",
  subtotal: "المجموع الفرعي",
  tax: "الضريبة",
  total: "الإجمالي",
  paidOrCredited: "المدفوع أو المخصوم",
  amountDue: "المبلغ المستحق",
  dueDate: "تاريخ الاستحقاق",
  pay: (amount) => `ادفع ${amount}`,
  starting: "جارٍ البدء...",
  processingButton: "جارٍ المعالجة...",
  poweredBy: "Powered by Recurso",

  paidTitle: "تم الدفع بنجاح",
  paidBody: (n) => `تم دفع الفاتورة ${n}.`,
  closePage: "يمكنك إغلاق هذه الصفحة.",
  failedTitle: "لم تكتمل عملية الدفع",
  failedBody: (n) => `تم رفض أو إلغاء الدفع للفاتورة ${n}. لم يتم خصم أي مبلغ منك.`,
  tryAgain: "حاول مرة أخرى",
  processingTitle: "الدفع قيد المعالجة",
  processingBankBody: (n) =>
    `استلمنا دفعتك للفاتورة ${n}. تستغرق التحويلات البنكية بضعة أيام عمل، وسيصلك إيصال عند اكتمالها. لا يلزمك أي إجراء آخر.`,
  unconfirmedTitle: "لم يتم تأكيد الدفع بعد",
  initiatedBody: (n) =>
    `لم يؤكد البنك الدفع للفاتورة ${n}. إذا أغلقت خطوة التحقق، فحاول مرة أخرى.`,
  pendingBody: (n) =>
    `لا يزال البنك يؤكد الدفع للفاتورة ${n}. تحقق مرة أخرى بعد دقيقة. التحقق لا يخصم أي مبلغ إضافي.`,
  checkAgain: "تحقق مرة أخرى",
  verifyingTitle: "جارٍ تأكيد الدفع",
  verifyingBody: "يستغرق ذلك بضع ثوانٍ. أبقِ هذه الصفحة مفتوحة.",
  verifyErrorTitle: "تعذر تأكيد الدفع",

  errNetwork:
    "تعذر الاتصال بالخادم لتأكيد الدفع. تحقق من اتصالك ثم تحقق مرة أخرى. التحقق لا يخصم أي مبلغ إضافي.",
  errMismatch: (n, ref) =>
    `هذا الدفع لا يطابق الفاتورة ${n}. إذا تم خصم المبلغ منك، فتواصل مع المرسل واذكر مرجع الدفع ${ref}.`,
  errNotConfirmed: (n, ref) =>
    `تعذر تأكيد عملية الدفع هذه للفاتورة ${isolate(n)}. إذا تم خصم المبلغ منك، فتواصل مع الجهة المُصدِرة للفاتورة واذكر مرجع الدفع ${isolate(ref)}.`,
  errSettle:
    "تمت عملية الدفع، لكن تعذر تسجيلها حتى الآن. تحقق مرة أخرى بعد دقيقة. لن يُخصم المبلغ منك مرتين.",
  errNotFound: "لم نتمكن من العثور على هذه الفاتورة. تحقق من رابط الدفع في بريدك الإلكتروني، أو تواصل مع المرسل.",
  errUnavailable: "الدفع بالبطاقة غير متاح لهذه الفاتورة حاليًا. تواصل مع المرسل لترتيب الدفع.",
  errProvider:
    "تعذر تأكيد الدفع مع مزود الدفع حتى الآن. تحقق مرة أخرى بعد دقيقة. التحقق لا يخصم أي مبلغ إضافي.",
  errStart: "تعذر بدء عملية الدفع. حاول مرة أخرى، أو تواصل مع المرسل إذا تكررت المشكلة.",

  notConfigured: "صفحة الدفع هذه غير مكتملة الإعداد. يرجى التواصل مع المرسل لترتيب الدفع.",
  unsupportedCurrency: (c) =>
    `الدفع الذاتي بعملة ${c} غير متاح هنا بعد. يرجى التواصل مع المرسل لترتيب الدفع.`,
  razorpayNote: "ستفتح نافذة Razorpay آمنة لإتمام الدفع (UPI، البطاقات، الخدمات البنكية).",
  razorpayLoadError: "لم يتم تحميل نافذة الدفع. تحقق من اتصالك وحاول مرة أخرى.",
  paymentFailed: "لم تتم عملية الدفع. حاول مرة أخرى أو استخدم وسيلة أخرى.",
  stripeFailed: "تعذر إتمام الدفع. يرجى تجربة وسيلة دفع أخرى.",
  stripeIncomplete: "لم يكتمل الدفع. يرجى تجربة وسيلة دفع أخرى.",

  saveCardConsent:
    "بإتمام الدفع، توافق على حفظ بطاقتك وخصم مبالغ التجديدات القادمة لهذا الاشتراك منها تلقائيًا.",
  cardOnFileConsent: ({ merchant, amount, period }) =>
    `بإتمام الدفع، تفوّض ${isolate(merchant)} بحفظ هذه البطاقة وخصم ${isolate(amount)} منها ${period} حتى تلغي الاشتراك. تحل هذه البطاقة محل أي بطاقة محفوظة سابقًا لهذا الاشتراك. يمكنك الإلغاء في أي وقت من بوابة الفوترة.`,
  // Arabic counts take the dual for 2, the plural for 3–10 and the
  // accusative singular from 11.
  every: (unit, count) => {
    const [one, two, few, many] = AR_UNITS[unit];
    if (count === 1) return `كل ${one}`;
    if (count === 2) return `كل ${two}`;
    return `كل ${count} ${count <= 10 ? few : many}`;
  },
  cardForm: "بيانات البطاقة",
  cardFormLoading: "جارٍ تحميل نموذج البطاقة",
  cardFormLoadError: "لم يتم تحميل نموذج البطاقة. تحقق من اتصالك وحاول مرة أخرى.",
  retry: "إعادة المحاولة",
  cardFailed: "لم تتم عملية الدفع بالبطاقة. تحقق من بيانات البطاقة أو استخدم بطاقة أخرى.",
};

const COPY = { en, ar };

export function checkoutCopy(locale) {
  return COPY[locale] || en;
}

// saveCardConsentText is the stored-credential consent shown before a payment
// that saves the card. `terms` is the server's card_on_file object
// ({ merchant, amount, currency, interval, interval_count }); it names who
// charges, how much and how often. Anything missing or unrecognised falls
// back to the generic sentence rather than guessing.
export function saveCardConsentText(t, terms) {
  const merchant = typeof terms?.merchant === "string" ? terms.merchant.trim() : "";
  const raw = typeof terms?.interval === "string" ? terms.interval.toLowerCase() : "";
  const unit = UNIT_ALIASES[raw] || raw;
  const count = terms?.interval_count ?? 1;
  if (
    !merchant ||
    !Number.isInteger(terms?.amount) ||
    terms.amount <= 0 ||
    !terms?.currency ||
    !EN_UNITS[unit] ||
    !Number.isInteger(count) ||
    count < 1
  ) {
    return t.saveCardConsent;
  }
  return t.cardOnFileConsent({
    merchant,
    amount: formatCurrency(terms.amount, terms.currency),
    period: t.every(unit, count),
  });
}

// verifyErrorMessage maps a failed confirmation call to buyer copy with a next
// action. `err` is an axios-style error ({ response: { status, data } }) or a
// plain { status, code } object; a missing response means the network failed.
export function verifyErrorMessage(t, err, { invoiceNumber, paymentId } = {}) {
  const { status, code } = verifyErrorParts(err);
  if (!status) return t.errNetwork;
  if (status === 503) return t.errUnavailable;
  if (status === 404 || code === "not_found") return t.errNotFound;
  if (code === "payment_mismatch") return t.errMismatch(invoiceNumber || "", paymentId || "");
  // Any other refusal (a bad signature, a malformed request) isn't proof of
  // a mismatch, only that this payment couldn't be confirmed.
  if (status >= 400 && status < 500) return t.errNotConfirmed(invoiceNumber || "", paymentId || "");
  // The provider reported the payment, but recording it failed on our side.
  if (status === 500) return t.errSettle;
  return t.errProvider;
}

// verifyErrorRetryable reports whether checking again could give a different
// answer: a network failure, or a server or provider error other than the
// gateway being switched off.
export function verifyErrorRetryable(err) {
  const { status } = verifyErrorParts(err);
  return !status || (status >= 500 && status !== 503);
}

function verifyErrorParts(err) {
  return {
    status: err?.response?.status ?? err?.status,
    code: err?.response?.data?.error?.code ?? err?.code,
  };
}
