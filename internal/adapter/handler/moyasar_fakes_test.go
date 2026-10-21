package handler

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
	"github.com/recurso-dev/recurso/internal/core/port/porttest"
	"github.com/recurso-dev/recurso/internal/service"
)

// Shared fakes for the Moyasar checkout and webhook tests. The handlers own
// the HTTP mapping only; what settlement does to the invoice, the card and
// dunning is tested in the service package, so here it is a recording fake.

// fakePaymentReader returns its canned payment only for that payment's id,
// so a test proves which id was fetched.
type fakePaymentReader struct {
	payment *port.GatewayPayment
	err     error
	gotIDs  []string
}

func (f *fakePaymentReader) FetchPayment(_ context.Context, id string) (*port.GatewayPayment, error) {
	f.gotIDs = append(f.gotIDs, id)
	if f.err != nil {
		return nil, f.err
	}
	if f.payment == nil || f.payment.ID != id {
		return nil, fmt.Errorf("payment %s not found", id)
	}
	cp := *f.payment
	return &cp, nil
}

// fakeCheckoutInvoices serves the unscoped invoice lookup the public routes
// make.
type fakeCheckoutInvoices struct {
	porttest.UnimplementedInvoiceRepository
	inv     *domain.Invoice
	loadErr error
}

func (r *fakeCheckoutInvoices) GetByIDPublic(_ context.Context, id uuid.UUID) (*domain.Invoice, error) {
	if r.loadErr != nil {
		return nil, r.loadErr
	}
	if r.inv == nil || r.inv.ID != id {
		return nil, nil
	}
	cp := *r.inv
	return &cp, nil
}

type settleCall struct {
	invoiceID uuid.UUID
	paymentID string
}

// fakePaymentSettler records what the handlers ask of the settlement path and
// answers with canned results.
type fakePaymentSettler struct {
	transitioned bool
	err          error
	savesCard    bool
	terms        *service.CardOnFileTerms
	settled      []settleCall

	// The already-captured payment check; existing marks the invoice paid
	// in the fixture's repository, as settling would.
	existing      bool
	existingErr   error
	existingCalls int
	invoices      *fakeCheckoutInvoices
}

func (f *fakePaymentSettler) SettleExistingPayment(_ context.Context, inv *domain.Invoice) (bool, error) {
	f.existingCalls++
	if f.existingErr != nil {
		return false, f.existingErr
	}
	if f.existing && f.invoices != nil && f.invoices.inv.ID == inv.ID {
		f.invoices.inv.Status = domain.InvoiceStatusPaid
		f.invoices.inv.AmountDue = 0
	}
	return f.existing, nil
}

func (f *fakePaymentSettler) Settle(_ context.Context, inv *domain.Invoice, p *port.GatewayPayment) (bool, error) {
	f.settled = append(f.settled, settleCall{inv.ID, p.ID})
	if f.err != nil {
		return false, f.err
	}
	return f.transitioned, nil
}

func (f *fakePaymentSettler) CardOnFile(context.Context, *domain.Invoice) (bool, *service.CardOnFileTerms) {
	return f.savesCard, f.terms
}

// moyasarHandlerFixture is an open SAR invoice with 80.00 still due and a
// paid payment of exactly that bound to it.
type moyasarHandlerFixture struct {
	inv      *domain.Invoice
	payment  *port.GatewayPayment
	reader   *fakePaymentReader
	invoices *fakeCheckoutInvoices
	settler  *fakePaymentSettler
}

func newMoyasarHandlerFixture() *moyasarHandlerFixture {
	subID := uuid.New()
	inv := &domain.Invoice{
		ID: uuid.New(), TenantID: uuid.New(), CustomerID: uuid.New(), SubscriptionID: &subID,
		InvoiceNumber: "INV-42", Status: domain.InvoiceStatusOpen, Currency: "SAR",
		Total: 10000, AmountPaid: 1500, CreditApplied: 500, AmountDue: 8000,
	}
	payment := &port.GatewayPayment{
		ID: uuid.NewString(), Status: "paid", Amount: 8000, Currency: "SAR", InvoiceID: inv.ID.String(),
		CardToken: "token_abc", CardBrand: "mada", CardLast4: "1010",
	}
	invoices := &fakeCheckoutInvoices{inv: inv}
	return &moyasarHandlerFixture{
		inv: inv, payment: payment,
		reader:   &fakePaymentReader{payment: payment},
		invoices: invoices,
		settler:  &fakePaymentSettler{transitioned: true, savesCard: true, invoices: invoices},
	}
}

// settledOnce reports whether the fixture's payment was settled exactly once
// against the fixture's invoice.
func (f *moyasarHandlerFixture) settledOnce() bool {
	return len(f.settler.settled) == 1 && f.settler.settled[0] == settleCall{f.inv.ID, f.payment.ID}
}
