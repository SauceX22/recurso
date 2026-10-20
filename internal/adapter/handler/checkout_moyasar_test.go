package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
	"github.com/recurso-dev/recurso/internal/core/port/porttest"
)

type moyCheckoutGateway struct {
	porttest.UnimplementedPaymentGateway
	gateway      string
	clientSecret string
}

func (g *moyCheckoutGateway) CreateOrder(_ context.Context, amount int64, currency, receipt, _ string) (*port.PaymentOrder, error) {
	return &port.PaymentOrder{ID: "ord_1", Amount: amount, Currency: currency, Receipt: receipt, Gateway: g.gateway, ClientSecret: g.clientSecret}, nil
}

func moyDo(t *testing.T, method, path string, handler gin.HandlerFunc, route string, body any) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Handle(method, route, handler)
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func moyData(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	d, ok := out["data"].(map[string]any)
	if !ok {
		t.Fatalf("response has no data object: %v", out)
	}
	return d
}

func moyErrorCode(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

type moyCheckoutFixture struct {
	*moyasarSettlementFixture
	h *CheckoutHandler
}

// newMoyCheckoutFixture wires a checkout whose order gateway is gw and whose
// Moyasar verify settles through the shared settlement path.
func newMoyCheckoutFixture(gw string) *moyCheckoutFixture {
	f := &moyCheckoutFixture{moyasarSettlementFixture: newMoyasarSettlementFixture()}
	f.h = NewCheckoutHandler(f.invoices, &moyCheckoutGateway{gateway: gw, clientSecret: map[string]string{"stripe": "cs_1"}[gw]}, nil, nil, "pk_stripe")
	f.h.SetMoyasar(f.reader, f.settler, "pk_test_abc")
	return f
}

func (f *moyCheckoutFixture) pay(t *testing.T) (int, map[string]any) {
	return moyDo(t, http.MethodPost, "/checkout/"+f.inv.ID.String()+"/pay", f.h.InitiatePayment, "/checkout/:id/pay", nil)
}

func (f *moyCheckoutFixture) verify(t *testing.T, body any) (int, map[string]any) {
	return moyDo(t, http.MethodPost, "/checkout/"+f.inv.ID.String()+"/moyasar/verify", f.h.MoyasarVerify, "/checkout/:id/moyasar/verify", body)
}

func (f *moyCheckoutFixture) verifyPayment(t *testing.T) (int, map[string]any) {
	return f.verify(t, map[string]string{"payment_id": f.payment.ID})
}

func TestShowCheckout_ReportsAmountDue(t *testing.T) {
	f := newMoyCheckoutFixture("moyasar")
	code, out := moyDo(t, http.MethodGet, "/checkout/"+f.inv.ID.String(), f.h.ShowCheckout, "/checkout/:id", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %v", code, out)
	}
	d := moyData(t, out)
	for k, want := range map[string]float64{"total": 10000, "amount_paid": 1500, "credit_applied": 500, "amount_due": 8000} {
		if d[k] != want {
			t.Errorf("%s = %v, want %v", k, d[k], want)
		}
	}
	if d["display_amount_due"] != "80.00" {
		t.Errorf("display_amount_due = %v, want 80.00", d["display_amount_due"])
	}
}

func TestCheckoutInitiate_MoyasarReturnsFormConfig(t *testing.T) {
	f := newMoyCheckoutFixture("moyasar")
	code, out := f.pay(t)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %v", code, out)
	}
	d := moyData(t, out)
	if d["gateway"] != "moyasar" || d["moyasar_publishable_key"] != "pk_test_abc" || d["currency"] != "SAR" {
		t.Errorf("gateway=%v key=%v currency=%v", d["gateway"], d["moyasar_publishable_key"], d["currency"])
	}
	if d["amount"] != float64(8000) {
		t.Errorf("amount = %v, want the amount due 8000", d["amount"])
	}
	if d["save_card"] != true {
		t.Errorf("save_card = %v, want true for a renewing subscription", d["save_card"])
	}
	if d["description"] != "Invoice INV-42" {
		t.Errorf("description = %v", d["description"])
	}
	if md, _ := d["metadata"].(map[string]any); md["invoice_id"] != f.inv.ID.String() {
		t.Errorf("metadata = %v", d["metadata"])
	}
}

func TestCheckoutInitiate_MoyasarSaveCardOnlyForRenewingSubscriptions(t *testing.T) {
	cases := map[string]func(*moyCheckoutFixture){
		"one-off invoice":       func(f *moyCheckoutFixture) { f.inv.SubscriptionID = nil },
		"canceled subscription": func(f *moyCheckoutFixture) { f.sub.Status = domain.SubscriptionStatusCanceled },
		"cancels at period end": func(f *moyCheckoutFixture) { f.sub.CancelAtPeriodEnd = true },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newMoyCheckoutFixture("moyasar")
			mutate(f)
			code, out := f.pay(t)
			if code != http.StatusOK {
				t.Fatalf("status = %d, body %v", code, out)
			}
			if got := moyData(t, out)["save_card"]; got != false {
				t.Fatalf("save_card = %v, want false", got)
			}
		})
	}
}

func TestCheckoutInitiate_MoyasarUnconfigured503(t *testing.T) {
	cases := map[string]func(*moyCheckoutFixture){
		"no publishable key": func(f *moyCheckoutFixture) { f.h.SetMoyasar(f.reader, f.settler, "") },
		"no settlement":      func(f *moyCheckoutFixture) { f.h.SetMoyasar(f.reader, nil, "pk_test_abc") },
	}
	for name, unset := range cases {
		t.Run(name, func(t *testing.T) {
			f := newMoyCheckoutFixture("moyasar")
			unset(f)
			code, out := f.pay(t)
			if code != http.StatusServiceUnavailable || moyErrorCode(out) != codeCheckoutUnavailable {
				t.Fatalf("status = %d code = %q, want 503 %s", code, moyErrorCode(out), codeCheckoutUnavailable)
			}
		})
	}
}

func TestCheckoutInitiate_StripeResponseHasNoMoyasarKeys(t *testing.T) {
	f := newMoyCheckoutFixture("stripe")
	f.inv.Currency = "USD"
	code, out := f.pay(t)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %v", code, out)
	}
	d := moyData(t, out)
	want := []string{"order_id", "amount", "currency", "invoice_id", "invoice_number", "gateway", "client_secret", "publishable_key", "razorpay_key_id"}
	if len(d) != len(want) {
		t.Errorf("stripe response keys = %v, want exactly %v", d, want)
	}
	for _, k := range want {
		if _, ok := d[k]; !ok {
			t.Errorf("stripe response missing %q", k)
		}
	}
	if d["amount"] != float64(f.inv.Total) {
		t.Errorf("stripe amount = %v, want order amount %d", d["amount"], f.inv.Total)
	}
}

func TestMoyasarVerify_PaidSettlesRecordsAndRecovers(t *testing.T) {
	f := newMoyCheckoutFixture("moyasar")
	code, out := f.verifyPayment(t)
	if code != http.StatusOK || moyData(t, out)["status"] != "paid" {
		t.Fatalf("status = %d body %v, want 200 paid", code, out)
	}
	if len(f.reader.gotIDs) != 1 || f.reader.gotIDs[0] != f.payment.ID {
		t.Errorf("fetched %v, want [%s]", f.reader.gotIDs, f.payment.ID)
	}
	if f.invoices.markPaidCalls != 1 || f.invoices.markPaidTenant != f.inv.TenantID || f.invoices.getByIDTenant != f.inv.TenantID {
		t.Errorf("settled %d times under tenant %s / ctx %v, want once under %s",
			f.invoices.markPaidCalls, f.invoices.markPaidTenant, f.invoices.getByIDTenant, f.inv.TenantID)
	}
	if f.inv.GatewayPaymentID != f.payment.ID || f.invoices.claimTenant != f.inv.TenantID {
		t.Errorf("recorded payment = %q under %s", f.inv.GatewayPaymentID, f.invoices.claimTenant)
	}
	if len(f.cards.saved) != 1 || f.cards.saved[0] != f.wantSavedCard() {
		t.Errorf("saved cards = %+v, want [%+v]", f.cards.saved, f.wantSavedCard())
	}
	if len(f.dunning.recovered) != 1 || f.dunning.recovered[0] != f.inv.ID || len(f.dunning.outcomes) != 1 {
		t.Errorf("dunning recovery = %v / %d outcomes, want one for %s", f.dunning.recovered, len(f.dunning.outcomes), f.inv.ID)
	}
}

// The webhook (or another tab) settled the invoice with a different payment
// while this verify ran: report paid, but keep the recorded refund target and
// card and count no second recovery.
func TestMoyasarVerify_SecondPaymentDoesNotOverwrite(t *testing.T) {
	f := newMoyCheckoutFixture("moyasar")
	f.invoices.settledElsewhere = true
	f.inv.GatewayPaymentID = "pay-A"
	code, out := f.verifyPayment(t)
	if code != http.StatusOK || moyData(t, out)["status"] != "paid" {
		t.Fatalf("status = %d body %v", code, out)
	}
	if f.inv.GatewayPaymentID != "pay-A" || len(f.cards.saved) != 0 || len(f.dunning.outcomes) != 0 {
		t.Fatalf("second payment overwrote: payment=%q cards=%d outcomes=%d", f.inv.GatewayPaymentID, len(f.cards.saved), len(f.dunning.outcomes))
	}
}

func TestMoyasarVerify_NotPaidDoesNotSettle(t *testing.T) {
	for _, tc := range []struct{ gw, want string }{{"failed", "failed"}, {"initiated", "processing"}} {
		t.Run(tc.gw, func(t *testing.T) {
			f := newMoyCheckoutFixture("moyasar")
			f.payment.Status = tc.gw
			code, out := f.verifyPayment(t)
			if code != http.StatusOK {
				t.Fatalf("status = %d, body %v", code, out)
			}
			if d := moyData(t, out); d["status"] != tc.want || d["payment_status"] != tc.gw {
				t.Errorf("data = %v, want status %s", d, tc.want)
			}
			if f.invoices.markPaidCalls != 0 || len(f.invoices.claims) != 0 || len(f.cards.saved) != 0 {
				t.Error("unpaid payment had effects")
			}
		})
	}
}

func TestMoyasarVerify_BindingFailuresShareOneCode(t *testing.T) {
	cases := map[string]func(*port.GatewayPayment){
		"wrong invoice": func(p *port.GatewayPayment) { p.InvoiceID = uuid.NewString() },
		"currency":      func(p *port.GatewayPayment) { p.Currency = "USD" },
		"short amount":  func(p *port.GatewayPayment) { p.Amount-- },
		"refunded":      func(p *port.GatewayPayment) { p.RefundedAmount = 1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newMoyCheckoutFixture("moyasar")
			mutate(f.payment)
			code, out := f.verifyPayment(t)
			if code != http.StatusBadRequest || moyErrorCode(out) != codePaymentMismatch {
				t.Fatalf("status = %d code = %q, want 400 %s", code, moyErrorCode(out), codePaymentMismatch)
			}
			if msg := out["error"].(map[string]any)["message"]; msg != "payment does not match this invoice" {
				t.Errorf("message = %v, want the generic binding message", msg)
			}
			if f.invoices.markPaidCalls != 0 || len(f.cards.saved) != 0 {
				t.Error("unbound payment settled or saved a card")
			}
		})
	}
}

func TestMoyasarVerify_FetchError502(t *testing.T) {
	f := newMoyCheckoutFixture("moyasar")
	f.reader.err = errors.New("boom")
	code, out := f.verifyPayment(t)
	if code != http.StatusBadGateway || moyErrorCode(out) != codeGatewayUnavailable {
		t.Fatalf("status = %d code = %q, want 502 %s", code, moyErrorCode(out), codeGatewayUnavailable)
	}
	if f.invoices.markPaidCalls != 0 {
		t.Error("settled on fetch error")
	}
}

// Without the read-back or the settlement path, verify must not report paid.
func TestMoyasarVerify_Unconfigured503(t *testing.T) {
	cases := map[string]func(*moyCheckoutFixture){
		"no read-back":  func(f *moyCheckoutFixture) { f.h.SetMoyasar(nil, f.settler, "pk_test_abc") },
		"no settlement": func(f *moyCheckoutFixture) { f.h.SetMoyasar(f.reader, nil, "pk_test_abc") },
	}
	for name, unset := range cases {
		t.Run(name, func(t *testing.T) {
			f := newMoyCheckoutFixture("moyasar")
			unset(f)
			code, out := f.verifyPayment(t)
			if code != http.StatusServiceUnavailable || moyErrorCode(out) != codeCheckoutUnavailable {
				t.Fatalf("status = %d code = %q, want 503 %s", code, moyErrorCode(out), codeCheckoutUnavailable)
			}
			if f.invoices.markPaidCalls != 0 {
				t.Error("settled without the settlement path")
			}
		})
	}
}

func TestMoyasarVerify_AlreadyPaidSkipsFetch(t *testing.T) {
	f := newMoyCheckoutFixture("moyasar")
	f.inv.Status = domain.InvoiceStatusPaid
	code, out := f.verifyPayment(t)
	if code != http.StatusOK || moyData(t, out)["status"] != "paid" {
		t.Fatalf("status = %d body %v, want 200 paid", code, out)
	}
	if len(f.reader.gotIDs) != 0 || f.invoices.markPaidCalls != 0 {
		t.Errorf("fetch %d settle %d, want none", len(f.reader.gotIDs), f.invoices.markPaidCalls)
	}
}

func TestMoyasarVerify_MissingPaymentID400(t *testing.T) {
	f := newMoyCheckoutFixture("moyasar")
	code, out := f.verify(t, map[string]string{})
	if code != http.StatusBadRequest || moyErrorCode(out) != codeValidationFailed {
		t.Fatalf("status = %d code = %q, want 400 validation_failed", code, moyErrorCode(out))
	}
	if len(f.reader.gotIDs) != 0 {
		t.Error("fetched without a payment id")
	}
}

func TestMoyasarVerify_SettleError500(t *testing.T) {
	f := newMoyCheckoutFixture("moyasar")
	f.invoices.markPaidErr = errors.New("db down")
	code, out := f.verifyPayment(t)
	if code != http.StatusInternalServerError || moyErrorCode(out) != codeInternalError {
		t.Fatalf("status = %d code = %q, want 500", code, moyErrorCode(out))
	}
	if len(f.cards.saved) != 0 || len(f.invoices.claims) != 0 {
		t.Error("recorded or saved despite failed settlement")
	}
}

func TestMoyasarVerify_InvalidAndUnknownInvoice(t *testing.T) {
	f := newMoyCheckoutFixture("moyasar")
	body := map[string]string{"payment_id": f.payment.ID}
	if code, _ := moyDo(t, http.MethodPost, "/checkout/nope/moyasar/verify", f.h.MoyasarVerify, "/checkout/:id/moyasar/verify", body); code != http.StatusBadRequest {
		t.Errorf("invalid id status = %d, want 400", code)
	}
	if code, _ := moyDo(t, http.MethodPost, "/checkout/"+uuid.NewString()+"/moyasar/verify", f.h.MoyasarVerify, "/checkout/:id/moyasar/verify", body); code != http.StatusNotFound {
		t.Errorf("unknown invoice status = %d, want 404", code)
	}
}
