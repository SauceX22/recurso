package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
)

type settleCall struct {
	invoiceID uuid.UUID
	tenant    any
}

type fakeInvoicePaidMarker struct {
	transitioned bool
	err          error
	calls        []settleCall
}

func (f *fakeInvoicePaidMarker) MarkInvoicePaid(ctx context.Context, id uuid.UUID) (bool, error) {
	f.calls = append(f.calls, settleCall{id, ctx.Value(domain.TenantIDKey)})
	return f.transitioned, f.err
}

type claimCall struct {
	tenantID, invoiceID uuid.UUID
	paymentID           string
}

// fakePaymentClaimer mirrors ClaimGatewayPaymentID: the first payment wins,
// the same payment re-claims.
type fakePaymentClaimer struct {
	recorded string
	err      error
	calls    []claimCall
}

func (f *fakePaymentClaimer) ClaimGatewayPaymentID(_ context.Context, tenantID, invoiceID uuid.UUID, paymentID string) (bool, error) {
	f.calls = append(f.calls, claimCall{tenantID, invoiceID, paymentID})
	if f.err != nil {
		return false, f.err
	}
	if f.recorded == "" || f.recorded == paymentID {
		f.recorded = paymentID
		return true, nil
	}
	return false, nil
}

type fakeSettlementSubs struct {
	sub    *domain.Subscription
	tenant any
}

func (f *fakeSettlementSubs) GetByID(ctx context.Context, id uuid.UUID) (*domain.Subscription, error) {
	f.tenant = ctx.Value(domain.TenantIDKey)
	if f.sub == nil || f.sub.ID != id {
		return nil, errors.New("subscription not found")
	}
	return f.sub, nil
}

type savedMethodCall struct {
	ctxTenant            any
	tenantID, customerID uuid.UUID
	provider             domain.GatewayProvider
	ref, brand, last4    string
	expMonth, expYear    int
}

type fakeMethodSaver struct{ calls []savedMethodCall }

func (f *fakeMethodSaver) SetGatewayPaymentMethod(ctx context.Context, tenantID, customerID uuid.UUID, provider domain.GatewayProvider, ref, brand, last4 string, expMonth, expYear int) error {
	f.calls = append(f.calls, savedMethodCall{ctx.Value(domain.TenantIDKey), tenantID, customerID, provider, ref, brand, last4, expMonth, expYear})
	return nil
}

type fakeTokenReader struct {
	month, year int
	err         error
	got         []string
}

func (f *fakeTokenReader) FetchToken(_ context.Context, id string) (int, int, error) {
	f.got = append(f.got, id)
	return f.month, f.year, f.err
}

type fakeCampaignRecovery struct{ invoices []uuid.UUID }

func (f *fakeCampaignRecovery) MarkRecovered(_ context.Context, id uuid.UUID) error {
	f.invoices = append(f.invoices, id)
	return nil
}

type fakeOutcomeRecorder struct{ outcomes []domain.DunningHistory }

func (f *fakeOutcomeRecorder) RecordOutcome(_ context.Context, h domain.DunningHistory) error {
	f.outcomes = append(f.outcomes, h)
	return nil
}

type settlementFixture struct {
	svc       *GatewaySettlementService
	settler   *fakeInvoicePaidMarker
	claims    *fakePaymentClaimer
	subs      *fakeSettlementSubs
	cards     *fakeMethodSaver
	tokens    *fakeTokenReader
	campaigns *fakeCampaignRecovery
	outcomes  *fakeOutcomeRecorder
	inv       *domain.Invoice
	payment   *port.GatewayPayment
}

// newSettlementFixture: an open 80.00 SAR balance on an active subscription's
// invoice, paid in full by a tokenized card payment.
func newSettlementFixture() *settlementFixture {
	sub := &domain.Subscription{ID: uuid.New(), Status: domain.SubscriptionStatusActive}
	inv := &domain.Invoice{
		ID: uuid.New(), TenantID: uuid.New(), CustomerID: uuid.New(), SubscriptionID: &sub.ID,
		Status: domain.InvoiceStatusOpen, Currency: "SAR", Total: 10000, AmountPaid: 1500, CreditApplied: 500, AmountDue: 8000,
		DunningActionID: "retry_24h", DunningContextKey: "ctx-key",
	}
	sub.TenantID = inv.TenantID
	f := &settlementFixture{
		settler:   &fakeInvoicePaidMarker{transitioned: true},
		claims:    &fakePaymentClaimer{},
		subs:      &fakeSettlementSubs{sub: sub},
		cards:     &fakeMethodSaver{},
		tokens:    &fakeTokenReader{month: 7, year: 2031},
		campaigns: &fakeCampaignRecovery{},
		outcomes:  &fakeOutcomeRecorder{},
		inv:       inv,
		payment: &port.GatewayPayment{
			ID: "pay-1", Status: "paid", Amount: 8000, Currency: "SAR", InvoiceID: inv.ID.String(),
			CardToken: "token_abc", CardBrand: "mada", CardLast4: "1010",
		},
	}
	f.svc = NewGatewaySettlementService(domain.GatewayMoyasar, f.settler, f.claims, f.subs)
	f.svc.SetCardSaving(f.cards, f.tokens)
	f.svc.SetDunningRecovery(f.campaigns, f.outcomes)
	return f
}

func TestGatewaySettlement_SettlesRecordsSavesAndRecovers(t *testing.T) {
	f := newSettlementFixture()
	transitioned, err := f.svc.Settle(context.Background(), f.inv, f.payment)
	if err != nil || !transitioned {
		t.Fatalf("Settle = %v, %v; want transitioned", transitioned, err)
	}
	if len(f.settler.calls) != 1 || f.settler.calls[0] != (settleCall{f.inv.ID, f.inv.TenantID}) {
		t.Fatalf("MarkInvoicePaid calls = %+v, want one under the invoice tenant %s", f.settler.calls, f.inv.TenantID)
	}
	if want := (claimCall{f.inv.TenantID, f.inv.ID, "pay-1"}); len(f.claims.calls) != 1 || f.claims.calls[0] != want {
		t.Fatalf("claims = %+v, want [%+v]", f.claims.calls, want)
	}
	if f.subs.tenant != f.inv.TenantID {
		t.Fatalf("subscription read under tenant %v, want %s", f.subs.tenant, f.inv.TenantID)
	}
	want := savedMethodCall{f.inv.TenantID, f.inv.TenantID, f.inv.CustomerID, domain.GatewayMoyasar, "token_abc", "mada", "1010", 7, 2031}
	if len(f.cards.calls) != 1 || f.cards.calls[0] != want {
		t.Fatalf("saved = %+v, want [%+v]", f.cards.calls, want)
	}
	if len(f.tokens.got) != 1 || f.tokens.got[0] != "token_abc" {
		t.Fatalf("FetchToken calls = %v, want [token_abc]", f.tokens.got)
	}
	if len(f.campaigns.invoices) != 1 || f.campaigns.invoices[0] != f.inv.ID {
		t.Fatalf("campaign recoveries = %v, want [%s]", f.campaigns.invoices, f.inv.ID)
	}
	if len(f.outcomes.outcomes) != 1 {
		t.Fatalf("dunning outcomes = %d, want 1", len(f.outcomes.outcomes))
	}
	o := f.outcomes.outcomes[0]
	if o.InvoiceID != f.inv.ID || o.TenantID != f.inv.TenantID || o.Outcome != "success" || o.Reward != 1.0 ||
		o.ContextKey != "ctx-key" || o.ActionID != "retry_24h" {
		t.Fatalf("dunning outcome = %+v", o)
	}
}

func TestGatewaySettlement_BindingFailuresDoNotSettle(t *testing.T) {
	cases := map[string]struct {
		mutate func(*port.GatewayPayment)
		want   error
	}{
		"another invoice":         {func(p *port.GatewayPayment) { p.InvoiceID = uuid.NewString() }, ErrPaymentWrongInvoice},
		"no invoice metadata":     {func(p *port.GatewayPayment) { p.InvoiceID = "" }, ErrPaymentWrongInvoice},
		"other currency":          {func(p *port.GatewayPayment) { p.Currency = "USD" }, ErrPaymentWrongCurrency},
		"short of the amount due": {func(p *port.GatewayPayment) { p.Amount = 7999 }, ErrPaymentShortAmount},
		"refund leaves it short":  {func(p *port.GatewayPayment) { p.RefundedAmount = 1 }, ErrPaymentShortAmount},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newSettlementFixture()
			tc.mutate(f.payment)
			if _, err := f.svc.Settle(context.Background(), f.inv, f.payment); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if len(f.settler.calls)+len(f.claims.calls)+len(f.cards.calls)+len(f.outcomes.outcomes) != 0 {
				t.Fatalf("unbound payment had effects: settle=%d claim=%d cards=%d outcomes=%d",
					len(f.settler.calls), len(f.claims.calls), len(f.cards.calls), len(f.outcomes.outcomes))
			}
		})
	}
}

func TestBindPaymentToInvoice_AcceptsLowerCaseCurrencyAndOverpayment(t *testing.T) {
	f := newSettlementFixture()
	f.payment.Currency = "sar"
	f.payment.Amount = 9000
	if err := BindPaymentToInvoice(f.payment, f.inv); err != nil {
		t.Fatalf("BindPaymentToInvoice = %v, want nil", err)
	}
}

// A second paid payment for an invoice another payment settled must not
// replace the refund target or the saved card.
func TestGatewaySettlement_SecondPaymentIsFlaggedNotRecorded(t *testing.T) {
	f := newSettlementFixture()
	f.settler.transitioned = false
	f.claims.recorded = "pay-A"
	transitioned, err := f.svc.Settle(context.Background(), f.inv, f.payment)
	if err != nil || transitioned {
		t.Fatalf("Settle = %v, %v; want not transitioned, nil", transitioned, err)
	}
	if f.claims.recorded != "pay-A" {
		t.Fatalf("recorded payment = %q, want pay-A kept", f.claims.recorded)
	}
	if len(f.cards.calls) != 0 || len(f.tokens.got) != 0 {
		t.Fatalf("duplicate payment saved its card: %+v", f.cards.calls)
	}
	if len(f.outcomes.outcomes) != 0 || len(f.campaigns.invoices) != 0 {
		t.Fatal("dunning recovery recorded without the paid transition")
	}
}

// The webhook and the checkout return both settle the same payment; the one
// that did not transition still records it (it owns the invoice) but must
// not count the recovery twice.
func TestGatewaySettlement_RedeliveryOfRecordedPayment(t *testing.T) {
	for name, recorded := range map[string]string{"already recorded": "pay-1", "never recorded": ""} {
		t.Run(name, func(t *testing.T) {
			f := newSettlementFixture()
			f.settler.transitioned = false
			f.claims.recorded = recorded
			if _, err := f.svc.Settle(context.Background(), f.inv, f.payment); err != nil {
				t.Fatal(err)
			}
			if f.claims.recorded != "pay-1" || len(f.cards.calls) != 1 {
				t.Fatalf("recorded=%q cards=%d, want pay-1 and the card saved", f.claims.recorded, len(f.cards.calls))
			}
			if len(f.outcomes.outcomes) != 0 || len(f.campaigns.invoices) != 0 {
				t.Fatal("redelivery recorded the dunning recovery again")
			}
		})
	}
}

func TestGatewaySettlement_SavesCardOnlyForRenewingSubscriptions(t *testing.T) {
	cases := map[string]func(*settlementFixture){
		"one-off invoice":         func(f *settlementFixture) { f.inv.SubscriptionID = nil },
		"canceled subscription":   func(f *settlementFixture) { f.subs.sub.Status = domain.SubscriptionStatusCanceled },
		"cancels at period end":   func(f *settlementFixture) { f.subs.sub.CancelAtPeriodEnd = true },
		"subscription not found":  func(f *settlementFixture) { f.subs.sub = nil },
		"payment created no card": func(f *settlementFixture) { f.payment.CardToken = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newSettlementFixture()
			mutate(f)
			if transitioned, err := f.svc.Settle(context.Background(), f.inv, f.payment); err != nil || !transitioned {
				t.Fatalf("Settle = %v, %v; the invoice must still settle", transitioned, err)
			}
			if len(f.cards.calls) != 0 || len(f.tokens.got) != 0 {
				t.Fatalf("card saved: %+v", f.cards.calls)
			}
			if f.claims.recorded != "pay-1" {
				t.Fatalf("payment id not recorded: %q", f.claims.recorded)
			}
		})
	}
}

func TestGatewaySettlement_SavesCard(t *testing.T) {
	t.Run("trialing subscription", func(t *testing.T) {
		f := newSettlementFixture()
		f.subs.sub.Status = domain.SubscriptionStatusTrialing
		if !f.svc.SavesCard(context.Background(), f.inv) {
			t.Fatal("trialing subscription renews; SavesCard = false")
		}
	})
	t.Run("no card saver wired", func(t *testing.T) {
		f := newSettlementFixture()
		f.svc.SetCardSaving(nil, nil)
		if f.svc.SavesCard(context.Background(), f.inv) {
			t.Fatal("SavesCard = true without a saver")
		}
	})
	t.Run("nil service", func(t *testing.T) {
		var s *GatewaySettlementService
		if s.SavesCard(context.Background(), newSettlementFixture().inv) {
			t.Fatal("nil service SavesCard = true")
		}
	})
}

func TestGatewaySettlement_TokenExpiryIsBestEffort(t *testing.T) {
	f := newSettlementFixture()
	f.tokens.err = errors.New("gateway down")
	if _, err := f.svc.Settle(context.Background(), f.inv, f.payment); err != nil {
		t.Fatal(err)
	}
	if len(f.cards.calls) != 1 || f.cards.calls[0].expMonth != 0 || f.cards.calls[0].expYear != 0 {
		t.Fatalf("saved = %+v, want the card with unknown expiry", f.cards.calls)
	}
}

func TestGatewaySettlement_SettleErrorStopsEverything(t *testing.T) {
	f := newSettlementFixture()
	f.settler.err = errors.New("db down")
	if _, err := f.svc.Settle(context.Background(), f.inv, f.payment); err == nil {
		t.Fatal("Settle swallowed the settlement error")
	}
	if len(f.claims.calls)+len(f.cards.calls)+len(f.outcomes.outcomes) != 0 {
		t.Fatal("effects ran after a failed settlement")
	}
}

func TestGatewaySettlement_ClaimErrorKeepsTransitionOwnership(t *testing.T) {
	f := newSettlementFixture()
	f.claims.err = errors.New("db down")
	if transitioned, err := f.svc.Settle(context.Background(), f.inv, f.payment); err != nil || !transitioned {
		t.Fatalf("Settle = %v, %v", transitioned, err)
	}
	if len(f.cards.calls) != 1 {
		t.Fatalf("the transitioning payment owns the invoice; cards = %d, want 1", len(f.cards.calls))
	}
}
