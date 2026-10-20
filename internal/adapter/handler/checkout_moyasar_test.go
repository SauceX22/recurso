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

type moyCheckoutInvoices struct {
	porttest.UnimplementedInvoiceRepository
	inv          *domain.Invoice
	setPaymentID string
}

func (r *moyCheckoutInvoices) GetByIDPublic(_ context.Context, id uuid.UUID) (*domain.Invoice, error) {
	if r.inv == nil || r.inv.ID != id {
		return nil, nil
	}
	return r.inv, nil
}

func (r *moyCheckoutInvoices) SetGatewayPaymentID(_ context.Context, _, _ uuid.UUID, pid string) error {
	r.setPaymentID = pid
	return nil
}

type moyCheckoutGateway struct {
	porttest.UnimplementedPaymentGateway
	gateway      string
	clientSecret string
}

func (g *moyCheckoutGateway) CreateOrder(_ context.Context, amount int64, currency, receipt, _ string) (*port.PaymentOrder, error) {
	return &port.PaymentOrder{ID: "ord_1", Amount: amount, Currency: currency, Receipt: receipt, Gateway: g.gateway, ClientSecret: g.clientSecret}, nil
}

type moyCheckoutSettler struct {
	calls     int
	hadTenant bool
	err       error
}

func (s *moyCheckoutSettler) MarkInvoicePaid(ctx context.Context, _ uuid.UUID) (bool, error) {
	s.calls++
	_, s.hadTenant = ctx.Value(domain.TenantIDKey).(uuid.UUID)
	return true, s.err
}

type moyCheckoutFetcher struct {
	p     *port.GatewayPayment
	err   error
	calls int
	gotID string
}

func (f *moyCheckoutFetcher) FetchPayment(_ context.Context, id string) (*port.GatewayPayment, error) {
	f.calls++
	f.gotID = id
	return f.p, f.err
}

type moyCheckoutCards struct {
	calls    int
	provider string
	ref      string
	customer uuid.UUID
}

func (s *moyCheckoutCards) SetGatewayPaymentMethod(_ context.Context, _, customerID uuid.UUID, provider domain.GatewayProvider, ref, _, _ string, _, _ int) error {
	s.calls++
	s.provider, s.ref, s.customer = string(provider), ref, customerID
	return nil
}

func moyTestInvoice() *domain.Invoice {
	return &domain.Invoice{
		ID:            uuid.New(),
		TenantID:      uuid.New(),
		CustomerID:    uuid.New(),
		InvoiceNumber: "INV-42",
		Status:        domain.InvoiceStatusOpen,
		Currency:      "SAR",
		Total:         10000,
		AmountPaid:    1500,
		CreditApplied: 500,
	}
}

func moyPaidPayment(inv *domain.Invoice) *port.GatewayPayment {
	return &port.GatewayPayment{
		ID:        "7f1c0e6e-1111-4c2b-9d1a-000000000001",
		Status:    "paid",
		Amount:    moyasarAmountDue(inv),
		Currency:  "SAR",
		InvoiceID: inv.ID.String(),
		CardToken: "token_abc",
		CardBrand: "mada",
		CardLast4: "1111",
	}
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

func TestCheckoutInitiate_MoyasarReturnsFormConfig(t *testing.T) {
	inv := moyTestInvoice()
	h := NewCheckoutHandler(&moyCheckoutInvoices{inv: inv}, &moyCheckoutGateway{gateway: "moyasar"}, nil, nil, "")
	h.SetMoyasar(&moyCheckoutFetcher{}, nil, "pk_test_abc")

	code, out := moyDo(t, http.MethodPost, "/checkout/"+inv.ID.String()+"/pay", h.InitiatePayment, "/checkout/:id/pay", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %v", code, out)
	}
	d := moyData(t, out)
	if d["gateway"] != "moyasar" {
		t.Errorf("gateway = %v", d["gateway"])
	}
	if d["moyasar_publishable_key"] != "pk_test_abc" {
		t.Errorf("moyasar_publishable_key = %v", d["moyasar_publishable_key"])
	}
	if got := d["amount"].(float64); int64(got) != 8000 {
		t.Errorf("amount = %v, want amount due 8000", got)
	}
	if d["currency"] != "SAR" {
		t.Errorf("currency = %v", d["currency"])
	}
	if d["description"] != "Invoice INV-42" {
		t.Errorf("description = %v", d["description"])
	}
	md, _ := d["metadata"].(map[string]any)
	if md["invoice_id"] != inv.ID.String() {
		t.Errorf("metadata = %v", d["metadata"])
	}
}

func TestCheckoutInitiate_MoyasarMissingKey503(t *testing.T) {
	inv := moyTestInvoice()
	h := NewCheckoutHandler(&moyCheckoutInvoices{inv: inv}, &moyCheckoutGateway{gateway: "moyasar"}, nil, nil, "")

	code, out := moyDo(t, http.MethodPost, "/checkout/"+inv.ID.String()+"/pay", h.InitiatePayment, "/checkout/:id/pay", nil)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %v)", code, out)
	}
}

func TestCheckoutInitiate_StripeResponseHasNoMoyasarKeys(t *testing.T) {
	inv := moyTestInvoice()
	inv.Currency = "USD"
	h := NewCheckoutHandler(&moyCheckoutInvoices{inv: inv}, &moyCheckoutGateway{gateway: "stripe", clientSecret: "cs_1"}, nil, nil, "pk_stripe")
	h.SetMoyasar(&moyCheckoutFetcher{}, nil, "pk_test_abc")

	code, out := moyDo(t, http.MethodPost, "/checkout/"+inv.ID.String()+"/pay", h.InitiatePayment, "/checkout/:id/pay", nil)
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
	if int64(d["amount"].(float64)) != inv.Total {
		t.Errorf("stripe amount = %v, want order amount %d", d["amount"], inv.Total)
	}
}

type moyVerifyFixture struct {
	inv     *domain.Invoice
	repo    *moyCheckoutInvoices
	settler *moyCheckoutSettler
	fetcher *moyCheckoutFetcher
	cards   *moyCheckoutCards
	h       *CheckoutHandler
}

func newMoyVerifyFixture(p func(*domain.Invoice) *port.GatewayPayment) *moyVerifyFixture {
	f := &moyVerifyFixture{inv: moyTestInvoice(), settler: &moyCheckoutSettler{}, cards: &moyCheckoutCards{}}
	f.repo = &moyCheckoutInvoices{inv: f.inv}
	f.fetcher = &moyCheckoutFetcher{}
	if p != nil {
		f.fetcher.p = p(f.inv)
	}
	f.h = NewCheckoutHandler(f.repo, &moyCheckoutGateway{}, nil, f.settler, "")
	f.h.SetMoyasar(f.fetcher, f.cards, "pk_test_abc")
	return f
}

func (f *moyVerifyFixture) verify(t *testing.T, body any) (int, map[string]any) {
	return moyDo(t, http.MethodPost, "/checkout/"+f.inv.ID.String()+"/moyasar/verify", f.h.MoyasarVerify, "/checkout/:id/moyasar/verify", body)
}

func TestMoyasarVerify_PaidSettlesAndSavesCard(t *testing.T) {
	f := newMoyVerifyFixture(moyPaidPayment)
	code, out := f.verify(t, map[string]string{"payment_id": "pay-1"})
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %v", code, out)
	}
	if moyData(t, out)["status"] != "paid" {
		t.Errorf("status = %v, want paid", out)
	}
	if f.fetcher.gotID != "pay-1" {
		t.Errorf("fetched %q, want pay-1", f.fetcher.gotID)
	}
	if f.settler.calls != 1 || !f.settler.hadTenant {
		t.Errorf("settler calls = %d (tenant ctx %v), want 1 with tenant", f.settler.calls, f.settler.hadTenant)
	}
	if f.repo.setPaymentID != f.fetcher.p.ID {
		t.Errorf("gateway payment id = %q, want %q", f.repo.setPaymentID, f.fetcher.p.ID)
	}
	if f.cards.calls != 1 || f.cards.provider != string(domain.GatewayMoyasar) || f.cards.ref != "token_abc" || f.cards.customer != f.inv.CustomerID {
		t.Errorf("card save = %+v", f.cards)
	}
}

func TestMoyasarVerify_NotPaidDoesNotSettle(t *testing.T) {
	for _, tc := range []struct{ gw, want string }{{"failed", "failed"}, {"initiated", "processing"}} {
		f := newMoyVerifyFixture(func(inv *domain.Invoice) *port.GatewayPayment {
			p := moyPaidPayment(inv)
			p.Status = tc.gw
			return p
		})
		code, out := f.verify(t, map[string]string{"payment_id": "pay-1"})
		if code != http.StatusOK {
			t.Fatalf("%s: status = %d, body %v", tc.gw, code, out)
		}
		d := moyData(t, out)
		if d["status"] != tc.want || d["payment_status"] != tc.gw {
			t.Errorf("%s: data = %v, want status %s", tc.gw, d, tc.want)
		}
		if f.settler.calls != 0 || f.cards.calls != 0 || f.repo.setPaymentID != "" {
			t.Errorf("%s: must not settle/save (settle %d, cards %d, pid %q)", tc.gw, f.settler.calls, f.cards.calls, f.repo.setPaymentID)
		}
	}
}

func TestMoyasarVerify_BindingFailures400(t *testing.T) {
	cases := map[string]func(*port.GatewayPayment){
		"wrong invoice": func(p *port.GatewayPayment) { p.InvoiceID = uuid.NewString() },
		"currency":      func(p *port.GatewayPayment) { p.Currency = "USD" },
		"short amount":  func(p *port.GatewayPayment) { p.Amount-- },
	}
	for name, mutate := range cases {
		f := newMoyVerifyFixture(func(inv *domain.Invoice) *port.GatewayPayment {
			p := moyPaidPayment(inv)
			mutate(p)
			return p
		})
		code, out := f.verify(t, map[string]string{"payment_id": "pay-1"})
		if code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body %v)", name, code, out)
		}
		if f.settler.calls != 0 || f.cards.calls != 0 {
			t.Errorf("%s: settled or saved card on a bad binding", name)
		}
	}
}

func TestMoyasarVerify_FetchError502(t *testing.T) {
	f := newMoyVerifyFixture(nil)
	f.fetcher.err = errors.New("boom")
	code, _ := f.verify(t, map[string]string{"payment_id": "pay-1"})
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", code)
	}
	if f.settler.calls != 0 {
		t.Error("settled on fetch error")
	}
}

func TestMoyasarVerify_NoFetcher503(t *testing.T) {
	f := newMoyVerifyFixture(moyPaidPayment)
	f.h.SetMoyasar(nil, nil, "")
	code, _ := f.verify(t, map[string]string{"payment_id": "pay-1"})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", code)
	}
}

func TestMoyasarVerify_AlreadyPaidSkipsFetch(t *testing.T) {
	f := newMoyVerifyFixture(moyPaidPayment)
	f.inv.Status = domain.InvoiceStatusPaid
	code, out := f.verify(t, map[string]string{"payment_id": "pay-1"})
	if code != http.StatusOK || moyData(t, out)["status"] != "paid" {
		t.Fatalf("status = %d body %v, want 200 paid", code, out)
	}
	if f.fetcher.calls != 0 || f.settler.calls != 0 {
		t.Errorf("fetch %d settle %d, want none", f.fetcher.calls, f.settler.calls)
	}
}

func TestMoyasarVerify_MissingPaymentID400(t *testing.T) {
	f := newMoyVerifyFixture(moyPaidPayment)
	code, _ := f.verify(t, map[string]string{})
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if f.fetcher.calls != 0 {
		t.Error("fetched without a payment id")
	}
}

func TestMoyasarVerify_SettleError500(t *testing.T) {
	f := newMoyVerifyFixture(moyPaidPayment)
	f.settler.err = errors.New("db down")
	code, _ := f.verify(t, map[string]string{"payment_id": "pay-1"})
	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", code)
	}
	if f.cards.calls != 0 {
		t.Error("saved card despite failed settlement")
	}
}

func TestMoyasarVerify_InvalidAndUnknownInvoice(t *testing.T) {
	f := newMoyVerifyFixture(moyPaidPayment)
	code, _ := moyDo(t, http.MethodPost, "/checkout/nope/moyasar/verify", f.h.MoyasarVerify, "/checkout/:id/moyasar/verify", map[string]string{"payment_id": "x"})
	if code != http.StatusBadRequest {
		t.Errorf("invalid id status = %d, want 400", code)
	}
	code, _ = moyDo(t, http.MethodPost, "/checkout/"+uuid.NewString()+"/moyasar/verify", f.h.MoyasarVerify, "/checkout/:id/moyasar/verify", map[string]string{"payment_id": "x"})
	if code != http.StatusNotFound {
		t.Errorf("unknown invoice status = %d, want 404", code)
	}
}
