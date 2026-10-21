import { StrictMode } from "react";
import { render, screen, waitFor, cleanup, fireEvent, act } from "@testing-library/react";
import { MemoryRouter, Routes, Route } from "react-router";
import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import axios from "axios";

import Checkout from "../Checkout";
import { API_ROOT, endpoints } from "../../lib/api";
import {
  MOYASAR_CSS,
  MOYASAR_CSS_INTEGRITY,
  MOYASAR_JS,
  MOYASAR_JS_INTEGRITY,
  moyasarPaymentKey,
} from "../../components/checkout/moyasarAssets";

vi.mock("@stripe/stripe-js", () => ({ loadStripe: vi.fn(() => Promise.resolve(null)) }));
vi.mock("@stripe/react-stripe-js", () => ({
  Elements: ({ children }) => <div>{children}</div>,
  PaymentElement: () => <div data-testid="payment-element" />,
  AddressElement: () => null,
  useStripe: () => null,
  useElements: () => null,
}));

const INVOICE_ID = "11111111-2222-3333-4444-555555555555";
const PAYMENT_ID = "79cced57-9deb-4c4b-8f48-59c124f79688";

// A partly paid invoice: the total is 100.00 but only 80.00 is still owed, so
// every assertion on the visible amount proves which number the page uses.
const invoice = {
  id: INVOICE_ID,
  invoice_number: "INV-42",
  status: "open",
  currency: "SAR",
  subtotal: 10000,
  tax_amount: 0,
  total: 10000,
  amount_due: 8000,
  due_date: "2026-11-01",
};

const payResponse = (over = {}) => ({
  order_id: "ord_1",
  amount: 8000,
  currency: "SAR",
  invoice_id: INVOICE_ID,
  invoice_number: "INV-42",
  gateway: "moyasar",
  client_secret: "",
  moyasar_publishable_key: "pk_test_abc",
  description: "Invoice INV-42",
  metadata: { invoice_id: INVOICE_ID },
  save_card: true,
  ...over,
});

// GET /checkout/:id returns the invoice; POST /checkout/:id/pay returns `pay`.
const mockFetch = (pay, inv = invoice) => {
  const fetchMock = vi.fn((url, opts) => {
    if (opts?.method === "POST") {
      return Promise.resolve({ ok: true, json: () => Promise.resolve({ data: pay }) });
    }
    return Promise.resolve({ ok: true, json: () => Promise.resolve({ data: inv }) });
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
};

const tree = (search) => (
  <MemoryRouter initialEntries={[`/checkout/${INVOICE_ID}${search}`]}>
    <Routes>
      <Route path="/checkout/:id" element={<Checkout />} />
    </Routes>
  </MemoryRouter>
);

const renderAt = (search = "", { strict = false } = {}) => {
  window.history.replaceState(null, "", `/checkout/${INVOICE_ID}${search}`);
  return render(strict ? <StrictMode>{tree(search)}</StrictMode> : tree(search));
};

// The Pay CTA carries the amount due, so match on it rather than any "Pay…".
const PAY_DUE = /pay sar\s?80\.00/i;

const startPayment = async () => {
  fireEvent.click(await screen.findByRole("button", { name: PAY_DUE }));
};

const moyasarScript = () => document.querySelector(`script[src="${MOYASAR_JS}"]`);

// Simulates the CDN script finishing: the global appears, then `load` fires.
const loadMoyasar = async (init = vi.fn()) => {
  await waitFor(() => expect(moyasarScript()).not.toBeNull());
  window.Moyasar = { init };
  moyasarScript().dispatchEvent(new Event("load"));
  return init;
};

const removeMoyasarTags = () => {
  document.querySelectorAll(`script[src="${MOYASAR_JS}"]`).forEach((s) => s.remove());
  document.querySelectorAll(`link[href="${MOYASAR_CSS}"]`).forEach((l) => l.remove());
};

const never = () => new Promise(() => {});

describe("Checkout — Moyasar embedded form", () => {
  beforeEach(() => {
    delete window.Moyasar;
    removeMoyasarTags();
    sessionStorage.clear();
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    delete window.Moyasar;
    removeMoyasarTags();
    sessionStorage.clear();
    window.history.replaceState(null, "", "/");
  });

  it("shows the amount due, not the total, and charges exactly that", async () => {
    mockFetch(payResponse());
    renderAt();
    expect(await screen.findByTestId("amount-due")).toHaveTextContent(/80\.00/);
    expect(screen.getByText(/amount due/i)).toBeInTheDocument();
    expect(screen.getByText(/paid or credited/i)).toBeInTheDocument();
    await startPayment();
    const init = await loadMoyasar();
    await waitFor(() => expect(init).toHaveBeenCalledTimes(1));
    expect(init.mock.calls[0][0].amount).toBe(8000);
  });

  it("falls back to the total when the server doesn't send amount_due", async () => {
    const legacy = { ...invoice };
    delete legacy.amount_due;
    mockFetch(payResponse({ amount: 10000 }), legacy);
    renderAt();
    expect(await screen.findByTestId("amount-due")).toHaveTextContent(/100\.00/);
    expect(screen.queryByText(/paid or credited/i)).not.toBeInTheDocument();
  });

  it("mounts the form on its own element with the server's binding, save_card and SRI assets", async () => {
    mockFetch(payResponse());
    renderAt();
    await startPayment();
    // Consent is shown before the buyer pays.
    expect(
      await screen.findByText(/card will be saved and charged automatically for future renewals/i)
    ).toBeInTheDocument();
    const init = await loadMoyasar();
    await waitFor(() => expect(init).toHaveBeenCalledTimes(1));

    const cfg = init.mock.calls[0][0];
    expect(cfg.element).toBe(document.querySelector(".mysr-form"));
    expect(cfg).toMatchObject({
      amount: 8000,
      currency: "SAR",
      description: "Invoice INV-42",
      publishable_api_key: "pk_test_abc",
      callback_url: `${window.location.origin}/checkout/${INVOICE_ID}?gateway=moyasar`,
      methods: ["creditcard"],
      supported_networks: ["mada", "visa", "mastercard"],
      metadata: { invoice_id: INVOICE_ID },
      language: "en",
      credit_card: { save_card: true },
    });

    const script = moyasarScript();
    expect(script.getAttribute("integrity")).toBe(MOYASAR_JS_INTEGRITY);
    expect(script.getAttribute("crossorigin")).toBe("anonymous");
    const links = document.querySelectorAll(`link[href="${MOYASAR_CSS}"]`);
    expect(links).toHaveLength(1);
    expect(links[0].getAttribute("integrity")).toBe(MOYASAR_CSS_INTEGRITY);
    expect(links[0].getAttribute("crossorigin")).toBe("anonymous");
    expect(MOYASAR_JS_INTEGRITY).toMatch(/^sha384-[A-Za-z0-9+/]{64}$/);
  });

  it("does not save the card or show consent when the server says save_card is false", async () => {
    mockFetch(payResponse({ save_card: false }));
    renderAt();
    await startPayment();
    const init = await loadMoyasar();
    await waitFor(() => expect(init).toHaveBeenCalledTimes(1));
    expect(init.mock.calls[0][0].credit_card).toEqual({ save_card: false });
    expect(screen.queryByText(/will be saved/i)).not.toBeInTheDocument();
  });

  it("treats a missing save_card flag as do-not-save", async () => {
    mockFetch(payResponse({ save_card: undefined }));
    renderAt();
    await startPayment();
    const init = await loadMoyasar();
    await waitFor(() => expect(init).toHaveBeenCalledTimes(1));
    expect(init.mock.calls[0][0].credit_card).toEqual({ save_card: false });
  });

  it("uses server-provided methods, networks and callback_url when present", async () => {
    mockFetch(
      payResponse({
        methods: ["creditcard", "applepay"],
        supported_networks: ["mada"],
        callback_url: "https://pay.example/checkout/x",
      })
    );
    renderAt();
    await startPayment();
    const init = await loadMoyasar();
    await waitFor(() => expect(init).toHaveBeenCalledTimes(1));
    expect(init.mock.calls[0][0]).toMatchObject({
      methods: ["creditcard", "applepay"],
      supported_networks: ["mada"],
      callback_url: "https://pay.example/checkout/x",
    });
  });

  it("refuses to take a card when the invoice binding is missing", async () => {
    mockFetch(payResponse({ metadata: undefined }));
    renderAt();
    await startPayment();
    expect(await screen.findByText(/isn't fully configured/i)).toBeInTheDocument();
    expect(moyasarScript()).toBeNull();
  });

  it("stores the created payment id so the return is recognised", async () => {
    mockFetch(payResponse());
    renderAt();
    await startPayment();
    const init = await loadMoyasar();
    await waitFor(() => expect(init).toHaveBeenCalledTimes(1));
    init.mock.calls[0][0].on_completed({ id: PAYMENT_ID });
    expect(sessionStorage.getItem(moyasarPaymentKey(INVOICE_ID))).toBe(PAYMENT_ID);
  });

  it("surfaces a form-level failure in an alert", async () => {
    mockFetch(payResponse());
    renderAt();
    await startPayment();
    const init = await loadMoyasar();
    await waitFor(() => expect(init).toHaveBeenCalledTimes(1));
    init.mock.calls[0][0].on_failure("DECLINED");
    expect(await screen.findByRole("alert")).toHaveTextContent(/card payment didn't go through/i);
  });

  it("offers Retry after a script error and mounts the form on the next attempt", async () => {
    mockFetch(payResponse());
    renderAt();
    await startPayment();
    await waitFor(() => expect(moyasarScript()).not.toBeNull());
    moyasarScript().dispatchEvent(new Event("error"));

    expect(await screen.findByText(/card form didn't load/i)).toBeInTheDocument();
    // The failed tag is gone, so a retry can't wait on a dead element.
    expect(moyasarScript()).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: /retry/i }));

    const init = await loadMoyasar();
    await waitFor(() => expect(init).toHaveBeenCalledTimes(1));
    expect(document.querySelectorAll(`script[src="${MOYASAR_JS}"]`)).toHaveLength(1);
    expect(screen.queryByText(/card form didn't load/i)).not.toBeInTheDocument();
  });

  it("injects one script and inits once under StrictMode", async () => {
    mockFetch(payResponse());
    renderAt("", { strict: true });
    await startPayment();
    const init = await loadMoyasar();
    await waitFor(() => expect(init).toHaveBeenCalled());
    // Flush any duplicate effect pass before counting.
    await act(async () => {});
    expect(init).toHaveBeenCalledTimes(1);
    expect(document.querySelectorAll(`script[src="${MOYASAR_JS}"]`)).toHaveLength(1);
  });

  it("never calls init when the form unmounts before the script loads", async () => {
    mockFetch(payResponse());
    const { unmount } = renderAt();
    await startPayment();
    await waitFor(() => expect(moyasarScript()).not.toBeNull());
    unmount();
    const init = vi.fn();
    window.Moyasar = { init };
    await act(async () => moyasarScript().dispatchEvent(new Event("load")));
    expect(init).not.toHaveBeenCalled();
  });

  it("passes Arabic to Moyasar and lays the page out right-to-left for an Arabic browser", async () => {
    vi.spyOn(window.navigator, "language", "get").mockReturnValue("ar-SA");
    vi.spyOn(window.navigator, "languages", "get").mockReturnValue(["ar-SA", "en"]);
    mockFetch(payResponse());
    const { container } = renderAt();
    const pay = await screen.findByRole("button", { name: /ادفع/ });
    const shell = container.querySelector("[dir]");
    expect(shell).toHaveAttribute("dir", "rtl");
    expect(shell).toHaveAttribute("lang", "ar");
    fireEvent.click(pay);
    const init = await loadMoyasar();
    await waitFor(() => expect(init).toHaveBeenCalledTimes(1));
    expect(init.mock.calls[0][0].language).toBe("ar");
  });

  it("still shows the unavailable message for an unknown gateway", async () => {
    mockFetch(payResponse({ gateway: "other", moyasar_publishable_key: undefined }));
    renderAt();
    await startPayment();
    expect(
      await screen.findByText(/self-serve checkout for SAR isn't available/i)
    ).toBeInTheDocument();
    expect(document.querySelector(".mysr-form")).toBeNull();
  });
});

describe("Checkout — Moyasar return verification", () => {
  beforeEach(() => sessionStorage.clear());

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    sessionStorage.clear();
    window.history.replaceState(null, "", "/");
  });

  it("shows a verifying screen with no Pay button while the server confirms", async () => {
    mockFetch(payResponse());
    vi.spyOn(endpoints, "verifyMoyasarCheckout").mockImplementation(never);
    renderAt(`?gateway=moyasar&id=${PAYMENT_ID}&status=paid`);
    expect(await screen.findByText(/confirming your payment/i)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /pay/i })).not.toBeInTheDocument();
  });

  it("verifies the returned payment id server-side, shows paid and clears the URL", async () => {
    mockFetch(payResponse());
    sessionStorage.setItem(moyasarPaymentKey(INVOICE_ID), PAYMENT_ID);
    const verify = vi
      .spyOn(endpoints, "verifyMoyasarCheckout")
      .mockResolvedValue({ data: { data: { status: "paid" } } });
    renderAt(`?gateway=moyasar&id=${PAYMENT_ID}&status=paid&message=Succeeded`);

    expect(await screen.findByText(/payment successful/i)).toBeInTheDocument();
    expect(verify).toHaveBeenCalledWith(INVOICE_ID, { payment_id: PAYMENT_ID });
    expect(window.location.search).toBe("");
    expect(window.location.pathname).toBe(`/checkout/${INVOICE_ID}`);
    expect(sessionStorage.getItem(moyasarPaymentKey(INVOICE_ID))).toBeNull();
  });

  it("calls the real verify route when the gateway marker was dropped but the id was stored", async () => {
    mockFetch(payResponse());
    sessionStorage.setItem(moyasarPaymentKey(INVOICE_ID), PAYMENT_ID);
    const post = vi
      .spyOn(axios, "post")
      .mockResolvedValue({ data: { data: { status: "paid" } } });
    renderAt(`?id=${PAYMENT_ID}&status=paid&message=Succeeded`);

    expect(await screen.findByText(/payment successful/i)).toBeInTheDocument();
    expect(post).toHaveBeenCalledWith(`${API_ROOT}/checkout/${INVOICE_ID}/moyasar/verify`, {
      payment_id: PAYMENT_ID,
    });
  });

  it("does not verify a UUID-shaped id and status without the marker or a stored id", async () => {
    mockFetch(payResponse());
    const verify = vi.spyOn(endpoints, "verifyMoyasarCheckout");
    renderAt(`?id=${PAYMENT_ID}&status=paid`);
    expect(await screen.findByRole("button", { name: PAY_DUE })).toBeInTheDocument();
    expect(verify).not.toHaveBeenCalled();
  });

  it("does not verify a stored id that differs from the returned one", async () => {
    mockFetch(payResponse());
    sessionStorage.setItem(moyasarPaymentKey(INVOICE_ID), "another-payment");
    const verify = vi.spyOn(endpoints, "verifyMoyasarCheckout");
    renderAt(`?id=${PAYMENT_ID}&status=paid`);
    expect(await screen.findByRole("button", { name: PAY_DUE })).toBeInTheDocument();
    expect(verify).not.toHaveBeenCalled();
  });

  it("keeps the stored payment and the URL when it unmounts before verify resolves", async () => {
    mockFetch(payResponse());
    sessionStorage.setItem(moyasarPaymentKey(INVOICE_ID), PAYMENT_ID);
    let resolveVerify;
    vi.spyOn(endpoints, "verifyMoyasarCheckout").mockImplementation(
      () => new Promise((r) => (resolveVerify = r))
    );
    const { unmount } = renderAt(`?gateway=moyasar&id=${PAYMENT_ID}&status=paid`);
    await waitFor(() => expect(resolveVerify).toBeTypeOf("function"));
    unmount();
    await act(async () => resolveVerify({ data: { data: { status: "paid" } } }));
    expect(sessionStorage.getItem(moyasarPaymentKey(INVOICE_ID))).toBe(PAYMENT_ID);
    expect(window.location.search).toContain(`id=${PAYMENT_ID}`);
  });

  it("verifies once under StrictMode", async () => {
    mockFetch(payResponse());
    const verify = vi
      .spyOn(endpoints, "verifyMoyasarCheckout")
      .mockResolvedValue({ data: { data: { status: "paid" } } });
    renderAt(`?gateway=moyasar&id=${PAYMENT_ID}&status=paid`, { strict: true });
    expect(await screen.findByText(/payment successful/i)).toBeInTheDocument();
    expect(verify).toHaveBeenCalledTimes(1);
  });

  it("never shows paid from the status query param when the server says failed", async () => {
    mockFetch(payResponse());
    vi.spyOn(endpoints, "verifyMoyasarCheckout").mockResolvedValue({
      data: { data: { status: "failed", payment_status: "failed" } },
    });
    renderAt(`?gateway=moyasar&id=${PAYMENT_ID}&status=paid&message=Succeeded`);

    expect(await screen.findByText(/payment not completed/i)).toBeInTheDocument();
    expect(screen.queryByText(/payment successful/i)).not.toBeInTheDocument();
    expect(window.location.search).toBe("");
  });

  it("says an initiated payment is unconfirmed, never received, and offers Try again", async () => {
    mockFetch(payResponse());
    vi.spyOn(endpoints, "verifyMoyasarCheckout").mockResolvedValue({
      data: { data: { status: "processing", payment_status: "initiated" } },
    });
    renderAt(`?gateway=moyasar&id=${PAYMENT_ID}&status=initiated`);
    expect(await screen.findByText(/payment not confirmed yet/i)).toBeInTheDocument();
    expect(screen.getByText(/closed the verification step/i)).toBeInTheDocument();
    expect(screen.queryByText(/received your payment/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/ACH/)).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /try again/i }));
    expect(await screen.findByRole("button", { name: PAY_DUE })).toBeInTheDocument();
    expect(window.location.search).toBe("");
  });

  it("shows a verify-error screen whose Check again re-runs verification", async () => {
    mockFetch(payResponse());
    const verify = vi
      .spyOn(endpoints, "verifyMoyasarCheckout")
      .mockRejectedValueOnce({
        response: { status: 502, data: { error: { code: "internal_error", message: "x" } } },
      })
      .mockResolvedValueOnce({ data: { data: { status: "paid" } } });
    renderAt(`?gateway=moyasar&id=${PAYMENT_ID}&status=paid`);

    expect(await screen.findByText(/couldn't confirm your payment/i)).toBeInTheDocument();
    expect(screen.getByText(/check again in a minute/i)).toBeInTheDocument();
    // A live Pay button here would invite a second charge.
    expect(screen.queryByRole("button", { name: /pay/i })).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /check again/i }));
    expect(await screen.findByText(/payment successful/i)).toBeInTheDocument();
    expect(verify).toHaveBeenCalledTimes(2);
  });

  it.each([
    [{ response: { status: 400, data: { error: { code: "validation_failed" } } } }, /doesn't match invoice INV-42.*79cced57/],
    [{ response: { status: 404, data: { error: { code: "not_found" } } } }, /couldn't find this invoice/],
    [{ response: { status: 503, data: { error: { code: "internal_error" } } } }, /card payments aren't available/i],
    [{ message: "Network Error" }, /couldn't reach the server/],
  ])("maps verify error %# to buyer copy without leaking the server message", async (err, copy) => {
    mockFetch(payResponse());
    vi.spyOn(endpoints, "verifyMoyasarCheckout").mockRejectedValue({
      ...err,
      response: err.response && {
        ...err.response,
        data: { error: { ...err.response.data.error, message: "payment does not belong to this invoice" } },
      },
    });
    renderAt(`?gateway=moyasar&id=${PAYMENT_ID}&status=paid`);
    expect(await screen.findByText(copy)).toBeInTheDocument();
    expect(screen.queryByText(/does not belong/)).not.toBeInTheDocument();
    expect(screen.getByRole("alert")).toBeInTheDocument();
  });
});
