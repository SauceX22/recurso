package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
	"github.com/recurso-dev/recurso/internal/core/port/porttest"
	"github.com/recurso-dev/recurso/internal/service"
	"github.com/stripe/stripe-go/v76"
)

type paymentFailedInvoiceRepo struct {
	porttest.UnimplementedInvoiceRepository
	inv *domain.Invoice
}

func (r *paymentFailedInvoiceRepo) GetByIDPublic(_ context.Context, id uuid.UUID) (*domain.Invoice, error) {
	if id != r.inv.ID {
		return nil, nil
	}
	return r.inv, nil
}

func (r *paymentFailedInvoiceRepo) Update(context.Context, *domain.Invoice) error { return nil }

// strictTenantCustomerRepo mirrors db.CustomerRepository.GetByID: it fails
// closed with "tenant_id missing from context" when ctx carries no tenant.
type strictTenantCustomerRepo struct {
	porttest.UnimplementedCustomerRepository
	customer *domain.Customer
}

func (r *strictTenantCustomerRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Customer, error) {
	tenantID, ok := ctx.Value(domain.TenantIDKey).(uuid.UUID)
	if !ok {
		return nil, fmt.Errorf("tenant_id missing from context")
	}
	if tenantID != r.customer.TenantID || id != r.customer.ID {
		return nil, nil
	}
	return r.customer, nil
}

type recordingEmailSender struct {
	to []string
}

func (s *recordingEmailSender) Send(_ context.Context, msg port.EmailMessage) error {
	s.to = append(s.to, msg.To)
	return nil
}

// TestHandleInvoicePaymentFailed_EmailsCustomerUnderInvoiceTenant: the public
// Stripe webhook runs on a tenantless request ctx. The payment-failed email's
// customer lookup is tenant-scoped, so the handler must inject the invoice's
// tenant or the email is never sent.
func TestHandleInvoicePaymentFailed_EmailsCustomerUnderInvoiceTenant(t *testing.T) {
	tenantID := uuid.New()
	cust := &domain.Customer{ID: uuid.New(), TenantID: tenantID, Email: "failed@example.test"}
	inv := &domain.Invoice{ID: uuid.New(), TenantID: tenantID, CustomerID: cust.ID,
		InvoiceNumber: "INV-PF-1", Total: 2500, Currency: "USD"}
	sender := &recordingEmailSender{}
	h := &WebhookHandler{
		logger:              slog.Default(),
		invoiceRepo:         &paymentFailedInvoiceRepo{inv: inv},
		customerRepo:        &strictTenantCustomerRepo{customer: cust},
		notificationService: service.NewNotificationService(sender, "http://example.test"),
	}

	raw, _ := json.Marshal(stripe.Invoice{ID: "in_x", Metadata: map[string]string{"invoice_id": inv.ID.String()}})
	event := stripe.Event{Type: "invoice.payment_failed", Data: &stripe.EventData{Raw: raw}}

	if err := h.handleInvoicePaymentFailed(context.Background(), event); err != nil {
		t.Fatalf("handleInvoicePaymentFailed: %v", err)
	}
	if len(sender.to) != 1 || sender.to[0] != cust.Email {
		t.Fatalf("payment-failed email sent to %v, want [%s]", sender.to, cust.Email)
	}
}
