package handler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
	"github.com/recurso-dev/recurso/internal/core/port/porttest"
	"github.com/recurso-dev/recurso/internal/service"
)

// Shared fakes for the Moyasar checkout and webhook tests. Both handlers
// settle through a real service.GatewaySettlementService and a real
// MarkInvoicePaid; only the repositories and the gateway are faked.

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

// fakeSettlementInvoices serves the unscoped lookup plus the reads and writes
// settlement makes, recording the tenant each one ran under.
type fakeSettlementInvoices struct {
	porttest.UnimplementedInvoiceRepository
	inv *domain.Invoice
	// settledElsewhere makes MarkPaid lose the race to another settler.
	settledElsewhere bool
	loadErr          error
	markPaidErr      error

	markPaidCalls  int
	markPaidTenant uuid.UUID
	getByIDTenant  any
	claims         []string
	claimTenant    uuid.UUID
}

func (r *fakeSettlementInvoices) GetByIDPublic(_ context.Context, id uuid.UUID) (*domain.Invoice, error) {
	if r.loadErr != nil {
		return nil, r.loadErr
	}
	if r.inv == nil || r.inv.ID != id {
		return nil, nil
	}
	cp := *r.inv
	return &cp, nil
}

func (r *fakeSettlementInvoices) GetByID(ctx context.Context, id uuid.UUID) (*domain.Invoice, error) {
	r.getByIDTenant = ctx.Value(domain.TenantIDKey)
	return r.GetByIDPublic(ctx, id)
}

func (r *fakeSettlementInvoices) MarkPaid(_ context.Context, tenantID, _ uuid.UUID, paidAt time.Time) (bool, error) {
	r.markPaidCalls++
	r.markPaidTenant = tenantID
	if r.markPaidErr != nil {
		return false, r.markPaidErr
	}
	if r.settledElsewhere || r.inv.Status == domain.InvoiceStatusPaid {
		return false, nil
	}
	r.inv.Status = domain.InvoiceStatusPaid
	r.inv.PaidAt = &paidAt
	r.inv.AmountPaid = r.inv.Total
	return true, nil
}

func (r *fakeSettlementInvoices) ClaimGatewayPaymentID(_ context.Context, tenantID, _ uuid.UUID, paymentID string) (bool, error) {
	r.claims = append(r.claims, paymentID)
	r.claimTenant = tenantID
	if r.inv.GatewayPaymentID != "" && r.inv.GatewayPaymentID != paymentID {
		return false, nil
	}
	r.inv.GatewayPaymentID = paymentID
	return true, nil
}

type fakeSettlementSubs struct{ sub *domain.Subscription }

func (f *fakeSettlementSubs) GetByID(_ context.Context, id uuid.UUID) (*domain.Subscription, error) {
	if f.sub == nil || f.sub.ID != id {
		return nil, errors.New("subscription not found")
	}
	return f.sub, nil
}

type savedGatewayCard struct {
	ctxTenant            any
	tenantID, customerID uuid.UUID
	provider             domain.GatewayProvider
	ref, brand, last4    string
	expMonth, expYear    int
}

type fakeGatewayCards struct{ saved []savedGatewayCard }

func (f *fakeGatewayCards) SetGatewayPaymentMethod(ctx context.Context, tenantID, customerID uuid.UUID, provider domain.GatewayProvider, ref, brand, last4 string, expMonth, expYear int) error {
	f.saved = append(f.saved, savedGatewayCard{ctx.Value(domain.TenantIDKey), tenantID, customerID, provider, ref, brand, last4, expMonth, expYear})
	return nil
}

type fakeCardTokens struct{}

func (fakeCardTokens) FetchToken(context.Context, string) (int, int, error) { return 7, 2031, nil }

type fakeDunningRecovery struct {
	recovered []uuid.UUID
	outcomes  []domain.DunningHistory
}

func (f *fakeDunningRecovery) MarkRecovered(_ context.Context, id uuid.UUID) error {
	f.recovered = append(f.recovered, id)
	return nil
}

func (f *fakeDunningRecovery) RecordOutcome(_ context.Context, h domain.DunningHistory) error {
	f.outcomes = append(f.outcomes, h)
	return nil
}

// moyasarSettlementFixture is an open SAR invoice of a renewing subscription
// with 80.00 still due, and a paid tokenized payment of exactly that, wired
// through the real settlement service.
type moyasarSettlementFixture struct {
	inv      *domain.Invoice
	sub      *domain.Subscription
	payment  *port.GatewayPayment
	reader   *fakePaymentReader
	invoices *fakeSettlementInvoices
	subs     *fakeSettlementSubs
	cards    *fakeGatewayCards
	dunning  *fakeDunningRecovery
	settler  *service.GatewaySettlementService
}

func newMoyasarSettlementFixture() *moyasarSettlementFixture {
	sub := &domain.Subscription{ID: uuid.New(), Status: domain.SubscriptionStatusActive}
	inv := &domain.Invoice{
		ID: uuid.New(), TenantID: uuid.New(), CustomerID: uuid.New(), SubscriptionID: &sub.ID,
		InvoiceNumber: "INV-42", Status: domain.InvoiceStatusOpen, Currency: "SAR",
		Total: 10000, AmountPaid: 1500, CreditApplied: 500, AmountDue: 8000,
		DunningActionID: "retry_24h", DunningContextKey: "ctx-key",
	}
	sub.TenantID = inv.TenantID
	payment := &port.GatewayPayment{
		ID: uuid.NewString(), Status: "paid", Amount: 8000, Currency: "SAR", InvoiceID: inv.ID.String(),
		CardToken: "token_abc", CardBrand: "mada", CardLast4: "1010",
	}
	f := &moyasarSettlementFixture{
		inv: inv, sub: sub, payment: payment,
		reader:   &fakePaymentReader{payment: payment},
		invoices: &fakeSettlementInvoices{inv: inv},
		subs:     &fakeSettlementSubs{sub: sub},
		cards:    &fakeGatewayCards{},
		dunning:  &fakeDunningRecovery{},
	}
	subSvc := service.NewSubscriptionService(nil, f.invoices, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	f.settler = service.NewGatewaySettlementService(domain.GatewayMoyasar, subSvc, f.invoices, f.subs)
	f.settler.SetCardSaving(f.cards, fakeCardTokens{})
	f.settler.SetDunningRecovery(f.dunning, f.dunning)
	return f
}

// wantSavedCard is the card the fixture's payment must store.
func (f *moyasarSettlementFixture) wantSavedCard() savedGatewayCard {
	return savedGatewayCard{f.inv.TenantID, f.inv.TenantID, f.inv.CustomerID, domain.GatewayMoyasar, "token_abc", "mada", "1010", 7, 2031}
}
