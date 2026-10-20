import { render, screen, waitFor, cleanup, fireEvent } from "@testing-library/react";
import { MemoryRouter, Routes, Route } from "react-router";
import { describe, it, expect, vi, afterEach } from "vitest";

import Checkout from "../Checkout";

// Stripe is loaded lazily and mounts an <Elements> tree; stub both packages so
// the checkout renders in jsdom without a real Stripe.js.
vi.mock("@stripe/stripe-js", () => ({ loadStripe: vi.fn(() => Promise.resolve(null)) }));
vi.mock("@stripe/react-stripe-js", () => ({
    Elements: ({ children }) => <div>{children}</div>,
    PaymentElement: () => <div data-testid="payment-element" />,
    useStripe: () => null,
    useElements: () => null,
}));

const invoice = (over = {}) => ({
    status: "open",
    currency: "USD",
    display_amount: "42.00",
    subtotal: 4000,
    total: 4200,
    ...over,
});

const json = (status, body) =>
    Promise.resolve({ ok: status >= 200 && status < 300, status, json: () => Promise.resolve(body) });

const renderAt = (id, search = "") => {
    window.history.replaceState(null, "", `/checkout/${id}${search}`);
    return render(
        <MemoryRouter initialEntries={[`/checkout/${id}${search}`]}>
            <Routes>
                <Route path="/checkout/:id" element={<Checkout />} />
            </Routes>
        </MemoryRouter>
    );
};

describe("Checkout (money-critical public flow)", () => {
    afterEach(() => {
        cleanup();
        vi.restoreAllMocks();
        vi.unstubAllGlobals();
        window.history.replaceState(null, "", "/");
    });

    it("loads the invoice and shows the amount to pay", async () => {
        vi.stubGlobal("fetch", vi.fn(() => json(200, { data: invoice() })));
        renderAt("inv_1");
        // The pay CTA and the totals both carry the amount, rendered through the
        // exponent-aware formatter so the currency shows as its symbol ($ for
        // USD) consistently — not a mix of "$42.00" and "USD 42.00".
        await waitFor(() => expect(screen.getAllByText(/\$42\.00/).length).toBeGreaterThan(0));
    });

    it("shows a not-found error when the invoice can't be loaded", async () => {
        vi.stubGlobal("fetch", vi.fn(() => Promise.resolve({ ok: false })));
        renderAt("missing");
        await waitFor(() => expect(screen.getAllByText(/not found/i).length).toBeGreaterThan(0));
    });

    it("reflects an already-paid invoice", async () => {
        vi.stubGlobal("fetch", vi.fn(() => json(200, { data: invoice({ status: "paid" }) })));
        renderAt("inv_paid");
        await waitFor(() => expect(screen.getByText(/paid/i)).toBeInTheDocument());
    });

    it("puts the amount due on the Pay button for a partly paid invoice", async () => {
        vi.stubGlobal("fetch", vi.fn(() => json(200, { data: invoice({ amount_due: 1200 }) })));
        renderAt("inv_partial");
        expect(await screen.findByRole("button", { name: /pay \$12\.00/i })).toBeInTheDocument();
        expect(screen.getByTestId("amount-due")).toHaveTextContent("$12.00");
        expect(screen.getByText("Paid or credited")).toBeInTheDocument();
    });

    it("maps a /pay failure to buyer copy instead of the server string", async () => {
        vi.stubGlobal(
            "fetch",
            vi.fn((url, opts) =>
                opts?.method === "POST"
                    ? json(500, { error: { code: "internal_error", message: "failed to create payment order" } })
                    : json(200, { data: invoice() })
            )
        );
        renderAt("inv_1");
        fireEvent.click(await screen.findByRole("button", { name: /pay \$42\.00/i }));
        expect(await screen.findByRole("alert")).toHaveTextContent(/couldn't start the payment/i);
        expect(screen.queryByText(/failed to create payment order/)).not.toBeInTheDocument();
    });

    it("shows paid when /pay reports the invoice was already paid", async () => {
        vi.stubGlobal(
            "fetch",
            vi.fn((url, opts) =>
                opts?.method === "POST"
                    ? json(400, { error: { code: "invoice_already_paid", message: "invoice already paid" } })
                    : json(200, { data: invoice() })
            )
        );
        renderAt("inv_1");
        fireEvent.click(await screen.findByRole("button", { name: /pay \$42\.00/i }));
        expect(await screen.findByText(/payment successful/i)).toBeInTheDocument();
    });

    describe("returning from a Stripe bank redirect", () => {
        it("confirms with no Pay button, then shows paid and clears the URL", async () => {
            let resolveSuccess;
            vi.stubGlobal(
                "fetch",
                vi.fn((url) =>
                    String(url).includes("/success?")
                        ? new Promise((r) => {
                              resolveSuccess = r;
                          })
                        : json(200, { data: invoice() })
                )
            );
            renderAt("inv_1", "?payment_intent=pi_123");
            expect(await screen.findByText(/confirming your payment/i)).toBeInTheDocument();
            expect(screen.queryByRole("button", { name: /pay/i })).not.toBeInTheDocument();
            await waitFor(() => expect(resolveSuccess).toBeTypeOf("function"));
            resolveSuccess({ ok: true, status: 200, json: () => Promise.resolve({ data: { status: "paid" } }) });
            expect(await screen.findByText(/payment successful/i)).toBeInTheDocument();
            expect(window.location.search).toBe("");
        });

        it("keeps the bank-debit processing copy for Stripe", async () => {
            vi.stubGlobal(
                "fetch",
                vi.fn((url) =>
                    String(url).includes("/success?")
                        ? json(200, { data: { status: "processing" } })
                        : json(200, { data: invoice() })
                )
            );
            renderAt("inv_1", "?payment_intent=pi_123");
            expect(await screen.findByText(/payment processing/i)).toBeInTheDocument();
            expect(screen.getByText(/ACH/)).toBeInTheDocument();
        });

        it("shows Check again instead of a Pay button when confirmation fails", async () => {
            vi.stubGlobal(
                "fetch",
                vi.fn((url) =>
                    String(url).includes("/success?")
                        ? json(500, { error: { code: "internal_error", message: "failed to settle invoice" } })
                        : json(200, { data: invoice() })
                )
            );
            renderAt("inv_1", "?payment_intent=pi_123");
            expect(await screen.findByRole("button", { name: /check again/i })).toBeInTheDocument();
            expect(screen.queryByRole("button", { name: /pay/i })).not.toBeInTheDocument();
            expect(screen.queryByText(/failed to settle invoice/)).not.toBeInTheDocument();
        });
    });
});
