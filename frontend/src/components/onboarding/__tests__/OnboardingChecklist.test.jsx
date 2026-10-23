import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { describe, it, expect, vi, beforeEach } from "vitest";
import OnboardingChecklist from "../OnboardingChecklist";
import { endpoints } from "../../../lib/api";

vi.mock("../../../lib/api", () => ({
  endpoints: {
    getGatewayConnections: vi.fn(),
    getVersion: vi.fn(),
    getPlans: vi.fn(),
    getCustomers: vi.fn(),
    getSubscriptions: vi.fn(),
  },
}));

const renderChecklist = () =>
  render(
    <QueryClientProvider
      client={new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })}
    >
      <MemoryRouter>
        <OnboardingChecklist />
      </MemoryRouter>
    </QueryClientProvider>
  );

// GET /v1/gateway-connections wraps an object, not an array.
const connections = (list) => ({ data: { data: { connections: list, vault_ready: true } } });

describe("OnboardingChecklist — payment gateway step", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    window.localStorage?.removeItem("recurso.onboarding.dismissed");
    endpoints.getPlans.mockResolvedValue({ data: { data: [] } });
    endpoints.getCustomers.mockResolvedValue({ data: { data: [] } });
    endpoints.getSubscriptions.mockResolvedValue({ data: { data: [] } });
    endpoints.getVersion.mockResolvedValue({ data: { version: "x", gateway_mode: "none" } });
  });

  it("counts the connections array, so a connected gateway completes the step", async () => {
    endpoints.getGatewayConnections.mockResolvedValue(connections([{ id: "gc-1", provider: "stripe" }]));
    renderChecklist();
    expect(await screen.findByText(/1 of 4 steps done/)).toBeInTheDocument();
  });

  it("treats a server-configured gateway (env keys, e.g. Moyasar) as connected", async () => {
    endpoints.getGatewayConnections.mockResolvedValue(connections([]));
    endpoints.getVersion.mockResolvedValue({ data: { version: "x", gateway_mode: "live" } });
    renderChecklist();
    expect(await screen.findByText(/1 of 4 steps done/)).toBeInTheDocument();
  });

  it("leaves the step open with no connection and no server gateway", async () => {
    endpoints.getGatewayConnections.mockResolvedValue(connections([]));
    renderChecklist();
    expect(await screen.findByText(/0 of 4 steps done/)).toBeInTheDocument();
  });
});
