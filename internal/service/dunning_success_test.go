package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
)

func TestDunningSuccessRecorder_ClosesCampaignAndRewardsAction(t *testing.T) {
	campaigns, outcomes := &fakeCampaignRecovery{}, &fakeOutcomeRecorder{}
	inv := &domain.Invoice{ID: uuid.New(), TenantID: uuid.New(), DunningActionID: domain.Action24Hour.ID, DunningContextKey: "ctx-key"}
	NewDunningSuccessRecorder(campaigns, outcomes).Record(context.Background(), inv)
	if len(campaigns.invoices) != 1 || campaigns.invoices[0] != inv.ID {
		t.Fatalf("campaign recoveries = %v, want [%s]", campaigns.invoices, inv.ID)
	}
	if len(outcomes.outcomes) != 1 {
		t.Fatalf("outcomes = %d, want 1", len(outcomes.outcomes))
	}
	o := outcomes.outcomes[0]
	if o.InvoiceID != inv.ID || o.TenantID != inv.TenantID || o.ContextKey != "ctx-key" || o.ActionID != domain.Action24Hour.ID ||
		o.Outcome != "success" || o.Reward != 1.0 || o.RetryInterval != int64(domain.Action24Hour.Interval.Seconds()) {
		t.Fatalf("outcome = %+v", o)
	}
}

// Without the action that preceded the payment there is no arm to reward,
// but the campaign still closes.
func TestDunningSuccessRecorder_NoActionNoOutcome(t *testing.T) {
	for name, inv := range map[string]*domain.Invoice{
		"no action":      {ID: uuid.New(), DunningContextKey: "ctx-key"},
		"no context key": {ID: uuid.New(), DunningActionID: domain.Action24Hour.ID},
	} {
		t.Run(name, func(t *testing.T) {
			campaigns, outcomes := &fakeCampaignRecovery{}, &fakeOutcomeRecorder{}
			NewDunningSuccessRecorder(campaigns, outcomes).Record(context.Background(), inv)
			if len(campaigns.invoices) != 1 || len(outcomes.outcomes) != 0 {
				t.Fatalf("recoveries=%d outcomes=%d, want 1/0", len(campaigns.invoices), len(outcomes.outcomes))
			}
		})
	}
}

func TestDunningSuccessRecorder_NilSafe(t *testing.T) {
	inv := &domain.Invoice{ID: uuid.New(), DunningActionID: domain.Action24Hour.ID, DunningContextKey: "k"}
	NewDunningSuccessRecorder(nil, nil).Record(context.Background(), inv)
	var r *DunningSuccessRecorder
	r.Record(context.Background(), inv)
}
