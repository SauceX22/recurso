package main

import (
	"context"
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
	for i := 0; i < 30; i++ {
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
