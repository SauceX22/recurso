package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
	"github.com/recurso-dev/recurso/internal/service"
)

// fixedCardUpdatePolicy reports one provider for every customer.
type fixedCardUpdatePolicy struct {
	provider  domain.GatewayProvider
	selfServe bool
	asked     []uuid.UUID
}

func (p *fixedCardUpdatePolicy) Provider(_ context.Context, customerID uuid.UUID, _ string) domain.GatewayProvider {
	p.asked = append(p.asked, customerID)
	return p.provider
}

func (p *fixedCardUpdatePolicy) SelfServe(provider domain.GatewayProvider) bool {
	return provider == p.provider && p.selfServe
}

type recordingCustStore struct {
	achCustStore
	defaults int
}

func (s *recordingCustStore) SetDefaultPaymentMethod(context.Context, uuid.UUID, string, string, string, int, int, *uuid.UUID) error {
	s.defaults++
	return nil
}

// countingSetup counts every Stripe call the card-update flow makes.
type countingSetup struct {
	achSetup
	ensures, intents, finalizes int
}

func (s *countingSetup) EnsureStripeCustomer(ctx context.Context, existingID, email, name string) (string, error) {
	s.ensures++
	return s.achSetup.EnsureStripeCustomer(ctx, existingID, email, name)
}

func (s *countingSetup) CreateSetupIntent(context.Context, string, map[string]string) (string, error) {
	s.intents++
	return "seti_card", nil
}

func (s *countingSetup) CreateBankAccountSetupIntent(context.Context, string, map[string]string) (string, error) {
	s.intents++
	return "seti_bank", nil
}

func (s *countingSetup) FinalizeSetupIntent(context.Context, string) (*port.SavedCard, error) {
	s.finalizes++
	return &port.SavedCard{Status: "succeeded", PaymentMethodID: "pm_new", Brand: "visa", Last4: "4242"}, nil
}

func newCardUpdateHandler(cid uuid.UUID, policy cardUpdatePolicy) (*PortalAPIHandler, *recordingCustStore, *countingSetup) {
	store := &recordingCustStore{achCustStore: achCustStore{cust: &domain.Customer{ID: cid, Email: "a@b.co"}}}
	setup := &countingSetup{}
	h := &PortalAPIHandler{}
	h.SetPaymentMethodSetup(store, setup, "pk_test_x")
	if policy != nil {
		h.SetCardUpdatePolicy(policy)
	}
	return h, store, setup
}

func portalCardCtx(customerID uuid.UUID, path, body string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("portal_customer_id", customerID)
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, w
}

type cardUpdateEndpoint struct {
	name, path, body string
	call             func(h *PortalAPIHandler, c *gin.Context)
}

var cardUpdateEndpoints = []cardUpdateEndpoint{
	{"card setup", "/portal/api/payment-method/setup-intent", "", (*PortalAPIHandler).StartPaymentMethodSetup},
	{"bank setup", "/portal/api/payment-method/bank-setup-intent", "", (*PortalAPIHandler).StartBankAccountSetup},
	{"confirm", "/portal/api/payment-method/confirm", `{"setup_intent_id":"seti_1"}`, (*PortalAPIHandler).ConfirmPaymentMethod},
}

// Saving a Stripe method for a customer whose card lives on a gateway without
// a setup flow would silently move their renewals; the portal refuses it.
func TestPortalCardUpdate_UnsupportedGatewayConflicts(t *testing.T) {
	for _, ep := range cardUpdateEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			cid := uuid.New()
			h, store, setup := newCardUpdateHandler(cid, &fixedCardUpdatePolicy{provider: domain.GatewayMoyasar})
			c, w := portalCardCtx(cid, ep.path, ep.body)
			ep.call(h, c)
			if w.Code != http.StatusConflict {
				t.Fatalf("status = %d body=%s, want 409", w.Code, w.Body.String())
			}
			var out struct {
				Error struct{ Code string } `json:"error"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &out)
			if out.Error.Code != codeCardUpdateUnsupported {
				t.Fatalf("code = %q, want %q", out.Error.Code, codeCardUpdateUnsupported)
			}
			if setup.ensures+setup.intents+setup.finalizes != 0 || store.defaults != 0 {
				t.Fatalf("refused update still reached Stripe (ensure=%d intent=%d finalize=%d) or saved (%d)",
					setup.ensures, setup.intents, setup.finalizes, store.defaults)
			}
		})
	}
}

func TestPortalCardUpdate_SelfServeCustomerProceeds(t *testing.T) {
	for _, policy := range map[string]cardUpdatePolicy{
		"stripe customer": &fixedCardUpdatePolicy{provider: domain.GatewayStripe, selfServe: true},
		"no policy":       nil,
	} {
		for _, ep := range cardUpdateEndpoints {
			t.Run(ep.name, func(t *testing.T) {
				cid := uuid.New()
				h, _, _ := newCardUpdateHandler(cid, policy)
				c, w := portalCardCtx(cid, ep.path, ep.body)
				ep.call(h, c)
				if w.Code == http.StatusConflict {
					t.Fatalf("self-serve customer refused: %s", w.Body.String())
				}
			})
		}
	}
}

type profileCustomerRepo struct {
	port.CustomerRepository
	cust *domain.Customer
}

func (r *profileCustomerRepo) GetByIDPublic(context.Context, uuid.UUID) (*domain.Customer, error) {
	return r.cust, nil
}

func TestPortalProfile_ExposesCardUpdateProvider(t *testing.T) {
	cases := map[string]struct {
		policy        cardUpdatePolicy
		wantProvider  string
		wantSupported bool
	}{
		"gateway without setup": {&fixedCardUpdatePolicy{provider: domain.GatewayMoyasar}, "moyasar", false},
		"stripe":                {&fixedCardUpdatePolicy{provider: domain.GatewayStripe, selfServe: true}, "stripe", true},
		"no policy":             {nil, "stripe", true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cid := uuid.New()
			cardName := "Acme"
			h := NewPortalAPIHandler(service.NewPortalService(&profileCustomerRepo{cust: &domain.Customer{ID: cid, Email: "a@b.co", Name: &cardName}}, nil, nil, nil, nil, nil, nil, ""))
			if tc.policy != nil {
				h.SetCardUpdatePolicy(tc.policy)
			}
			gin.SetMode(gin.TestMode)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Set("portal_customer_id", cid)
			c.Request = httptest.NewRequest(http.MethodGet, "/portal/api/profile", nil)
			h.GetProfile(c)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
			}
			var out map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if out["email"] != "a@b.co" || out["id"] != cid.String() {
				t.Fatalf("profile lost the customer fields: %v", out)
			}
			if out["payment_method_provider"] != tc.wantProvider || out["card_update_supported"] != tc.wantSupported {
				t.Fatalf("provider=%v supported=%v, want %s/%v", out["payment_method_provider"], out["card_update_supported"], tc.wantProvider, tc.wantSupported)
			}
			if p, ok := tc.policy.(*fixedCardUpdatePolicy); ok && (len(p.asked) != 1 || p.asked[0] != cid) {
				t.Fatalf("policy asked about %v, want the session customer %s", p.asked, cid)
			}
		})
	}
}
