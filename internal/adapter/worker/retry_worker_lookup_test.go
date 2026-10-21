package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
	"github.com/recurso-dev/recurso/internal/service"
)

// A failure to resolve the saved method (a database blip, a BYO connection
// whose secret can't be opened right now) says nothing about whether the
// customer has a card. It must take the short infrastructure retry, never the
// gateway's interactive retry, whose "no saved method" answer would hand the
// invoice to email dunning for good.
func TestRetryWorker_SavedMethodResolutionFailureIsRetriedNotHandedOff(t *testing.T) {
	for _, tc := range []struct {
		name   string
		lookup *fakeCustomerLookup
		router *fakeCardRouter
	}{
		{"lookup error", &fakeCustomerLookup{err: errors.New("connection reset")}, &fakeCardRouter{charger: &fakeSavedCharger{name: "x"}}},
		{"connection store error", &fakeCustomerLookup{}, &fakeCardRouter{err: errors.New("load saved-card gateway connection: timeout")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := pastDueSARInvoice(1)
			repo := &mockInvoiceRepo{invoices: []*domain.Invoice{inv}}
			gw := &mockGateway{result: &port.PaymentResult{Success: false, ErrorCode: port.ErrCodeNoSavedMethod}}
			decider := &countingDecider{}

			w := NewRetryWorker(repo, decider, gw, &mockNotifier{})
			w.SetSavedMethodCharging(tc.lookup, tc.router)
			w.processRetries(context.Background())

			if gw.calls != 0 {
				t.Fatalf("interactive retry called %d times; a resolution failure must not fall through to it", gw.calls)
			}
			got := repo.updated[len(repo.updated)-1]
			if got.DunningManagedBy != "worker" || got.NextRetryAt == nil {
				t.Fatalf("managed_by=%q next_retry=%v; want a short worker retry, not a hand-off", got.DunningManagedBy, got.NextRetryAt)
			}
			if d := time.Until(*got.NextRetryAt); d < 4*time.Minute || d > 6*time.Minute {
				t.Fatalf("next retry in %v, want ~5m", d)
			}
			if got.RetryCount != 1 || decider.decides != 0 || len(decider.outcomes) != 0 {
				t.Fatalf("retry_count=%d decides=%d outcomes=%d; an infra failure is no attempt", got.RetryCount, decider.decides, len(decider.outcomes))
			}
		})
	}
}

// A method that can never be charged as configured (its gateway isn't set
// up, its connection is gone) falls through to the interactive retry.
func TestRetryWorker_NotChargeableMethodFallsThrough(t *testing.T) {
	inv := pastDueSARInvoice(1)
	repo := &mockInvoiceRepo{invoices: []*domain.Invoice{inv}}
	gw := &mockGateway{result: &port.PaymentResult{Success: false, ErrorCode: port.ErrCodeNoSavedMethod}}

	w := NewRetryWorker(repo, &countingDecider{}, gw, &mockNotifier{})
	w.SetSavedMethodCharging(&fakeCustomerLookup{}, &fakeCardRouter{err: fmt.Errorf("%w: connection gone", service.ErrSavedMethodNotChargeable)})
	w.processRetries(context.Background())

	if gw.calls != 1 {
		t.Fatalf("interactive retry calls = %d, want 1", gw.calls)
	}
	assertHandedToEmailDunning(t, repo.updated[0])
}
