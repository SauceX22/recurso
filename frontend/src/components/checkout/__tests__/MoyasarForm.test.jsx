import { render, screen, waitFor, act, cleanup } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import MoyasarForm from "../MoyasarForm";
import { MOYASAR_CSS, MOYASAR_JS } from "../moyasarAssets";
import { checkoutCopy } from "@/lib/checkoutCopy";
import { validateMoyasarCallbacks } from "./fixtures/moyasarCallbackValidator";

const t = checkoutCopy("en");
const order = {
  amount: 8000,
  currency: "SAR",
  description: "Invoice INV-1",
  moyasar_publishable_key: "pk_test_abc",
  metadata: { invoice_id: "inv-1" },
  save_card: false,
};

const script = () => document.querySelector(`script[src="${MOYASAR_JS}"]`);

// Simulates the CDN script finishing: the global appears, then `load` fires.
const loadMoyasar = async (init) => {
  await waitFor(() => expect(script()).not.toBeNull());
  window.Moyasar = { init };
  await act(async () => script().dispatchEvent(new Event("load")));
};

const mountWith = async (init, props = {}) => {
  const view = render(<MoyasarForm order={order} invoiceId="inv-1" locale="en" t={t} {...props} />);
  await loadMoyasar(init);
  return view;
};

describe("MoyasarForm", () => {
  beforeEach(() => sessionStorage.clear());

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
    delete window.Moyasar;
    document
      .querySelectorAll(`script[src="${MOYASAR_JS}"],link[href="${MOYASAR_CSS}"]`)
      .forEach((n) => n.remove());
    sessionStorage.clear();
  });

  it("hands Moyasar async callbacks, as its init validation requires", async () => {
    const init = vi.fn();
    await mountWith(init);
    await waitFor(() => expect(init).toHaveBeenCalledTimes(1));
    const cfg = init.mock.calls[0][0];
    for (const name of ["on_initiating", "on_completed", "on_failure"]) {
      expect(cfg[name], name).toBeTypeOf("function");
      expect(cfg[name].constructor.name, name).toBe("AsyncFunction");
    }
  });

  it("passes the pinned bundle's own callback validation", async () => {
    // init runs Moyasar's real validator, so a plain-function callback throws
    // here exactly as it would in the browser.
    const init = vi.fn((cfg) => validateMoyasarCallbacks(cfg));
    await mountWith(init);
    await waitFor(() => expect(init).toHaveReturnedWith(true));
    expect(screen.queryByText(t.cardFormLoadError)).not.toBeInTheDocument();
  });

  it("the copied validator rejects a plain arrow callback", () => {
    expect(() => validateMoyasarCallbacks({ on_completed: () => {} })).toThrow(
      /On completed must be a promise based callback/
    );
  });

  it("shows the load-error state with Retry when init throws, and re-inits on Retry", async () => {
    const init = vi
      .fn()
      .mockImplementationOnce(() => {
        throw new Error("amount must be non-zero value");
      })
      .mockImplementationOnce(() => undefined);
    await mountWith(init);
    expect(await screen.findByText(t.cardFormLoadError)).toBeInTheDocument();

    await act(async () => screen.getByRole("button", { name: t.retry }).click());
    await waitFor(() => expect(init).toHaveBeenCalledTimes(2));
    expect(screen.queryByText(t.cardFormLoadError)).not.toBeInTheDocument();
  });

  it("mounts the form once even when the parent re-renders with a new order object", async () => {
    const init = vi.fn();
    const { rerender } = await mountWith(init);
    await waitFor(() => expect(init).toHaveBeenCalledTimes(1));
    // A parent re-render (e.g. Checkout state change) hands a fresh but equal order.
    rerender(<MoyasarForm order={{ ...order }} invoiceId="inv-1" locale="en" t={{ ...t }} />);
    await act(async () => {});
    expect(init).toHaveBeenCalledTimes(1);
  });

  it("stores the payment id from on_completed and clears a stale error on the next attempt", async () => {
    const init = vi.fn();
    await mountWith(init);
    await waitFor(() => expect(init).toHaveBeenCalledTimes(1));
    const cfg = init.mock.calls[0][0];

    await act(() => cfg.on_failure("DECLINED"));
    expect(screen.getByRole("alert")).toHaveTextContent(t.cardFailed);

    await act(() => cfg.on_initiating());
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();

    await act(() => cfg.on_completed({ id: "pay_1" }));
    expect(sessionStorage.getItem("moyasar:inv-1")).toBe("pay_1");
  });
});
