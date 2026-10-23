import { render, screen, waitFor } from "@testing-library/react";
import { BrowserRouter } from "react-router";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { describe, it, expect, vi, beforeEach } from "vitest";
import OfflinePayments from "../OfflinePayments";
import { endpoints } from "../../lib/api";
import { money } from "@/test/money";

vi.mock("../../lib/api", () => ({
  endpoints: {
    getOfflinePayments: vi.fn(),
    getVirtualAccounts: vi.fn().mockResolvedValue({ data: { data: [] } }),
    recordOfflinePayment: vi.fn(),
    createVirtualAccount: vi.fn(),
    getCustomers: vi.fn().mockResolvedValue({ data: { data: [] } }),
    getInvoices: vi.fn().mockResolvedValue({ data: { data: [] } }),
  },
}));
vi.mock("@/components/ui/sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

const wrapper = ({ children }) => (
  <BrowserRouter>
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })}>
      {children}
    </QueryClientProvider>
  </BrowserRouter>
);

describe("OfflinePayments page", () => {
  beforeEach(() => vi.clearAllMocks());

  it("renders a recorded offline payment amount", async () => {
    endpoints.getOfflinePayments.mockResolvedValue({
      data: { data: [{ id: "op1", customer_id: "cus_1", amount: 250000, currency: "USD", method: "neft" }] },
    });
    render(<OfflinePayments />, { wrapper });
    // $2,500.00
    await waitFor(() => expect(screen.getByText(money("$2,500.00"))).toBeInTheDocument());
  });

  it("shows the empty state with no offline payments", async () => {
    endpoints.getOfflinePayments.mockResolvedValue({ data: { data: [] } });
    render(<OfflinePayments />, { wrapper });
    await waitFor(() => expect(screen.getByText("No offline payments recorded")).toBeInTheDocument());
  });
});

describe("OfflinePayments — record currency", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    if (!Element.prototype.hasPointerCapture) Element.prototype.hasPointerCapture = () => false;
    if (!Element.prototype.scrollIntoView) Element.prototype.scrollIntoView = () => {};
    endpoints.getOfflinePayments.mockResolvedValue({ data: { data: [] } });
    endpoints.getCustomers.mockResolvedValue({
      data: { data: [{ id: "cus_9", name: "Riyadh Co", email: "ap@riyadh.sa" }] },
    });
    endpoints.getInvoices.mockResolvedValue({ data: { data: [] } });
    endpoints.recordOfflinePayment.mockResolvedValue({ data: { data: { id: "op9" } } });
  });

  it("asks for a billing currency (no INR default) and always sends the chosen one", async () => {
    const { default: userEvent } = await import("@testing-library/user-event");
    const user = userEvent.setup();
    render(<OfflinePayments />, { wrapper });
    await waitFor(() => expect(screen.getByText("No offline payments recorded")).toBeInTheDocument());

    await user.click(screen.getAllByRole("button", { name: /Record payment/ })[0]);
    const currency = await screen.findByRole("combobox", { name: "Currency" });
    expect(currency).not.toHaveTextContent("INR");
    expect(screen.queryByText("Amount (INR)")).not.toBeInTheDocument();

    await user.click(document.getElementById("record-customer"));
    await user.click(await screen.findByRole("option", { name: /Riyadh Co/ }));
    await user.type(document.getElementById("amount"), "115");

    const submit = () => screen.getAllByRole("button", { name: "Record payment" }).at(-1);
    // No currency yet: the payment can't be recorded (the API would assume INR).
    expect(submit()).toBeDisabled();

    await user.click(currency);
    await user.click(await screen.findByRole("option", { name: "SAR" }));
    await user.click(submit());
    await waitFor(() =>
      expect(endpoints.recordOfflinePayment).toHaveBeenCalledWith(
        expect.objectContaining({ customer_id: "cus_9", currency: "SAR", amount: 11500 })
      )
    );
  });
});
