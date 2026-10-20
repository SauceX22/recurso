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

// countingDecider is a retry decider that counts calls and never schedules.
type countingDecider struct {
	decides  int
	outcomes []domain.DunningHistory
}

func (d *countingDecider) DecideRetry(context.Context, *domain.Invoice, string) *service.RetryDecision {
	d.decides++
	return nil
}

func (d *countingDecider) RecordOutcome(_ context.Context, h domain.DunningHistory) error {
	d.outcomes = append(d.outcomes, h)
	return nil
}

// assertHandedToEmailDunning checks the invoice is back on the dunning
// scheduler's email ladder: no worker retry pending and managed by the
// scheduler, so ClaimOverdueForDunning picks it up and it can still escalate
// to uncollectible.
func assertHandedToEmailDunning(t *testing.T, got *domain.Invoice) {
	t.Helper()
	if got.Status != domain.InvoiceStatusPastDue {
		t.Fatalf("status = %s, want past_due", got.Status)
	}
	if got.NextRetryAt != nil {
		t.Fatalf("NextRetryAt = %v, want nil (no auto-retry pending)", got.NextRetryAt)
	}
	if got.DunningManagedBy != "scheduler" {
		t.Fatalf("DunningManagedBy = %q, want scheduler (else neither claim ever picks it up again)", got.DunningManagedBy)
	}
	if got.DunningActionID != "" || got.DunningContextKey != "" {
		t.Fatalf("dunning action not cleared: %q/%q", got.DunningActionID, got.DunningContextKey)
	}
}

// TestRetryWorker_PermanentDeclineHandsInvoiceToEmailDunning: a hard decline
// stops auto-retries without deciding a next one, and hands the invoice back
// to email dunning instead of orphaning it.
func TestRetryWorker_PermanentDeclineHandsInvoiceToEmailDunning(t *testing.T) {
	inv := pastDueSARInvoice(1)
	inv.DunningActionID = "" // no previous action → RecordOutcome is skipped too
	repo := &mockInvoiceRepo{invoices: []*domain.Invoice{inv}}
	gw := &mockGateway{result: &port.PaymentResult{Success: false, ErrorCode: "expired_card", Permanent: true}}
	decider := &countingDecider{}

	w := NewRetryWorker(repo, decider, gw, &mockNotifier{})
	w.processRetries(context.Background())

	if len(repo.updated) != 1 {
		t.Fatalf("updates = %d, want 1", len(repo.updated))
	}
	got := repo.updated[0]
	assertHandedToEmailDunning(t, got)
	if got.LastPaymentError != "expired_card" || got.RetryCount != 2 {
		t.Fatalf("LastPaymentError=%q RetryCount=%d, want expired_card/2", got.LastPaymentError, got.RetryCount)
	}
	if decider.decides != 0 {
		t.Fatalf("DecideRetry called %d times after a hard decline, want 0", decider.decides)
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
	assertHandedToEmailDunning(t, repo.updated[0])
}

// TestRetryWorker_NoSavedMethodHandsInvoiceToEmailDunning: with nothing to
// charge off-session, the worker neither reschedules a pointless retry nor
// stops dunning; the invoice goes back to the email ladder, and the bandit is
// not fed an outcome that says nothing about the retry timing.
func TestRetryWorker_NoSavedMethodHandsInvoiceToEmailDunning(t *testing.T) {
	inv := pastDueSARInvoice(1)
	inv.DunningActionID = "24h"
	inv.DunningContextKey = "SAR:insufficient_funds"
	repo := &mockInvoiceRepo{invoices: []*domain.Invoice{inv}}
	gw := &mockGateway{result: &port.PaymentResult{Success: false, ErrorCode: port.ErrCodeNoSavedMethod}}
	decider := &countingDecider{}

	w := NewRetryWorker(repo, decider, gw, &mockNotifier{})
	w.processRetries(context.Background())

	if len(repo.updated) != 1 {
		t.Fatalf("updates = %d, want 1", len(repo.updated))
	}
	got := repo.updated[0]
	assertHandedToEmailDunning(t, got)
	if got.RetryCount != 1 {
		t.Fatalf("RetryCount = %d, want 1 (no charge was attempted)", got.RetryCount)
	}
	if decider.decides != 0 || len(decider.outcomes) != 0 {
		t.Fatalf("decides=%d outcomes=%d, want 0/0", decider.decides, len(decider.outcomes))
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

// settlingRepo is mockInvoiceRepo plus the reads and writes of a settled
// retry: the re-read after MarkInvoicePaid and the payment id record.
type settlingRepo struct {
	mockInvoiceRepo
	paymentIDs []string
}

func (r *settlingRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.Invoice, error) {
	for _, inv := range r.invoices {
		if inv.ID == id {
			cp := *inv
			cp.Status = domain.InvoiceStatusPaid
			return &cp, nil
		}
	}
	return nil, nil
}

func (r *settlingRepo) SetGatewayPaymentID(_ context.Context, _, _ uuid.UUID, id string) error {
	r.paymentIDs = append(r.paymentIDs, id)
	return nil
}

type fakeSettler struct{ transitioned bool }

func (f fakeSettler) MarkInvoicePaid(context.Context, uuid.UUID) (bool, error) {
	return f.transitioned, nil
}

// The payment a retry settles with is the invoice's refund target, recorded
// only when that payment settled the invoice and a real charge was made.
func TestRetryWorker_RecordsSettlingPaymentID(t *testing.T) {
	for _, tc := range []struct {
		name         string
		transitioned bool
		creditedAll  bool
		want         []string
	}{
		{"settled by this charge", true, false, []string{"pay_moyasar"}},
		{"already settled elsewhere", false, false, nil},
		{"nothing was charged", true, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := pastDueSARInvoice(1)
			if tc.creditedAll {
				inv.CreditApplied = inv.Total
			}
			repo := &settlingRepo{mockInvoiceRepo: mockInvoiceRepo{invoices: []*domain.Invoice{inv}}}
			moyasar := &fakeSavedCharger{name: "moyasar"}
			router := service.NewSavedCardGatewayRouter(nil, nil, nil)
			router.RegisterProvider(domain.GatewayMoyasar, moyasar)

			w := NewRetryWorker(repo, newTestRetryService(), &mockGateway{}, &mockNotifier{})
			w.SetSavedMethodCharging(nil, &fakeCustomerLookup{provider: domain.GatewayMoyasar, ref: "token_r"})
			w.SetChargerRouter(router)
			w.SetSettler(fakeSettler{transitioned: tc.transitioned})
			w.processRetries(context.Background())

			if len(repo.paymentIDs) != len(tc.want) || (len(tc.want) == 1 && repo.paymentIDs[0] != tc.want[0]) {
				t.Fatalf("recorded payment ids = %v, want %v", repo.paymentIDs, tc.want)
			}
		})
	}
}
