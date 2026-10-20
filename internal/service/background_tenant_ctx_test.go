package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
	"github.com/recurso-dev/recurso/internal/core/port/porttest"
)

// strictTenantCustomerRepo mirrors db.CustomerRepository.GetByID: it fails
// closed with "tenant_id missing from context" when ctx carries no tenant (or
// the wrong one), exactly as the real repository does. Every lookup result is
// published on looked so tests can synchronize with background goroutines.
type strictTenantCustomerRepo struct {
	porttest.UnimplementedCustomerRepository
	tenantID uuid.UUID
	customer *domain.Customer
	looked   chan error
}

func (f *strictTenantCustomerRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Customer, error) {
	err := f.check(ctx, id)
	if f.looked != nil {
		f.looked <- err
	}
	if err != nil {
		return nil, err
	}
	return f.customer, nil
}

func (f *strictTenantCustomerRepo) check(ctx context.Context, id uuid.UUID) error {
	got, ok := ctx.Value(domain.TenantIDKey).(uuid.UUID)
	if !ok {
		return fmt.Errorf("tenant_id missing from context")
	}
	if got != f.tenantID || id != f.customer.ID {
		return fmt.Errorf("customer not found")
	}
	return nil
}

// chanEmailSender publishes every send on a channel so tests can wait on a
// background goroutine without sleeping.
type chanEmailSender struct {
	sent chan port.EmailMessage
}

func (s *chanEmailSender) Send(_ context.Context, msg port.EmailMessage) error {
	s.sent <- msg
	return nil
}

// TestNotifyInvoiceCreated_InjectsInvoiceTenant: the invoice-created email
// goroutine runs on a fresh background context, so it must carry the invoice's
// own tenant into the tenant-scoped customer lookup. Otherwise the lookup
// always fails and no invoice email (with its Pay Now link) is ever sent.
func TestNotifyInvoiceCreated_InjectsInvoiceTenant(t *testing.T) {
	tenantID := uuid.New()
	cust := &domain.Customer{ID: uuid.New(), TenantID: tenantID, Email: "payer@example.test"}
	repo := &strictTenantCustomerRepo{tenantID: tenantID, customer: cust, looked: make(chan error, 1)}
	sender := &chanEmailSender{sent: make(chan port.EmailMessage, 1)}
	svc := &InvoiceService{
		CustomerRepo:        repo,
		NotificationService: NewNotificationService(sender, "http://example.test"),
	}

	inv := &domain.Invoice{
		ID: uuid.New(), TenantID: tenantID, CustomerID: cust.ID,
		InvoiceNumber: "INV-1", Total: 1000, Currency: "USD", DueDate: time.Now(),
	}
	svc.notifyInvoiceCreated(inv)

	// The goroutine always reaches the customer lookup first; its outcome
	// decides whether a send follows.
	if err := <-repo.looked; err != nil {
		t.Fatalf("customer lookup in notify goroutine failed: %v", err)
	}
	msg := <-sender.sent
	if msg.To != cust.Email {
		t.Fatalf("email sent to %q, want %q", msg.To, cust.Email)
	}
}
