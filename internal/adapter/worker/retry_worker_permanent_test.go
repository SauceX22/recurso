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

// main.go wires *service.ProviderSavedMethods directly; no adapter needed.
var _ providerSavedMethodResolver = (*service.ProviderSavedMethods)(nil)

// argsSavedCharger records the arguments of each off-session charge.
type argsSavedCharger struct {
	result      *port.PaymentResult
	calls       int
	customerRef string
	methodRef   string
	amount      int64
	currency    string
	invoiceID   string
	key         string
}

func (c *argsSavedCharger) ChargeSavedPaymentMethod(_ context.Context, customerRef, methodRef string, amount int64, currency, invoiceID, key string) (*port.PaymentResult, error) {
	c.calls++
	c.customerRef, c.methodRef, c.amount, c.currency, c.invoiceID, c.key = customerRef, methodRef, amount, currency, invoiceID, key
	return c.result, nil
}

// fakeProviderResolver resolves every customer to one charger + ref when ok.
type fakeProviderResolver struct {
	charger service.SavedCardCharger
	ref     string
	ok      bool
}

func (f *fakeProviderResolver) Resolve(_ context.Context, _ uuid.UUID) (service.SavedCardCharger, string, bool) {
	return f.charger, f.ref, f.ok
}

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

// TestRetryWorker_PermanentDeclineStopsRetries: a hard decline leaves the
// invoice past_due with no next retry and never consults DecideRetry. The
// worker runs with a nil SmartRetryService, so reaching DecideRetry would
// panic.
func TestRetryWorker_PermanentDeclineStopsRetries(t *testing.T) {
	inv := pastDueSARInvoice(1)
	inv.DunningActionID = "" // no previous action → RecordOutcome is skipped too
	repo := &mockInvoiceRepo{invoices: []*domain.Invoice{inv}}
	gw := &mockGateway{result: &port.PaymentResult{Success: false, ErrorCode: "card_declined_54", Permanent: true}}

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
	if got.LastPaymentError != "card_declined_54" || got.RetryCount != 2 {
		t.Fatalf("LastPaymentError=%q RetryCount=%d, want card_declined_54/2", got.LastPaymentError, got.RetryCount)
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
	gw := &mockGateway{result: &port.PaymentResult{Success: false, ErrorCode: "no_saved_payment_method", Permanent: true}}
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
	gw := &mockGateway{result: &port.PaymentResult{Success: false, ErrorCode: "card_declined_51"}}
	dunningRepo := &mockDunningRepo{weights: make(map[string]domain.DunningWeight)}

	w := NewRetryWorker(repo, service.NewSmartRetryService(dunningRepo), gw, &mockNotifier{})
	w.processRetries(context.Background())

	got := repo.updated[0]
	if got.NextRetryAt == nil || got.DunningActionID == "" || got.Status != domain.InvoiceStatusPastDue {
		t.Fatalf("invoice = %+v, want rescheduled with a dunning action", got)
	}
}

// TestRetryWorker_ProviderSavedMethodCharged: a Moyasar-saved customer is
// charged on the provider charger with the per-attempt key, ahead of the
// Stripe saved-card path.
func TestRetryWorker_ProviderSavedMethodCharged(t *testing.T) {
	inv := pastDueSARInvoice(3)
	repo := &mockInvoiceRepo{invoices: []*domain.Invoice{inv}}
	gw := &mockGateway{result: &port.PaymentResult{Success: true}}
	moyasar := &argsSavedCharger{result: &port.PaymentResult{Success: true, PaymentID: uuid.NewString()}}
	stripe := &fakeSavedCharger{}

	w := NewRetryWorker(repo, service.NewSmartRetryService(&mockDunningRepo{weights: map[string]domain.DunningWeight{}}), gw, &mockNotifier{})
	w.SetSavedMethodCharging(stripe, &fakeCustomerLookup{})
	w.SetProviderSavedMethods(&fakeProviderResolver{charger: moyasar, ref: "token_r", ok: true})
	w.processRetries(context.Background())

	if moyasar.calls != 1 || gw.calls != 0 || stripe.charges != 0 {
		t.Fatalf("moyasar calls=%d interactive=%d stripe=%d, want 1/0/0", moyasar.calls, gw.calls, stripe.charges)
	}
	wantKey := "retry-" + inv.ID.String() + "-3"
	if moyasar.customerRef != "" || moyasar.methodRef != "token_r" || moyasar.amount != 10000 ||
		moyasar.currency != "SAR" || moyasar.invoiceID != inv.ID.String() || moyasar.key != wantKey {
		t.Fatalf("charge args = %+v, want ('', token_r, 10000, SAR, %s, %s)", moyasar, inv.ID, wantKey)
	}
	if repo.updated[0].Status != domain.InvoiceStatusPaid {
		t.Fatalf("status = %s, want paid", repo.updated[0].Status)
	}
}

// TestRetryWorker_ProviderUnresolvedFallsThrough: ok=false leaves the
// existing path in charge (here, the interactive gateway retry).
func TestRetryWorker_ProviderUnresolvedFallsThrough(t *testing.T) {
	inv := pastDueSARInvoice(0)
	repo := &mockInvoiceRepo{invoices: []*domain.Invoice{inv}}
	gw := &mockGateway{result: &port.PaymentResult{Success: true}}
	moyasar := &argsSavedCharger{result: &port.PaymentResult{Success: true}}

	w := NewRetryWorker(repo, service.NewSmartRetryService(&mockDunningRepo{weights: map[string]domain.DunningWeight{}}), gw, &mockNotifier{})
	w.SetProviderSavedMethods(&fakeProviderResolver{charger: moyasar, ok: false})
	w.processRetries(context.Background())

	if moyasar.calls != 0 || gw.calls != 1 || gw.lastAmount != 10000 {
		t.Fatalf("moyasar=%d interactive=%d amount=%d, want 0/1/10000", moyasar.calls, gw.calls, gw.lastAmount)
	}
}
