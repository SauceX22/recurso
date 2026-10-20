import { render, screen, waitFor, cleanup, fireEvent } from "@testing-library/react";
import { MemoryRouter, Routes, Route } from "react-router";
import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";

import Checkout from "../Checkout";
import { endpoints } from "../../lib/api";

vi.mock("@stripe/stripe-js", () => ({ loadStripe: vi.fn(() => Promise.resolve(null)) }));
vi.mock("@stripe/react-stripe-js", () => ({
    Elements: ({ children }) => <div>{children}</div>,
    PaymentElement: () => <div data-testid="payment-element" />,
    AddressElement: () => null,
    useStripe: () => null,
    useElements: () => null,
}));

const INVOICE_ID = "11111111-2222-3333-4444-555555555555";
const MOYASAR_JS =
    "https://cdn.jsdelivr.net/npm/moyasar-payment-form@2.3.0/dist/moyasar.umd.min.js";
const MOYASAR_CSS =
    "https://cdn.jsdelivr.net/npm/moyasar-payment-form@2.3.0/dist/moyasar.css";

const invoice = {
    id: INVOICE_ID,
    invoice_number: "INV-42",
    status: "open",
    currency: "SAR",
    subtotal: 10000,
    tax_amount: 0,
    total: 10000,
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
    ...over,
});

// GET /checkout/:id returns the invoice; POST /checkout/:id/pay returns `pay`.
const mockFetch = (pay) => {
    global.fetch = vi.fn((url, opts) => {
        if (opts?.method === "POST") {
            return Promise.resolve({ ok: true, json: () => Promise.resolve({ data: pay }) });
        }
        return Promise.resolve({ ok: true, json: () => Promise.resolve({ data: invoice }) });
    });
};

const renderAt = (search = "") => {
    window.history.replaceState(null, "", `/checkout/${INVOICE_ID}${search}`);
    return render(
        <MemoryRouter initialEntries={[`/checkout/${INVOICE_ID}${search}`]}>
            <Routes>
                <Route path="/checkout/:id" element={<Checkout />} />
            </Routes>
        </MemoryRouter>
    );
};

const startPayment = async () => {
    const pay = await screen.findByRole("button", { name: /pay/i });
    fireEvent.click(pay);
};

describe("Checkout — Moyasar embedded form", () => {
    beforeEach(() => {
        delete window.Moyasar;
        document.head.innerHTML = "";
        document.querySelectorAll("script").forEach((s) => s.remove());
    });

    afterEach(() => {
        cleanup();
        vi.restoreAllMocks();
        delete window.Moyasar;
        window.history.replaceState(null, "", "/");
    });

    it("loads the form assets once and mounts Moyasar with save_card and the invoice binding", async () => {
        mockFetch(payResponse());
        const init = vi.fn();
        renderAt();
        await startPayment();

        // Script is injected on demand; simulate it loading.
        await waitFor(() =>
            expect(document.querySelector(`script[src="${MOYASAR_JS}"]`)).not.toBeNull()
        );
        expect(document.querySelectorAll(`link[href="${MOYASAR_CSS}"]`)).toHaveLength(1);
        window.Moyasar = { init };
        document.querySelector(`script[src="${MOYASAR_JS}"]`).dispatchEvent(new Event("load"));

        await waitFor(() => expect(init).toHaveBeenCalledTimes(1));
        expect(document.querySelector(".mysr-form")).not.toBeNull();
        const cfg = init.mock.calls[0][0];
        expect(cfg).toMatchObject({
            element: ".mysr-form",
            amount: 8000,
            currency: "SAR",
            description: "Invoice INV-42",
            publishable_api_key: "pk_test_abc",
            callback_url: `${window.location.origin}/checkout/${INVOICE_ID}?gateway=moyasar`,
            methods: ["creditcard"],
            supported_networks: ["mada", "visa", "mastercard"],
            metadata: { invoice_id: INVOICE_ID },
            credit_card: { save_card: true },
        });
        expect(screen.getByText(/saved securely for future renewals/i)).toBeInTheDocument();
        expect(document.querySelectorAll(`script[src="${MOYASAR_JS}"]`)).toHaveLength(1);
    });

    it("shows an error when the form script fails to load", async () => {
        mockFetch(payResponse());
        renderAt();
        await startPayment();
        await waitFor(() =>
            expect(document.querySelector(`script[src="${MOYASAR_JS}"]`)).not.toBeNull()
        );
        document.querySelector(`script[src="${MOYASAR_JS}"]`).dispatchEvent(new Event("error"));
        expect(await screen.findByText(/could not load the payment form/i)).toBeInTheDocument();
    });

    it("verifies the returned payment id server-side and shows the paid state", async () => {
        mockFetch(payResponse());
        const verify = vi
            .spyOn(endpoints, "verifyMoyasarCheckout")
            .mockResolvedValue({ data: { data: { status: "paid" } } });
        renderAt("?gateway=moyasar&id=pay-123&status=paid&message=Succeeded");

        expect(await screen.findByText(/payment successful/i)).toBeInTheDocument();
        expect(verify).toHaveBeenCalledWith(INVOICE_ID, { payment_id: "pay-123" });
    });

    it("recognizes Moyasar's documented redirect even if our gateway marker is dropped", async () => {
        mockFetch(payResponse());
        const verify = vi
            .spyOn(endpoints, "verifyMoyasarCheckout")
            .mockResolvedValue({ data: { data: { status: "paid" } } });
        const paymentId = "79cced57-9deb-4c4b-8f48-59c124f79688";
        renderAt(`?id=${paymentId}&status=paid&message=Succeeded`);

        expect(await screen.findByText(/payment successful/i)).toBeInTheDocument();
        expect(verify).toHaveBeenCalledWith(INVOICE_ID, { payment_id: paymentId });
    });

    it("never shows paid from the status query param when the server says failed", async () => {
        mockFetch(payResponse());
        vi.spyOn(endpoints, "verifyMoyasarCheckout").mockResolvedValue({
            data: { data: { status: "failed", payment_status: "failed" } },
        });
        renderAt("?gateway=moyasar&id=pay-123&status=paid&message=Succeeded");

        expect(await screen.findByText(/payment not completed/i)).toBeInTheDocument();
        expect(screen.queryByText(/payment successful/i)).not.toBeInTheDocument();
    });

    it("shows the processing state when the payment is still in flight", async () => {
        mockFetch(payResponse());
        vi.spyOn(endpoints, "verifyMoyasarCheckout").mockResolvedValue({
            data: { data: { status: "processing", payment_status: "initiated" } },
        });
        renderAt("?gateway=moyasar&id=pay-123&status=initiated");
        expect(await screen.findByText(/payment processing/i)).toBeInTheDocument();
    });

    it("surfaces the server's message when verification is rejected", async () => {
        mockFetch(payResponse());
        vi.spyOn(endpoints, "verifyMoyasarCheckout").mockRejectedValue({
            response: { data: { error: { message: "payment does not belong to this invoice" } } },
        });
        renderAt("?gateway=moyasar&id=pay-999&status=paid");
        expect(
            await screen.findByText(/payment does not belong to this invoice/i)
        ).toBeInTheDocument();
        expect(screen.queryByText(/payment successful/i)).not.toBeInTheDocument();
    });

    it("does not call verify without the moyasar return params", async () => {
        mockFetch(payResponse());
        const verify = vi.spyOn(endpoints, "verifyMoyasarCheckout");
        renderAt("?id=pay-123");
        await screen.findByRole("button", { name: /pay/i });
        expect(verify).not.toHaveBeenCalled();
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
