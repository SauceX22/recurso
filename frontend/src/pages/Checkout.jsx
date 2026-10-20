import { useState, useEffect, useMemo, useRef, useCallback } from "react";
import { useParams } from "react-router";
import { CheckCircle2, AlertCircle, Clock, Loader2 } from "lucide-react";
import { loadStripe } from "@stripe/stripe-js";
import {
  Elements,
  PaymentElement,
  AddressElement,
  useStripe,
  useElements,
} from "@stripe/react-stripe-js";

import { API_ROOT as API_BASE, endpoints } from "../lib/api";
import { formatCurrency } from "@/lib/utils";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/patterns/LoadingSkeleton";
import MoyasarForm from "@/components/checkout/MoyasarForm";
import {
  forgetMoyasarPayment,
  storedMoyasarPayment,
} from "@/components/checkout/moyasarAssets";
import {
  RTL_LOCALES,
  checkoutCopy,
  detectCheckoutLocale,
  verifyErrorMessage,
} from "@/lib/checkoutCopy";
import { loadExternalScript } from "@/lib/useExternalScript";

const RAZORPAY_JS = "https://checkout.razorpay.com/v1/checkout.js";

// readGatewayReturn inspects the URL once, on first render, for a return from
// a gateway redirect. Stripe appends ?payment_intent. Moyasar appends
// ?id=&status=&message= to our callback_url, which carries ?gateway=moyasar;
// the id is also recognised when it equals the payment id the form stored for
// this invoice. The status param is never trusted: the server decides.
function readGatewayReturn(invoiceId) {
  const params = new URLSearchParams(window.location.search);
  const pi = params.get("payment_intent");
  if (pi) return { gateway: "stripe", paymentId: pi };
  const paymentId = params.get("id");
  if (!paymentId) return null;
  const marked = params.get("gateway") === "moyasar";
  if (marked || storedMoyasarPayment(invoiceId) === paymentId) {
    return { gateway: "moyasar", paymentId };
  }
  return null;
}

// verifyReturn asks the server for the outcome of a returned payment and
// resolves { status, paymentStatus }. It throws an axios-style error
// ({ response: { status, data } }, or no response on a network failure).
async function verifyReturn(invoiceId, ret) {
  if (ret.gateway === "moyasar") {
    const res = await endpoints.verifyMoyasarCheckout(invoiceId, {
      payment_id: ret.paymentId,
    });
    const d = res.data?.data || {};
    return { status: d.status, paymentStatus: d.payment_status };
  }
  const res = await fetch(
    `${API_BASE}/checkout/${invoiceId}/success?payment_intent=${encodeURIComponent(ret.paymentId)}`
  );
  const body = await res.json().catch(() => ({}));
  if (!body.data?.status && !res.ok) {
    throw { response: { status: res.status, data: body } };
  }
  return { status: body.data?.status };
}

// PaymentForm renders the Stripe Payment Element and confirms the intent. It
// must live inside <Elements>. On a card success it settles inline; methods
// that need a bank redirect come back to this page's return_url, where the
// parent verifies via ?payment_intent.
function PaymentForm({ payLabel, t, onPaid, onProcessing }) {
  const stripe = useStripe();
  const elements = useElements();
  const { id } = useParams();
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState(null);

  // Verify + settle server-side (the endpoint checks the intent succeeded and
  // that its metadata invoice_id matches this invoice before marking paid).
  const settle = async (paymentIntentId) => {
    const res = await fetch(
      `${API_BASE}/checkout/${id}/success?payment_intent=${encodeURIComponent(paymentIntentId)}`
    );
    const data = await res.json().catch(() => ({}));
    if (res.ok && data.data?.status === "paid") {
      onPaid();
    } else if (data.data?.status === "failed") {
      setError(t.stripeFailed);
    } else {
      onProcessing();
    }
  };

  const handleSubmit = async (e) => {
    e.preventDefault();
    if (!stripe || !elements) return;
    setSubmitting(true);
    setError(null);

    // redirect: "if_required" keeps card payments on-page and only redirects
    // for methods that require it (some banks). return_url brings those back
    // here with ?payment_intent for verification.
    const { error: confirmError, paymentIntent } = await stripe.confirmPayment({
      elements,
      confirmParams: {
        return_url: `${window.location.origin}${window.location.pathname}`,
      },
      redirect: "if_required",
    });

    if (confirmError) {
      setError(confirmError.message || t.stripeFailed);
      setSubmitting(false);
      return;
    }

    if (paymentIntent?.status === "succeeded") {
      await settle(paymentIntent.id);
    } else if (paymentIntent?.status === "processing") {
      // ACH and other delayed-settlement methods: authorized, settles later
      // (the webhook marks the invoice paid once funds clear).
      onProcessing();
    } else {
      setError(t.stripeIncomplete);
    }
    setSubmitting(false);
  };

  return (
    <form onSubmit={handleSubmit} className="space-y-4">
      {/* Billing name + address. Required for India-export (foreign-currency)
          Stripe charges; confirmPayment automatically attaches this element's
          value to the payment method's billing_details. */}
      <AddressElement options={{ mode: "billing" }} />
      <PaymentElement />
      {error && (
        <div
          role="alert"
          className="rounded-lg bg-destructive/5 px-3 py-3 text-sm text-destructive ring-1 ring-inset ring-destructive/20"
        >
          {error}
        </div>
      )}
      <Button
        type="submit"
        disabled={!stripe || submitting}
        size="lg"
        className="w-full"
      >
        {submitting ? t.processingButton : payLabel}
      </Button>
    </form>
  );
}

export default function Checkout() {
  const { id } = useParams();
  const [locale] = useState(detectCheckoutLocale);
  const t = checkoutCopy(locale);

  const [invoice, setInvoice] = useState(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(null);
  const [initiating, setInitiating] = useState(false);

  // A gateway return is read once, so a verifying screen replaces the Pay
  // button from the very first render: a buyer who just paid must never see
  // a live Pay button while the server confirms.
  const [gatewayReturn] = useState(() => readGatewayReturn(id));
  // open | verifying | verify_error | paid | failed | processing
  const [status, setStatus] = useState(() => (gatewayReturn ? "verifying" : "open"));
  const [paymentStatus, setPaymentStatus] = useState(null);
  const [verifyError, setVerifyError] = useState(null);

  // Payment session from POST /pay.
  const [order, setOrder] = useState(null);
  const gateway = order?.gateway || null;

  // Load the invoice for display + paid check.
  useEffect(() => {
    fetch(`${API_BASE}/checkout/${id}`)
      .then((res) => {
        if (!res.ok) throw new Error("Invoice not found");
        return res.json();
      })
      .then((data) => {
        setInvoice(data.data);
        if (data.data.status === "paid") setStatus("paid");
      })
      .catch((err) => setError(err.message))
      .finally(() => setLoading(false));
  }, [id]);

  // aliveRef drops results that arrive after unmount. A plain per-effect
  // `cancelled` flag would also drop the single verify call under StrictMode,
  // whose second effect pass is collapsed by startedRef below.
  const aliveRef = useRef(false);
  useEffect(() => {
    aliveRef.current = true;
    return () => {
      aliveRef.current = false;
    };
  }, []);

  const runVerify = useCallback(() => {
    if (!gatewayReturn) return;
    verifyReturn(id, gatewayReturn)
      .then(({ status: s, paymentStatus: ps }) => {
        if (!aliveRef.current) return;
        if (s === "paid" || s === "failed") {
          // Terminal: drop the payment id from the URL, history and Referer,
          // so a refresh doesn't re-verify.
          window.history.replaceState(null, "", window.location.pathname);
          if (gatewayReturn.gateway === "moyasar") forgetMoyasarPayment(id);
          setStatus(s);
        } else {
          setPaymentStatus(ps || null);
          setStatus("processing");
        }
      })
      .catch((err) => {
        if (!aliveRef.current) return;
        setVerifyError(err);
        setStatus("verify_error");
      });
  }, [id, gatewayReturn]);

  const startedRef = useRef(false);
  useEffect(() => {
    if (!gatewayReturn || startedRef.current) return;
    startedRef.current = true;
    runVerify();
  }, [gatewayReturn, runVerify]);

  const handleCheckAgain = () => {
    setVerifyError(null);
    setStatus("verifying");
    runVerify();
  };

  // Clear the redirect params and payment session for a fresh attempt.
  const handleRetryPayment = () => {
    window.history.replaceState(null, "", window.location.pathname);
    forgetMoyasarPayment(id);
    setOrder(null);
    setError(null);
    setPaymentStatus(null);
    setStatus("open");
  };

  // Load Stripe.js only once we have the publishable key from /pay.
  const publishableKey = order?.publishable_key || null;
  const stripePromise = useMemo(
    () => (publishableKey ? loadStripe(publishableKey) : null),
    [publishableKey]
  );

  const openRazorpay = async (rzpOrder) => {
    if (!rzpOrder) return;
    const ok = await loadExternalScript(RAZORPAY_JS, {
      isReady: () => !!window.Razorpay,
    });
    if (!ok || !window.Razorpay) {
      setError(t.razorpayLoadError);
      return;
    }
    const rzp = new window.Razorpay({
      key: rzpOrder.razorpay_key_id,
      order_id: rzpOrder.order_id,
      amount: rzpOrder.amount,
      currency: rzpOrder.currency,
      name: "Recurso",
      description: `Invoice ${invoice?.invoice_number || ""}`,
      theme: { color: "#10b981" },
      handler: async (resp) => {
        // Verify server-side (signature + order↔invoice bind) then settle.
        let vres;
        try {
          vres = await fetch(`${API_BASE}/checkout/${id}/razorpay/verify`, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({
              razorpay_order_id: resp.razorpay_order_id,
              razorpay_payment_id: resp.razorpay_payment_id,
              razorpay_signature: resp.razorpay_signature,
            }),
          });
        } catch {
          setError(t.errNetwork);
          return;
        }
        const body = await vres.json().catch(() => ({}));
        if (vres.ok && body.data?.status === "paid") {
          setStatus("paid");
        } else {
          setError(
            verifyErrorMessage(
              t,
              { status: vres.status, code: body?.error?.code },
              {
                invoiceNumber: invoice?.invoice_number,
                paymentId: resp.razorpay_payment_id,
              }
            )
          );
        }
      },
    });
    rzp.on("payment.failed", (r) =>
      setError(r?.error?.description || t.paymentFailed)
    );
    rzp.open();
  };

  const handleInitiate = async () => {
    setInitiating(true);
    setError(null);
    try {
      const res = await fetch(`${API_BASE}/checkout/${id}/pay`, {
        method: "POST",
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        if (data.error?.code === "invoice_already_paid") {
          setStatus("paid");
          return;
        }
        setError(res.status === 503 ? t.notConfigured : t.errStart);
        return;
      }
      setOrder(data.data);
      if (data.data.gateway === "razorpay") openRazorpay(data.data);
    } catch {
      setError(t.errStart);
    } finally {
      setInitiating(false);
    }
  };

  if (loading) {
    return (
      <CheckoutShell locale={locale}>
        <div role="status" aria-busy="true" aria-label={t.loading} className="space-y-4">
          <Skeleton className="mx-auto h-12 w-12 rounded-xl" />
          <Skeleton className="mx-auto h-6 w-32" />
          <Skeleton className="h-32 w-full rounded-xl" />
          <Skeleton className="h-11 w-full" />
        </div>
      </CheckoutShell>
    );
  }

  if (error && !invoice) {
    return (
      <CheckoutShell locale={locale}>
        <StatusScreen tone="destructive" title={t.notFoundTitle}>
          {t.notFoundBody}
        </StatusScreen>
      </CheckoutShell>
    );
  }

  const invoiceNumber = invoice?.invoice_number;

  if (status === "paid") {
    return (
      <CheckoutShell locale={locale}>
        <StatusScreen tone="success" title={t.paidTitle}>
          {t.paidBody(invoiceNumber)}
          <span className="mt-4 block text-xs text-subtle">{t.closePage}</span>
        </StatusScreen>
      </CheckoutShell>
    );
  }

  if (status === "failed") {
    return (
      <CheckoutShell locale={locale}>
        <StatusScreen
          tone="destructive"
          title={t.failedTitle}
          actions={
            <Button onClick={handleRetryPayment} size="lg" className="w-full">
              {t.tryAgain}
            </Button>
          }
        >
          {t.failedBody(invoiceNumber)}
        </StatusScreen>
      </CheckoutShell>
    );
  }

  if (status === "verifying") {
    return (
      <CheckoutShell locale={locale}>
        <StatusScreen tone="pending" title={t.verifyingTitle} busy>
          {t.verifyingBody}
        </StatusScreen>
      </CheckoutShell>
    );
  }

  if (status === "verify_error") {
    return (
      <CheckoutShell locale={locale}>
        <StatusScreen
          tone="destructive"
          title={t.verifyErrorTitle}
          alert
          actions={
            <Button onClick={handleCheckAgain} size="lg" className="w-full">
              {t.checkAgain}
            </Button>
          }
        >
          {verifyErrorMessage(t, verifyError, {
            invoiceNumber,
            paymentId: gatewayReturn?.paymentId,
          })}
        </StatusScreen>
      </CheckoutShell>
    );
  }

  if (status === "processing") {
    // Only a bank debit that Stripe accepted is "received". A card payment
    // still waiting on the bank (Moyasar `initiated`: 3DS not completed) has
    // taken no money, so it says so and offers a way forward.
    const cardPending = gatewayReturn?.gateway === "moyasar";
    return (
      <CheckoutShell locale={locale}>
        {cardPending ? (
          <StatusScreen
            tone="warning"
            title={t.unconfirmedTitle}
            actions={
              <div className="space-y-2">
                {paymentStatus === "initiated" && (
                  <Button onClick={handleRetryPayment} size="lg" className="w-full">
                    {t.tryAgain}
                  </Button>
                )}
                <Button
                  onClick={handleCheckAgain}
                  size="lg"
                  variant={paymentStatus === "initiated" ? "outline" : "default"}
                  className="w-full"
                >
                  {t.checkAgain}
                </Button>
              </div>
            }
          >
            {paymentStatus === "initiated"
              ? t.initiatedBody(invoiceNumber)
              : t.pendingBody(invoiceNumber)}
          </StatusScreen>
        ) : (
          <StatusScreen tone="warning" title={t.processingTitle}>
            {t.processingBankBody(invoiceNumber)}
          </StatusScreen>
        )}
      </CheckoutShell>
    );
  }

  // What the buyer owes now: the total less payments and credits already
  // applied. Older servers don't send amount_due; fall back to the total.
  const amountDue =
    typeof invoice.amount_due === "number" ? invoice.amount_due : invoice.total;
  const settledPart = invoice.total - amountDue;
  const money = (v) => formatCurrency(v, invoice.currency);
  // Once a gateway order exists, its button shows the amount that order will
  // actually charge.
  const orderPayLabel = t.pay(money(typeof order?.amount === "number" ? order.amount : amountDue));

  const clientSecret = order?.client_secret || null;
  const showStripeForm = gateway === "stripe" && clientSecret && stripePromise;

  return (
    <CheckoutShell locale={locale}>
      <div className="mb-6 text-center">
        <div className="mx-auto mb-4 flex h-12 w-12 items-center justify-center rounded-xl bg-success/50 text-2xl font-bold text-white">
          R
        </div>
        <h1 className="text-2xl font-semibold tracking-tight text-foreground">
          {t.checkoutTitle}
        </h1>
      </div>

      <div className="mb-6 space-y-3 rounded-xl border border-border bg-muted p-4">
        <SummaryRow label={t.invoice} value={invoiceNumber} strong />
        {invoice.subtotal !== invoice.total && (
          <>
            <SummaryRow label={t.subtotal} value={money(invoice.subtotal)} />
            <SummaryRow label={t.tax} value={money(invoice.tax_amount)} />
            <div className="border-t border-border pt-2" />
          </>
        )}
        {settledPart !== 0 && (
          <>
            <SummaryRow label={t.total} value={money(invoice.total)} />
            <SummaryRow label={t.paidOrCredited} value={money(-settledPart)} />
          </>
        )}
        <div className="flex items-center justify-between">
          <span className="text-sm text-muted-foreground">
            {settledPart !== 0 ? t.amountDue : t.total}
          </span>
          <span
            data-testid="amount-due"
            className="text-lg font-bold tabular-nums text-foreground"
          >
            {money(amountDue)}
          </span>
        </div>
        <SummaryRow label={t.dueDate} value={invoice.due_date} />
      </div>

      {error && (
        <div
          role="alert"
          className="mb-4 rounded-lg bg-destructive/5 px-3 py-3 text-sm text-destructive ring-1 ring-inset ring-destructive/20"
        >
          {error}
        </div>
      )}

      {showStripeForm ? (
        <Elements stripe={stripePromise} options={{ clientSecret }}>
          <PaymentForm
            payLabel={orderPayLabel}
            t={t}
            onPaid={() => setStatus("paid")}
            onProcessing={() => setStatus("processing")}
          />
        </Elements>
      ) : gateway === "stripe" ? (
        // gateway said "stripe" but the key or client_secret is missing —
        // showing the Pay button again would mint a new PaymentIntent per
        // click with no form ever appearing.
        <div className="rounded-lg bg-warning/5 px-3 py-3 text-sm text-warning ring-1 ring-inset ring-warning/20">
          {t.notConfigured}
        </div>
      ) : gateway === "razorpay" ? (
        <div className="space-y-3">
          <Button onClick={() => openRazorpay(order)} size="lg" className="w-full">
            {orderPayLabel}
          </Button>
          <p className="text-center text-xs text-subtle">{t.razorpayNote}</p>
        </div>
      ) : gateway === "moyasar" ? (
        <MoyasarForm order={order} invoiceId={id} locale={locale} t={t} />
      ) : gateway ? (
        <div className="rounded-lg bg-warning/5 px-3 py-3 text-sm text-warning ring-1 ring-inset ring-warning/20">
          {t.unsupportedCurrency(invoice.currency)}
        </div>
      ) : (
        <Button
          onClick={handleInitiate}
          disabled={initiating}
          size="lg"
          className="w-full"
        >
          {initiating ? t.starting : t.pay(money(amountDue))}
        </Button>
      )}

      <p className="mt-4 text-center text-xs text-subtle">{t.poweredBy}</p>
    </CheckoutShell>
  );
}

function CheckoutShell({ locale = "en", children }) {
  return (
    <div
      lang={locale}
      dir={RTL_LOCALES.has(locale) ? "rtl" : "ltr"}
      className="flex min-h-screen items-center justify-center bg-muted p-4"
    >
      <div className="w-full max-w-md rounded-2xl border border-border bg-white p-8 shadow-sm">
        {children}
      </div>
    </div>
  );
}

const TONES = {
  success: { Icon: CheckCircle2, ring: "bg-success/5", icon: "text-success" },
  destructive: { Icon: AlertCircle, ring: "bg-destructive/5", icon: "text-destructive" },
  warning: { Icon: Clock, ring: "bg-warning/5", icon: "text-warning" },
  pending: { Icon: Loader2, ring: "bg-muted", icon: "animate-spin text-subtle" },
};

// StatusScreen is the single-message state of the checkout (paid, failed,
// verifying, ...). Focus moves to its heading so screen-reader users hear the
// new state.
function StatusScreen({ tone, title, children, actions, busy, alert }) {
  const headingRef = useRef(null);
  useEffect(() => {
    headingRef.current?.focus();
  }, [title]);
  const { Icon, ring, icon } = TONES[tone];
  return (
    <div
      className="text-center"
      role={alert ? "alert" : busy ? "status" : undefined}
      aria-busy={busy || undefined}
    >
      <div className={`mx-auto mb-4 flex h-14 w-14 items-center justify-center rounded-full ${ring}`}>
        <Icon className={`h-7 w-7 ${icon}`} />
      </div>
      <h1
        ref={headingRef}
        tabIndex={-1}
        className="text-2xl font-semibold tracking-tight text-foreground outline-none"
      >
        {title}
      </h1>
      <p className="mt-2 text-sm text-muted-foreground">{children}</p>
      {actions && <div className="mt-6">{actions}</div>}
    </div>
  );
}

function SummaryRow({ label, value, strong }) {
  return (
    <div className="flex justify-between">
      <span className="text-sm text-muted-foreground">{label}</span>
      <span
        className={
          strong
            ? "text-sm font-semibold text-foreground"
            : "text-sm tabular-nums text-foreground"
        }
      >
        {value}
      </span>
    </div>
  );
}
