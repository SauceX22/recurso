package handler

import (
	"bytes"
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
	*moyasarSettlementFixture
	h     *WebhookHandler
	dedup *mapDedup
	logs  *bytes.Buffer
}

func newMoyasarFixture(t *testing.T) *moyasarWebhookFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	f := &moyasarWebhookFixture{moyasarSettlementFixture: newMoyasarSettlementFixture(), dedup: &mapDedup{}, logs: &bytes.Buffer{}}
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
			if len(f.reader.gotIDs) != 0 || f.invoices.markPaidCalls != 0 || f.dedup.marks != 0 {
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
			if len(f.reader.gotIDs) != 0 || f.dedup.marks != 0 || f.invoices.markPaidCalls != 0 {
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
	if f.invoices.markPaidCalls != 1 || f.invoices.markPaidTenant != f.inv.TenantID {
		t.Fatalf("MarkPaid calls=%d tenant=%s, want 1 under %s", f.invoices.markPaidCalls, f.invoices.markPaidTenant, f.inv.TenantID)
	}
	if f.invoices.getByIDTenant != f.inv.TenantID {
		t.Fatalf("MarkInvoicePaid ran under tenant %v, want %s", f.invoices.getByIDTenant, f.inv.TenantID)
	}
	if f.inv.Status != domain.InvoiceStatusPaid || f.inv.GatewayPaymentID != f.payment.ID || f.invoices.claimTenant != f.inv.TenantID {
		t.Fatalf("invoice status=%s payment=%q claim tenant=%s", f.inv.Status, f.inv.GatewayPaymentID, f.invoices.claimTenant)
	}
	if len(f.cards.saved) != 1 || f.cards.saved[0] != f.wantSavedCard() {
		t.Fatalf("saved cards = %+v, want [%+v]", f.cards.saved, f.wantSavedCard())
	}
	if len(f.dunning.recovered) != 1 || f.dunning.recovered[0] != f.inv.ID || len(f.dunning.outcomes) != 1 || f.dunning.outcomes[0].Outcome != "success" {
		t.Fatalf("dunning recovery = %v / %+v, want one success for %s", f.dunning.recovered, f.dunning.outcomes, f.inv.ID)
	}
	if f.dedup.marks != 1 {
		t.Fatalf("dedup marks = %d, want 1", f.dedup.marks)
	}
}

func TestMoyasarWebhook_LowerCaseCurrencySettles(t *testing.T) {
	f := newMoyasarFixture(t)
	f.payment.Currency = "sar"
	if w := f.deliver("ev_lc", "payment_paid"); w.Code != http.StatusOK || f.inv.Status != domain.InvoiceStatusPaid {
		t.Fatalf("got %d status %s, want settled", w.Code, f.inv.Status)
	}
}

func TestMoyasarWebhook_DuplicateDeliverySettlesOnce(t *testing.T) {
	f := newMoyasarFixture(t)
	for i := range 2 {
		if w := f.deliver("ev_dup", "payment_paid"); w.Code != http.StatusOK {
			t.Fatalf("delivery %d: got %d want 200", i+1, w.Code)
		}
	}
	if len(f.reader.gotIDs) != 1 || f.invoices.markPaidCalls != 1 || len(f.cards.saved) != 1 {
		t.Fatalf("duplicate reprocessed: fetch=%d markPaid=%d cards=%d", len(f.reader.gotIDs), f.invoices.markPaidCalls, len(f.cards.saved))
	}
}

// A redelivery under a new event id runs again but must not count the
// recovery twice or move the recorded payment.
func TestMoyasarWebhook_RedeliveryNewEventIDIsIdempotent(t *testing.T) {
	f := newMoyasarFixture(t)
	f.deliver("ev_a", "payment_paid")
	if w := f.deliver("ev_b", "payment_paid"); w.Code != http.StatusOK {
		t.Fatalf("second event: got %d want 200 (body %s)", w.Code, w.Body.String())
	}
	if f.inv.GatewayPaymentID != f.payment.ID {
		t.Fatalf("recorded payment = %q, want %s", f.inv.GatewayPaymentID, f.payment.ID)
	}
	if len(f.dunning.recovered) != 1 || len(f.dunning.outcomes) != 1 {
		t.Fatalf("dunning recovery counted %d/%d times, want once", len(f.dunning.recovered), len(f.dunning.outcomes))
	}
}

func TestMoyasarWebhook_SecondPaymentDoesNotOverwrite(t *testing.T) {
	f := newMoyasarFixture(t)
	f.inv.Status = domain.InvoiceStatusPaid
	f.inv.GatewayPaymentID = "pay-A"
	if w := f.deliver("ev_b", "payment_paid"); w.Code != http.StatusOK {
		t.Fatalf("got %d want 200 (body %s)", w.Code, w.Body.String())
	}
	if f.inv.GatewayPaymentID != "pay-A" || len(f.cards.saved) != 0 || len(f.dunning.outcomes) != 0 {
		t.Fatalf("second payment overwrote: payment=%q cards=%d outcomes=%d", f.inv.GatewayPaymentID, len(f.cards.saved), len(f.dunning.outcomes))
	}
}

// An invoice settled before its payment id was recorded (a crash between the
// two writes) still records it on redelivery.
func TestMoyasarWebhook_PaidInvoiceWithoutRecordedPaymentRecordsIt(t *testing.T) {
	f := newMoyasarFixture(t)
	f.inv.Status = domain.InvoiceStatusPaid
	if w := f.deliver("ev_r", "payment_paid"); w.Code != http.StatusOK {
		t.Fatalf("got %d want 200", w.Code)
	}
	if f.inv.GatewayPaymentID != f.payment.ID || len(f.cards.saved) != 1 {
		t.Fatalf("payment=%q cards=%d, want recorded and saved", f.inv.GatewayPaymentID, len(f.cards.saved))
	}
	if len(f.dunning.outcomes) != 0 {
		t.Fatal("dunning recovery recorded without the paid transition")
	}
}

func TestMoyasarWebhook_InitiatedPaymentIsRedelivered(t *testing.T) {
	f := newMoyasarFixture(t)
	f.payment.Status = "initiated"
	w := f.deliver("ev_i", "payment_paid")
	if w.Code != http.StatusServiceUnavailable || f.dedup.marks != 0 || f.invoices.markPaidCalls != 0 {
		t.Fatalf("got %d marks=%d markPaid=%d, want 503, unrecorded, unsettled", w.Code, f.dedup.marks, f.invoices.markPaidCalls)
	}
	f.payment.Status = "paid"
	if w := f.deliver("ev_i", "payment_paid"); w.Code != http.StatusOK || f.inv.Status != domain.InvoiceStatusPaid {
		t.Fatalf("redelivery got %d status %s, want settled", w.Code, f.inv.Status)
	}
}

func TestMoyasarWebhook_OneOffInvoiceSettlesWithoutSavingCard(t *testing.T) {
	f := newMoyasarFixture(t)
	f.inv.SubscriptionID = nil
	if w := f.deliver("ev_o", "payment_paid"); w.Code != http.StatusOK || f.inv.Status != domain.InvoiceStatusPaid {
		t.Fatalf("got %d status %s, want settled", w.Code, f.inv.Status)
	}
	if len(f.cards.saved) != 0 {
		t.Fatalf("one-off invoice saved card %+v", f.cards.saved)
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
			if f.invoices.markPaidCalls != 0 || len(f.invoices.claims) != 0 || len(f.cards.saved) != 0 {
				t.Fatal("untrusted payment had effects")
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
		"settlement write fails": {func(f *moyasarWebhookFixture) { f.invoices.markPaidErr = errors.New("db down") }, http.StatusInternalServerError},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newMoyasarFixture(t)
			tc.breakIt(f)
			if w := f.deliver("ev_retry", "payment_paid"); w.Code != tc.status {
				t.Fatalf("got %d want %d", w.Code, tc.status)
			}
			if f.dedup.marks != 0 || len(f.cards.saved) != 0 {
				t.Fatalf("failed delivery recorded (marks=%d) or saved a card", f.dedup.marks)
			}
			f.reader.err, f.invoices.loadErr, f.invoices.markPaidErr = nil, nil, nil
			if w := f.deliver("ev_retry", "payment_paid"); w.Code != http.StatusOK || f.inv.Status != domain.InvoiceStatusPaid || f.dedup.marks != 1 {
				t.Fatalf("redelivery got %d status %s marks %d, want settled and recorded", w.Code, f.inv.Status, f.dedup.marks)
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
	if len(f.reader.gotIDs) != 0 || f.invoices.markPaidCalls != 0 {
		t.Fatal("dedup outage processed the event")
	}
}

func TestMoyasarWebhook_PaymentFailedAcked(t *testing.T) {
	f := newMoyasarFixture(t)
	if w := f.deliver("ev_fail", "payment_failed"); w.Code != http.StatusOK {
		t.Fatalf("got %d want 200", w.Code)
	}
	if f.invoices.markPaidCalls != 0 || f.inv.Status != domain.InvoiceStatusOpen || f.dedup.marks != 1 {
		t.Fatalf("payment_failed: markPaid=%d status=%s marks=%d", f.invoices.markPaidCalls, f.inv.Status, f.dedup.marks)
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
			if f.invoices.markPaidCalls != 0 || len(f.reader.gotIDs) != 1 {
				t.Fatalf("markPaid=%d fetches=%d, want 0/1", f.invoices.markPaidCalls, len(f.reader.gotIDs))
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
	if f.invoices.markPaidCalls != 0 || len(f.reader.gotIDs) != 0 {
		t.Fatal("unhandled event fetched or settled")
	}
}
