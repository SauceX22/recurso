package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
)

type fakeTermsPlans struct {
	plan   *domain.Plan
	err    error
	tenant any
}

func (f *fakeTermsPlans) GetByID(ctx context.Context, id uuid.UUID) (*domain.Plan, error) {
	f.tenant = ctx.Value(domain.TenantIDKey)
	if f.err != nil || f.plan == nil || f.plan.ID != id {
		return nil, f.err
	}
	return f.plan, nil
}

type fakeTermsTenants struct {
	tenant *domain.Tenant
	err    error
}

func (f *fakeTermsTenants) GetByID(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
	if f.err != nil || f.tenant == nil || f.tenant.ID != id {
		return nil, f.err
	}
	return f.tenant, nil
}

type termsFixture struct {
	*settlementFixture
	plans   *fakeTermsPlans
	tenants *fakeTermsTenants
}

func newTermsFixture() *termsFixture {
	f := &termsFixture{settlementFixture: newSettlementFixture()}
	plan := &domain.Plan{ID: uuid.New(), TenantID: f.inv.TenantID, IntervalUnit: domain.IntervalMonth, IntervalCount: 3}
	f.subs.sub.PlanID = plan.ID
	f.inv.Total = 11500
	f.plans = &fakeTermsPlans{plan: plan}
	f.tenants = &fakeTermsTenants{tenant: &domain.Tenant{ID: f.inv.TenantID, Name: "Acme Arabia"}}
	f.svc.SetCardOnFileTerms(f.plans, f.tenants)
	return f
}

// The consent the checkout shows names who charges, how much, and how often,
// in a shape any gateway's form can render.
func TestGatewaySettlement_CardOnFileTerms(t *testing.T) {
	f := newTermsFixture()
	saves, terms := f.svc.CardOnFile(context.Background(), f.inv)
	if !saves || terms == nil {
		t.Fatalf("CardOnFile = %v, %+v; want terms for a renewing subscription", saves, terms)
	}
	want := CardOnFileTerms{Merchant: "Acme Arabia", Amount: 11500, Currency: "SAR", Interval: "month", IntervalCount: 3}
	if *terms != want {
		t.Fatalf("terms = %+v, want %+v", *terms, want)
	}
	if f.plans.tenant != f.inv.TenantID {
		t.Fatalf("plan read under tenant %v, want %s", f.plans.tenant, f.inv.TenantID)
	}
}

func TestGatewaySettlement_CardOnFileTermsDefaultsIntervalCount(t *testing.T) {
	f := newTermsFixture()
	f.plans.plan.IntervalCount = 0
	if _, terms := f.svc.CardOnFile(context.Background(), f.inv); terms == nil || terms.IntervalCount != 1 {
		t.Fatalf("terms = %+v, want interval_count 1", terms)
	}
}

// Terms are all-or-nothing: the page falls back to its generic sentence
// rather than render a half-filled agreement. Saving still follows SavesCard.
func TestGatewaySettlement_CardOnFileTermsIncomplete(t *testing.T) {
	cases := map[string]func(*termsFixture){
		"plan unreadable":   func(f *termsFixture) { f.plans.err = errors.New("db") },
		"tenant unreadable": func(f *termsFixture) { f.tenants.err = errors.New("db") },
		"no merchant name":  func(f *termsFixture) { f.tenants.tenant.Name = "" },
		"not wired":         func(f *termsFixture) { f.svc.SetCardOnFileTerms(nil, nil) },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			f := newTermsFixture()
			breakIt(f)
			saves, terms := f.svc.CardOnFile(context.Background(), f.inv)
			if !saves || terms != nil {
				t.Fatalf("CardOnFile = %v, %+v; want saving without terms", saves, terms)
			}
		})
	}
}

func TestGatewaySettlement_CardOnFileNotSaving(t *testing.T) {
	f := newTermsFixture()
	f.subs.sub.CancelAtPeriodEnd = true
	if saves, terms := f.svc.CardOnFile(context.Background(), f.inv); saves || terms != nil {
		t.Fatalf("CardOnFile = %v, %+v; want neither for a cancelling subscription", saves, terms)
	}
	var nilSvc *GatewaySettlementService
	if saves, terms := nilSvc.CardOnFile(context.Background(), f.inv); saves || terms != nil {
		t.Fatal("nil service offered card on file")
	}
}
