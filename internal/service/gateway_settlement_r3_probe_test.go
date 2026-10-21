package service

import (
	"context"
	"testing"
)

// G16/G17: the consent is all-or-nothing and gateway-neutral: a plan without
// an interval gives no terms (never "every 1 " ), and the currency is the
// ISO code in upper case whatever the invoice stored.
func TestGatewaySettlementR3_CardOnFileTermsShape(t *testing.T) {
	f := newTermsFixture()
	f.inv.Currency = "sar"
	if _, terms := f.svc.CardOnFile(context.Background(), f.inv); terms == nil || terms.Currency != "SAR" {
		t.Fatalf("terms = %+v, want currency SAR", terms)
	}
	f = newTermsFixture()
	f.plans.plan.IntervalUnit = ""
	if saves, terms := f.svc.CardOnFile(context.Background(), f.inv); !saves || terms != nil {
		t.Fatalf("CardOnFile = %v, %+v; want saving without terms when the plan has no interval", saves, terms)
	}
}

// G19: without a payment claimer, only the payment that performed the paid
// transition may save its card; a late second payment must not replace the
// customer's renewal card.
func TestGatewaySettlementR3_NoClaimerSavesOnlyOnTransition(t *testing.T) {
	f := newSettlementFixture()
	f.svc.payments = nil
	f.settler.transitioned = false
	if _, err := f.svc.Settle(context.Background(), f.inv, f.payment); err != nil {
		t.Fatal(err)
	}
	if len(f.cards.calls) != 0 {
		t.Fatalf("saved %+v for a payment that did not settle the invoice", f.cards.calls)
	}
}
