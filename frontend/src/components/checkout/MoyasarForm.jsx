import { useEffect, useId, useRef, useState } from "react";
import * as Sentry from "@sentry/react";

import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/patterns/LoadingSkeleton";
import { saveCardConsentText } from "@/lib/checkoutCopy";
import { loadStylesheet, useExternalScript } from "@/lib/useExternalScript";
import {
  MOYASAR_CSS,
  MOYASAR_CSS_INTEGRITY,
  MOYASAR_JS,
  MOYASAR_JS_INTEGRITY,
  rememberMoyasarPayment,
} from "./moyasarAssets";
import "./MoyasarForm.css";

const DEFAULT_METHODS = ["creditcard"];
const DEFAULT_NETWORKS = ["mada", "visa", "mastercard"];

const moyasarReady = () => !!window.Moyasar;

/**
 * MoyasarForm mounts Moyasar's embedded card form for one /pay order.
 *
 * The server decides everything that matters: the amount (the invoice's amount
 * due), the invoice binding in metadata, and whether the card is saved for
 * renewals (`save_card`, true only for auto-renewing subscriptions). When it
 * is saved, the buyer sees the consent text before paying. The browser never
 * marks anything paid; the return to callback_url is verified server-side.
 */
export default function MoyasarForm({ order, invoiceId, locale, t }) {
  const containerRef = useRef(null);
  const consentId = useId();
  const initedRef = useRef(false);
  const [formError, setFormError] = useState(null);
  // initFailed: Moyasar.init refused the config. attempt re-runs the init
  // effect when the buyer retries.
  const [initFailed, setInitFailed] = useState(false);
  const [attempt, setAttempt] = useState(0);

  const invoiceBinding = order.metadata?.invoice_id;
  const saveCard = order.save_card === true;

  const { status, retry } = useExternalScript(MOYASAR_JS, {
    integrity: MOYASAR_JS_INTEGRITY,
    isReady: moyasarReady,
    enabled: !!invoiceBinding,
  });

  useEffect(() => {
    if (invoiceBinding) loadStylesheet(MOYASAR_CSS, { integrity: MOYASAR_CSS_INTEGRITY });
  }, [invoiceBinding]);

  useEffect(() => {
    // init clears the container and renders the card inputs into it. The guard
    // stops a parent re-render (a new `order` or `t` object) from wiping a
    // half-filled form and mounting it again.
    if (status !== "ready" || initedRef.current) return;
    const el = containerRef.current;
    if (!el || !window.Moyasar) return;
    initedRef.current = true;
    el.innerHTML = "";
    try {
      window.Moyasar.init({
        element: el,
        amount: order.amount,
        currency: order.currency,
        description: order.description,
        publishable_api_key: order.moyasar_publishable_key,
        callback_url:
          order.callback_url ||
          `${window.location.origin}/checkout/${encodeURIComponent(invoiceId)}?gateway=moyasar`,
        methods: order.methods || DEFAULT_METHODS,
        supported_networks: order.supported_networks || DEFAULT_NETWORKS,
        metadata: { invoice_id: invoiceBinding },
        language: locale === "ar" ? "ar" : "en",
        credit_card: { save_card: saveCard },
        // Moyasar rejects any callback that isn't a native async function (it
        // checks the constructor name or the "async" source prefix). Keep them
        // async, and never add a build transform that down-levels async.
        on_initiating: async () => {
          setFormError(null);
          return true;
        },
        // Fires once the payment exists, before any 3DS redirect.
        on_completed: async (payment) => {
          if (payment?.id) rememberMoyasarPayment(invoiceId, payment.id);
        },
        on_failure: async () => setFormError(t.cardFailed),
      });
    } catch (err) {
      // A config the library refuses must not crash the page: show the
      // load-error state and let Retry init again.
      initedRef.current = false;
      // The state records the outcome of the external init call this effect
      // just made; there is no render-time way to know it.
      // eslint-disable-next-line react-hooks/set-state-in-effect
      setInitFailed(true);
      Sentry.captureException(err);
    }
  }, [status, attempt, order, invoiceId, invoiceBinding, locale, saveCard, t]);

  const handleRetry = () => {
    if (!initFailed) return retry();
    setInitFailed(false);
    setAttempt((n) => n + 1);
  };

  // The server binds the payment to the invoice through this metadata; without
  // it the payment could never settle, so don't take the card at all.
  if (!invoiceBinding) {
    return (
      <div
        role="alert"
        className="rounded-lg bg-warning/5 px-3 py-3 text-sm text-warning ring-1 ring-inset ring-warning/20"
      >
        {t.notConfigured}
      </div>
    );
  }

  if (status === "error" || initFailed) {
    return (
      <div className="space-y-3">
        <div
          role="alert"
          className="rounded-lg bg-destructive/5 px-3 py-3 text-sm text-destructive ring-1 ring-inset ring-destructive/20"
        >
          {t.cardFormLoadError}
        </div>
        <Button onClick={handleRetry} size="lg" variant="outline" className="w-full">
          {t.retry}
        </Button>
      </div>
    );
  }

  return (
    <div className="space-y-3">
      {saveCard && (
        <p
          id={consentId}
          className="rounded-lg bg-muted px-3 py-3 text-sm text-foreground ring-1 ring-inset ring-border"
        >
          {saveCardConsentText(t, order.card_on_file)}
        </p>
      )}
      {formError && (
        <div
          role="alert"
          className="rounded-lg bg-destructive/5 px-3 py-3 text-sm text-destructive ring-1 ring-inset ring-destructive/20"
        >
          {formError}
        </div>
      )}
      {status !== "ready" && (
        <div role="status" aria-busy="true" aria-label={t.cardFormLoading}>
          <Skeleton className="h-64 w-full" />
        </div>
      )}
      <div
        ref={containerRef}
        className="mysr-form"
        aria-label={t.cardForm}
        aria-describedby={saveCard ? consentId : undefined}
        data-own-consent={saveCard || undefined}
        hidden={status !== "ready"}
      />
    </div>
  );
}
