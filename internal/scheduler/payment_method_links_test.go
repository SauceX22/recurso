package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/recurso-dev/recurso/internal/adapter/db"
	"github.com/recurso-dev/recurso/internal/adapter/memory"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
	"github.com/recurso-dev/recurso/internal/service"
)

type recordingEmailSender struct{ sent []port.EmailMessage }

func (r *recordingEmailSender) Send(_ context.Context, m port.EmailMessage) error {
	r.sent = append(r.sent, m)
	return nil
}

type linkCall struct {
	baseURL    string
	customerID uuid.UUID
	currency   string
	invoiceID  uuid.UUID
}

// fakePaymentMethodLinker answers with a checkout link and records the ask.
type fakePaymentMethodLinker struct{ calls []linkCall }

func (f *fakePaymentMethodLinker) UpdatePaymentURL(_ context.Context, baseURL string, customerID uuid.UUID, currency string, invoiceID uuid.UUID) string {
	f.calls = append(f.calls, linkCall{baseURL, customerID, currency, invoiceID})
	return "https://pay.test/checkout/linked"
}

type retryInfoRepo struct{ InvoiceRepoForDunning }

func (retryInfoRepo) UpdateRetryInfoWithDunning(context.Context, uuid.UUID, time.Time, int, string) error {
	return nil
}

func overdueSAR() domain.OverdueInvoice {
	return domain.OverdueInvoice{
		ID: uuid.New(), TenantID: uuid.New(), CustomerID: uuid.New(), CustomerEmail: "c@x.test",
		InvoiceNumber: "INV-SAR-1", Amount: 11500, Currency: "SAR", DueDate: time.Now().AddDate(0, 0, -2),
	}
}

func TestDunningEmail_UpdatePaymentLinkComesFromPolicy(t *testing.T) {
	sender := &recordingEmailSender{}
	links := &fakePaymentMethodLinker{}
	s := &DunningScheduler{invoiceRepo: retryInfoRepo{}, notificationSvc: service.NewNotificationService(sender, "http://app.test"),
		config: DefaultDunningConfig(), portalBaseURL: "http://portal.test"}
	s.SetPaymentMethodLinks(links)

	inv := overdueSAR()
	s.processInvoice(context.Background(), inv)

	if want := (linkCall{"http://portal.test", inv.CustomerID, "SAR", inv.ID}); len(links.calls) != 1 || links.calls[0] != want {
		t.Fatalf("link asked with %+v, want [%+v]", links.calls, want)
	}
	if len(sender.sent) != 1 || !strings.Contains(sender.sent[0].HTMLBody, `href="https://pay.test/checkout/linked"`) {
		t.Fatalf("dunning email lacks the policy's update link: %d sent", len(sender.sent))
	}
}

func TestDunningEmail_WithoutPolicyKeepsPortalLink(t *testing.T) {
	sender := &recordingEmailSender{}
	s := &DunningScheduler{invoiceRepo: retryInfoRepo{}, notificationSvc: service.NewNotificationService(sender, "http://app.test"),
		config: DefaultDunningConfig(), portalBaseURL: "http://portal.test"}
	s.processInvoice(context.Background(), overdueSAR())
	if len(sender.sent) != 1 || !strings.Contains(sender.sent[0].HTMLBody, `href="http://portal.test/portal/login"`) {
		t.Fatal("dunning email lost the portal update link")
	}
}

type expiringCardRepo struct{ cust db.CustomerWithExpiringCard }

func (r *expiringCardRepo) GetCustomersWithExpiringCards(context.Context, int, int) ([]db.CustomerWithExpiringCard, error) {
	return []db.CustomerWithExpiringCard{r.cust}, nil
}

func (r *expiringCardRepo) MarkCardExpiryNotificationSent(context.Context, uuid.UUID, uuid.UUID, int, int, string) error {
	return nil
}

func TestCardExpiryEmail_UpdatePaymentLinkComesFromPolicy(t *testing.T) {
	sender := &recordingEmailSender{}
	links := &fakePaymentMethodLinker{}
	cust := db.CustomerWithExpiringCard{CustomerID: uuid.New(), TenantID: uuid.New(), CustomerEmail: "c@x.test",
		CardBrand: "mada", CardLast4: "1010", CardExpMonth: 11, CardExpYear: 2026}
	s := NewCardExpiringScheduler(&expiringCardRepo{cust: cust}, service.NewNotificationService(sender, "http://app.test"),
		memory.NewNoOpLocker(), "http://portal.test")
	s.SetPaymentMethodLinks(links)

	s.runCardExpiryNotifications()

	if want := (linkCall{"http://portal.test", cust.CustomerID, "", uuid.Nil}); len(links.calls) != 1 || links.calls[0] != want {
		t.Fatalf("link asked with %+v, want [%+v]", links.calls, want)
	}
	if len(sender.sent) != 1 || !strings.Contains(sender.sent[0].HTMLBody, `href="https://pay.test/checkout/linked"`) {
		t.Fatalf("card-expiry email lacks the policy's update link: %d sent", len(sender.sent))
	}
}
