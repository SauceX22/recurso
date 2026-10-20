package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
	"github.com/recurso-dev/recurso/internal/core/port/porttest"
)

type dueWalletRepo struct {
	porttest.UnimplementedWalletRepository
	due []domain.Wallet
}

func (r *dueWalletRepo) ListDueForRecharge(context.Context, int) ([]domain.Wallet, error) {
	return r.due, nil
}

// noSavedMethodLookup reports no saved card, forcing the failure notice.
type noSavedMethodLookup struct{}

func (noSavedMethodLookup) GetSavedPaymentMethod(context.Context, uuid.UUID) (domain.SavedPaymentMethod, error) {
	return domain.SavedPaymentMethod{Provider: domain.GatewayStripe}, nil
}

type unusedWalletCharger struct{}

func (unusedWalletCharger) ChargeSavedPaymentMethod(context.Context, port.SavedMethodCharge) (*port.PaymentResult, error) {
	panic("charger must not be called without a saved method")
}

type recordingNotifier struct {
	porttest.UnimplementedNotifier
	to []string
}

func (n *recordingNotifier) SendEmail(_ context.Context, to, _, _ string) error {
	n.to = append(n.to, to)
	return nil
}

// TestWalletAutoRecharge_FailureNoticeInjectsWalletTenant: the auto-recharge
// sweep runs on context.Background(); the failure notice's customer lookup is
// tenant-scoped, so it must carry the wallet's tenant or the "update your
// payment method" email is never sent.
func TestWalletAutoRecharge_FailureNoticeInjectsWalletTenant(t *testing.T) {
	tenantID := uuid.New()
	cust := &domain.Customer{ID: uuid.New(), TenantID: tenantID, Email: "wallet@example.test"}
	amount := int64(5000)
	wallets := &dueWalletRepo{due: []domain.Wallet{{
		ID: uuid.New(), TenantID: tenantID, CustomerID: cust.ID, Currency: "USD", AutoRechargeAmount: &amount,
	}}}
	notifier := &recordingNotifier{}
	svc := NewWalletService(wallets, &strictTenantCustomerRepo{tenantID: tenantID, customer: cust}, nil)
	svc.SetSavedMethodCharging(unusedWalletCharger{}, noSavedMethodLookup{})
	svc.SetNotifier(notifier)

	if _, err := svc.ProcessAutoRecharges(context.Background()); err != nil {
		t.Fatalf("ProcessAutoRecharges: %v", err)
	}
	if len(notifier.to) != 1 || notifier.to[0] != cust.Email {
		t.Fatalf("recharge-failure notice sent to %v, want [%s]", notifier.to, cust.Email)
	}
}
