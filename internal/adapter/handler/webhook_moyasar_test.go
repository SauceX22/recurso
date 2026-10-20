package handler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
	"github.com/recurso-dev/recurso/internal/core/port/porttest"
	"github.com/recurso-dev/recurso/internal/service"
)

const moyasarTestSecret = "mysr_whsec_test"

// fakeMoyasarFetcher returns a canned payment read-back (or error) and counts
// calls, so tests can prove the body is never trusted.
type fakeMoyasarFetcher struct {
	payment *port.GatewayPayment
	err     error
	calls   int
}

func (f *fakeMoyasarFetcher) FetchPayment(_ context.Context, id string) (*port.GatewayPayment, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.payment == nil || f.payment.ID != id {
		return nil, fmt.Errorf("payment %s not found", id)
	}
	cp := *f.payment
	return &cp, nil
}

type savedCard struct {
	tenantID, customerID           uuid.UUID
	provider, ref, brand, lastFour string
}

type fakeCardSaver struct{ saved []savedCard }

func (f *fakeCardSaver) SetGatewayPaymentMethod(_ context.Context, tenantID, customerID uuid.UUID, provider, ref, brand, last4 string) error {
	f.saved = append(f.saved, savedCard{tenantID, customerID, provider, ref, brand, last4})
	return nil
}

// moyasarInvoiceRepo serves the unscoped webhook lookup plus the reads and
// writes MarkInvoicePaid makes, recording the tenant each settlement ran under.
type moyasarInvoiceRepo struct {
	porttest.UnimplementedInvoiceRepository
	inv            *domain.Invoice
	markPaidCalls  int
	markPaidTenant uuid.UUID
	getByIDTenant  any
	setPaymentIDs  []string
}

func (r *moyasarInvoiceRepo) GetByIDPublic(_ context.Context, id uuid.UUID) (*domain.Invoice, error) {
	if r.inv != nil && r.inv.ID == id {
		cp := *r.inv
		return &cp, nil
	}
	return nil, nil
}

func (r *moyasarInvoiceRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Invoice, error) {
	r.getByIDTenant = ctx.Value(domain.TenantIDKey)
	return r.GetByIDPublic(ctx, id)
}

func (r *moyasarInvoiceRepo) MarkPaid(_ context.Context, tenantID, _ uuid.UUID, paidAt time.Time) (bool, error) {
	r.markPaidCalls++
	r.markPaidTenant = tenantID
	if r.inv.Status == domain.InvoiceStatusPaid {
		return false, nil
	}
	r.inv.Status = domain.InvoiceStatusPaid
	r.inv.PaidAt = &paidAt
	r.inv.AmountPaid = r.inv.Total
	return true, nil
}

func (r *moyasarInvoiceRepo) SetGatewayPaymentID(_ context.Context, _, _ uuid.UUID, id string) error {
	r.setPaymentIDs = append(r.setPaymentIDs, id)
	r.inv.GatewayPaymentID = id
	return nil
}

type moyasarFixture struct {
	h       *WebhookHandler
	repo    *moyasarInvoiceRepo
	fetcher *fakeMoyasarFetcher
	cards   *fakeCardSaver
	dedup   *mapDedup
	inv     *domain.Invoice
	payment *port.GatewayPayment
}

// newMoyasarFixture wires a handler whose fetcher returns a paid payment bound
// to an open 100.00 SAR invoice.
func newMoyasarFixture(t *testing.T) *moyasarFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	inv := &domain.Invoice{
		ID: uuid.New(), TenantID: uuid.New(), CustomerID: uuid.New(),
		Status: domain.InvoiceStatusOpen, Total: 10000, Currency: "SAR",
	}
	payment := &port.GatewayPayment{
		ID: uuid.NewString(), Status: "paid", Amount: 10000, Currency: "SAR",
		InvoiceID: inv.ID.String(), CardToken: "token_abc", CardBrand: "mada", CardLast4: "1010",
	}
	repo := &moyasarInvoiceRepo{inv: inv}
	f := &moyasarFixture{
		repo:    repo,
		fetcher: &fakeMoyasarFetcher{payment: payment},
		cards:   &fakeCardSaver{},
		dedup:   &mapDedup{},
		inv:     inv,
		payment: payment,
	}
	f.h = &WebhookHandler{
		logger:       slog.Default(),
		invoiceRepo:  repo,
		inboundDedup: f.dedup,
		subService:   service.NewSubscriptionService(nil, repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil),
	}
	f.h.SetMoyasar(f.fetcher, f.cards, moyasarTestSecret)
	return f
}

func moyasarBody(eventID, typ, secret, paymentID string) []byte {
	return fmt.Appendf(nil,
		`{"id":%q,"type":%q,"created_at":"2026-10-21T10:00:00+00:00","secret_token":%q,"account_name":"Test","live":false,"data":{"id":%q,"status":"paid","amount":10000}}`,
		eventID, typ, secret, paymentID)
}

func postMoyasar(h *WebhookHandler, body []byte) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/webhooks/moyasar", bytes.NewReader(body))
	h.HandleMoyasar(c)
	return w
}

func TestMoyasarWebhook_NoSecretFailsClosed(t *testing.T) {
	f := newMoyasarFixture(t)
	f.h.SetMoyasar(f.fetcher, f.cards, "")
	w := postMoyasar(f.h, moyasarBody("ev1", "payment_paid", "", f.payment.ID))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no secret: got %d want 503", w.Code)
	}
	if f.fetcher.calls != 0 || f.repo.markPaidCalls != 0 {
		t.Fatalf("unconfigured webhook must not fetch or settle")
	}
}

func TestMoyasarWebhook_WrongSecretRejected(t *testing.T) {
	for name, secret := range map[string]string{"wrong": "not-the-secret", "missing": ""} {
		t.Run(name, func(t *testing.T) {
			f := newMoyasarFixture(t)
			w := postMoyasar(f.h, moyasarBody("ev1", "payment_paid", secret, f.payment.ID))
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("got %d want 401", w.Code)
			}
			if f.fetcher.calls != 0 {
				t.Fatalf("fetcher called %d times on a rejected webhook", f.fetcher.calls)
			}
			if f.dedup.marks != 0 || f.repo.markPaidCalls != 0 {
				t.Fatalf("rejected webhook must not be recorded or settle")
			}
		})
	}
}

func TestMoyasarWebhook_InvalidJSON(t *testing.T) {
	f := newMoyasarFixture(t)
	if got := postMoyasar(f.h, []byte(`{not json`)).Code; got != http.StatusBadRequest {
		t.Fatalf("bad JSON: got %d want 400", got)
	}
}

func TestMoyasarWebhook_PaymentPaidSettles(t *testing.T) {
	f := newMoyasarFixture(t)
	w := postMoyasar(f.h, moyasarBody("ev_paid", "payment_paid", moyasarTestSecret, f.payment.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("payment_paid: got %d want 200 (body %s)", w.Code, w.Body.String())
	}
	if f.fetcher.calls != 1 {
		t.Fatalf("fetch calls = %d, want 1", f.fetcher.calls)
	}
	if f.repo.markPaidCalls != 1 {
		t.Fatalf("MarkPaid calls = %d, want 1", f.repo.markPaidCalls)
	}
	if f.repo.markPaidTenant != f.inv.TenantID {
		t.Fatalf("MarkPaid tenant = %s, want invoice tenant %s", f.repo.markPaidTenant, f.inv.TenantID)
	}
	if got, ok := f.repo.getByIDTenant.(uuid.UUID); !ok || got != f.inv.TenantID {
		t.Fatalf("MarkInvoicePaid ran without the invoice's tenant in ctx: %v", f.repo.getByIDTenant)
	}
	if f.inv.Status != domain.InvoiceStatusPaid {
		t.Fatalf("invoice status = %s, want paid", f.inv.Status)
	}
	if len(f.repo.setPaymentIDs) != 1 || f.repo.setPaymentIDs[0] != f.payment.ID {
		t.Fatalf("gateway payment ids recorded = %v, want [%s]", f.repo.setPaymentIDs, f.payment.ID)
	}
	want := savedCard{f.inv.TenantID, f.inv.CustomerID, domain.SavedMethodMoyasar, "token_abc", "mada", "1010"}
	if len(f.cards.saved) != 1 || f.cards.saved[0] != want {
		t.Fatalf("saved cards = %+v, want [%+v]", f.cards.saved, want)
	}
	if f.dedup.marks != 1 {
		t.Fatalf("dedup marks = %d, want 1", f.dedup.marks)
	}
}

func TestMoyasarWebhook_DuplicateDeliverySettlesOnce(t *testing.T) {
	f := newMoyasarFixture(t)
	body := moyasarBody("ev_dup", "payment_paid", moyasarTestSecret, f.payment.ID)
	for i := range 2 {
		if w := postMoyasar(f.h, body); w.Code != http.StatusOK {
			t.Fatalf("delivery %d: got %d want 200", i+1, w.Code)
		}
	}
	if f.fetcher.calls != 1 || f.repo.markPaidCalls != 1 {
		t.Fatalf("duplicate reprocessed: fetch=%d markPaid=%d, want 1/1", f.fetcher.calls, f.repo.markPaidCalls)
	}
	if len(f.repo.setPaymentIDs) != 1 || len(f.cards.saved) != 1 {
		t.Fatalf("duplicate re-recorded: payment ids=%v cards=%d", f.repo.setPaymentIDs, len(f.cards.saved))
	}
}

func TestMoyasarWebhook_RedeliveryNewEventIDIsIdempotent(t *testing.T) {
	f := newMoyasarFixture(t)
	postMoyasar(f.h, moyasarBody("ev_a", "payment_paid", moyasarTestSecret, f.payment.ID))
	if w := postMoyasar(f.h, moyasarBody("ev_b", "payment_paid", moyasarTestSecret, f.payment.ID)); w.Code != http.StatusOK {
		t.Fatalf("second event: got %d want 200 (body %s)", w.Code, w.Body.String())
	}
	// The paid guard stops a second transition; the already-recorded payment
	// id is not rewritten.
	if f.inv.Status != domain.InvoiceStatusPaid || len(f.repo.setPaymentIDs) != 1 {
		t.Fatalf("status=%s payment ids=%v", f.inv.Status, f.repo.setPaymentIDs)
	}
}

func TestMoyasarWebhook_PaymentFailedDoesNotSettle(t *testing.T) {
	f := newMoyasarFixture(t)
	f.payment.Status = "failed"
	w := postMoyasar(f.h, moyasarBody("ev_fail", "payment_failed", moyasarTestSecret, f.payment.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("payment_failed: got %d want 200", w.Code)
	}
	if f.repo.markPaidCalls != 0 || f.inv.Status != domain.InvoiceStatusOpen {
		t.Fatalf("payment_failed settled the invoice")
	}
	if f.dedup.marks != 1 {
		t.Fatalf("dedup marks = %d, want 1", f.dedup.marks)
	}
}

func TestMoyasarWebhook_UnhandledEventsAcked(t *testing.T) {
	for _, typ := range []string{"payment_refunded", "payment_voided", "card_auth_failed"} {
		t.Run(typ, func(t *testing.T) {
			f := newMoyasarFixture(t)
			if w := postMoyasar(f.h, moyasarBody("ev_"+typ, typ, moyasarTestSecret, f.payment.ID)); w.Code != http.StatusOK {
				t.Fatalf("got %d want 200", w.Code)
			}
			if f.repo.markPaidCalls != 0 || f.fetcher.calls != 0 {
				t.Fatalf("%s must not fetch or settle", typ)
			}
		})
	}
}

// TestMoyasarWebhook_UntrustedPaymentDoesNotSettle: the body always claims a
// paid payment; settlement depends only on the fetched read-back.
func TestMoyasarWebhook_UntrustedPaymentDoesNotSettle(t *testing.T) {
	cases := map[string]func(f *moyasarFixture){
		"fetched status failed":    func(f *moyasarFixture) { f.payment.Status = "failed" },
		"fetched status initiated": func(f *moyasarFixture) { f.payment.Status = "initiated" },
		"bound to another invoice": func(f *moyasarFixture) { f.payment.InvoiceID = uuid.NewString() },
		"no invoice metadata":      func(f *moyasarFixture) { f.payment.InvoiceID = "" },
		"amount short":             func(f *moyasarFixture) { f.payment.Amount = 9999 },
		"currency mismatch":        func(f *moyasarFixture) { f.payment.Currency = "USD" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newMoyasarFixture(t)
			mutate(f)
			w := postMoyasar(f.h, moyasarBody("ev_forged", "payment_paid", moyasarTestSecret, f.payment.ID))
			if w.Code != http.StatusOK {
				t.Fatalf("got %d want 200 ack (body %s)", w.Code, w.Body.String())
			}
			if f.repo.markPaidCalls != 0 || f.inv.Status != domain.InvoiceStatusOpen {
				t.Fatalf("untrusted payment settled the invoice")
			}
			if len(f.repo.setPaymentIDs) != 0 || len(f.cards.saved) != 0 {
				t.Fatalf("untrusted payment recorded: ids=%v cards=%d", f.repo.setPaymentIDs, len(f.cards.saved))
			}
		})
	}
}

func TestMoyasarWebhook_FetchErrorIsRetried(t *testing.T) {
	f := newMoyasarFixture(t)
	f.fetcher.err = errors.New("moyasar: 503")
	w := postMoyasar(f.h, moyasarBody("ev_retry", "payment_paid", moyasarTestSecret, f.payment.ID))
	if w.Code < 500 {
		t.Fatalf("fetch error: got %d want 5xx so Moyasar redelivers", w.Code)
	}
	if f.dedup.marks != 0 {
		t.Fatalf("fetch error marked the event processed")
	}
	if f.repo.markPaidCalls != 0 {
		t.Fatalf("fetch error settled the invoice")
	}

	// The redelivery, once the gateway recovers, settles.
	f.fetcher.err = nil
	if w := postMoyasar(f.h, moyasarBody("ev_retry", "payment_paid", moyasarTestSecret, f.payment.ID)); w.Code != http.StatusOK {
		t.Fatalf("redelivery: got %d want 200", w.Code)
	}
	if f.repo.markPaidCalls != 1 || f.dedup.marks != 1 {
		t.Fatalf("redelivery: markPaid=%d marks=%d, want 1/1", f.repo.markPaidCalls, f.dedup.marks)
	}
}

func TestMoyasarWebhook_DedupStoreDownFailsClosed(t *testing.T) {
	f := newMoyasarFixture(t)
	f.h.inboundDedup = &errDedup{}
	w := postMoyasar(f.h, moyasarBody("ev_down", "payment_paid", moyasarTestSecret, f.payment.ID))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("dedup down: got %d want 503", w.Code)
	}
	if f.fetcher.calls != 0 || f.repo.markPaidCalls != 0 {
		t.Fatalf("dedup outage must not process the event")
	}
}

func TestMoyasarWebhook_NoFetcherIgnored(t *testing.T) {
	f := newMoyasarFixture(t)
	f.h.SetMoyasar(nil, nil, moyasarTestSecret)
	w := postMoyasar(f.h, moyasarBody("ev_nf", "payment_paid", moyasarTestSecret, f.payment.ID))
	if w.Code != http.StatusOK || f.repo.markPaidCalls != 0 {
		t.Fatalf("no fetcher: got %d markPaid=%d, want 200/0", w.Code, f.repo.markPaidCalls)
	}
}
