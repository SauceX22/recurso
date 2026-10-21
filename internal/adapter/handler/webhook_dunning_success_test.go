package handler

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/service"
)

type recordingDunningRepo struct{ history []domain.DunningHistory }

func (r *recordingDunningRepo) GetWeights(context.Context, string) ([]domain.DunningWeight, error) {
	return nil, nil
}
func (r *recordingDunningRepo) ApplyOutcome(context.Context, string, string, float64) error {
	return nil
}
func (r *recordingDunningRepo) RecordHistory(_ context.Context, h domain.DunningHistory) error {
	r.history = append(r.history, h)
	return nil
}

// The Stripe, Razorpay and GoCardless webhooks reward the bandit through the
// same recorder as gateway settlement, from the reloaded invoice.
func TestRecordDunningSuccess_RewardsTheInvoicesAction(t *testing.T) {
	inv := &domain.Invoice{ID: uuid.New(), TenantID: uuid.New(), DunningActionID: domain.Action24Hour.ID, DunningContextKey: "ctx"}
	repo := &recordingDunningRepo{}
	h := &WebhookHandler{logger: slog.Default(), invoiceRepo: &fakeCheckoutInvoices{inv: inv}, retryService: service.NewSmartRetryService(repo)}
	h.recordDunningSuccess(context.Background(), inv.ID)
	if len(repo.history) != 1 {
		t.Fatalf("outcomes = %d, want 1", len(repo.history))
	}
	if o := repo.history[0]; o.InvoiceID != inv.ID || o.TenantID != inv.TenantID || o.ActionID != domain.Action24Hour.ID || o.Outcome != "success" {
		t.Fatalf("outcome = %+v", o)
	}
}

// Unset services stay nil interfaces: nothing is called, nothing panics.
func TestRecordDunningSuccess_UnwiredIsANoOp(t *testing.T) {
	h := &WebhookHandler{logger: slog.Default()}
	h.recordDunningSuccess(context.Background(), uuid.New())
}
