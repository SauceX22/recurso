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
	svc.SetSavedMethodCharging(moyasarLookup(), moyasarRouter(moyasar))

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
			svc.SetSavedMethodCharging(moyasarLookup(), moyasarRouter(tc.charger))

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
	svc.SetSavedMethodCharging(lookup, moyasarRouter(moyasar))

	if _, err := svc.ProcessAutoRecharges(context.Background()); err != nil {
		t.Fatal(err)
	}
	if lookup.calls != 0 || moyasar.charges != 0 {
		t.Fatalf("lookups=%d charges=%d without an auto-recharge amount, want 0/0", lookup.calls, moyasar.charges)
	}
}

// The charge pays for a wallet top-up, not an invoice: gateways label it and
// bind it by wallet id.
func TestWalletRecharge_ChargesForTheWalletNotAnInvoice(t *testing.T) {
	svc, _, _, w := sarRechargeFixture()
	charger := &fakeRenewalCharger{result: &port.PaymentResult{Success: true}}
	svc.SetSavedMethodCharging(moyasarLookup(), moyasarRouter(charger))

	if _, err := svc.ProcessAutoRecharges(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := charger.reqs[0]; got.WalletID != w.ID.String() || got.InvoiceID != "" {
		t.Fatalf("charge WalletID=%q InvoiceID=%q, want the wallet id and no invoice", got.WalletID, got.InvoiceID)
	}
}

// Gateway idempotency keys (Moyasar's given_id never expires) must not repeat
// across legitimate recharges: a wallet drained back to the same balance would
// replay the first top-up's payment, and the credit would be dropped as a
// duplicate, so the wallet is never refilled.
func TestWalletRecharge_SameBalanceAgainIsANewCharge(t *testing.T) {
	svc, repo, ledger, w := sarRechargeFixture()
	charger := &fakeRenewalCharger{result: &port.PaymentResult{Success: true}}
	svc.SetSavedMethodCharging(moyasarLookup(), moyasarRouter(charger))

	if _, err := svc.ProcessAutoRecharges(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Drained back to the same balance later: same balance, newer row.
	again := *w
	again.UpdatedAt = w.UpdatedAt.Add(time.Hour)
	repo.dueList = []domain.Wallet{again}
	if _, err := svc.ProcessAutoRecharges(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(charger.reqs) != 2 || charger.reqs[0].IdempotencyKey == charger.reqs[1].IdempotencyKey {
		t.Fatalf("keys = %+v, want two different keys", charger.reqs)
	}
	if len(repo.topUps) != 2 || len(ledger.topUps) != 2 {
		t.Fatalf("top-ups=%d ledger=%d, want both recharges credited", len(repo.topUps), len(ledger.topUps))
	}
}

// After a confirmed decline the next sweep must try again under a new key;
// replaying the declined payment's key would decline forever, even after the
// customer fixes their card.
func TestWalletRecharge_DeclineAdvancesTheKey(t *testing.T) {
	svc, repo, _, w := sarRechargeFixture()
	charger := &fakeRenewalCharger{result: &port.PaymentResult{Success: false, ErrorCode: "insufficient_funds"}}
	svc.SetSavedMethodCharging(moyasarLookup(), moyasarRouter(charger))

	for i := 0; i < 2; i++ {
		if _, err := svc.ProcessAutoRecharges(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(repo.attempted) != 2 || repo.attempted[0] != w.ID {
		t.Fatalf("attempts recorded = %v, want the wallet twice", repo.attempted)
	}
	if charger.reqs[0].IdempotencyKey == charger.reqs[1].IdempotencyKey {
		t.Fatalf("key %q repeated after a decline", charger.reqs[0].IdempotencyKey)
	}
}

// An outage is not an answer: the payment may exist, so the next sweep must
// replay the same key rather than risk a second charge.
func TestWalletRecharge_ErrorKeepsTheKey(t *testing.T) {
	svc, repo, _, _ := sarRechargeFixture()
	charger := &fakeRenewalCharger{err: errors.New("timeout")}
	svc.SetSavedMethodCharging(moyasarLookup(), moyasarRouter(charger))

	for i := 0; i < 2; i++ {
		if _, err := svc.ProcessAutoRecharges(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(repo.attempted) != 0 {
		t.Fatalf("attempts recorded = %v, want none after an error", repo.attempted)
	}
	if charger.reqs[0].IdempotencyKey != charger.reqs[1].IdempotencyKey {
		t.Fatalf("keys %q and %q differ after an error", charger.reqs[0].IdempotencyKey, charger.reqs[1].IdempotencyKey)
	}
}

func TestWalletSavedMethod_StripeCustomerUnchanged(t *testing.T) {
	svc, repo, _, w := walletFixture(500)
	th, amt := int64(10000), int64(50000)
	w.AutoRechargeThreshold, w.AutoRechargeAmount = &th, &amt
	repo.dueList = []domain.Wallet{*w}

	stripe := &fakeRenewalCharger{result: &port.PaymentResult{Success: true}}
	moyasar := &fakeRenewalCharger{result: &port.PaymentResult{Success: true}}
	router := NewSavedCardGatewayRouter(nil, nil, stripe)
	router.RegisterProvider(domain.GatewayMoyasar, moyasar)
	svc.SetSavedMethodCharging(&fakeRenewalLookup{stripeID: "cus_1", methodID: "pm_1"}, router)

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
