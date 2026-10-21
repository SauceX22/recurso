package handler

import (
	"net/http"
	"testing"
)

// H15: an event without a payment id can never be settled; it is acknowledged
// as ignored (no redelivery loop) without spending a secret-key read-back.
func TestMoyasarWebhookR3_EventWithoutPaymentIDIsIgnored(t *testing.T) {
	f := newMoyasarFixture(t)
	w := f.post(moyasarBody("evt_noid", "payment_paid", moyasarTestSecret, ""))
	if w.Code != http.StatusOK || ackReason(t, w) != "no payment id" {
		t.Fatalf("status=%d body=%s; want 200 ignored \"no payment id\"", w.Code, w.Body.String())
	}
	if len(f.reader.gotIDs) != 0 || len(f.settler.settled) != 0 {
		t.Fatalf("fetched %v, settled %v; want neither", f.reader.gotIDs, f.settler.settled)
	}
}
