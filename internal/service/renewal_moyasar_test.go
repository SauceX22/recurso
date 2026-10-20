package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
)

// ctxRecordingSettler records each settled invoice and the tenant its ctx
// carried (uuid.Nil when none).
type ctxRecordingSettler struct {
	settled []uuid.UUID
	tenants []uuid.UUID
}

func (s *ctxRecordingSettler) MarkInvoicePaid(ctx context.Context, invoiceID uuid.UUID) (bool, error) {
	s.settled = append(s.settled, invoiceID)
	tid, _ := ctx.Value(domain.TenantIDKey).(uuid.UUID)
	s.tenants = append(s.tenants, tid)
	return true, nil
}

func moyasarProviderMethods(charger SavedCardCharger, ref string) (*ProviderSavedMethods, *fakeGatewayMethodLookup) {
	lookup := &fakeGatewayMethodLookup{method: domain.SavedPaymentMethod{Provider: domain.SavedMethodMoyasar, PaymentMethodRef: ref}}
	p := NewProviderSavedMethods(lookup)
	p.Register(domain.SavedMethodMoyasar, charger)
	return p, lookup
}

// sarRenewalFixture is renewalFixture with a SAR invoice that carries a
// tenant and partial credit, so amountDue differs from Total.
func sarRenewalFixture() (*RenewalService, *fakeRenewalInvoicer, *domain.Subscription) {
	svc, _, invoicer, sub := renewalFixture()
	invoicer.invoice.Currency = "SAR"
	invoicer.invoice.TenantID = sub.TenantID
	invoicer.invoice.Total = 11500
	invoicer.invoice.CreditApplied = 1500
	return svc, invoicer, sub
}

func TestRenewalMoyasar_ChargesTokenAndSettlesWithTenant(t *testing.T) {
	svc, invoicer, sub := sarRenewalFixture()
	moyasar := &recordingSavedCharger{result: &port.PaymentResult{Success: true, PaymentID: uuid.NewString()}}
	stripe := &fakeRenewalCharger{result: &port.PaymentResult{Success: true}}
	settler := &ctxRecordingSettler{}
	svc.SetSavedMethodCharging(stripe, &fakeRenewalLookup{stripeID: "cus_1", methodID: "pm_1"}, settler)
	pm, _ := moyasarProviderMethods(moyasar, "token_abc")
	svc.SetProviderSavedMethods(pm)

	// The sweep ctx carries no tenant.
	if err := svc.RenewSubscription(context.Background(), sub); err != nil {
		t.Fatalf("RenewSubscription: %v", err)
	}
	inv := invoicer.invoice
	if len(moyasar.calls) != 1 {
		t.Fatalf("moyasar charges = %d, want 1", len(moyasar.calls))
	}
	want := savedChargeCall{"", "token_abc", 10000, "SAR", inv.ID.String(), "renewal-" + inv.ID.String()}
	if moyasar.calls[0] != want {
		t.Fatalf("charge = %+v, want %+v", moyasar.calls[0], want)
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

func TestRenewalMoyasar_StripeMockedStillCharges(t *testing.T) {
	svc, invoicer, sub := sarRenewalFixture()
	moyasar := &recordingSavedCharger{result: &port.PaymentResult{Success: true}}
	settler := &ctxRecordingSettler{}
	// main.go with Stripe mocked: the charger interface is nil, lookup and
	// settler are still wired.
	svc.SetSavedMethodCharging(nil, &fakeRenewalLookup{}, settler)
	pm, _ := moyasarProviderMethods(moyasar, "token_abc")
	svc.SetProviderSavedMethods(pm)

	if err := svc.RenewSubscription(context.Background(), sub); err != nil {
		t.Fatal(err)
	}
	if len(moyasar.calls) != 1 || len(settler.settled) != 1 || settler.settled[0] != invoicer.invoice.ID {
		t.Fatalf("charges=%d settled=%v, want 1 charge and the invoice settled", len(moyasar.calls), settler.settled)
	}
}

func TestRenewalMoyasar_NonMoyasarCustomerFallsThroughToStripe(t *testing.T) {
	svc, invoicer, sub := sarRenewalFixture()
	moyasar := &recordingSavedCharger{result: &port.PaymentResult{Success: true}}
	stripe := &fakeRenewalCharger{result: &port.PaymentResult{Success: true}}
	settler := &ctxRecordingSettler{}
	svc.SetSavedMethodCharging(stripe, &fakeRenewalLookup{stripeID: "cus_1", methodID: "pm_1"}, settler)
	lookup := &fakeGatewayMethodLookup{method: domain.SavedPaymentMethod{CustomerRef: "cus_1", PaymentMethodRef: "pm_1"}} // pm_provider NULL
	pm := NewProviderSavedMethods(lookup)
	pm.Register(domain.SavedMethodMoyasar, moyasar)
	svc.SetProviderSavedMethods(pm)

	if err := svc.RenewSubscription(context.Background(), sub); err != nil {
		t.Fatal(err)
	}
	if len(moyasar.calls) != 0 {
		t.Fatalf("moyasar charged %d times for a Stripe customer", len(moyasar.calls))
	}
	if stripe.charges != 1 || stripe.amount != 10000 || stripe.idemKey != "renewal-"+invoicer.invoice.ID.String() {
		t.Fatalf("stripe charges/amount/key = %d/%d/%q", stripe.charges, stripe.amount, stripe.idemKey)
	}
	if len(settler.settled) != 1 || settler.settled[0] != invoicer.invoice.ID {
		t.Fatalf("stripe settlement = %v, want [%v]", settler.settled, invoicer.invoice.ID)
	}
}

func TestRenewalMoyasar_ChargeFailureLeavesInvoiceUnsettled(t *testing.T) {
	for name, charger := range map[string]*recordingSavedCharger{
		"decline": {result: &port.PaymentResult{Success: false, ErrorCode: "card_declined_51"}},
		"error":   {err: errors.New("network")},
		"nil":     {},
	} {
		t.Run(name, func(t *testing.T) {
			svc, _, sub := sarRenewalFixture()
			stripe := &fakeRenewalCharger{result: &port.PaymentResult{Success: true}}
			settler := &ctxRecordingSettler{}
			svc.SetSavedMethodCharging(stripe, &fakeRenewalLookup{stripeID: "cus_1", methodID: "pm_1"}, settler)
			pm, _ := moyasarProviderMethods(charger, "token_abc")
			svc.SetProviderSavedMethods(pm)

			if err := svc.RenewSubscription(context.Background(), sub); err != nil {
				t.Fatalf("a failed charge must not fail renewal: %v", err)
			}
			if len(charger.calls) != 1 {
				t.Fatalf("moyasar charges = %d, want 1", len(charger.calls))
			}
			if len(settler.settled) != 0 {
				t.Fatalf("settled %v after a failed charge", settler.settled)
			}
			if stripe.charges != 0 {
				t.Fatal("a failed Moyasar charge must not fall back to Stripe")
			}
		})
	}
}

func TestRenewalMoyasar_NoSettlerOrNothingDueSkipsProvider(t *testing.T) {
	// Without a settler the provider path is inert (the Stripe guard also needs it).
	svc, _, sub := sarRenewalFixture()
	moyasar := &recordingSavedCharger{result: &port.PaymentResult{Success: true}}
	pm, lookup := moyasarProviderMethods(moyasar, "token_abc")
	svc.SetProviderSavedMethods(pm)
	if err := svc.RenewSubscription(context.Background(), sub); err != nil {
		t.Fatal(err)
	}
	if len(moyasar.calls) != 0 || lookup.calls != 0 {
		t.Fatalf("provider path ran without a settler: charges=%d lookups=%d", len(moyasar.calls), lookup.calls)
	}

	// Fully credited invoice: nothing to charge.
	svc2, invoicer2, sub2 := sarRenewalFixture()
	invoicer2.invoice.CreditApplied = invoicer2.invoice.Total
	settler := &ctxRecordingSettler{}
	svc2.SetSavedMethodCharging(nil, &fakeRenewalLookup{}, settler)
	moyasar2 := &recordingSavedCharger{result: &port.PaymentResult{Success: true}}
	pm2, _ := moyasarProviderMethods(moyasar2, "token_abc")
	svc2.SetProviderSavedMethods(pm2)
	if err := svc2.RenewSubscription(context.Background(), sub2); err != nil {
		t.Fatal(err)
	}
	if len(moyasar2.calls) != 0 || len(settler.settled) != 0 {
		t.Fatalf("charged/settled a zero-due invoice: %d/%d", len(moyasar2.calls), len(settler.settled))
	}
}

func TestWalletMoyasar_AutoRechargeChargesToken(t *testing.T) {
	svc, repo, ledger, w := walletFixture(500)
	w.Currency = "SAR"
	th, amt := int64(10000), int64(50000)
	w.AutoRechargeThreshold, w.AutoRechargeAmount = &th, &amt
	repo.dueList = []domain.Wallet{*w}

	moyasar := &recordingSavedCharger{result: &port.PaymentResult{Success: true, PaymentID: uuid.NewString()}}
	pm, _ := moyasarProviderMethods(moyasar, "token_w")
	// Stripe mocked: no Stripe charger at all.
	svc.SetProviderSavedMethods(pm)

	recharged, err := svc.ProcessAutoRecharges(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if recharged != 1 || len(moyasar.calls) != 1 {
		t.Fatalf("recharged=%d charges=%d, want 1/1", recharged, len(moyasar.calls))
	}
	wantKey := "wallet-recharge-" + w.ID.String() + "-500"
	want := savedChargeCall{"", "token_w", 50000, "SAR", w.ID.String(), wantKey}
	if moyasar.calls[0] != want {
		t.Fatalf("charge = %+v, want %+v", moyasar.calls[0], want)
	}
	if len(repo.topUps) != 1 || repo.topUps[0].IdempotencyKey != wantKey || len(ledger.topUps) != 1 {
		t.Fatalf("top-ups=%+v ledger=%v, want one credited top-up", repo.topUps, ledger.topUps)
	}
}

func TestWalletMoyasar_StripeCustomerUnchanged(t *testing.T) {
	svc, repo, _, w := walletFixture(500)
	th, amt := int64(10000), int64(50000)
	w.AutoRechargeThreshold, w.AutoRechargeAmount = &th, &amt
	repo.dueList = []domain.Wallet{*w}

	stripe := &fakeRenewalCharger{result: &port.PaymentResult{Success: true}}
	svc.SetSavedMethodCharging(stripe, &fakeRenewalLookup{stripeID: "cus_1", methodID: "pm_1"})
	moyasar := &recordingSavedCharger{result: &port.PaymentResult{Success: true}}
	pm := NewProviderSavedMethods(&fakeGatewayMethodLookup{method: domain.SavedPaymentMethod{CustomerRef: "cus_1", PaymentMethodRef: "pm_1"}})
	pm.Register(domain.SavedMethodMoyasar, moyasar)
	svc.SetProviderSavedMethods(pm)

	if _, err := svc.ProcessAutoRecharges(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stripe.charges != 1 || len(moyasar.calls) != 0 {
		t.Fatalf("stripe=%d moyasar=%d, want 1/0", stripe.charges, len(moyasar.calls))
	}
}

// moyasarLikeSubGateway mimics SmartRouter→Moyasar for CreateSubscription:
// gateway-managed subscriptions are unsupported, so it errors with no id.
type moyasarLikeSubGateway struct {
	port.PaymentGateway
	calls    int
	currency string
}

func (g *moyasarLikeSubGateway) CreateSubscription(_ context.Context, _ string, _ int, _ string, _ *int64, currency string) (string, error) {
	g.calls++
	g.currency = currency
	return "", errors.New("gateway-managed subscriptions are not supported by moyasar (Recurso owns the cycle)")
}

// TestCreateSubscriptionSAR_GatewayErrorKeepsIDsEmpty guards the renewal
// claim: a subscription whose gateway returned ("", error) must not store a
// gateway subscription id, because the claim skips rows with a non-empty
// stripe_subscription_id / razorpay_subscription_id.
func TestCreateSubscriptionSAR_GatewayErrorKeepsIDsEmpty(t *testing.T) {
	planID, customerID := uuid.New(), uuid.New()
	planRepo := &subMockPlanRepo{plan: &domain.Plan{
		ID: planID, Code: "sar-monthly", IntervalUnit: domain.IntervalMonth, IntervalCount: 1,
		Prices: []domain.Price{{Amount: 11500, Currency: "SAR"}},
	}}
	custRepo := &subMockCustomerRepo{customer: &domain.Customer{ID: customerID, Email: "sar@x.com"}}
	subRepo := &subMockSubRepo{}
	gw := &moyasarLikeSubGateway{}
	svc := newTestSubscriptionService(subRepo, &subMockInvoiceRepo{}, planRepo, custRepo, &subMockCouponRepo{}, gw)

	sub, err := svc.CreateSubscription(context.Background(), CreateSubscriptionInput{
		TenantID: uuid.New(), CustomerID: customerID, PlanID: planID,
		StartDate: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), TrialDays: 7,
	})
	if err != nil {
		t.Fatalf("CreateSubscription must succeed when the gateway mirror fails: %v", err)
	}
	if gw.calls != 1 || gw.currency != "SAR" {
		t.Fatalf("gateway CreateSubscription calls=%d currency=%q, want 1/SAR", gw.calls, gw.currency)
	}
	if sub.StripeSubscriptionID != "" || sub.RazorpaySubscriptionID != "" {
		t.Fatalf("gateway ids stored: stripe=%q razorpay=%q, want both empty", sub.StripeSubscriptionID, sub.RazorpaySubscriptionID)
	}
	if subRepo.created == nil || subRepo.created.StripeSubscriptionID != "" {
		t.Fatalf("persisted subscription = %+v, want empty stripe_subscription_id", subRepo.created)
	}
}
