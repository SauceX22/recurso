package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/port"
	"github.com/recurso-dev/recurso/internal/core/port/porttest"
)

// fakeRoutedGateway records which methods reached it.
type fakeRoutedGateway struct {
	porttest.UnimplementedPaymentGateway
	name string

	orders, verifies, refunds int
	lastRefundID              string
}

func (f *fakeRoutedGateway) CreateOrder(_ context.Context, amount int64, currency, receipt, invoiceID string) (*port.PaymentOrder, error) {
	f.orders++
	return &port.PaymentOrder{ID: invoiceID, Gateway: f.name, Amount: amount, Currency: currency, Receipt: receipt}, nil
}

func (f *fakeRoutedGateway) VerifyPayment(_ context.Context, _, _, _ string) error {
	f.verifies++
	return nil
}

func (f *fakeRoutedGateway) Refund(_ context.Context, paymentID string, _ int64, _ string) (*port.RefundResult, error) {
	f.refunds++
	f.lastRefundID = paymentID
	return &port.RefundResult{RefundID: paymentID, Status: "refunded"}, nil
}

// newOverrideRouter routes SAR to an extra gateway named "moyasar".
func newOverrideRouter(t *testing.T, sar port.PaymentGateway) (*SmartRouter, *fakeRoutedGateway, *fakeRoutedGateway) {
	t.Helper()
	razorpay := &fakeRoutedGateway{name: "razorpay"}
	stripe := &fakeRoutedGateway{name: "stripe"}
	r := NewSmartRouter(razorpay, stripe)
	r.RegisterGateway("moyasar", sar)
	if err := r.SetCurrencyOverrides("SAR=moyasar"); err != nil {
		t.Fatalf("SetCurrencyOverrides: %v", err)
	}
	return r, razorpay, stripe
}

func TestSmartRouterOverride_CreateOrderSARRoutesToOverride(t *testing.T) {
	moyasar := &fakeRoutedGateway{name: "moyasar"}
	r, razorpay, stripe := newOverrideRouter(t, moyasar)
	invID := uuid.NewString()
	order, err := r.CreateOrder(context.Background(), 10000, "sar", "INV-1", invID)
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if moyasar.orders != 1 || stripe.orders != 0 || razorpay.orders != 0 {
		t.Fatalf("orders moyasar=%d stripe=%d razorpay=%d, want 1/0/0", moyasar.orders, stripe.orders, razorpay.orders)
	}
	if order.Gateway != "moyasar" || order.ID != invID {
		t.Fatalf("order = %+v, want moyasar order keyed by invoice id", order)
	}

	// USD and INR keep their built-in routes.
	if _, err := r.CreateOrder(context.Background(), 100, "USD", "r", "i"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateOrder(context.Background(), 100, "INR", "r", "i"); err != nil {
		t.Fatal(err)
	}
	if stripe.orders != 1 || razorpay.orders != 1 || moyasar.orders != 1 {
		t.Fatalf("after USD+INR: stripe=%d razorpay=%d moyasar=%d, want 1/1/1", stripe.orders, razorpay.orders, moyasar.orders)
	}
}

// Verification routes only by the order id's prefix: a bare UUID names no
// gateway, so it is never guessed.
func TestSmartRouterOverride_VerifyNeverGuessesFromUUID(t *testing.T) {
	srv, rec := moyasarTestServer(t, nil)
	r, razorpay, stripe := newOverrideRouter(t, testMoyasar(srv.URL))
	err := r.VerifyPayment(context.Background(), uuid.NewString(), uuid.NewString(), "")
	if err == nil || !strings.Contains(err.Error(), "unknown gateway") {
		t.Fatalf("err = %v, want unknown gateway", err)
	}
	if razorpay.verifies+stripe.verifies != 0 || len(rec.all()) != 0 {
		t.Fatalf("verifies razorpay=%d stripe=%d moyasar calls=%d, want none for an unprefixed id",
			razorpay.verifies, stripe.verifies, len(rec.all()))
	}
}

func TestSmartRouterOverride_RefundRouting(t *testing.T) {
	moyasar := &fakeRoutedGateway{name: "moyasar"}
	r, razorpay, stripe := newOverrideRouter(t, moyasar)
	payID := uuid.NewString()
	if _, err := r.Refund(context.Background(), payID, 500, "SAR"); err != nil {
		t.Fatalf("SAR refund: %v", err)
	}
	if moyasar.refunds != 1 || moyasar.lastRefundID != payID {
		t.Fatalf("moyasar refunds=%d last=%q, want 1/%q", moyasar.refunds, moyasar.lastRefundID, payID)
	}
	// Prefixed ids keep routing by prefix, whatever the currency.
	if _, err := r.Refund(context.Background(), "pi_123", 500, "SAR"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Refund(context.Background(), "pay_123", 500, "SAR"); err != nil {
		t.Fatal(err)
	}
	if stripe.refunds != 1 || razorpay.refunds != 1 || moyasar.refunds != 1 {
		t.Fatalf("refunds stripe=%d razorpay=%d moyasar=%d, want 1/1/1", stripe.refunds, razorpay.refunds, moyasar.refunds)
	}
}

// With the real adapter behind the override: a SAR subscription never gets a
// gateway subscription id (the renewal claim skips rows that have one), and a
// SAR retry without a saved token reports the missing method rather than a
// decline.
func TestSmartRouterOverride_SARReachesMoyasar(t *testing.T) {
	srv, rec := moyasarTestServer(t, nil)
	r, _, _ := newOverrideRouter(t, testMoyasar(srv.URL))

	id, err := r.CreateSubscription(context.Background(), "plan", 12, "c@example.com", nil, "SAR")
	if err == nil || id != "" {
		t.Fatalf("CreateSubscription = (%q, %v), want an error and no id", id, err)
	}
	res, err := r.RetryPayment(context.Background(), "inv-1", 10000, "SAR")
	if err != nil || res.Success || res.Permanent || res.ErrorCode != port.ErrCodeNoSavedMethod {
		t.Fatalf("RetryPayment = %+v, %v; want a non-permanent %s", res, err, port.ErrCodeNoSavedMethod)
	}
	if len(rec.all()) != 0 {
		t.Fatalf("calls = %d, want none", len(rec.all()))
	}
}
