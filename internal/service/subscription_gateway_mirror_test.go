package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port/porttest"
)

// unsupportedSubGateway answers CreateSubscription the way a gateway without
// gateway-managed subscriptions does: an error and no id.
type unsupportedSubGateway struct {
	porttest.UnimplementedPaymentGateway
	calls    int
	currency string
}

func (g *unsupportedSubGateway) CreateSubscription(_ context.Context, _ string, _ int, _ string, _ *int64, currency string) (string, error) {
	g.calls++
	g.currency = currency
	return "", errors.New("gateway-managed subscriptions are not supported")
}

// TestCreateSubscription_GatewayMirrorErrorKeepsIDsEmpty guards the renewal
// claim: a subscription whose gateway returned ("", error) must not store a
// gateway subscription id, because the claim skips rows with a non-empty
// stripe_subscription_id / razorpay_subscription_id.
func TestCreateSubscription_GatewayMirrorErrorKeepsIDsEmpty(t *testing.T) {
	planID, customerID := uuid.New(), uuid.New()
	planRepo := &subMockPlanRepo{plan: &domain.Plan{
		ID: planID, Code: "sar-monthly", IntervalUnit: domain.IntervalMonth, IntervalCount: 1,
		Prices: []domain.Price{{Amount: 11500, Currency: "SAR"}},
	}}
	custRepo := &subMockCustomerRepo{customer: &domain.Customer{ID: customerID, Email: "sar@x.com"}}
	subRepo := &subMockSubRepo{}
	gw := &unsupportedSubGateway{}
	svc := newTestSubscriptionService(subRepo, &subMockInvoiceRepo{}, planRepo, custRepo, &subMockCouponRepo{}, gw)

	sub, err := svc.CreateSubscription(context.Background(), CreateSubscriptionInput{
		TenantID: uuid.New(), CustomerID: customerID, PlanID: planID,
		StartDate: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), TrialDays: 7,
	})
	if err != nil {
		t.Fatalf("CreateSubscription must succeed when the gateway mirror fails: %v", err)
	}
	if gw.calls != 1 || gw.currency != "SAR" {
		t.Fatalf("gateway CreateSubscription calls=%d currency=%q, want 1/SAR", gw.calls, gw.currency)
	}
	if sub.StripeSubscriptionID != "" || sub.RazorpaySubscriptionID != "" {
		t.Fatalf("gateway ids stored: stripe=%q razorpay=%q, want both empty", sub.StripeSubscriptionID, sub.RazorpaySubscriptionID)
	}
	if subRepo.created == nil || subRepo.created.StripeSubscriptionID != "" {
		t.Fatalf("persisted subscription = %+v, want empty stripe_subscription_id", subRepo.created)
	}
}
