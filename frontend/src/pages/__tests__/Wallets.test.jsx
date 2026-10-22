import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { BrowserRouter } from "react-router";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { describe, it, expect, vi, beforeEach } from "vitest";
import Wallets from "../Wallets";
import { endpoints } from "../../lib/api";

vi.mock("../../lib/api", () => ({
  endpoints: {
    getWallets: vi.fn(),
    getCustomers: vi.fn().mockResolvedValue({ data: { data: [] } }),
    createWallet: vi.fn(),
    topUpWallet: vi.fn(),
    setWalletAutoRecharge: vi.fn(),
    closeWallet: vi.fn(),
  },
}));
vi.mock("@/components/ui/sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

const wallets = [
  { id: "w1", customer_id: "cus_1", balance: 500000, currency: "USD" },
];

const wrapper = ({ children }) => (
  <BrowserRouter>
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })}>
      {children}
    </QueryClientProvider>
  </BrowserRouter>
);

describe("Wallets page", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    endpoints.getWallets.mockResolvedValue({ data: { data: wallets } });
  });

  it("renders a wallet with its balance", async () => {
    render(<Wallets />, { wrapper });
    // 500000 minor USD = $5,000.00
    await waitFor(() => expect(screen.getByText(/5,000/)).toBeInTheDocument());
  });

  it("shows the empty state with no wallets", async () => {
    endpoints.getWallets.mockResolvedValue({ data: { data: [] } });
    render(<Wallets />, { wrapper });
    await waitFor(() => expect(screen.getByText("No wallets yet")).toBeInTheDocument());
  });

  // Closing posts a ledger entry only; no gateway refund is made.
  it("says closing books the paid balance but sends no money", async () => {
    endpoints.closeWallet.mockResolvedValue({ data: { data: { refunded: 500000, forfeited: 0 } } });
    render(<Wallets />, { wrapper });
    await waitFor(() => expect(screen.getByText(/5,000/)).toBeInTheDocument());

    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent(/No money is sent/);
    expect(dialog).not.toHaveTextContent(/refunded to the customer/);

    fireEvent.click(screen.getByRole("button", { name: "Close wallet" }));
    expect(await screen.findByText("Booked as returned to customer")).toBeInTheDocument();
    expect(screen.getByText(/issue the refund in your payment gateway/i)).toBeInTheDocument();
  });
});

describe("Wallets — create currency", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    if (!Element.prototype.hasPointerCapture) Element.prototype.hasPointerCapture = () => false;
    if (!Element.prototype.scrollIntoView) Element.prototype.scrollIntoView = () => {};
    endpoints.getWallets.mockResolvedValue({ data: { data: [] } });
    endpoints.getCustomers.mockResolvedValue({
      data: { data: [{ id: "cus_9", name: "Riyadh Co", email: "ap@riyadh.sa" }] },
    });
    endpoints.createWallet.mockResolvedValue({ data: { data: { id: "w9" } } });
  });

  it("asks for a billing currency (no INR default) and sends the chosen one", async () => {
    const { default: userEvent } = await import("@testing-library/user-event");
    const user = userEvent.setup();
    render(<Wallets />, { wrapper });
    await waitFor(() => expect(screen.getByText("No wallets yet")).toBeInTheDocument());

    await user.click(screen.getAllByRole("button", { name: /Create wallet/ })[0]);
    const currency = await screen.findByRole("combobox", { name: "Currency" });
    expect(currency).not.toHaveTextContent("INR");

    await user.click(document.getElementById("wallet-customer"));
    await user.click(await screen.findByRole("option", { name: /Riyadh Co/ }));
    await user.click(currency);
    await user.click(await screen.findByRole("option", { name: "SAR" }));

    const submit = screen.getAllByRole("button", { name: "Create wallet" }).at(-1);
    await user.click(submit);
    await waitFor(() =>
      expect(endpoints.createWallet).toHaveBeenCalledWith({ customer_id: "cus_9", currency: "SAR" })
    );
  });
});
