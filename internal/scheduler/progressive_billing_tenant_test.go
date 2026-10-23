package scheduler

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/recurso-dev/recurso/internal/adapter/memory"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port/porttest"
	"github.com/recurso-dev/recurso/internal/service"
)

type refSweepRepo struct {
	refs []domain.TenantScopedID
}

func (r *refSweepRepo) ListActiveProgressiveSubscriptionIDs(context.Context) ([]domain.TenantScopedID, error) {
	return r.refs, nil
}

// strictTenantSubscriptionRepo mirrors db.SubscriptionRepository.GetByID: it
// fails closed with "tenant_id missing from context" when ctx carries no
// tenant, and only returns subscriptions owned by the ctx tenant.
type strictTenantSubscriptionRepo struct {
	porttest.UnimplementedSubscriptionRepository
	subs   map[uuid.UUID]*domain.Subscription
	loaded []uuid.UUID
}

func (r *strictTenantSubscriptionRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Subscription, error) {
	tenantID, ok := ctx.Value(domain.TenantIDKey).(uuid.UUID)
	if !ok {
		return nil, fmt.Errorf("tenant_id missing from context")
	}
	sub := r.subs[id]
	if sub == nil || sub.TenantID != tenantID {
		return nil, nil
	}
	r.loaded = append(r.loaded, id)
	return sub, nil
}

// TestProgressiveSweep_InjectsEachSubscriptionTenant drives the sweep (which
// runs on context.Background()) into the real InvoiceService biller. Each
// candidate's own tenant must reach the tenant-scoped subscription read, or
// the sweep never bills anything. ProgressiveRepo is left nil so the biller
// stops right after loading the subscription.
func TestProgressiveSweep_InjectsEachSubscriptionTenant(t *testing.T) {
	tenantA, tenantB := uuid.New(), uuid.New()
	subA := &domain.Subscription{ID: uuid.New(), TenantID: tenantA}
	subB := &domain.Subscription{ID: uuid.New(), TenantID: tenantB}
	subs := &strictTenantSubscriptionRepo{subs: map[uuid.UUID]*domain.Subscription{subA.ID: subA, subB.ID: subB}}
	biller := &service.InvoiceService{SubscriptionRepo: subs}
	repo := &refSweepRepo{refs: []domain.TenantScopedID{
		{ID: subA.ID, TenantID: tenantA},
		{ID: subB.ID, TenantID: tenantB},
	}}

	NewProgressiveBillingScheduler(repo, biller, memory.NewNoOpLocker(), 0).run()

	if len(subs.loaded) != 2 {
		t.Fatalf("sweep loaded %d of 2 subscriptions under their own tenant: %v", len(subs.loaded), subs.loaded)
	}
}
