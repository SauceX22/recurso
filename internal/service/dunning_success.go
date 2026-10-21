package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
)

// DunningSuccessRecorder records that a payment recovered an invoice in
// dunning: its campaign is closed as recovered and the retry bandit is
// rewarded for the action that preceded the payment. Every settlement path
// (gateway webhooks, checkout verify) calls it once, on the paid transition.
type DunningSuccessRecorder struct {
	campaigns dunningRecoveryMarker
	outcomes  dunningOutcomeRecorder
	logger    *slog.Logger
}

// NewDunningSuccessRecorder builds the recorder; either dependency may be nil.
func NewDunningSuccessRecorder(campaigns dunningRecoveryMarker, outcomes dunningOutcomeRecorder) *DunningSuccessRecorder {
	return &DunningSuccessRecorder{campaigns: campaigns, outcomes: outcomes, logger: slog.Default().With("component", "dunning_success")}
}

// Record closes inv's dunning. Failures are logged, never returned: the
// payment has already settled and must not be retried for bookkeeping.
func (r *DunningSuccessRecorder) Record(ctx context.Context, inv *domain.Invoice) {
	if r == nil {
		return
	}
	if r.campaigns != nil {
		if err := r.campaigns.MarkRecovered(ctx, inv.ID); err != nil {
			r.logger.ErrorContext(ctx, "failed to mark dunning campaign recovered", "invoice_id", inv.ID, "error", err)
		}
	}
	if r.outcomes == nil || inv.DunningActionID == "" || inv.DunningContextKey == "" {
		return
	}
	err := r.outcomes.RecordOutcome(ctx, domain.DunningHistory{
		ID:            uuid.New(),
		TenantID:      inv.TenantID,
		InvoiceID:     inv.ID,
		ContextKey:    inv.DunningContextKey,
		ActionID:      inv.DunningActionID,
		RetryInterval: int64(actionByID(inv.DunningActionID).Interval.Seconds()),
		Outcome:       "success",
		Reward:        1.0,
		CreatedAt:     time.Now(),
	})
	if err != nil {
		r.logger.ErrorContext(ctx, "failed to record dunning success outcome", "invoice_id", inv.ID, "error", err)
		return
	}
	r.logger.InfoContext(ctx, "recorded dunning success outcome", "invoice_id", inv.ID, "action_id", inv.DunningActionID, "context_key", inv.DunningContextKey)
}
