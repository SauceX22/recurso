package worker

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
	"github.com/recurso-dev/recurso/internal/service"
)

func pastDueSARInvoice(retryCount int) *domain.Invoice {
	now := time.Now()
	return &domain.Invoice{
		ID:               uuid.New(),
		TenantID:         uuid.New(),
		CustomerID:       uuid.New(),
		InvoiceNumber:    "INV-SAR",
		Total:            11500,
		CreditApplied:    1500,
		Currency:         "SAR",
		Status:           domain.InvoiceStatusPastDue,
		RetryCount:       retryCount,
		DunningManagedBy: "worker",
		NextRetryAt:      &now,
	}
}

func newTestRetryService() *service.SmartRetryService {
	return service.NewSmartRetryService(&mockDunningRepo{weights: map[string]domain.DunningWeight{}})
}

// TestRetryWorker_PermanentDeclineStopsRetries: a hard decline leaves the
// invoice past_due with no next retry and never consults DecideRetry. The
// worker runs with a nil SmartRetryService, so reaching DecideRetry would
// panic.
func TestRetryWorker_PermanentDeclineStopsRetries(t *testing.T) {
	inv := pastDueSARInvoice(1)
	inv.DunningActionID = "" // no previous action → RecordOutcome is skipped too
	repo := &mockInvoiceRepo{invoices: []*domain.Invoice{inv}}
	gw := &mockGateway{result: &port.PaymentResult{Success: false, ErrorCode: "expired_card", Permanent: true}}

	w := NewRetryWorker(repo, nil, gw, &mockNotifier{})
	w.processRetries(context.Background())

	if len(repo.updated) != 1 {
		t.Fatalf("updates = %d, want 1", len(repo.updated))
	}
	got := repo.updated[0]
	if got.Status != domain.InvoiceStatusPastDue {
		t.Fatalf("status = %s, want past_due", got.Status)
	}
	if got.NextRetryAt != nil {
		t.Fatalf("NextRetryAt = %v, want nil (auto-retries stopped)", got.NextRetryAt)
	}
	if got.LastPaymentError != "expired_card" || got.RetryCount != 2 {
		t.Fatalf("LastPaymentError=%q RetryCount=%d, want expired_card/2", got.LastPaymentError, got.RetryCount)
	}
	if got.DunningActionID != "" || got.DunningContextKey != "" {
		t.Fatalf("dunning action not cleared: %q/%q", got.DunningActionID, got.DunningContextKey)
	}
}

// TestRetryWorker_PermanentDeclineStillRecordsPreviousOutcome: the bandit
// still learns the previous action failed before retries stop.
func TestRetryWorker_PermanentDeclineStillRecordsPreviousOutcome(t *testing.T) {
	inv := pastDueSARInvoice(2)
	inv.DunningActionID = "1h"
	inv.DunningContextKey = "SAR:card_declined"
	repo := &mockInvoiceRepo{invoices: []*domain.Invoice{inv}}
	gw := &mockGateway{result: &port.PaymentResult{Success: false, ErrorCode: "lost_or_stolen", Permanent: true}}
	dunningRepo := &mockDunningRepo{weights: make(map[string]domain.DunningWeight)}

	w := NewRetryWorker(repo, service.NewSmartRetryService(dunningRepo), gw, &mockNotifier{})
	w.processRetries(context.Background())

	if len(dunningRepo.history) != 1 || dunningRepo.history[0].Outcome != "failure" {
		t.Fatalf("history = %+v, want one failure outcome", dunningRepo.history)
	}
	got := repo.updated[0]
	if got.NextRetryAt != nil || got.Status != domain.InvoiceStatusPastDue || got.DunningActionID != "" {
		t.Fatalf("invoice = %+v, want past_due, no retry, action cleared", got)
	}
}

// TestRetryWorker_SoftDeclineStillReschedules: Permanent=false keeps the
// historical reschedule behavior.
func TestRetryWorker_SoftDeclineStillReschedules(t *testing.T) {
	inv := pastDueSARInvoice(1)
	repo := &mockInvoiceRepo{invoices: []*domain.Invoice{inv}}
	gw := &mockGateway{result: &port.PaymentResult{Success: false, ErrorCode: "insufficient_funds"}}
	dunningRepo := &mockDunningRepo{weights: make(map[string]domain.DunningWeight)}

	w := NewRetryWorker(repo, service.NewSmartRetryService(dunningRepo), gw, &mockNotifier{})
	w.processRetries(context.Background())

	got := repo.updated[0]
	if got.NextRetryAt == nil || got.DunningActionID == "" || got.Status != domain.InvoiceStatusPastDue {
		t.Fatalf("invoice = %+v, want rescheduled with a dunning action", got)
	}
}

// TestRetryWorker_ProviderSavedMethodCharged: a method saved on a non-Stripe
// gateway is charged on that gateway's charger with the per-attempt key, never
// on the platform Stripe charger or the interactive retry.
func TestRetryWorker_ProviderSavedMethodCharged(t *testing.T) {
	inv := pastDueSARInvoice(3)
	repo := &mockInvoiceRepo{invoices: []*domain.Invoice{inv}}
	gw := &mockGateway{result: &port.PaymentResult{Success: true}}
	moyasar := &fakeSavedCharger{name: "moyasar"}
	stripe := &fakeSavedCharger{name: "stripe"}
	router := service.NewSavedCardGatewayRouter(nil, nil, stripe)
	router.RegisterProvider(domain.GatewayMoyasar, moyasar)

	w := NewRetryWorker(repo, newTestRetryService(), gw, &mockNotifier{})
	w.SetSavedMethodCharging(stripe, &fakeCustomerLookup{provider: domain.GatewayMoyasar, ref: "token_r"})
	w.SetChargerRouter(router)
	w.processRetries(context.Background())

	if moyasar.charges != 1 || gw.calls != 0 || stripe.charges != 0 {
		t.Fatalf("moyasar calls=%d interactive=%d stripe=%d, want 1/0/0", moyasar.charges, gw.calls, stripe.charges)
	}
	want := port.SavedMethodCharge{
		MethodRef: "token_r", Amount: 10000, Currency: "SAR",
		InvoiceID: inv.ID.String(), IdempotencyKey: "retry-" + inv.ID.String() + "-3",
	}
	if moyasar.reqs[0] != want {
		t.Fatalf("charge = %+v, want %+v", moyasar.reqs[0], want)
	}
	if repo.updated[0].Status != domain.InvoiceStatusPaid {
		t.Fatalf("status = %s, want paid", repo.updated[0].Status)
	}
}

// TestRetryWorker_UnchargeableMethodUsesInteractiveRetry: a method whose
// provider has no charger is never charged elsewhere; the interactive gateway
// retry runs instead.
func TestRetryWorker_UnchargeableMethodUsesInteractiveRetry(t *testing.T) {
	inv := pastDueSARInvoice(0)
	repo := &mockInvoiceRepo{invoices: []*domain.Invoice{inv}}
	gw := &mockGateway{result: &port.PaymentResult{Success: true}}
	stripe := &fakeSavedCharger{name: "stripe"}

	w := NewRetryWorker(repo, newTestRetryService(), gw, &mockNotifier{})
	w.SetSavedMethodCharging(stripe, &fakeCustomerLookup{provider: domain.GatewayMoyasar, ref: "token_r"})
	w.processRetries(context.Background())

	if stripe.charges != 0 || gw.calls != 1 || gw.lastAmount != 10000 {
		t.Fatalf("stripe=%d interactive=%d amount=%d, want 0/1/10000", stripe.charges, gw.calls, gw.lastAmount)
	}
}
