package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
)

type fakeSavedMethods struct {
	m   domain.SavedPaymentMethod
	err error
}

func (f fakeSavedMethods) GetSavedPaymentMethod(context.Context, uuid.UUID) (domain.SavedPaymentMethod, error) {
	return f.m, f.err
}

type fakeCustomerInvoices struct{ list []*domain.Invoice }

func (f fakeCustomerInvoices) GetByCustomerID(context.Context, uuid.UUID) ([]*domain.Invoice, error) {
	return f.list, nil
}

// routeSAR mimics a deployment that routes SAR to the gateway without a
// portal setup flow.
func routeSAR(currency string) domain.GatewayProvider {
	if currency == "SAR" {
		return domain.GatewayMoyasar
	}
	return domain.GatewayStripe
}

func newUpdatePolicy(saved domain.SavedPaymentMethod, invoices ...*domain.Invoice) *PaymentMethodUpdatePolicy {
	return NewPaymentMethodUpdatePolicy(fakeSavedMethods{m: saved}, fakeCustomerInvoices{list: invoices}, routeSAR, domain.GatewayMoyasar)
}

func TestPaymentMethodUpdatePolicy_Provider(t *testing.T) {
	stripeCard := domain.SavedPaymentMethod{CustomerRef: "cus_1", PaymentMethodRef: "pm_1"}
	gatewayCard := domain.SavedPaymentMethod{Provider: "moyasar", PaymentMethodRef: "token_1"}
	sarInvoice := &domain.Invoice{ID: uuid.New(), Currency: "SAR", Status: domain.InvoiceStatusPaid}
	usdInvoice := &domain.Invoice{ID: uuid.New(), Currency: "USD", Status: domain.InvoiceStatusPaid}
	cases := []struct {
		name     string
		policy   *PaymentMethodUpdatePolicy
		currency string
		want     domain.GatewayProvider
	}{
		{"saved card on a gateway without setup", newUpdatePolicy(gatewayCard, usdInvoice), "", domain.GatewayMoyasar},
		{"stripe card but SAR billing", newUpdatePolicy(stripeCard), "SAR", domain.GatewayMoyasar},
		{"nothing saved, latest invoice SAR", newUpdatePolicy(domain.SavedPaymentMethod{}, sarInvoice, usdInvoice), "", domain.GatewayMoyasar},
		{"stripe card, USD billing", newUpdatePolicy(stripeCard, usdInvoice), "", domain.GatewayStripe},
		{"nothing saved, no invoices", newUpdatePolicy(domain.SavedPaymentMethod{}), "", domain.GatewayStripe},
		{"nil policy", nil, "SAR", domain.GatewayStripe},
		{"lookup error falls back to routing", NewPaymentMethodUpdatePolicy(fakeSavedMethods{err: errors.New("db")}, nil, routeSAR, domain.GatewayMoyasar), "SAR", domain.GatewayMoyasar},
		{"no blocked gateways", NewPaymentMethodUpdatePolicy(fakeSavedMethods{m: stripeCard}, nil, routeSAR), "SAR", domain.GatewayStripe},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.policy.Provider(context.Background(), uuid.New(), tc.currency)
			if got != tc.want {
				t.Fatalf("Provider = %q, want %q", got, tc.want)
			}
			if wantSelfServe := tc.want != domain.GatewayMoyasar; tc.policy.SelfServe(got) != wantSelfServe {
				t.Fatalf("SelfServe(%q) = %v, want %v", got, !wantSelfServe, wantSelfServe)
			}
		})
	}
}

func TestPaymentMethodUpdatePolicy_UpdatePaymentURL(t *testing.T) {
	gatewayCard := domain.SavedPaymentMethod{Provider: "moyasar", PaymentMethodRef: "token_1"}
	stripeCard := domain.SavedPaymentMethod{CustomerRef: "cus_1", PaymentMethodRef: "pm_1"}
	open := &domain.Invoice{ID: uuid.New(), Currency: "SAR", Status: domain.InvoiceStatusOpen}
	paid := &domain.Invoice{ID: uuid.New(), Currency: "SAR", Status: domain.InvoiceStatusPaid}
	given := uuid.New()
	const base = "https://app.example.com/"
	cases := []struct {
		name      string
		policy    *PaymentMethodUpdatePolicy
		invoiceID uuid.UUID
		want      string
	}{
		{"self-serve customer goes to the portal", newUpdatePolicy(stripeCard), given, "https://app.example.com/portal/login"},
		{"given invoice's checkout", newUpdatePolicy(gatewayCard, open), given, "https://app.example.com/checkout/" + given.String()},
		{"latest open invoice's checkout", newUpdatePolicy(gatewayCard, paid, open), uuid.Nil, "https://app.example.com/checkout/" + open.ID.String()},
		{"nothing to pay falls back to the portal", newUpdatePolicy(gatewayCard, paid), uuid.Nil, "https://app.example.com/portal/login"},
		{"nil policy keeps the portal", nil, given, "https://app.example.com/portal/login"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.policy.UpdatePaymentURL(context.Background(), base, uuid.New(), "", tc.invoiceID); got != tc.want {
				t.Fatalf("UpdatePaymentURL = %q, want %q", got, tc.want)
			}
		})
	}
}
