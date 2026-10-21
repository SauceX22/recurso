package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/service"
)

const moyasarTestSecret = "mysr_whsec_test"

type moyasarWebhookFixture struct {
	*moyasarHandlerFixture
	h     *WebhookHandler
	dedup *mapDedup
	logs  *bytes.Buffer
}

func newMoyasarFixture(t *testing.T) *moyasarWebhookFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	f := &moyasarWebhookFixture{moyasarHandlerFixture: newMoyasarHandlerFixture(), dedup: &mapDedup{}, logs: &bytes.Buffer{}}
	f.h = &WebhookHandler{
		logger:       slog.New(slog.NewTextHandler(f.logs, nil)),
		invoiceRepo:  f.invoices,
		inboundDedup: f.dedup,
	}
	f.h.SetMoyasar(f.reader, f.settler, moyasarTestSecret)
	return f
}

func moyasarBody(eventID, typ, secret, paymentID string) []byte {
	return fmt.Appendf(nil,
		`{"id":%q,"type":%q,"created_at":"2026-10-21T10:00:00+00:00","secret_token":%q,"account_name":"Test","live":false,"data":{"id":%q,"status":"paid","amount":10000}}`,
		eventID, typ, secret, paymentID)
}

func (f *moyasarWebhookFixture) post(body []byte) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/webhooks/moyasar", bytes.NewReader(body))
	f.h.HandleMoyasar(c)
	return w
}

func (f *moyasarWebhookFixture) deliver(eventID, typ string) *httptest.ResponseRecorder {
	return f.post(moyasarBody(eventID, typ, moyasarTestSecret, f.payment.ID))
}

func ackReason(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct{ Status, Reason string }
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode ack %q: %v", w.Body.String(), err)
	}
	return out.Reason
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Error struct{ Code string } `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode error %q: %v", w.Body.String(), err)
	}
	return out.Error.Code
}

func TestMoyasarWebhook_UnconfiguredFailsClosed(t *testing.T) {
	cases := map[string]func(f *moyasarWebhookFixture){
		"no secret":     func(f *moyasarWebhookFixture) { f.h.SetMoyasar(f.reader, f.settler, "") },
		"no read-back":  func(f *moyasarWebhookFixture) { f.h.SetMoyasar(nil, f.settler, moyasarTestSecret) },
		"no settlement": func(f *moyasarWebhookFixture) { f.h.SetMoyasar(f.reader, nil, moyasarTestSecret) },
	}
	for name, unset := range cases {
		t.Run(name, func(t *testing.T) {
			f := newMoyasarFixture(t)
			unset(f)
			if w := f.deliver("ev1", "payment_paid"); w.Code != http.StatusServiceUnavailable {
				t.Fatalf("got %d want 503", w.Code)
			}
			if len(f.reader.gotIDs) != 0 || len(f.settler.settled) != 0 || f.dedup.marks != 0 {
				t.Fatal("unconfigured webhook fetched, settled or was recorded")
			}
		})
	}
}

func TestMoyasarWebhook_WrongSecretRejected(t *testing.T) {
	for _, tc := range []struct{ name, secret string }{{"wrong", "not-the-secret"}, {"missing", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMoyasarFixture(t)
			if w := f.post(moyasarBody("ev1", "payment_paid", tc.secret, f.payment.ID)); w.Code != http.StatusUnauthorized {
				t.Fatalf("got %d want 401", w.Code)
			}
			if len(f.reader.gotIDs) != 0 || f.dedup.marks != 0 || len(f.settler.settled) != 0 {
				t.Fatal("rejected webhook fetched, was recorded or settled")
			}
		})
	}
}

func TestMoyasarWebhook_InvalidJSON(t *testing.T) {
	f := newMoyasarFixture(t)
	if got := f.post([]byte(`{not json`)).Code; got != http.StatusBadRequest {
		t.Fatalf("bad JSON: got %d want 400", got)
	}
}

func TestMoyasarWebhook_OversizedBodyRejected(t *testing.T) {
	f := newMoyasarFixture(t)
	body := moyasarBody("ev_big", "payment_paid", moyasarTestSecret, f.payment.ID)
	body = append(body[:len(body)-1], []byte(`,"pad":"`+strings.Repeat("x", maxMoyasarWebhookBytes)+`"}`)...)
	if w := f.post(body); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d want 413", w.Code)
	}
	if len(f.reader.gotIDs) != 0 {
		t.Fatal("oversized body was processed")
	}
}

func TestMoyasarWebhook_PaymentPaidSettles(t *testing.T) {
	f := newMoyasarFixture(t)
	w := f.deliver("ev_paid", "payment_paid")
	if w.Code != http.StatusOK {
		t.Fatalf("payment_paid: got %d want 200 (body %s)", w.Code, w.Body.String())
	}
	if len(f.reader.gotIDs) != 1 || f.reader.gotIDs[0] != f.payment.ID {
		t.Fatalf("fetched %v, want [%s]", f.reader.gotIDs, f.payment.ID)
	}
	if !f.settledOnce() {
		t.Fatalf("settled %+v, want the payment once", f.settler.settled)
	}
	if f.dedup.marks != 1 {
		t.Fatalf("dedup marks = %d, want 1", f.dedup.marks)
	}
}

func TestMoyasarWebhook_LowerCaseCurrencySettles(t *testing.T) {
	f := newMoyasarFixture(t)
	f.payment.Currency = "sar"
	if w := f.deliver("ev_lc", "payment_paid"); w.Code != http.StatusOK || !f.settledOnce() {
		t.Fatalf("got %d settled %+v, want settled", w.Code, f.settler.settled)
	}
}

func TestMoyasarWebhook_DuplicateDeliverySettlesOnce(t *testing.T) {
	f := newMoyasarFixture(t)
	for i := range 2 {
		if w := f.deliver("ev_dup", "payment_paid"); w.Code != http.StatusOK {
			t.Fatalf("delivery %d: got %d want 200", i+1, w.Code)
		}
	}
	if len(f.reader.gotIDs) != 1 || !f.settledOnce() {
		t.Fatalf("duplicate reprocessed: fetch=%d settled=%d", len(f.reader.gotIDs), len(f.settler.settled))
	}
}

// A settlement that loses to another payment (or a redelivery under a new
// event id) is still a handled delivery.
func TestMoyasarWebhook_NoTransitionIsAcked(t *testing.T) {
	f := newMoyasarFixture(t)
	f.settler.transitioned = false
	if w := f.deliver("ev_b", "payment_paid"); w.Code != http.StatusOK || f.dedup.marks != 1 {
		t.Fatalf("got %d marks %d, want 200 and recorded", w.Code, f.dedup.marks)
	}
}

// A paid invoice is still settled again, so a payment id lost between the
// two writes gets recorded on redelivery.
func TestMoyasarWebhook_PaidInvoiceStillSettles(t *testing.T) {
	f := newMoyasarFixture(t)
	f.inv.Status = domain.InvoiceStatusPaid
	if w := f.deliver("ev_r", "payment_paid"); w.Code != http.StatusOK || !f.settledOnce() {
		t.Fatalf("got %d settled %+v, want one settle", w.Code, f.settler.settled)
	}
}

func TestMoyasarWebhook_InitiatedPaymentIsRedelivered(t *testing.T) {
	f := newMoyasarFixture(t)
	f.payment.Status = "initiated"
	w := f.deliver("ev_i", "payment_paid")
	if w.Code != http.StatusServiceUnavailable || f.dedup.marks != 0 || len(f.settler.settled) != 0 {
		t.Fatalf("got %d marks=%d settled=%d, want 503, unrecorded, unsettled", w.Code, f.dedup.marks, len(f.settler.settled))
	}
	f.payment.Status = "paid"
	if w := f.deliver("ev_i", "payment_paid"); w.Code != http.StatusOK || !f.settledOnce() {
		t.Fatalf("redelivery got %d settled %+v, want settled", w.Code, f.settler.settled)
	}
}

// The body always claims a paid payment; settlement depends only on the
// fetched read-back, and the ack names the guard that refused it.
func TestMoyasarWebhook_UntrustedPaymentDoesNotSettle(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(f *moyasarWebhookFixture)
		reason string
	}{
		{"fetched status failed", func(f *moyasarWebhookFixture) { f.payment.Status = "failed" }, "payment not paid"},
		{"unknown invoice", func(f *moyasarWebhookFixture) { f.payment.InvoiceID = uuid.NewString() }, "unknown invoice_id"},
		{"no invoice metadata", func(f *moyasarWebhookFixture) { f.payment.InvoiceID = "" }, "no invoice_id"},
		{"amount short", func(f *moyasarWebhookFixture) { f.payment.Amount = 7999 }, service.ErrPaymentShortAmount.Error()},
		{"currency mismatch", func(f *moyasarWebhookFixture) { f.payment.Currency = "USD" }, service.ErrPaymentWrongCurrency.Error()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newMoyasarFixture(t)
			tc.mutate(f)
			w := f.deliver("ev_forged", "payment_paid")
			if w.Code != http.StatusOK {
				t.Fatalf("got %d want 200 ack (body %s)", w.Code, w.Body.String())
			}
			if got := ackReason(t, w); got != tc.reason {
				t.Fatalf("reason = %q, want %q", got, tc.reason)
			}
			if len(f.settler.settled) != 0 {
				t.Fatal("untrusted payment settled")
			}
		})
	}
}

func TestMoyasarWebhook_TransientFailuresAreRetried(t *testing.T) {
	cases := map[string]struct {
		breakIt func(f *moyasarWebhookFixture)
		status  int
	}{
		"gateway fetch fails":    {func(f *moyasarWebhookFixture) { f.reader.err = errors.New("moyasar: 503") }, http.StatusBadGateway},
		"invoice lookup fails":   {func(f *moyasarWebhookFixture) { f.invoices.loadErr = errors.New("db down") }, http.StatusInternalServerError},
		"settlement write fails": {func(f *moyasarWebhookFixture) { f.settler.err = errors.New("db down") }, http.StatusInternalServerError},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newMoyasarFixture(t)
			tc.breakIt(f)
			if w := f.deliver("ev_retry", "payment_paid"); w.Code != tc.status {
				t.Fatalf("got %d want %d", w.Code, tc.status)
			}
			if f.dedup.marks != 0 {
				t.Fatalf("failed delivery recorded (marks=%d)", f.dedup.marks)
			}
			f.reader.err, f.invoices.loadErr, f.settler.err = nil, nil, nil
			f.settler.settled = nil
			if w := f.deliver("ev_retry", "payment_paid"); w.Code != http.StatusOK || !f.settledOnce() || f.dedup.marks != 1 {
				t.Fatalf("redelivery got %d settled %+v marks %d, want settled and recorded", w.Code, f.settler.settled, f.dedup.marks)
			}
		})
	}
}

func TestMoyasarWebhook_FetchErrorCode(t *testing.T) {
	f := newMoyasarFixture(t)
	f.reader.err = errors.New("timeout")
	if got := errorCode(t, f.deliver("ev_x", "payment_paid")); got != codeGatewayUnavailable {
		t.Fatalf("code = %q, want %q", got, codeGatewayUnavailable)
	}
}

func TestMoyasarWebhook_DedupStoreDownFailsClosed(t *testing.T) {
	f := newMoyasarFixture(t)
	f.h.inboundDedup = &errDedup{}
	if w := f.deliver("ev_down", "payment_paid"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("dedup down: got %d want 503", w.Code)
	}
	if len(f.reader.gotIDs) != 0 || len(f.settler.settled) != 0 {
		t.Fatal("dedup outage processed the event")
	}
}

func TestMoyasarWebhook_PaymentFailedAcked(t *testing.T) {
	f := newMoyasarFixture(t)
	if w := f.deliver("ev_fail", "payment_failed"); w.Code != http.StatusOK {
		t.Fatalf("got %d want 200", w.Code)
	}
	if len(f.settler.settled) != 0 || f.dedup.marks != 1 {
		t.Fatalf("payment_failed: settled=%d marks=%d", len(f.settler.settled), f.dedup.marks)
	}
}

func TestMoyasarWebhook_ReversalOfSettledPaymentIsFlagged(t *testing.T) {
	for _, typ := range []string{"payment_refunded", "payment_voided"} {
		t.Run(typ, func(t *testing.T) {
			f := newMoyasarFixture(t)
			f.inv.Status = domain.InvoiceStatusPaid
			f.payment.RefundedAmount = 8000
			if w := f.deliver("ev_"+typ, typ); w.Code != http.StatusOK {
				t.Fatalf("got %d want 200", w.Code)
			}
			if len(f.settler.settled) != 0 || len(f.reader.gotIDs) != 1 {
				t.Fatalf("settled=%d fetches=%d, want 0/1", len(f.settler.settled), len(f.reader.gotIDs))
			}
			logs := f.logs.String()
			if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "refunded_amount=8000") || !strings.Contains(logs, f.inv.ID.String()) {
				t.Fatalf("reversal not flagged with invoice and amount; logs:\n%s", logs)
			}
		})
	}
}

func TestMoyasarWebhook_ReversalOfOpenInvoiceIsNotFlagged(t *testing.T) {
	f := newMoyasarFixture(t)
	if w := f.deliver("ev_v", "payment_voided"); w.Code != http.StatusOK {
		t.Fatalf("got %d want 200", w.Code)
	}
	if strings.Contains(f.logs.String(), "level=WARN") {
		t.Fatalf("void of an unsettled payment flagged; logs:\n%s", f.logs.String())
	}
}

func TestMoyasarWebhook_UnhandledEventsIgnored(t *testing.T) {
	f := newMoyasarFixture(t)
	if w := f.deliver("ev_cauth", "card_auth_failed"); w.Code != http.StatusOK {
		t.Fatalf("got %d want 200", w.Code)
	}
	if len(f.settler.settled) != 0 || len(f.reader.gotIDs) != 0 {
		t.Fatal("unhandled event fetched or settled")
	}
}

// unknownRefundNotes serves one refund note of unknown outcome and records
// how it is resolved. Every other repository method is unused here.
type unknownRefundNotes struct {
	service.CreditNoteRepository
	notes   []*domain.CreditNote
	listErr error
	updates map[uuid.UUID]domain.CreditNoteRefundStatus
}

func (r *unknownRefundNotes) List(context.Context, uuid.UUID, domain.CreditNoteFilter) ([]*domain.CreditNote, error) {
	return r.notes, r.listErr
}

func (r *unknownRefundNotes) UpdateRefund(_ context.Context, id uuid.UUID, status domain.CreditNoteRefundStatus, _ *string, _ string) error {
	r.updates[id] = status
	return nil
}

func (r *unknownRefundNotes) SumActiveRefundsForInvoice(context.Context, uuid.UUID) (int64, error) {
	return 0, nil
}

// M3: Moyasar's payment_refunded webhook carries the payment's refunded
// total, which resolves a refund whose outcome was unknown when it was issued.
func TestMoyasarWebhook_RefundResolvesUnknownRefund(t *testing.T) {
	f := newMoyasarFixture(t)
	f.inv.Status = domain.InvoiceStatusPaid
	f.inv.GatewayPaymentID = f.payment.ID
	f.payment.RefundedAmount = 3000
	refundID := f.payment.ID + ":unknown:" + uuid.NewString()
	note := &domain.CreditNote{ID: uuid.New(), TenantID: f.inv.TenantID, CustomerID: f.inv.CustomerID, InvoiceID: &f.inv.ID,
		Type: domain.CreditNoteTypeRefund, Status: domain.CreditNoteStatusIssued, Amount: 3000,
		RefundStatus: domain.RefundStatusPending, RefundID: &refundID}
	notes := &unknownRefundNotes{notes: []*domain.CreditNote{note}, updates: map[uuid.UUID]domain.CreditNoteRefundStatus{}}
	f.h.SetCreditNoteService(service.NewCreditNoteService(notes, nil, nil, nil))

	if w := f.deliver("ev_refunded", "payment_refunded"); w.Code != http.StatusOK {
		t.Fatalf("got %d want 200 (body %s)", w.Code, w.Body.String())
	}
	if notes.updates[note.ID] != domain.RefundStatusProcessed {
		t.Fatalf("unknown refund resolved to %q, want processed", notes.updates[note.ID])
	}

	// A failure to read the notes is retried: the event is not recorded.
	notes.listErr = errors.New("db down")
	if w := f.deliver("ev_refunded_2", "payment_refunded"); w.Code != http.StatusInternalServerError || f.dedup.marks != 1 {
		t.Fatalf("got %d marks %d, want 500 and the event left unprocessed", w.Code, f.dedup.marks)
	}
}

// A paid payment for a void or draft invoice is refused by settlement (and
// flagged there for a refund); redelivering it changes nothing, so it is
// acknowledged.
func TestMoyasarWebhook_NonPayableInvoiceIsAcked(t *testing.T) {
	f := newMoyasarFixture(t)
	f.settler.err = service.ErrInvoiceNotPayable
	w := f.deliver("ev_void", "payment_paid")
	if w.Code != http.StatusOK || ackReason(t, w) != "invoice not payable" || f.dedup.marks != 1 {
		t.Fatalf("got %d body %s marks %d, want 200 ignored \"invoice not payable\" and recorded", w.Code, w.Body.String(), f.dedup.marks)
	}
}
