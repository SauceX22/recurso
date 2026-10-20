package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/port"
	"github.com/recurso-dev/recurso/internal/core/port/porttest"
)

// fakeRoutedGateway records which methods reached it. ownsUUIDs makes it
// claim bare-UUID payment ids the way the Moyasar adapter does.
type fakeRoutedGateway struct {
	porttest.UnimplementedPaymentGateway
	name      string
	ownsUUIDs bool

	orders, verifies, refunds int
	lastVerifyOrder           string
	lastVerifyPayment         string
	lastRefundID              string
}

func (f *fakeRoutedGateway) CreateOrder(_ context.Context, amount int64, currency, receipt, invoiceID string) (*port.PaymentOrder, error) {
	f.orders++
	return &port.PaymentOrder{ID: invoiceID, Gateway: f.name, Amount: amount, Currency: currency, Receipt: receipt}, nil
}

func (f *fakeRoutedGateway) VerifyPayment(_ context.Context, orderID, paymentID, _ string) error {
	f.verifies++
	f.lastVerifyOrder = orderID
	f.lastVerifyPayment = paymentID
	return nil
}

func (f *fakeRoutedGateway) Refund(_ context.Context, paymentID string, _ int64, _ string) (*port.RefundResult, error) {
	f.refunds++
	f.lastRefundID = paymentID
	return &port.RefundResult{RefundID: paymentID, Status: "refunded"}, nil
}

// fakeOwner adds OwnsPaymentID on top of fakeRoutedGateway.
type fakeOwner struct {
	*fakeRoutedGateway
}

func (f fakeOwner) OwnsPaymentID(id string) bool {
	if !f.ownsUUIDs {
		return false
	}
	_, err := uuid.Parse(id)
	return err == nil
}

func newMoyasarRouter(t *testing.T) (*SmartRouter, *fakeRoutedGateway, *fakeRoutedGateway, *fakeRoutedGateway) {
	t.Helper()
	razorpay := &fakeRoutedGateway{name: "razorpay"}
	stripe := &fakeRoutedGateway{name: "stripe"}
	moyasar := &fakeRoutedGateway{name: "moyasar", ownsUUIDs: true}
	r := NewSmartRouter(razorpay, stripe)
	r.RegisterGateway("moyasar", fakeOwner{moyasar})
	if err := r.SetCurrencyOverrides("SAR=moyasar"); err != nil {
		t.Fatalf("SetCurrencyOverrides: %v", err)
	}
	return r, razorpay, stripe, moyasar
}

func TestSmartRouterMoyasar_CreateOrderSARRoutesToMoyasar(t *testing.T) {
	r, razorpay, stripe, moyasar := newMoyasarRouter(t)
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

func TestSmartRouterMoyasar_VerifyUUIDPaymentRoutesToOwner(t *testing.T) {
	r, razorpay, stripe, moyasar := newMoyasarRouter(t)
	invID := uuid.NewString()
	payID := uuid.NewString()
	if err := r.VerifyPayment(context.Background(), invID, payID, ""); err != nil {
		t.Fatalf("VerifyPayment: %v", err)
	}
	if moyasar.verifies != 1 || stripe.verifies != 0 || razorpay.verifies != 0 {
		t.Fatalf("verifies moyasar=%d stripe=%d razorpay=%d, want 1/0/0", moyasar.verifies, stripe.verifies, razorpay.verifies)
	}
	if moyasar.lastVerifyOrder != invID || moyasar.lastVerifyPayment != payID {
		t.Fatalf("moyasar verify got (%q,%q), want (%q,%q)", moyasar.lastVerifyOrder, moyasar.lastVerifyPayment, invID, payID)
	}
}

func TestSmartRouterMoyasar_StripeAndRazorpayVerifyUnchanged(t *testing.T) {
	r, razorpay, stripe, moyasar := newMoyasarRouter(t)
	// A UUID payment id must not hijack the prefixed routes.
	if err := r.VerifyPayment(context.Background(), "order_abc", uuid.NewString(), "sig"); err != nil {
		t.Fatal(err)
	}
	if err := r.VerifyPayment(context.Background(), "pi_abc", uuid.NewString(), "sig"); err != nil {
		t.Fatal(err)
	}
	if razorpay.verifies != 1 || stripe.verifies != 1 || moyasar.verifies != 0 {
		t.Fatalf("verifies razorpay=%d stripe=%d moyasar=%d, want 1/1/0", razorpay.verifies, stripe.verifies, moyasar.verifies)
	}
}

func TestSmartRouterMoyasar_UnknownIDWithoutOwnerErrors(t *testing.T) {
	r, razorpay, stripe, moyasar := newMoyasarRouter(t)
	for _, payID := range []string{"not-a-uuid", ""} {
		err := r.VerifyPayment(context.Background(), "ch_unknown", payID, "sig")
		if err == nil || !strings.Contains(err.Error(), "unknown gateway") {
			t.Fatalf("payment %q: err = %v, want unknown gateway", payID, err)
		}
	}
	if razorpay.verifies+stripe.verifies+moyasar.verifies != 0 {
		t.Fatal("no gateway should verify an unowned id")
	}

	// An extra gateway that does not implement OwnsPaymentID is never asked.
	plain := &fakeRoutedGateway{name: "adyen"}
	r2 := NewSmartRouter(&fakeRoutedGateway{}, &fakeRoutedGateway{})
	r2.RegisterGateway("adyen", plain)
	if err := r2.VerifyPayment(context.Background(), uuid.NewString(), uuid.NewString(), ""); err == nil {
		t.Fatal("expected unknown gateway error with no owner registered")
	}
	if plain.verifies != 0 {
		t.Fatal("non-owner extra gateway must not verify")
	}
}

func TestSmartRouterMoyasar_OwnerPickedInSortedOrder(t *testing.T) {
	a := &fakeRoutedGateway{name: "aaa", ownsUUIDs: true}
	z := &fakeRoutedGateway{name: "zzz", ownsUUIDs: true}
	r := NewSmartRouter(&fakeRoutedGateway{}, &fakeRoutedGateway{})
	r.RegisterGateway("zzz", fakeOwner{z})
	r.RegisterGateway("aaa", fakeOwner{a})
	for i := 0; i < 20; i++ {
		if err := r.VerifyPayment(context.Background(), "x", uuid.NewString(), ""); err != nil {
			t.Fatal(err)
		}
	}
	if a.verifies != 20 || z.verifies != 0 {
		t.Fatalf("aaa=%d zzz=%d, want 20/0 (sorted-name order)", a.verifies, z.verifies)
	}
}

func TestSmartRouterMoyasar_RefundRouting(t *testing.T) {
	r, razorpay, stripe, moyasar := newMoyasarRouter(t)
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
