package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
)

// sarRenewalFixture is renewalFixture with a SAR invoice that carries a
// tenant, a wallet drain and partial credit, so the amount due differs from
// Total by both.
func sarRenewalFixture() (*RenewalService, *fakeRenewalInvoicer, *domain.Subscription) {
	svc, _, invoicer, sub := renewalFixture()
	invoicer.invoice.Currency = "SAR"
	invoicer.invoice.TenantID = sub.TenantID
	invoicer.invoice.Total = 11500
	invoicer.invoice.AmountPaid = 2000
	invoicer.invoice.CreditApplied = 1500
	return svc, invoicer, sub
}

// moyasarRouter is a router whose only charger is c, registered for Moyasar.
func moyasarRouter(c SavedCardCharger) *SavedCardGatewayRouter {
	r := NewSavedCardGatewayRouter(nil, nil, nil)
	r.RegisterProvider(domain.GatewayMoyasar, c)
	return r
}

func moyasarLookup() *fakeRenewalLookup {
	return &fakeRenewalLookup{provider: domain.GatewayMoyasar, methodID: "token_abc", stripeID: "cus_left_over"}
}

func TestRenewalSavedMethod_ChargesOnItsProviderAndSettlesWithTenant(t *testing.T) {
	svc, invoicer, sub := sarRenewalFixture()
	moyasar := &fakeRenewalCharger{result: &port.PaymentResult{Success: true, PaymentID: uuid.NewString()}}
	stripe := &fakeRenewalCharger{result: &port.PaymentResult{Success: true}}
	settler := &fakeRenewalSettler{}
	router := NewSavedCardGatewayRouter(nil, nil, stripe)
	router.RegisterProvider(domain.GatewayMoyasar, moyasar)
	svc.SetSavedMethodCharging(moyasarLookup(), router, settler)

	// The sweep ctx carries no tenant.
	if err := svc.RenewSubscription(context.Background(), sub); err != nil {
		t.Fatalf("RenewSubscription: %v", err)
	}
	inv := invoicer.invoice
	if len(moyasar.reqs) != 1 {
		t.Fatalf("moyasar charges = %d, want 1", len(moyasar.reqs))
	}
	want := port.SavedMethodCharge{
		CustomerRef: "cus_left_over", MethodRef: "token_abc", Amount: 8000, Currency: "SAR",
		InvoiceID: inv.ID.String(), IdempotencyKey: "renewal-" + inv.ID.String(),
	}
	if moyasar.reqs[0] != want {
		t.Fatalf("charge = %+v, want %+v (amount = total - paid - credit)", moyasar.reqs[0], want)
	}
	if stripe.charges != 0 {
		t.Fatalf("stripe charged %d times, want 0", stripe.charges)
	}
	if len(settler.settled) != 1 || settler.settled[0] != inv.ID {
		t.Fatalf("settled = %v, want [%v]", settler.settled, inv.ID)
	}
	if settler.tenants[0] != inv.TenantID {
		t.Fatalf("settlement ctx tenant = %v, want invoice tenant %v", settler.tenants[0], inv.TenantID)
	}
}

func TestRenewalSavedMethod_StripeMockedStillCharges(t *testing.T) {
	svc, invoicer, sub := sarRenewalFixture()
	moyasar := &fakeRenewalCharger{result: &port.PaymentResult{Success: true}}
	settler := &fakeRenewalSettler{}
	// main.go with Stripe mocked: no Stripe charger; lookup, settler and the
	// router are still wired.
	svc.SetSavedMethodCharging(moyasarLookup(), moyasarRouter(moyasar), settler)

	if err := svc.RenewSubscription(context.Background(), sub); err != nil {
		t.Fatal(err)
	}
	if moyasar.charges != 1 || len(settler.settled) != 1 || settler.settled[0] != invoicer.invoice.ID {
		t.Fatalf("charges=%d settled=%v, want 1 charge and the invoice settled", moyasar.charges, settler.settled)
	}
}

// A Moyasar token must never reach Stripe, even when the customer still has a
// stripe_customer_id from an earlier card and no Moyasar charger is
// registered (key removed).
func TestRenewalSavedMethod_UnregisteredProviderNeverChargesStripe(t *testing.T) {
	svc, _, sub := sarRenewalFixture()
	stripe := &fakeRenewalCharger{result: &port.PaymentResult{Success: true}}
	settler := &fakeRenewalSettler{}
	svc.SetSavedMethodCharging(moyasarLookup(), NewSavedCardGatewayRouter(nil, nil, stripe), settler)
	if err := svc.RenewSubscription(context.Background(), sub); err != nil {
		t.Fatal(err)
	}
	if stripe.charges != 0 || len(settler.settled) != 0 {
		t.Fatalf("stripe charged %d times / settled %v with a moyasar token", stripe.charges, settler.settled)
	}
}

// The customer's one saved slot holds a Stripe card, but SAR routes to
// Moyasar: the renewal skips the charge and leaves the invoice to dunning.
func TestRenewalSavedMethod_MethodOnAnotherGatewayIsSkipped(t *testing.T) {
	svc, _, sub := sarRenewalFixture()
	stripe := &fakeRenewalCharger{result: &port.PaymentResult{Success: true}}
	settler := &fakeRenewalSettler{}
	router := NewSavedCardGatewayRouter(nil, nil, stripe)
	// Production always registers the gateway an override names (boot fails
	// otherwise), and the guard keys off that registration.
	router.RegisterProvider(domain.GatewayMoyasar, &fakeRenewalCharger{})
	router.SetCurrencyRouting(fixedRoutes{"SAR": domain.GatewayMoyasar})
	svc.SetSavedMethodCharging(&fakeRenewalLookup{stripeID: "cus_1", methodID: "pm_1"}, router, settler)

	if err := svc.RenewSubscription(context.Background(), sub); err != nil {
		t.Fatal(err)
	}
	if stripe.charges != 0 || len(settler.settled) != 0 {
		t.Fatalf("stripe charged %d times / settled %v for a SAR invoice", stripe.charges, settler.settled)
	}
}

func TestRenewalSavedMethod_ChargeFailureLeavesInvoiceUnsettled(t *testing.T) {
	for _, tc := range []struct {
		name    string
		charger *fakeRenewalCharger
	}{
		{"decline", &fakeRenewalCharger{result: &port.PaymentResult{Success: false, ErrorCode: "insufficient_funds"}}},
		{"error", &fakeRenewalCharger{err: errors.New("network")}},
		{"nil result", &fakeRenewalCharger{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, sub := sarRenewalFixture()
			settler := &fakeRenewalSettler{}
			svc.SetSavedMethodCharging(moyasarLookup(), moyasarRouter(tc.charger), settler)

			if err := svc.RenewSubscription(context.Background(), sub); err != nil {
				t.Fatalf("a failed charge must not fail renewal: %v", err)
			}
			if tc.charger.charges != 1 {
				t.Fatalf("charges = %d, want 1", tc.charger.charges)
			}
			if len(settler.settled) != 0 {
				t.Fatalf("settled %v after a failed charge", settler.settled)
			}
		})
	}
}

// Only an open invoice with something left to pay is charged; the lookup is
// not even made otherwise.
func TestRenewalSavedMethod_NothingToCollectSkipsLookup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*domain.Invoice)
	}{
		{"paid by wallet", func(inv *domain.Invoice) { inv.Status = domain.InvoiceStatusPaid }},
		{"void", func(inv *domain.Invoice) { inv.Status = domain.InvoiceStatusVoid }},
		{"fully credited", func(inv *domain.Invoice) { inv.CreditApplied = inv.Total - inv.AmountPaid }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, invoicer, sub := sarRenewalFixture()
			tc.mutate(invoicer.invoice)
			lookup := moyasarLookup()
			moyasar := &fakeRenewalCharger{result: &port.PaymentResult{Success: true}}
			settler := &fakeRenewalSettler{}
			svc.SetSavedMethodCharging(lookup, moyasarRouter(moyasar), settler)

			if err := svc.RenewSubscription(context.Background(), sub); err != nil {
				t.Fatal(err)
			}
			if lookup.calls != 0 || moyasar.charges != 0 || len(settler.settled) != 0 {
				t.Fatalf("lookups=%d charges=%d settled=%d, want all zero", lookup.calls, moyasar.charges, len(settler.settled))
			}
		})
	}
}

func TestRenewalSavedMethod_LookupErrorLeavesInvoiceOpen(t *testing.T) {
	svc, _, sub := sarRenewalFixture()
	lookup := moyasarLookup()
	lookup.err = errors.New("db down")
	moyasar := &fakeRenewalCharger{result: &port.PaymentResult{Success: true}}
	settler := &fakeRenewalSettler{}
	svc.SetSavedMethodCharging(lookup, moyasarRouter(moyasar), settler)

	if err := svc.RenewSubscription(context.Background(), sub); err != nil {
		t.Fatal(err)
	}
	if moyasar.charges != 0 || len(settler.settled) != 0 {
		t.Fatalf("charges=%d settled=%d after a lookup error, want 0/0", moyasar.charges, len(settler.settled))
	}
}

// fakePaymentRecorder records ClaimGatewayPaymentID calls; a non-empty
// recorded id is a payment that already holds the slot.
type fakePaymentRecorder struct {
	calls    []recordedPayment
	recorded string
}

type recordedPayment struct {
	tenantID, invoiceID uuid.UUID
	paymentID           string
}

func (f *fakePaymentRecorder) ClaimGatewayPaymentID(_ context.Context, tenantID, invoiceID uuid.UUID, paymentID string) (bool, error) {
	f.calls = append(f.calls, recordedPayment{tenantID, invoiceID, paymentID})
	if f.recorded != "" && f.recorded != paymentID {
		return false, nil
	}
	f.recorded = paymentID
	return true, nil
}

// The payment that settles a renewal is the invoice's refund target; without
// recording it, refunds depend on a webhook arriving.
func TestRenewalSavedMethod_RecordsSettlingPaymentID(t *testing.T) {
	svc, invoicer, sub := sarRenewalFixture()
	moyasar := &fakeRenewalCharger{result: &port.PaymentResult{Success: true, PaymentID: "pay_renewal_1"}}
	svc.SetSavedMethodCharging(moyasarLookup(), moyasarRouter(moyasar), &fakeRenewalSettler{})
	rec := &fakePaymentRecorder{}
	svc.SetPaymentRecorder(rec)

	if err := svc.RenewSubscription(context.Background(), sub); err != nil {
		t.Fatal(err)
	}
	inv := invoicer.invoice
	want := []recordedPayment{{inv.TenantID, inv.ID, "pay_renewal_1"}}
	if len(rec.calls) != 1 || rec.calls[0] != want[0] {
		t.Fatalf("recorded = %+v, want %+v", rec.calls, want)
	}
}

// When something else already settled the invoice, this charge did not: its
// id must not overwrite the refund target.
func TestRenewalSavedMethod_DoesNotRecordPaymentThatDidNotSettle(t *testing.T) {
	svc, _, sub := sarRenewalFixture()
	moyasar := &fakeRenewalCharger{result: &port.PaymentResult{Success: true, PaymentID: "pay_late"}}
	already := false
	svc.SetSavedMethodCharging(moyasarLookup(), moyasarRouter(moyasar), &fakeRenewalSettler{transitioned: &already})
	rec := &fakePaymentRecorder{}
	svc.SetPaymentRecorder(rec)

	if err := svc.RenewSubscription(context.Background(), sub); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("recorded = %+v, want nothing", rec.calls)
	}
}
