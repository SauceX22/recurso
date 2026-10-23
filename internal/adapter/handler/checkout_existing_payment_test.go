package handler

import (
	"errors"
	"net/http"
	"testing"

	"github.com/recurso-dev/recurso/internal/core/domain"
)

// A payment the gateway already captured for the invoice (a renewal whose
// answer was lost, a missed webhook) is settled before the buyer is given a
// way to pay again.

// Showing the page is a public GET: it never spends a secret-key gateway
// read-back (an unauthenticated caller could drain the quota renewals share)
// and never settles. Paying is where an earlier capture is checked.
func TestShowCheckout_NeverChecksTheGateway(t *testing.T) {
	f := newMoyCheckoutFixture("moyasar")
	f.settler.existing = true
	code, out := f.show(t)
	if code != http.StatusOK || moyData(t, out)["status"] != "open" {
		t.Fatalf("status = %d body %v, want the open invoice", code, out)
	}
	if f.settler.existingCalls != 0 || len(f.settler.settled) != 0 {
		t.Fatalf("existing-payment checks = %d settled = %v, want none", f.settler.existingCalls, f.settler.settled)
	}
}

// A buyer who already paid (the answer was lost) reopens the link and clicks
// Pay: /pay finds the capture, settles it and says "paid" instead of handing
// out a form that would take a second payment.
func TestCheckout_PaidBuyerReopeningTheLinkIsToldPaidNotChargedAgain(t *testing.T) {
	f := newMoyCheckoutFixture("moyasar")
	f.settler.existing = true
	if code, out := f.show(t); code != http.StatusOK || moyData(t, out)["status"] != "open" {
		t.Fatalf("show: status = %d body %v", code, out)
	}
	code, out := f.pay(t)
	if code != http.StatusBadRequest || moyErrorCode(out) != codeInvoiceAlreadyPaid {
		t.Fatalf("pay: status = %d code = %q, want 400 %s", code, moyErrorCode(out), codeInvoiceAlreadyPaid)
	}
	if _, ok := out["data"]; ok {
		t.Fatal("pay handed out a payment form for an invoice already captured")
	}
	if f.settler.existingCalls != 1 || f.invoices.inv.Status != "paid" {
		t.Fatalf("checks = %d status = %s, want one check that settled the invoice", f.settler.existingCalls, f.invoices.inv.Status)
	}
	if code, out := f.show(t); code != http.StatusOK || moyData(t, out)["status"] != "paid" {
		t.Fatalf("reload: status = %d body %v, want paid", code, out)
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

// Only an owed invoice (open, past_due, uncollectible) gets a payment form,
// on every gateway: a buyer who pays a void or draft invoice is charged for
// nothing, and verify would then refuse the payment.
func TestCheckoutInitiate_NonPayableInvoiceGetsNoOrder(t *testing.T) {
	for _, gw := range []string{"moyasar", "stripe", "razorpay"} {
		for _, status := range []domain.InvoiceStatus{domain.InvoiceStatusVoid, domain.InvoiceStatusDraft} {
			f := newMoyCheckoutFixture(gw)
			f.inv.Status = status
			code, out := f.pay(t)
			if code != http.StatusConflict || moyErrorCode(out) != codeConflict {
				t.Fatalf("%s %s: status = %d code = %q, want 409 %s", gw, status, code, moyErrorCode(out), codeConflict)
			}
			if f.gw.gotAmount != 0 || f.settler.existingCalls != 0 {
				t.Fatalf("%s %s: order amount = %d checks = %d, want no order and no gateway check", gw, status, f.gw.gotAmount, f.settler.existingCalls)
			}
		}
	}
}

func TestCheckoutInitiate_UncollectibleInvoiceIsPayable(t *testing.T) {
	f := newMoyCheckoutFixture("moyasar")
	f.inv.Status = domain.InvoiceStatusUncollectible
	if code, out := f.pay(t); code != http.StatusOK {
		t.Fatalf("status = %d body %v, want the form", code, out)
	}
}
