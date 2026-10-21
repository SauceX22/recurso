package handler

import (
	"errors"
	"net/http"
	"testing"
)

// A payment the gateway already captured for the invoice (a renewal whose
// answer was lost, a missed webhook) is settled before the buyer is shown a
// way to pay again.

func TestShowCheckout_SettlesAnAlreadyCapturedPayment(t *testing.T) {
	f := newMoyCheckoutFixture("moyasar")
	f.settler.existing = true
	code, out := f.show(t)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %v", code, out)
	}
	if d := moyData(t, out); d["status"] != "paid" || d["amount_due"] != float64(0) {
		t.Fatalf("data = %v, want the invoice reported paid with nothing due", d)
	}
	if f.settler.existingCalls != 1 {
		t.Fatalf("existing-payment checks = %d, want 1", f.settler.existingCalls)
	}
}

// Showing the page is harmless; paying is what the failed check must block.
func TestShowCheckout_ExistingPaymentCheckErrorStillShows(t *testing.T) {
	f := newMoyCheckoutFixture("moyasar")
	f.settler.existingErr = errors.New("moyasar: 503")
	code, out := f.show(t)
	if code != http.StatusOK || moyData(t, out)["status"] != "open" {
		t.Fatalf("status = %d body %v, want the open invoice", code, out)
	}
}

func TestShowCheckout_PaidInvoiceIsNotChecked(t *testing.T) {
	f := newMoyCheckoutFixture("moyasar")
	f.inv.Status = "paid"
	if code, _ := f.show(t); code != http.StatusOK || f.settler.existingCalls != 0 {
		t.Fatalf("status = %d checks = %d, want no check", code, f.settler.existingCalls)
	}
}

func TestCheckoutInitiate_MoyasarAlreadyCapturedIsNotPaidAgain(t *testing.T) {
	f := newMoyCheckoutFixture("moyasar")
	f.settler.existing = true
	code, out := f.pay(t)
	if code != http.StatusBadRequest || moyErrorCode(out) != codeInvoiceAlreadyPaid {
		t.Fatalf("status = %d code = %q, want 400 %s", code, moyErrorCode(out), codeInvoiceAlreadyPaid)
	}
}

// When the check can't run, the form is not handed out: a second capture is
// worse than a retry later.
func TestCheckoutInitiate_MoyasarExistingPaymentCheckFailsClosed(t *testing.T) {
	f := newMoyCheckoutFixture("moyasar")
	f.settler.existingErr = errors.New("moyasar: 503")
	code, out := f.pay(t)
	if code != http.StatusBadGateway || moyErrorCode(out) != codeGatewayUnavailable {
		t.Fatalf("status = %d code = %q, want 502 %s", code, moyErrorCode(out), codeGatewayUnavailable)
	}
	if _, ok := out["data"]; ok {
		t.Fatal("form config returned although the check failed")
	}
}

func TestCheckoutInitiate_MoyasarNothingCapturedReturnsForm(t *testing.T) {
	f := newMoyCheckoutFixture("moyasar")
	if code, out := f.pay(t); code != http.StatusOK || f.settler.existingCalls != 1 {
		t.Fatalf("status = %d checks = %d body %v, want the form after one check", code, f.settler.existingCalls, out)
	}
}

func TestCheckoutInitiate_StripeSkipsExistingPaymentCheck(t *testing.T) {
	f := newMoyCheckoutFixture("stripe")
	f.inv.Currency = "USD"
	if code, _ := f.pay(t); code != http.StatusOK || f.settler.existingCalls != 0 {
		t.Fatalf("status = %d checks = %d, want no check", code, f.settler.existingCalls)
	}
}

// The returned payment failed, but an earlier one was captured: the buyer has
// paid.
func TestMoyasarVerify_FailedPaymentButEarlierCaptureReportsPaid(t *testing.T) {
	f := newMoyCheckoutFixture("moyasar")
	f.payment.Status = "failed"
	f.settler.existing = true
	code, out := f.verifyPayment(t)
	if code != http.StatusOK || moyData(t, out)["status"] != "paid" {
		t.Fatalf("status = %d body %v, want paid", code, out)
	}
	if len(f.settler.settled) != 0 {
		t.Fatal("the failed payment itself was settled")
	}
}

func TestMoyasarVerify_FailedPaymentCheckErrorReportsFailed(t *testing.T) {
	f := newMoyCheckoutFixture("moyasar")
	f.payment.Status = "failed"
	f.settler.existingErr = errors.New("moyasar: 503")
	code, out := f.verifyPayment(t)
	if code != http.StatusOK || moyData(t, out)["status"] != "failed" {
		t.Fatalf("status = %d body %v, want failed", code, out)
	}
}
