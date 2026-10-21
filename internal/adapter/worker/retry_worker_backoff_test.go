package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
	"github.com/recurso-dev/recurso/internal/service"
)

func moyasarRetryWorker(repo port.InvoiceRepository, charger *fakeSavedCharger, ref string) *RetryWorker {
	router := service.NewSavedCardGatewayRouter(nil, nil, nil)
	router.RegisterProvider(domain.GatewayMoyasar, charger)
	w := NewRetryWorker(repo, &countingDecider{}, &mockGateway{}, &mockNotifier{})
	w.SetSavedMethodCharging(&fakeCustomerLookup{provider: domain.GatewayMoyasar, ref: ref}, router)
	return w
}

// A hard decline records the declined card. When the scheduler hands the
// invoice back after its next email, that card is not charged again (card
// networks forbid re-attempting lost, stolen or closed cards); a card the
// customer saved since is.
func TestRetryWorker_HardDeclinedMethodIsNotChargedAgain(t *testing.T) {
	inv := pastDueSARInvoice(1)
	repo := &mockInvoiceRepo{invoices: []*domain.Invoice{inv}}
	charger := &fakeSavedCharger{name: "moyasar", result: &port.PaymentResult{Success: false, ErrorCode: "lost_or_stolen", Permanent: true}}

	moyasarRetryWorker(repo, charger, "token_lost").processRetries(context.Background())
	declined := repo.updated[0]
	assertHandedToEmailDunning(t, declined)
	if declined.DeclinedMethodRef != "token_lost" {
		t.Fatalf("DeclinedMethodRef = %q, want token_lost", declined.DeclinedMethodRef)
	}

	// The scheduler emails and hands it back with its own count.
	handedBack := *declined
	handedBack.DunningManagedBy, handedBack.RetryCount = "worker", 2
	repo = &mockInvoiceRepo{invoices: []*domain.Invoice{&handedBack}}
	moyasarRetryWorker(repo, charger, "token_lost").processRetries(context.Background())
	if charger.charges != 1 {
		t.Fatalf("hard-declined card charged %d times, want once", charger.charges)
	}
	got := repo.updated[0]
	assertHandedToEmailDunning(t, got)
	if got.RetryCount != 2 || got.DeclinedMethodRef != "token_lost" || got.LastPaymentError != "lost_or_stolen" {
		t.Fatalf("got retry_count=%d declined=%q last_error=%q, want 2/token_lost/lost_or_stolen", got.RetryCount, got.DeclinedMethodRef, got.LastPaymentError)
	}

	repo = &mockInvoiceRepo{invoices: []*domain.Invoice{&handedBack}}
	fresh := &fakeSavedCharger{name: "moyasar"}
	w := moyasarRetryWorker(repo, fresh, "token_new")
	w.SetSettler(&fakeSettler{transitioned: true})
	w.processRetries(context.Background())
	if fresh.charges != 1 {
		t.Fatalf("a newly saved card was charged %d times, want 1", fresh.charges)
	}
}

// Gateway errors back off exponentially and, after a run of them, hand the
// invoice to email dunning instead of calling a broken gateway every five
// minutes forever. An error is not an attempt: the retry count and the
// pending dunning action stay.
func TestRetryWorker_InfraErrorsBackOffThenHandOff(t *testing.T) {
	gw := &mockGateway{err: errors.New("moyasar /payments: HTTP 401")}
	for _, tc := range []struct {
		prior     int
		wantDelay time.Duration
	}{
		{0, 5 * time.Minute},
		{1, 10 * time.Minute},
		{3, 40 * time.Minute},
		{5, 160 * time.Minute},
	} {
		inv := pastDueSARInvoice(1)
		inv.DunningActionID, inv.DunningContextKey = "24h", "SAR:card_declined"
		inv.RetryInfraFailures = tc.prior
		repo := &mockInvoiceRepo{invoices: []*domain.Invoice{inv}}
		NewRetryWorker(repo, &countingDecider{}, gw, &mockNotifier{}).processRetries(context.Background())

		got := repo.updated[0]
		if got.RetryInfraFailures != tc.prior+1 || got.DunningManagedBy != "worker" || got.NextRetryAt == nil {
			t.Fatalf("after %d prior errors: failures=%d managed_by=%q next=%v", tc.prior, got.RetryInfraFailures, got.DunningManagedBy, got.NextRetryAt)
		}
		if d := time.Until(*got.NextRetryAt); d < tc.wantDelay-time.Minute || d > tc.wantDelay+time.Minute {
			t.Fatalf("after %d prior errors: retry in %v, want ~%v", tc.prior, d, tc.wantDelay)
		}
		if got.RetryCount != 1 || got.DunningActionID != "24h" || got.DunningContextKey != "SAR:card_declined" {
			t.Fatalf("after %d prior errors: retry_count=%d action=%q/%q changed", tc.prior, got.RetryCount, got.DunningActionID, got.DunningContextKey)
		}
	}

	inv := pastDueSARInvoice(1)
	inv.RetryInfraFailures = maxInfraFailures
	repo := &mockInvoiceRepo{invoices: []*domain.Invoice{inv}}
	NewRetryWorker(repo, &countingDecider{}, gw, &mockNotifier{}).processRetries(context.Background())
	got := repo.updated[0]
	assertHandedToEmailDunning(t, got)
	if got.RetryInfraFailures != 0 || got.RetryCount != 1 {
		t.Fatalf("failures=%d retry_count=%d, want the error streak reset and the count kept", got.RetryInfraFailures, got.RetryCount)
	}
}

// A gateway that answers ends the error streak.
func TestRetryWorker_AnswerResetsInfraFailures(t *testing.T) {
	inv := pastDueSARInvoice(1)
	inv.RetryInfraFailures = 4
	repo := &mockInvoiceRepo{invoices: []*domain.Invoice{inv}}
	gw := &mockGateway{result: &port.PaymentResult{Success: false, ErrorCode: "insufficient_funds"}}
	NewRetryWorker(repo, newTestRetryService(), gw, &mockNotifier{}).processRetries(context.Background())
	if got := repo.updated[0]; got.RetryInfraFailures != 0 {
		t.Fatalf("RetryInfraFailures = %d after a decline, want 0", got.RetryInfraFailures)
	}
}
