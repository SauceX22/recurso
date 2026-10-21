package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
	"github.com/recurso-dev/recurso/internal/core/port/porttest"
)

// tenantCheckingSettler mimics *SubscriptionService.MarkInvoicePaid's real
// dependency: InvoiceRepository.GetByID fails without a tenant in context.
type tenantCheckingSettler struct {
	gotTenant uuid.UUID
	settled   int
}

func (f *tenantCheckingSettler) MarkInvoicePaid(ctx context.Context, invoiceID uuid.UUID) (bool, error) {
	tid, ok := ctx.Value(domain.TenantIDKey).(uuid.UUID)
	if !ok {
		return false, errTenantMissingForTest
	}
	f.gotTenant = tid
	f.settled++
	return true, nil
}

var errTenantMissingForTest = errors.New("tenant_id missing from context")

// The billing-cycle sweep runs on a background context with no tenant, so the
// settlement after a successful off-session charge must carry the invoice's
// own tenant — otherwise the tenant-scoped invoice lookup fails and the paid
// invoice stays open until (and only if) a gateway webhook settles it.
func TestRenewSubscription_SettlesWithInvoiceTenant(t *testing.T) {
	svc, _, invoicer, sub := renewalFixture()
	invoicer.invoice.TenantID = sub.TenantID
	settler := &tenantCheckingSettler{}
	svc.SetSavedMethodCharging(&fakeRenewalLookup{stripeID: "cus_1", methodID: "pm_1"},
		NewSavedCardGatewayRouter(nil, nil, &fakeRenewalCharger{result: &port.PaymentResult{Success: true}}), settler)

	if err := svc.RenewSubscription(context.Background(), sub); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if settler.settled != 1 {
		t.Fatalf("settled %d times, want 1 (settlement must not depend on the webhook)", settler.settled)
	}
	if settler.gotTenant != sub.TenantID {
		t.Fatalf("settled under tenant %v, want the invoice's tenant %v", settler.gotTenant, sub.TenantID)
	}
}

// tenantScopedPlans mimics db.PlanRepository.GetByID, which refuses a
// context without a tenant.
type tenantScopedPlans struct {
	porttest.UnimplementedPlanRepository
	plan *domain.Plan
}

func (p *tenantScopedPlans) GetByID(ctx context.Context, id uuid.UUID) (*domain.Plan, error) {
	if _, ok := ctx.Value(domain.TenantIDKey).(uuid.UUID); !ok {
		return nil, errTenantMissingForTest
	}
	return p.plan, nil
}

// The scheduler calls ProcessDueRenewals on context.Background(), so every
// tenant-scoped lookup inside a renewal (plan, invoice generation,
// settlement) must run under the claimed subscription's tenant.
func TestProcessDueRenewals_RunsUnderSubscriptionTenant(t *testing.T) {
	_, subRepo, invoicer, sub := renewalFixture()
	plans := &tenantScopedPlans{plan: &domain.Plan{
		ID: sub.PlanID, IntervalUnit: domain.IntervalMonth, IntervalCount: 1,
		Prices: []domain.Price{{Amount: 100000, Currency: "INR"}},
	}}
	svc := NewRenewalService(subRepo, plans, invoicer)
	svc.now = func() time.Time { return time.Date(2026, 7, 1, 0, 5, 0, 0, time.UTC) }

	renewed, err := svc.ProcessDueRenewals(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if renewed != 1 || invoicer.generated != 1 {
		t.Fatalf("renewed=%d generated=%d, want 1/1 (plan lookup must see the tenant)", renewed, invoicer.generated)
	}
}
