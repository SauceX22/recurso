package service

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
)

// An off-session success records its payment as the invoice's refund target
// through the conditional claim, so it can't overwrite a payment that settled
// the invoice first; losing the claim, or charging an invoice that was
// already settled, is logged as a possible duplicate charge.
func TestRecordSettlingPayment(t *testing.T) {
	for _, tc := range []struct {
		name         string
		transitioned bool
		paymentID    string
		recorded     string
		wantClaims   int
		wantWarn     bool
	}{
		{"settled by this payment", true, "pay_1", "", 1, false},
		{"redelivered with the same payment", true, "pay_1", "pay_1", 1, false},
		{"another payment holds the slot", true, "pay_2", "pay_1", 1, true},
		{"invoice already settled before the charge", false, "pay_2", "", 0, true},
		{"nothing was charged", true, "", "", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureDefaultLog(t)
			inv := &domain.Invoice{ID: uuid.New(), TenantID: uuid.New()}
			rec := &fakePaymentRecorder{recorded: tc.recorded}

			RecordSettlingPayment(context.Background(), rec, inv, tc.transitioned, tc.paymentID)

			if len(rec.calls) != tc.wantClaims {
				t.Fatalf("claims = %+v, want %d", rec.calls, tc.wantClaims)
			}
			if tc.wantClaims == 1 && rec.calls[0] != (recordedPayment{inv.TenantID, inv.ID, tc.paymentID}) {
				t.Fatalf("claim = %+v, want the invoice's tenant and id", rec.calls[0])
			}
			if warned := strings.Contains(logs.String(), "level=WARN"); warned != tc.wantWarn {
				t.Fatalf("warned = %v, want %v:\n%s", warned, tc.wantWarn, logs.String())
			}
		})
	}
}
