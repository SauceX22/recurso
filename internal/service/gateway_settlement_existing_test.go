package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
)

type paidLookupCall struct {
	invoiceID, currency string
	amount              int64
}

type fakePaidPaymentFinder struct {
	found *port.GatewayPayment
	err   error
	calls []paidLookupCall
}

func (f *fakePaidPaymentFinder) FindPaidPayment(_ context.Context, invoiceID, currency string, amount int64) (*port.GatewayPayment, error) {
	f.calls = append(f.calls, paidLookupCall{invoiceID, currency, amount})
	return f.found, f.err
}

func withPaidLookup(f *settlementFixture, finder *fakePaidPaymentFinder) {
	f.svc.SetPaidPaymentLookup(finder, routeSAR)
}

// A payment the gateway already captured (a renewal whose answer was lost)
// settles the invoice instead of letting the buyer pay a second time.
func TestGatewaySettlement_SettleExistingPaymentSettlesIt(t *testing.T) {
	f := newSettlementFixture()
	finder := &fakePaidPaymentFinder{found: f.payment}
	withPaidLookup(f, finder)
	settled, err := f.svc.SettleExistingPayment(context.Background(), f.inv)
	if err != nil || !settled {
		t.Fatalf("SettleExistingPayment = %v, %v; want settled", settled, err)
	}
	if want := (paidLookupCall{f.inv.ID.String(), "SAR", 8000}); len(finder.calls) != 1 || finder.calls[0] != want {
		t.Fatalf("lookup calls = %+v, want [%+v] (the amount due)", finder.calls, want)
	}
	if len(f.settler.calls) != 1 || len(f.claims.calls) != 1 || f.claims.calls[0].paymentID != f.payment.ID {
		t.Fatalf("settle=%d claims=%+v, want the found payment settled through Settle", len(f.settler.calls), f.claims.calls)
	}
}

func TestGatewaySettlement_SettleExistingPaymentNothingFound(t *testing.T) {
	f := newSettlementFixture()
	withPaidLookup(f, &fakePaidPaymentFinder{})
	settled, err := f.svc.SettleExistingPayment(context.Background(), f.inv)
	if err != nil || settled || len(f.settler.calls) != 0 {
		t.Fatalf("got %v, %v with %d settles; want nothing", settled, err, len(f.settler.calls))
	}
}

// The caller must not take a new payment when it could not check.
func TestGatewaySettlement_SettleExistingPaymentLookupErrorIsReturned(t *testing.T) {
	f := newSettlementFixture()
	withPaidLookup(f, &fakePaidPaymentFinder{err: errors.New("moyasar: 503")})
	if settled, err := f.svc.SettleExistingPayment(context.Background(), f.inv); err == nil || settled {
		t.Fatalf("got %v, %v; want the lookup error", settled, err)
	}
}

// A found payment still has to bind to the invoice.
func TestGatewaySettlement_SettleExistingPaymentStillBinds(t *testing.T) {
	f := newSettlementFixture()
	f.payment.Amount = 7999
	withPaidLookup(f, &fakePaidPaymentFinder{found: f.payment})
	if settled, err := f.svc.SettleExistingPayment(context.Background(), f.inv); !errors.Is(err, ErrPaymentShortAmount) || settled {
		t.Fatalf("got %v, %v; want ErrPaymentShortAmount", settled, err)
	}
	if len(f.settler.calls) != 0 {
		t.Fatal("unbound payment settled")
	}
}

func TestGatewaySettlement_SettleExistingPaymentSkips(t *testing.T) {
	cases := map[string]func(*settlementFixture, *fakePaidPaymentFinder){
		"currency routed elsewhere": func(f *settlementFixture, finder *fakePaidPaymentFinder) {
			withPaidLookup(f, finder)
			f.inv.Currency = "USD"
		},
		"already paid": func(f *settlementFixture, finder *fakePaidPaymentFinder) {
			withPaidLookup(f, finder)
			f.inv.Status = domain.InvoiceStatusPaid
		},
		"nothing due": func(f *settlementFixture, finder *fakePaidPaymentFinder) {
			withPaidLookup(f, finder)
			f.inv.AmountDue = 0
		},
		"no lookup wired": func(*settlementFixture, *fakePaidPaymentFinder) {},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newSettlementFixture()
			finder := &fakePaidPaymentFinder{found: f.payment}
			setup(f, finder)
			settled, err := f.svc.SettleExistingPayment(context.Background(), f.inv)
			if err != nil || settled || len(finder.calls) != 0 || len(f.settler.calls) != 0 {
				t.Fatalf("got %v, %v, lookups=%d settles=%d; want a no-op", settled, err, len(finder.calls), len(f.settler.calls))
			}
		})
	}
}

func TestGatewaySettlement_SettleExistingPaymentNilService(t *testing.T) {
	var s *GatewaySettlementService
	if settled, err := s.SettleExistingPayment(context.Background(), &domain.Invoice{}); settled || err != nil {
		t.Fatalf("nil service = %v, %v", settled, err)
	}
}

// Public checkout carries no tenant, so the routing check runs as the
// invoice's tenant (a BYO-Stripe tenant's SAR never queries Moyasar).
func TestGatewaySettlement_SettleExistingPaymentRoutesAsInvoiceTenant(t *testing.T) {
	f := newSettlementFixture()
	finder := &fakePaidPaymentFinder{found: f.payment}
	var routedAs uuid.UUID
	f.svc.SetPaidPaymentLookup(finder, func(ctx context.Context, currency string) domain.GatewayProvider {
		routedAs, _ = ctx.Value(domain.TenantIDKey).(uuid.UUID)
		return domain.GatewayStripe
	})
	settled, err := f.svc.SettleExistingPayment(context.Background(), f.inv)
	if err != nil || settled || len(finder.calls) != 0 {
		t.Fatalf("SettleExistingPayment = %v, %v, lookups %d; want no lookup for a Stripe-routed tenant", settled, err, len(finder.calls))
	}
	if routedAs != f.inv.TenantID {
		t.Fatalf("routed as tenant %v, want the invoice's %v", routedAs, f.inv.TenantID)
	}
}
