package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/adapter/handler"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port/porttest"
)

// noInvoices answers every public invoice lookup with "not found", so a
// request that passes the route's limiters gets a 404 from the handler.
type noInvoices struct {
	porttest.UnimplementedInvoiceRepository
}

func (noInvoices) GetByIDPublic(context.Context, uuid.UUID) (*domain.Invoice, error) {
	return nil, nil
}

func newPublicCheckoutRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	registerPublicRoutes(r, &publicHandlers{
		checkoutHandler: handler.NewCheckoutHandler(noInvoices{}, nil, nil, nil, ""),
		publicLimit:     func(c *gin.Context) { c.Next() },
	})
	return r
}

func serve(r *gin.Engine, method, path, body string) int {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

// Anyone holding a checkout link can post verifies for it. Junk verifies
// (other payment ids) must not use up the real buyer's allowance, or the
// buyer returning from 3DS is shown an error.
func TestCheckoutVerifyLimit_JunkCannotLockOutTheBuyer(t *testing.T) {
	r := newPublicCheckoutRouter(t)
	path := "/checkout/" + uuid.NewString() + "/moyasar/verify"
	// Junk stays under the per-invoice cap across all payment ids, which
	// bounds read-backs for an invoice whoever asks.
	for i := 0; i < verifyPerInvoiceLimit-1; i++ {
		serve(r, http.MethodPost, path, `{"payment_id":"`+uuid.NewString()+`"}`)
	}
	if code := serve(r, http.MethodPost, path, `{"payment_id":"`+uuid.NewString()+`"}`); code == http.StatusTooManyRequests {
		t.Fatal("the buyer's verify was rate limited by other callers' verifies")
	}
}

// One payment id is still bounded per invoice: re-posting it from rotating
// IPs cannot spend the gateway quota without limit.
func TestCheckoutVerifyLimit_BoundsOnePaymentPerInvoice(t *testing.T) {
	r := newPublicCheckoutRouter(t)
	path := "/checkout/" + uuid.NewString() + "/moyasar/verify"
	body := `{"payment_id":"` + uuid.NewString() + `"}`
	for i := 0; i < 10; i++ {
		if code := serve(r, http.MethodPost, path, body); code != http.StatusNotFound {
			t.Fatalf("verify %d = %d, want 404 from the handler", i+1, code)
		}
	}
	if code := serve(r, http.MethodPost, path, body); code != http.StatusTooManyRequests {
		t.Fatalf("11th verify of one payment = %d, want 429", code)
	}
}

// /pay checks the gateway for an earlier capture (a secret-key call) and
// creates orders, so it is bounded per invoice across all callers.
func TestCheckoutPayLimit_BoundedPerInvoice(t *testing.T) {
	r := newPublicCheckoutRouter(t)
	path := "/checkout/" + uuid.NewString() + "/pay"
	for i := 0; i < 10; i++ {
		if code := serve(r, http.MethodPost, path, ""); code != http.StatusNotFound {
			t.Fatalf("pay %d = %d, want 404 from the handler", i+1, code)
		}
	}
	if code := serve(r, http.MethodPost, path, ""); code != http.StatusTooManyRequests {
		t.Fatalf("11th pay = %d, want 429", code)
	}
	// Another invoice has its own allowance.
	if code := serve(r, http.MethodPost, "/checkout/"+uuid.NewString()+"/pay", ""); code != http.StatusNotFound {
		t.Fatalf("other invoice pay = %d, want 404", code)
	}
}

// uuid.Parse accepts several spellings of one id; the per-invoice limiters
// key on the canonical form, so respelling the id gets no fresh bucket.
func TestCheckoutLimits_KeyOnTheCanonicalInvoiceID(t *testing.T) {
	id := uuid.New()
	spellings := []string{
		id.String(),
		strings.ToUpper(id.String()),
		strings.ReplaceAll(id.String(), "-", ""),
		"{" + id.String() + "}",
		"urn:uuid:" + id.String(),
	}
	r := newPublicCheckoutRouter(t)
	for i := 0; i < 10; i++ {
		serve(r, http.MethodPost, "/checkout/"+spellings[i%len(spellings)]+"/pay", "")
	}
	for _, s := range spellings {
		if code := serve(r, http.MethodPost, "/checkout/"+s+"/pay", ""); code != http.StatusTooManyRequests {
			t.Fatalf("pay via %q after the invoice's allowance = %d, want 429", s, code)
		}
	}

	r = newPublicCheckoutRouter(t)
	body := `{"payment_id":"` + uuid.NewString() + `"}`
	for i := 0; i < 10; i++ {
		serve(r, http.MethodPost, "/checkout/"+spellings[i%len(spellings)]+"/moyasar/verify", body)
	}
	for _, s := range spellings {
		if code := serve(r, http.MethodPost, "/checkout/"+s+"/moyasar/verify", body); code != http.StatusTooManyRequests {
			t.Fatalf("verify via %q after the payment's allowance = %d, want 429", s, code)
		}
	}
}

// Random payment ids each get their own per-payment bucket, so the invoice
// also caps verifies across all payment ids: a gateway read-back per request
// stays bounded however many ids or IPs ask.
func TestCheckoutVerifyLimit_CapsAllPaymentsPerInvoice(t *testing.T) {
	r := newPublicCheckoutRouter(t)
	path := "/checkout/" + uuid.NewString() + "/moyasar/verify"
	for i := 0; i < verifyPerInvoiceLimit; i++ {
		if code := serve(r, http.MethodPost, path, `{"payment_id":"`+uuid.NewString()+`"}`); code != http.StatusNotFound {
			t.Fatalf("verify %d = %d, want 404 from the handler", i+1, code)
		}
	}
	if code := serve(r, http.MethodPost, path, `{"payment_id":"`+uuid.NewString()+`"}`); code != http.StatusTooManyRequests {
		t.Fatalf("verify past the invoice cap = %d, want 429", code)
	}
}

// The key middlewares leave the body intact for the handler and key on the
// canonical ids.
func TestCheckoutVerifyKeys_RestoreBodyAndCanonicalise(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	var gotBody, gotInvoice, gotVerify string
	r.POST("/checkout/:id/moyasar/verify", invoiceKey, verifyKey, func(c *gin.Context) {
		b, _ := io.ReadAll(c.Request.Body)
		gotBody, gotInvoice, gotVerify = string(b), c.Param(invoiceKeyParam), c.Param(verifyKeyParam)
		c.Status(http.StatusNoContent)
	})
	inv, pay := uuid.New(), uuid.New()
	body := `{"payment_id":"` + strings.ToUpper(pay.String()) + `"}`
	if code := serve(r, http.MethodPost, "/checkout/"+strings.ToUpper(inv.String())+"/moyasar/verify", body); code != http.StatusNoContent {
		t.Fatalf("code = %d", code)
	}
	if gotBody != body || gotInvoice != inv.String() || gotVerify != inv.String()+":"+pay.String() {
		t.Fatalf("body=%q invoice_key=%q verify_key=%q", gotBody, gotInvoice, gotVerify)
	}
}

// Non-UUID invoice ids all share one bucket.
func TestCheckoutPayLimit_NonUUIDIDsShareABucket(t *testing.T) {
	r := newPublicCheckoutRouter(t)
	for i := 0; i < 10; i++ {
		serve(r, http.MethodPost, "/checkout/junk"+uuid.NewString()[:4]+"/pay", "")
	}
	if code := serve(r, http.MethodPost, "/checkout/other-junk/pay", ""); code != http.StatusTooManyRequests {
		t.Fatalf("non-UUID pay after the shared allowance = %d, want 429", code)
	}
}
