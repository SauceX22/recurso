package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
)

// sarRechargeFixture is a SAR wallet below its auto-recharge threshold.
func sarRechargeFixture() (*WalletService, *fakeWalletRepo, *fakeWalletLedger, *domain.Wallet) {
	svc, repo, ledger, w := walletFixture(500)
	w.Currency = "SAR"
	th, amt := int64(10000), int64(50000)
	w.AutoRechargeThreshold, w.AutoRechargeAmount = &th, &amt
	repo.dueList = []domain.Wallet{*w}
	return svc, repo, ledger, w
}

func TestWalletSavedMethod_AutoRechargeChargesProviderMethod(t *testing.T) {
	svc, repo, ledger, w := sarRechargeFixture()
	moyasar := &fakeRenewalCharger{result: &port.PaymentResult{Success: true, PaymentID: uuid.NewString()}}
	// Stripe mocked: no Stripe charger at all, only the router.
	svc.SetSavedMethodCharging(nil, moyasarLookup())
	svc.SetChargerRouter(moyasarRouter(moyasar))

	recharged, err := svc.ProcessAutoRecharges(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if recharged != 1 || len(moyasar.reqs) != 1 {
		t.Fatalf("recharged=%d charges=%d, want 1/1", recharged, len(moyasar.reqs))
	}
	got := moyasar.reqs[0]
	if got.MethodRef != "token_abc" || got.Amount != 50000 || got.Currency != "SAR" || got.IdempotencyKey == "" {
		t.Fatalf("charge = %+v, want token_abc for 50000 SAR with a key", got)
	}
	if len(repo.topUps) != 1 || repo.topUps[0].IdempotencyKey != got.IdempotencyKey || len(ledger.topUps) != 1 {
		t.Fatalf("top-ups=%+v ledger=%v, want one credited top-up under the charge key", repo.topUps, ledger.topUps)
	}
	if repo.topUps[0].WalletID != w.ID {
		t.Fatalf("top-up wallet = %v, want %v", repo.topUps[0].WalletID, w.ID)
	}
}

func TestWalletSavedMethod_DeclineOrErrorCreditsNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		charger *fakeRenewalCharger
	}{
		{"decline", &fakeRenewalCharger{result: &port.PaymentResult{Success: false, ErrorCode: "card_declined"}}},
		{"error", &fakeRenewalCharger{err: errors.New("network")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, repo, ledger, _ := sarRechargeFixture()
			svc.SetSavedMethodCharging(nil, moyasarLookup())
			svc.SetChargerRouter(moyasarRouter(tc.charger))

			recharged, err := svc.ProcessAutoRecharges(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if recharged != 0 || tc.charger.charges != 1 || len(repo.topUps) != 0 || len(ledger.topUps) != 0 {
				t.Fatalf("recharged=%d charges=%d topups=%d ledger=%d, want 0/1/0/0",
					recharged, tc.charger.charges, len(repo.topUps), len(ledger.topUps))
			}
		})
	}
}

func TestWalletSavedMethod_NoAutoRechargeAmountNoLookup(t *testing.T) {
	svc, repo, _, w := sarRechargeFixture()
	w.AutoRechargeAmount = nil
	repo.dueList = []domain.Wallet{*w}
	lookup := moyasarLookup()
	moyasar := &fakeRenewalCharger{result: &port.PaymentResult{Success: true}}
	svc.SetSavedMethodCharging(nil, lookup)
	svc.SetChargerRouter(moyasarRouter(moyasar))

	if _, err := svc.ProcessAutoRecharges(context.Background()); err != nil {
		t.Fatal(err)
	}
	if lookup.calls != 0 || moyasar.charges != 0 {
		t.Fatalf("lookups=%d charges=%d without an auto-recharge amount, want 0/0", lookup.calls, moyasar.charges)
	}
}

func TestWalletSavedMethod_StripeCustomerUnchanged(t *testing.T) {
	svc, repo, _, w := walletFixture(500)
	th, amt := int64(10000), int64(50000)
	w.AutoRechargeThreshold, w.AutoRechargeAmount = &th, &amt
	repo.dueList = []domain.Wallet{*w}

	stripe := &fakeRenewalCharger{result: &port.PaymentResult{Success: true}}
	moyasar := &fakeRenewalCharger{result: &port.PaymentResult{Success: true}}
	svc.SetSavedMethodCharging(stripe, &fakeRenewalLookup{stripeID: "cus_1", methodID: "pm_1"})
	router := NewSavedCardGatewayRouter(nil, nil, stripe)
	router.RegisterProvider(domain.GatewayMoyasar, moyasar)
	svc.SetChargerRouter(router)

	if _, err := svc.ProcessAutoRecharges(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stripe.charges != 1 || moyasar.charges != 0 {
		t.Fatalf("stripe=%d moyasar=%d, want 1/0", stripe.charges, moyasar.charges)
	}
	if got := stripe.reqs[0]; got.CustomerRef != "cus_1" || got.MethodRef != "pm_1" {
		t.Fatalf("stripe charge refs = %q/%q, want cus_1/pm_1", got.CustomerRef, got.MethodRef)
	}
}
