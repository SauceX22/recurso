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
	svc, repo, ledger, _ := sarRechargeFixture()
	charger := &fakeRenewalCharger{result: &port.PaymentResult{Success: true}}
	svc.SetSavedMethodCharging(moyasarLookup(), moyasarRouter(charger))

	// Two recharges of the same wallet at the same balance.
	for i := 0; i < 2; i++ {
		if _, err := svc.ProcessAutoRecharges(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(charger.reqs) != 2 || charger.reqs[0].IdempotencyKey == charger.reqs[1].IdempotencyKey {
		t.Fatalf("keys = %+v, want two different keys", charger.reqs)
	}
	if len(repo.topUps) != 2 || len(ledger.topUps) != 2 {
		t.Fatalf("top-ups=%d ledger=%d, want both recharges credited", len(repo.topUps), len(ledger.topUps))
	}
	if o := repo.outcomes[0]; !o.Answered || o.Failures != 0 || o.RetryAt != nil || o.BlockedMethodRef != "" {
		t.Fatalf("success outcome = %+v, want answered with the failure run reset", o)
	}
}

// After a decline the next attempt is a new charge under a new key (replaying
// the declined payment's key would decline forever), but not on the next
// sweep: every five minutes is a retry storm card networks fine. The wait
// grows with each failure.
func TestWalletRecharge_DeclineBacksOffAndAdvancesTheKey(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		priorFailures int
		wantWait      time.Duration
	}{{0, time.Hour}, {1, 6 * time.Hour}, {2, 24 * time.Hour}, {maxRechargeFailures - 2, 24 * time.Hour}} {
		svc, repo, _, _ := sarRechargeFixture()
		repo.dueList[0].RechargeFailures = tc.priorFailures
		charger := &fakeRenewalCharger{result: &port.PaymentResult{Success: false, ErrorCode: "insufficient_funds"}}
		svc.SetSavedMethodCharging(moyasarLookup(), moyasarRouter(charger))

		for i := 0; i < 2; i++ {
			if _, err := svc.ProcessAutoRecharges(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		o := repo.outcomes[0]
		if !o.Answered || o.Failures != tc.priorFailures+1 || o.RetryAt == nil || !o.RetryAt.Equal(now.Add(tc.wantWait)) || o.BlockedMethodRef != "" {
			t.Fatalf("after %d failures: outcome = %+v, want answered, failure %d, retry at +%v", tc.priorFailures, o, tc.priorFailures+1, tc.wantWait)
		}
		if charger.reqs[0].IdempotencyKey == charger.reqs[1].IdempotencyKey {
			t.Fatalf("key %q repeated after a decline", charger.reqs[0].IdempotencyKey)
		}
	}
}

// A lost response is not an answer: the payment may exist, so every later
// attempt replays the same key, even after the wallet was drained further in
// between (its balance and updated_at both moved), until an answer arrives.
func TestWalletRecharge_LostResponseKeepsTheKey(t *testing.T) {
	svc, repo, ledger, _ := sarRechargeFixture()
	charger := &fakeRenewalCharger{err: errors.New("timeout")}
	svc.SetSavedMethodCharging(moyasarLookup(), moyasarRouter(charger))

	if _, err := svc.ProcessAutoRecharges(context.Background()); err != nil {
		t.Fatal(err)
	}
	if o := repo.outcomes[0]; o.Answered || o.Failures != 1 || o.RetryAt == nil {
		t.Fatalf("outcome = %+v, want unanswered with a backoff", o)
	}
	// Usage drained the wallet further before the next sweep.
	repo.dueList[0].Balance -= 300
	repo.dueList[0].UpdatedAt = repo.dueList[0].UpdatedAt.Add(time.Hour)
	charger.err, charger.result = nil, &port.PaymentResult{Success: true}
	if _, err := svc.ProcessAutoRecharges(context.Background()); err != nil {
		t.Fatal(err)
	}
	if charger.reqs[0].IdempotencyKey != charger.reqs[1].IdempotencyKey {
		t.Fatalf("keys %q and %q differ across a lost response", charger.reqs[0].IdempotencyKey, charger.reqs[1].IdempotencyKey)
	}
	if len(repo.topUps) != 1 || len(ledger.topUps) != 1 {
		t.Fatalf("top-ups=%d ledger=%d, want the replayed payment credited once", len(repo.topUps), len(ledger.topUps))
	}
}

// A hard decline (lost, stolen, closed card) stops auto-recharge until the
// customer saves another method; the repository filters on it.
func TestWalletRecharge_HardDeclineBlocksTheMethod(t *testing.T) {
	svc, repo, _, _ := sarRechargeFixture()
	charger := &fakeRenewalCharger{result: &port.PaymentResult{Success: false, ErrorCode: "lost_or_stolen", Permanent: true}}
	svc.SetSavedMethodCharging(moyasarLookup(), moyasarRouter(charger))

	if _, err := svc.ProcessAutoRecharges(context.Background()); err != nil {
		t.Fatal(err)
	}
	if o := repo.outcomes[0]; !o.Answered || o.BlockedMethodRef != "token_abc" {
		t.Fatalf("outcome = %+v, want the declined token blocked", o)
	}
}

// With nothing to charge the customer is told once per backoff step, not on
// every sweep.
func TestWalletRecharge_NoSavedMethodBacksOff(t *testing.T) {
	svc, repo, _, _ := sarRechargeFixture()
	svc.SetSavedMethodCharging(&fakeRenewalLookup{}, moyasarRouter(&fakeRenewalCharger{}))

	if _, err := svc.ProcessAutoRecharges(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repo.outcomes) != 1 || repo.outcomes[0].RetryAt == nil || repo.outcomes[0].Answered {
		t.Fatalf("outcomes = %+v, want one backoff that keeps the key", repo.outcomes)
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

// keyedCharger is a gateway that keeps every payment under its idempotency
// key, as Moyasar's given_id does: a replay with the same amount returns the
// stored payment, one with another amount is refused as a processing error
// with no error (moyasar.go's mismatch read-back). dropFirst loses the first
// response after the payment was captured.
type keyedCharger struct {
	payments  map[string]int64
	dropFirst bool
	reqs      []port.SavedMethodCharge
}

func (c *keyedCharger) ChargeSavedPaymentMethod(_ context.Context, req port.SavedMethodCharge) (*port.PaymentResult, error) {
	c.reqs = append(c.reqs, req)
	if amount, ok := c.payments[req.IdempotencyKey]; ok {
		if amount != req.Amount {
			return &port.PaymentResult{Success: false, ErrorCode: "processing_error"}, nil
		}
		return &port.PaymentResult{Success: true, PaymentID: req.IdempotencyKey}, nil
	}
	c.payments[req.IdempotencyKey] = req.Amount
	if c.dropFirst {
		c.dropFirst = false
		return nil, errors.New("connection reset")
	}
	return &port.PaymentResult{Success: true, PaymentID: req.IdempotencyKey}, nil
}

// A recharge whose response was lost replays the amount it charged, even when
// the auto-recharge amount changed in between: the replay finds the captured
// payment and credits it, instead of being refused as a decline (which
// advanced the key, so the captured payment was never credited and the
// customer was charged again).
func TestWalletRecharge_LostResponseReplaysTheChargedAmount(t *testing.T) {
	svc, repo, ledger, _ := sarRechargeFixture()
	charger := &keyedCharger{payments: map[string]int64{}, dropFirst: true}
	svc.SetSavedMethodCharging(moyasarLookup(), moyasarRouter(charger))

	if _, err := svc.ProcessAutoRecharges(context.Background()); err != nil {
		t.Fatal(err)
	}
	more := int64(60000)
	repo.dueList[0].AutoRechargeAmount = &more
	if _, err := svc.ProcessAutoRecharges(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(charger.reqs) != 2 || charger.reqs[1].Amount != 50000 || charger.reqs[1].IdempotencyKey != charger.reqs[0].IdempotencyKey {
		t.Fatalf("charges = %+v, want the lost 50000 charge replayed under its key", charger.reqs)
	}
	if len(repo.topUps) != 1 || repo.topUps[0].Amount != 50000 || len(ledger.topUps) != 1 {
		t.Fatalf("top-ups = %+v, want the captured 50000 credited once", repo.topUps)
	}
	if o := repo.outcomes[len(repo.outcomes)-1]; !o.Answered || o.Failures != 0 {
		t.Fatalf("last outcome = %+v, want a success", o)
	}

	// The next recharge is a new charge at the new amount.
	if _, err := svc.ProcessAutoRecharges(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := charger.reqs[2]; got.Amount != 60000 || got.IdempotencyKey == charger.reqs[0].IdempotencyKey {
		t.Fatalf("next recharge = %+v, want 60000 under a new key", got)
	}
}

// Soft declines are capped: card networks limit reattempts on a declined
// card, so after maxRechargeFailures failed attempts since the last success
// auto-recharge stops on that method until the customer saves another, as
// for a hard decline. The customer hears about the first failure and about
// the stop, not about every daily retry in between.
func TestWalletRecharge_SoftDeclinesAreCapped(t *testing.T) {
	for _, tc := range []struct {
		priorFailures int
		wantBlocked   bool
		wantEmail     bool
	}{
		{0, false, true},
		{1, false, false},
		{maxRechargeFailures - 2, false, false},
		{maxRechargeFailures - 1, true, true},
	} {
		svc, repo, _, _ := sarRechargeFixture()
		repo.dueList[0].RechargeFailures = tc.priorFailures
		notifier := &recordingNotifier{}
		svc.SetNotifier(notifier)
		charger := &fakeRenewalCharger{result: &port.PaymentResult{Success: false, ErrorCode: "insufficient_funds"}}
		svc.SetSavedMethodCharging(moyasarLookup(), moyasarRouter(charger))

		if _, err := svc.ProcessAutoRecharges(context.Background()); err != nil {
			t.Fatal(err)
		}
		o := repo.outcomes[0]
		if blocked := o.BlockedMethodRef == "token_abc"; blocked != tc.wantBlocked || !o.Answered || o.Failures != tc.priorFailures+1 {
			t.Fatalf("after %d prior failures: outcome = %+v, want blocked=%v", tc.priorFailures, o, tc.wantBlocked)
		}
		if emailed := len(notifier.to) == 1; emailed != tc.wantEmail || len(notifier.to) > 1 {
			t.Fatalf("after %d prior failures: emails = %v, want emailed=%v", tc.priorFailures, notifier.to, tc.wantEmail)
		}
	}
}

// tenantCapture records the tenant the routing guard was asked to route as.
type tenantCapture struct{ got uuid.UUID }

func (c *tenantCapture) ProviderFor(ctx context.Context, _ string) domain.GatewayProvider {
	c.got, _ = ctx.Value(domain.TenantIDKey).(uuid.UUID)
	return domain.GatewayMoyasar
}

// The recharge sweep runs on a background ctx; routing is per tenant (BYO
// connections), so the guard must see the wallet's tenant.
func TestWalletSavedMethod_RoutesAsTheWalletTenant(t *testing.T) {
	svc, _, _, w := sarRechargeFixture()
	moyasar := &fakeRenewalCharger{result: &port.PaymentResult{Success: true, PaymentID: uuid.NewString()}}
	router := moyasarRouter(moyasar)
	routes := &tenantCapture{}
	router.SetCurrencyRouting(routes)
	svc.SetSavedMethodCharging(moyasarLookup(), router)

	if _, err := svc.ProcessAutoRecharges(context.Background()); err != nil {
		t.Fatal(err)
	}
	if routes.got != w.TenantID {
		t.Fatalf("routed as tenant %v, want the wallet's %v", routes.got, w.TenantID)
	}
}
