import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import CreateCreditNote from '../CreateCreditNote';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { endpoints } from '../../lib/api';

// jsdom lacks these; Radix (Sheet/Select) touches them.
beforeEach(() => {
    if (!Element.prototype.hasPointerCapture) Element.prototype.hasPointerCapture = () => false;
    if (!Element.prototype.scrollIntoView) Element.prototype.scrollIntoView = () => {};
});

const navigateMock = vi.fn();
vi.mock('react-router', async () => {
    const actual = await vi.importActual('react-router');
    return { ...actual, useNavigate: () => navigateMock };
});

vi.mock('../../lib/api', () => ({
    endpoints: {
        getCustomers: vi.fn(),
        getInvoices: vi.fn(),
        createCreditNote: vi.fn(),
    },
}));

const ACME = { id: 'c0000000-0000-0000-0000-000000000001', name: 'Acme Corp', email: 'ops@acme.com' };
const GLOBEX = { id: 'c0000000-0000-0000-0000-000000000002', name: 'Globex', email: 'ap@globex.com' };
const RIYADH = { id: 'c0000000-0000-0000-0000-000000000003', name: 'Riyadh Co', email: 'ap@riyadh.sa' };
const NEWCO = { id: 'c0000000-0000-0000-0000-000000000004', name: 'Newco', email: 'ap@newco.io' };

// One INR invoice for Acme, one USD invoice for Globex — proves the picker is
// scoped to the selected customer and that the currency follows the invoice.
const ACME_INV = {
    id: 'i0000000-0000-0000-0000-0000000000a1',
    invoice_number: 'INV-ACME-01',
    customer_id: ACME.id,
    total: 500000,
    currency: 'INR',
    status: 'open',
};
const GLOBEX_INV = {
    id: 'i0000000-0000-0000-0000-0000000000b1',
    invoice_number: 'INV-GLBX-01',
    customer_id: GLOBEX.id,
    total: 4200,
    amount_paid: 4200,
    currency: 'USD',
    status: 'paid',
};
// A paid SAR invoice for the Saudi customer — the refund path.
const RIYADH_INV = {
    id: 'i0000000-0000-0000-0000-0000000000c1',
    invoice_number: 'INV-RYD-01',
    customer_id: RIYADH.id,
    total: 11500,
    amount_paid: 11500,
    currency: 'SAR',
    status: 'paid',
};

const renderPage = () =>
    render(
        <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })}>
            <MemoryRouter>
                <CreateCreditNote />
            </MemoryRouter>
        </QueryClientProvider>
    );

const pickOption = async (user, triggerId, optionText) => {
    await user.click(document.getElementById(triggerId));
    const option = await screen.findByRole('option', { name: optionText });
    await user.click(option);
};

describe('CreateCreditNote — linked-invoice picker', () => {
    beforeEach(() => {
        vi.clearAllMocks();
        endpoints.getCustomers.mockResolvedValue({ data: { data: [ACME, GLOBEX, RIYADH, NEWCO] } });
        endpoints.getInvoices.mockResolvedValue({ data: { data: [ACME_INV, GLOBEX_INV, RIYADH_INV] } });
        endpoints.createCreditNote.mockResolvedValue({ data: { data: { id: 'cn-1' } } });
    });

    it('disables the invoice picker until a customer is chosen', async () => {
        renderPage();
        await waitFor(() => expect(endpoints.getCustomers).toHaveBeenCalled());
        expect(screen.getByText('Select a customer first')).toBeInTheDocument();
        expect(document.getElementById('invoice_id')).toBeDisabled();
    });

    it('only offers the selected customer\'s invoices and links the real invoice id (never a typed UUID)', async () => {
        const user = userEvent.setup();
        renderPage();
        await waitFor(() => expect(endpoints.getCustomers).toHaveBeenCalled());

        await pickOption(user, 'customer_id', /Acme Corp/);
        // Open the now-enabled invoice picker.
        await user.click(document.getElementById('invoice_id'));

        // Acme's invoice is offered; Globex's is not (scoped to the customer).
        expect(await screen.findByRole('option', { name: /INV-ACME-01/ })).toBeInTheDocument();
        expect(screen.queryByRole('option', { name: /INV-GLBX-01/ })).not.toBeInTheDocument();

        await user.click(await screen.findByRole('option', { name: /INV-ACME-01/ }));

        // Linking the invoice adopts its currency (INR) for the amount prefix.
        await waitFor(() => expect(screen.getByText('INR')).toBeInTheDocument());

        fireEvent.change(document.getElementById('amount'), { target: { value: '1000' } });
        fireEvent.submit(document.getElementById('create-credit-note-form'));

        await waitFor(() => expect(endpoints.createCreditNote).toHaveBeenCalledTimes(1));
        const payload = endpoints.createCreditNote.mock.calls[0][0];
        expect(payload.invoice_id).toBe(ACME_INV.id);
        expect(payload.currency).toBe('INR');
    });

    it('resets the linked invoice when the customer changes', async () => {
        const user = userEvent.setup();
        renderPage();
        await waitFor(() => expect(endpoints.getCustomers).toHaveBeenCalled());

        await pickOption(user, 'customer_id', /Acme Corp/);
        await pickOption(user, 'invoice_id', /INV-ACME-01/);
        await waitFor(() => expect(screen.getByText('INR')).toBeInTheDocument());

        // Switch to Globex — the Acme link and its INR currency must clear:
        // the currency follows Globex's own (USD) invoices.
        await pickOption(user, 'customer_id', /Globex/);
        await waitFor(() => expect(document.getElementById('currency')).toHaveTextContent('USD'));
        // The currency picker is back (no invoice linked) and holds no INR.
        expect(document.getElementById('currency')).not.toHaveTextContent('INR');

        // ...and a submit now carries no linked invoice.
        fireEvent.change(document.getElementById('amount'), { target: { value: '25' } });
        fireEvent.submit(document.getElementById('create-credit-note-form'));
        await waitFor(() => expect(endpoints.createCreditNote).toHaveBeenCalledTimes(1));
        expect(endpoints.createCreditNote.mock.calls[0][0].invoice_id).toBeUndefined();
    });
});

describe('CreateCreditNote — currency', () => {
    beforeEach(() => {
        vi.clearAllMocks();
        endpoints.getCustomers.mockResolvedValue({ data: { data: [ACME, GLOBEX, RIYADH, NEWCO] } });
        endpoints.getInvoices.mockResolvedValue({ data: { data: [ACME_INV, GLOBEX_INV, RIYADH_INV] } });
        endpoints.createCreditNote.mockResolvedValue({ data: { data: { id: 'cn-1' } } });
    });

    it('defaults a standalone credit to the customer\'s billing currency, not USD', async () => {
        const user = userEvent.setup();
        renderPage();
        await waitFor(() => expect(endpoints.getCustomers).toHaveBeenCalled());

        await pickOption(user, 'customer_id', /Riyadh Co/);
        await waitFor(() => expect(document.getElementById('currency')).toHaveTextContent('SAR'));

        fireEvent.change(document.getElementById('amount'), { target: { value: '50' } });
        fireEvent.submit(document.getElementById('create-credit-note-form'));
        await waitFor(() => expect(endpoints.createCreditNote).toHaveBeenCalledTimes(1));
        const payload = endpoints.createCreditNote.mock.calls[0][0];
        expect(payload.currency).toBe('SAR');
        expect(payload.amount).toBe(5000);
        expect(payload.invoice_id).toBeUndefined();
    });

    it('lets the operator pick any billing currency, SAR included, for a customer with no invoices', async () => {
        const user = userEvent.setup();
        renderPage();
        await waitFor(() => expect(endpoints.getCustomers).toHaveBeenCalled());

        await pickOption(user, 'customer_id', /Newco/);
        fireEvent.change(document.getElementById('amount'), { target: { value: '10' } });
        // No currency known yet: the form asks instead of assuming USD.
        fireEvent.submit(document.getElementById('create-credit-note-form'));
        expect(await screen.findByText('Select a currency.')).toBeInTheDocument();
        expect(endpoints.createCreditNote).not.toHaveBeenCalled();

        await pickOption(user, 'currency', 'SAR');
        fireEvent.submit(document.getElementById('create-credit-note-form'));
        await waitFor(() => expect(endpoints.createCreditNote).toHaveBeenCalledTimes(1));
        expect(endpoints.createCreditNote.mock.calls[0][0].currency).toBe('SAR');
    });
});

describe('CreateCreditNote — refund to the original payment', () => {
    beforeEach(() => {
        vi.clearAllMocks();
        endpoints.getCustomers.mockResolvedValue({ data: { data: [ACME, GLOBEX, RIYADH, NEWCO] } });
        endpoints.getInvoices.mockResolvedValue({ data: { data: [ACME_INV, GLOBEX_INV, RIYADH_INV] } });
        endpoints.createCreditNote.mockResolvedValue({ data: { data: { id: 'cn-1', type: 'refund' } } });
    });

    it('offers a refund only once a paid invoice is linked, and explains why', async () => {
        const user = userEvent.setup();
        renderPage();
        await waitFor(() => expect(endpoints.getCustomers).toHaveBeenCalled());

        await pickOption(user, 'customer_id', /Acme Corp/);
        expect(screen.getByText(/A refund needs a linked paid invoice/)).toBeInTheDocument();
        await user.click(document.getElementById('type'));
        expect(await screen.findByRole('option', { name: /Refund to the original payment/ }))
            .toHaveAttribute('aria-disabled', 'true');
        await user.keyboard('{Escape}');

        // An open (unpaid) invoice can't be refunded either.
        await pickOption(user, 'invoice_id', /INV-ACME-01/);
        await user.click(document.getElementById('type'));
        expect(await screen.findByRole('option', { name: /Refund to the original payment/ }))
            .toHaveAttribute('aria-disabled', 'true');
    });

    it('issues a refund against the linked paid invoice and opens the note to show its refund status', async () => {
        const user = userEvent.setup();
        renderPage();
        await waitFor(() => expect(endpoints.getCustomers).toHaveBeenCalled());

        await pickOption(user, 'customer_id', /Riyadh Co/);
        await pickOption(user, 'invoice_id', /INV-RYD-01/);
        await pickOption(user, 'type', /Refund to the original payment/);

        // A refund carries no spendable balance, so there is no expiry.
        expect(document.getElementById('expires_at')).toBeNull();
        expect(screen.getByText(/Up to SAR\s?115\.00/)).toBeInTheDocument();

        fireEvent.change(document.getElementById('amount'), { target: { value: '40' } });
        fireEvent.submit(document.getElementById('create-credit-note-form'));

        await waitFor(() => expect(endpoints.createCreditNote).toHaveBeenCalledTimes(1));
        const payload = endpoints.createCreditNote.mock.calls[0][0];
        expect(payload).toMatchObject({
            type: 'refund',
            invoice_id: RIYADH_INV.id,
            currency: 'SAR',
            amount: 4000,
        });
        expect(payload.expires_at).toBeUndefined();
        await waitFor(() => expect(navigateMock).toHaveBeenCalledWith('/credit-notes/cn-1'));
    });

    it('blocks a refund larger than what was paid on the invoice', async () => {
        const user = userEvent.setup();
        renderPage();
        await waitFor(() => expect(endpoints.getCustomers).toHaveBeenCalled());

        await pickOption(user, 'customer_id', /Riyadh Co/);
        await pickOption(user, 'invoice_id', /INV-RYD-01/);
        await pickOption(user, 'type', /Refund to the original payment/);

        fireEvent.change(document.getElementById('amount'), { target: { value: '200' } });
        fireEvent.submit(document.getElementById('create-credit-note-form'));
        expect(await screen.findByText(/more than was paid/)).toBeInTheDocument();
        expect(endpoints.createCreditNote).not.toHaveBeenCalled();
    });

    it('falls back to a balance credit when the linked invoice changes to an unpaid one', async () => {
        const user = userEvent.setup();
        renderPage();
        await waitFor(() => expect(endpoints.getCustomers).toHaveBeenCalled());

        await pickOption(user, 'customer_id', /Riyadh Co/);
        await pickOption(user, 'invoice_id', /INV-RYD-01/);
        await pickOption(user, 'type', /Refund to the original payment/);
        await pickOption(user, 'invoice_id', /No linked invoice/);

        expect(document.getElementById('type')).toHaveTextContent('Credit to balance');
        fireEvent.change(document.getElementById('amount'), { target: { value: '5' } });
        fireEvent.submit(document.getElementById('create-credit-note-form'));
        await waitFor(() => expect(endpoints.createCreditNote).toHaveBeenCalledTimes(1));
        expect(endpoints.createCreditNote.mock.calls[0][0].type).toBe('adjustment');
        expect(navigateMock).toHaveBeenCalledWith('/credit-notes');
    });
});
