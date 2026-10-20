package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
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
	svc.SetSavedMethodCharging(&fakeRenewalCharger{result: &port.PaymentResult{Success: true}},
		&fakeRenewalLookup{stripeID: "cus_1", methodID: "pm_1"}, settler)

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
