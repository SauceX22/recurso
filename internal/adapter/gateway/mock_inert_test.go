package gateway

import (
	"context"
	"testing"

	"github.com/recurso-dev/recurso/internal/core/port"
)

// A server with no real gateway keys outside demo/dev uses the inert mock: a
// background dunning retry or mandate debit must never report money collected.
func TestInertMockGateway_NeverCollects(t *testing.T) {
	g := NewInertMockGateway()
	for i := 0; i < 200; i++ {
		r, err := g.RetryPayment(context.Background(), "inv_1", 2900, "USD")
		if err != nil || r == nil || r.Success || r.ErrorCode != port.ErrCodeNoSavedMethod {
			t.Fatalf("retry #%d = %+v, %v; want a no_saved_payment_method failure", i, r, err)
		}
	}
	r, err := g.ExecuteMandateDebit(context.Background(), port.MandateDebitRequest{TokenID: "tok", Amount: 500, Currency: "INR"})
	if err != nil || r == nil || r.Success {
		t.Fatalf("mandate debit = %+v, %v; want an unsuccessful result", r, err)
	}
}
