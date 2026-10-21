package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
	"github.com/recurso-dev/recurso/internal/core/port/porttest"
	"github.com/recurso-dev/recurso/internal/service"
)

type moyCheckoutGateway struct {
	porttest.UnimplementedPaymentGateway
	gateway      string
	clientSecret string
	gotAmount    int64
}

func (g *moyCheckoutGateway) CreateOrder(_ context.Context, amount int64, currency, receipt, _ string) (*port.PaymentOrder, error) {
	g.gotAmount = amount
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
	*moyasarHandlerFixture
	gw *moyCheckoutGateway
	h  *CheckoutHandler
}

// newMoyCheckoutFixture wires a checkout whose order gateway is gw and whose
// Moyasar verify settles through the recording settler.
func newMoyCheckoutFixture(gw string) *moyCheckoutFixture {
	f := &moyCheckoutFixture{moyasarHandlerFixture: newMoyasarHandlerFixture()}
	f.gw = &moyCheckoutGateway{gateway: gw, clientSecret: map[string]string{"stripe": "cs_1"}[gw]}
	f.h = NewCheckoutHandler(f.invoices, f.gw, nil, nil, "pk_stripe")
	f.h.SetRazorpay(nil, "rzp_key")
	f.h.SetMoyasar(f.reader, f.settler, "pk_test_abc")
	return f
}

func (f *moyCheckoutFixture) show(t *testing.T) (int, map[string]any) {
	return moyDo(t, http.MethodGet, "/checkout/"+f.inv.ID.String(), f.h.ShowCheckout, "/checkout/:id", nil)
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
	code, out := f.show(t)
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
	if d["description"] != "Invoice INV-42" {
		t.Errorf("description = %v", d["description"])
	}
	if md, _ := d["metadata"].(map[string]any); md["invoice_id"] != f.inv.ID.String() {
		t.Errorf("metadata = %v", d["metadata"])
	}
}

// save_card is the settlement path's decision, passed through unchanged.
func TestCheckoutInitiate_MoyasarSaveCardFollowsSettlement(t *testing.T) {
	for _, saves := range []bool{true, false} {
		t.Run(fmt.Sprint(saves), func(t *testing.T) {
			f := newMoyCheckoutFixture("moyasar")
			f.settler.savesCard = saves
			code, out := f.pay(t)
			if code != http.StatusOK {
				t.Fatalf("status = %d, body %v", code, out)
			}
			if got := moyData(t, out)["save_card"]; got != saves {
				t.Fatalf("save_card = %v, want %v", got, saves)
			}
		})
	}
}

// The consent terms ride on the pay response only when the payment saves the
// card and the terms are complete; the page falls back otherwise.
func TestCheckoutInitiate_CardOnFileTerms(t *testing.T) {
	terms := &service.CardOnFileTerms{Merchant: "Acme Arabia", Amount: 11500, Currency: "SAR", Interval: "month", IntervalCount: 1}
	cases := []struct {
		name  string
		saves bool
		terms *service.CardOnFileTerms
		want  map[string]any
	}{
		{"saves with terms", true, terms, map[string]any{"merchant": "Acme Arabia", "amount": float64(11500), "currency": "SAR", "interval": "month", "interval_count": float64(1)}},
		{"saves without terms", true, nil, nil},
		{"does not save", false, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newMoyCheckoutFixture("moyasar")
			f.settler.savesCard, f.settler.terms = tc.saves, tc.terms
			code, out := f.pay(t)
			if code != http.StatusOK {
				t.Fatalf("status = %d, body %v", code, out)
			}
			d := moyData(t, out)
			got, present := d["card_on_file"]
			if tc.want == nil {
				if present {
					t.Fatalf("card_on_file = %v, want absent", got)
				}
				return
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("card_on_file = %v, want %v", got, tc.want)
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
	if d["amount"] != float64(f.gw.gotAmount) {
		t.Errorf("stripe amount = %v, want the order amount %d", d["amount"], f.gw.gotAmount)
	}
}

// Every gateway's order is for what the buyer still owes after partial
// payments and applied credit, not the invoice total, the way the retry
// worker charges.
func TestCheckoutInitiate_OrderIsForAmountDue(t *testing.T) {
	for gw, currency := range map[string]string{"stripe": "USD", "razorpay": "INR", "moyasar": "SAR"} {
		t.Run(gw, func(t *testing.T) {
			f := newMoyCheckoutFixture(gw)
			f.inv.Currency = currency
			code, out := f.pay(t)
			if code != http.StatusOK {
				t.Fatalf("status = %d, body %v", code, out)
			}
			if f.gw.gotAmount != f.inv.AmountDue {
				t.Errorf("order created for %d, want the amount due %d (total %d)", f.gw.gotAmount, f.inv.AmountDue, f.inv.Total)
			}
			if got := moyData(t, out)["amount"]; got != float64(f.inv.AmountDue) {
				t.Errorf("response amount = %v, want %d", got, f.inv.AmountDue)
			}
		})
	}
}

func TestCheckoutInitiate_NothingDueCreatesNoOrder(t *testing.T) {
	f := newMoyCheckoutFixture("stripe")
	f.inv.Currency, f.inv.AmountDue = "USD", 0
	code, out := f.pay(t)
	if code != http.StatusBadRequest || moyErrorCode(out) != codeInvoiceAlreadyPaid {
		t.Fatalf("status = %d code = %q, want 400 %s", code, moyErrorCode(out), codeInvoiceAlreadyPaid)
	}
	if f.gw.gotAmount != 0 {
		t.Errorf("created an order for %d", f.gw.gotAmount)
	}
}

func TestMoyasarVerify_PaidSettles(t *testing.T) {
	f := newMoyCheckoutFixture("moyasar")
	code, out := f.verifyPayment(t)
	if code != http.StatusOK || moyData(t, out)["status"] != "paid" {
		t.Fatalf("status = %d body %v, want 200 paid", code, out)
	}
	if len(f.reader.gotIDs) != 1 || f.reader.gotIDs[0] != f.payment.ID {
		t.Errorf("fetched %v, want [%s]", f.reader.gotIDs, f.payment.ID)
	}
	if !f.settledOnce() {
		t.Errorf("settled %+v, want the payment once", f.settler.settled)
	}
}

// Another payment already settled the invoice: still paid for the buyer.
func TestMoyasarVerify_NoTransitionStillReportsPaid(t *testing.T) {
	f := newMoyCheckoutFixture("moyasar")
	f.settler.transitioned = false
	code, out := f.verifyPayment(t)
	if code != http.StatusOK || moyData(t, out)["status"] != "paid" {
		t.Fatalf("status = %d body %v", code, out)
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
			if len(f.settler.settled) != 0 {
				t.Error("unpaid payment settled")
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
			if len(f.settler.settled) != 0 {
				t.Error("unbound payment settled")
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
	if len(f.settler.settled) != 0 {
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
			if len(f.settler.settled) != 0 {
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
	if len(f.reader.gotIDs) != 0 || len(f.settler.settled) != 0 {
		t.Errorf("fetch %d settle %d, want none", len(f.reader.gotIDs), len(f.settler.settled))
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

// A void or draft invoice must never be settled to paid; a written-off
// (uncollectible) one is still collectable.
func TestMoyasarVerify_OnlyPayableInvoicesSettle(t *testing.T) {
	for _, tc := range []struct {
		status domain.InvoiceStatus
		code   int
	}{
		{domain.InvoiceStatusVoid, http.StatusConflict},
		{domain.InvoiceStatusDraft, http.StatusConflict},
		{domain.InvoiceStatusUncollectible, http.StatusOK},
		{domain.InvoiceStatusPastDue, http.StatusOK},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			f := newMoyCheckoutFixture("moyasar")
			f.inv.Status = tc.status
			code, out := f.verifyPayment(t)
			if code != tc.code {
				t.Fatalf("status = %d body %v, want %d", code, out, tc.code)
			}
			if tc.code == http.StatusConflict && (len(f.reader.gotIDs) != 0 || len(f.settler.settled) != 0) {
				t.Errorf("unpayable invoice fetched %d / settled %d", len(f.reader.gotIDs), len(f.settler.settled))
			}
		})
	}
}

// Moyasar payment ids are UUIDs; anything else is refused before the
// secret-key read-back.
func TestMoyasarVerify_MalformedPaymentID400(t *testing.T) {
	f := newMoyCheckoutFixture("moyasar")
	code, out := f.verify(t, map[string]string{"payment_id": "../tokens/x"})
	if code != http.StatusBadRequest || moyErrorCode(out) != codeValidationFailed {
		t.Fatalf("status = %d code = %q, want 400 validation_failed", code, moyErrorCode(out))
	}
	if len(f.reader.gotIDs) != 0 {
		t.Error("fetched a malformed payment id")
	}
}

func TestMoyasarVerify_SettleError500(t *testing.T) {
	f := newMoyCheckoutFixture("moyasar")
	f.settler.err = errors.New("db down")
	code, out := f.verifyPayment(t)
	if code != http.StatusInternalServerError || moyErrorCode(out) != codeInternalError {
		t.Fatalf("status = %d code = %q, want 500", code, moyErrorCode(out))
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
