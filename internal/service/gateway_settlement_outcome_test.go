package service

import (
	"context"
	"testing"
)

// The retry bandit learns only about an action it chose: an invoice paid
// with no pending dunning action closes its campaign but feeds the bandit
// nothing, or a reward would land on an empty action and context.
func TestGatewaySettlement_NoDunningOutcomeWithoutAnAction(t *testing.T) {
	for _, tc := range []struct{ name, action, contextKey string }{
		{"no action", "", "ctx-key"},
		{"no context", "retry_24h", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSettlementFixture()
			f.inv.DunningActionID, f.inv.DunningContextKey = tc.action, tc.contextKey
			if _, err := f.svc.Settle(context.Background(), f.inv, f.payment); err != nil {
				t.Fatal(err)
			}
			if len(f.campaigns.invoices) != 1 {
				t.Fatalf("campaign recoveries = %v, want the invoice's campaign closed", f.campaigns.invoices)
			}
			if len(f.outcomes.outcomes) != 0 {
				t.Fatalf("dunning outcomes = %+v, want none without an action", f.outcomes.outcomes)
			}
		})
	}
}
