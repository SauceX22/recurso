package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/recurso-dev/recurso/internal/core/domain"
)

func TestInvoicePayable(t *testing.T) {
	for status, want := range map[domain.InvoiceStatus]bool{
		domain.InvoiceStatusOpen: true, domain.InvoiceStatusPastDue: true, domain.InvoiceStatusUncollectible: true,
		domain.InvoiceStatusPaid: false, domain.InvoiceStatusVoid: false, domain.InvoiceStatusDraft: false,
	} {
		if got := InvoicePayable(&domain.Invoice{Status: status}); got != want {
			t.Errorf("InvoicePayable(%s) = %v, want %v", status, got, want)
		}
	}
}

// A paid gateway payment for a void or draft invoice is money taken for
// nothing owed: it never settles the invoice (a void invoice must not flip to
// paid and post cash against a receivable the void removed), records nothing,
// saves no card, and is flagged for a refund.
func TestGatewaySettlement_NonPayableInvoiceIsNotSettled(t *testing.T) {
	for _, status := range []domain.InvoiceStatus{domain.InvoiceStatusVoid, domain.InvoiceStatusDraft} {
		t.Run(string(status), func(t *testing.T) {
			logs := captureDefaultLog(t)
			f := newSettlementFixture() // after the capture: its logger writes there
			f.inv.Status = status
			transitioned, err := f.svc.Settle(context.Background(), f.inv, f.payment)
			if !errors.Is(err, ErrInvoiceNotPayable) || transitioned {
				t.Fatalf("Settle = %v, %v; want ErrInvoiceNotPayable", transitioned, err)
			}
			if len(f.settler.calls)+len(f.claims.calls)+len(f.cards.calls) != 0 {
				t.Fatalf("settles=%d claims=%d cards=%d; want no effect", len(f.settler.calls), len(f.claims.calls), len(f.cards.calls))
			}
			if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "review for refund") || !strings.Contains(out, "payment_id=pay-1") {
				t.Fatalf("payment on a non-payable invoice not flagged for refund review; logs:\n%s", out)
			}
		})
	}
}

// Written-off (uncollectible) and past-due invoices are still owed.
func TestGatewaySettlement_UncollectibleAndPastDueSettle(t *testing.T) {
	for _, status := range []domain.InvoiceStatus{domain.InvoiceStatusUncollectible, domain.InvoiceStatusPastDue} {
		f := newSettlementFixture()
		f.inv.Status = status
		if transitioned, err := f.svc.Settle(context.Background(), f.inv, f.payment); err != nil || !transitioned {
			t.Fatalf("%s: Settle = %v, %v; want settled", status, transitioned, err)
		}
	}
}

// A non-payable invoice is never looked up at the gateway: nothing is owed.
func TestGatewaySettlement_SettleExistingPaymentSkipsNonPayable(t *testing.T) {
	for _, status := range []domain.InvoiceStatus{domain.InvoiceStatusVoid, domain.InvoiceStatusDraft} {
		f := newSettlementFixture()
		f.inv.Status = status
		finder := &fakePaidPaymentFinder{found: f.payment}
		withPaidLookup(f, finder)
		if settled, err := f.svc.SettleExistingPayment(context.Background(), f.inv); err != nil || settled || len(finder.calls) != 0 {
			t.Fatalf("%s: got %v, %v with %d lookups; want no lookup", status, settled, err, len(finder.calls))
		}
	}
}
