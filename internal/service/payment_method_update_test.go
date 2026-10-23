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

// fakeCustomerInvoices serves the two targeted reads the policy may make and
// records their bounds; it has no unbounded history read to call.
type fakeCustomerInvoices struct {
	list       []*domain.Invoice // newest first
	cardSaving []*domain.Invoice // as ListCardSavingInvoices orders them
	pagedLimit []int
	savingCall int
}

func (f *fakeCustomerInvoices) GetByCustomerIDPaged(_ context.Context, _ uuid.UUID, limit, offset int) ([]*domain.Invoice, error) {
	f.pagedLimit = append(f.pagedLimit, limit)
	if offset >= len(f.list) {
		return nil, nil
	}
	return f.list[offset:min(len(f.list), offset+limit)], nil
}

func (f *fakeCustomerInvoices) ListCardSavingInvoices(_ context.Context, _ uuid.UUID, limit int) ([]*domain.Invoice, error) {
	f.savingCall++
	return f.cardSaving[:min(len(f.cardSaving), limit)], nil
}

// routeSAR mimics a deployment that routes SAR to the gateway without a
// portal setup flow.
func routeSAR(_ context.Context, currency string) domain.GatewayProvider {
	if currency == "SAR" {
		return domain.GatewayMoyasar
	}
	return domain.GatewayStripe
}

func newUpdatePolicy(saved domain.SavedPaymentMethod, invoices ...*domain.Invoice) *PaymentMethodUpdatePolicy {
	return NewPaymentMethodUpdatePolicy(fakeSavedMethods{m: saved}, &fakeCustomerInvoices{list: invoices}, routeSAR, domain.GatewayMoyasar)
}

var (
	stripeCard  = domain.SavedPaymentMethod{Provider: domain.GatewayStripe, CustomerRef: "cus_1", PaymentMethodRef: "pm_1"}
	gatewayCard = domain.SavedPaymentMethod{Provider: domain.GatewayMoyasar, PaymentMethodRef: "token_1"}
)

func TestPaymentMethodUpdatePolicy_Provider(t *testing.T) {
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

// The portal calls this on every profile load: it reads at most the latest
// invoice, never the customer's whole history.
func TestPaymentMethodUpdatePolicy_ProviderReadsOnlyTheLatestInvoice(t *testing.T) {
	invoices := &fakeCustomerInvoices{list: []*domain.Invoice{{Currency: "USD"}, {Currency: "SAR"}, {Currency: "SAR"}}}
	p := NewPaymentMethodUpdatePolicy(fakeSavedMethods{m: stripeCard}, invoices, routeSAR, domain.GatewayMoyasar)
	if got := p.Provider(context.Background(), uuid.New(), ""); got != domain.GatewayStripe {
		t.Fatalf("Provider = %q, want stripe from the latest (USD) invoice", got)
	}
	if len(invoices.pagedLimit) != 1 || invoices.pagedLimit[0] != 1 {
		t.Fatalf("paged reads = %v, want one read of limit 1", invoices.pagedLimit)
	}
}

func TestPaymentMethodUpdatePolicy_CardUpdateInvoice(t *testing.T) {
	sarOpen := &domain.Invoice{ID: uuid.New(), Currency: "SAR", Status: domain.InvoiceStatusOpen}
	usdOpen := &domain.Invoice{ID: uuid.New(), Currency: "USD", Status: domain.InvoiceStatusOpen}
	cases := []struct {
		name       string
		cardSaving []*domain.Invoice
		provider   domain.GatewayProvider
		want       uuid.UUID
	}{
		{"first card-saving invoice on the gateway", []*domain.Invoice{sarOpen}, domain.GatewayMoyasar, sarOpen.ID},
		{"skips one collected on another gateway", []*domain.Invoice{usdOpen, sarOpen}, domain.GatewayMoyasar, sarOpen.ID},
		{"none on the gateway", []*domain.Invoice{usdOpen}, domain.GatewayMoyasar, uuid.Nil},
		{"none at all", nil, domain.GatewayMoyasar, uuid.Nil},
		{"self-serve needs none", []*domain.Invoice{usdOpen}, domain.GatewayStripe, uuid.Nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPaymentMethodUpdatePolicy(fakeSavedMethods{}, &fakeCustomerInvoices{cardSaving: tc.cardSaving}, routeSAR, domain.GatewayMoyasar)
			if got := p.CardUpdateInvoice(context.Background(), uuid.New(), tc.provider); got != tc.want {
				t.Fatalf("CardUpdateInvoice = %s, want %s", got, tc.want)
			}
		})
	}
	var nilPolicy *PaymentMethodUpdatePolicy
	if got := nilPolicy.CardUpdateInvoice(context.Background(), uuid.New(), domain.GatewayMoyasar); got != uuid.Nil {
		t.Fatalf("nil policy = %s", got)
	}
}

func TestPaymentMethodUpdatePolicy_UpdatePaymentURL(t *testing.T) {
	open := &domain.Invoice{ID: uuid.New(), Currency: "SAR", Status: domain.InvoiceStatusOpen}
	given := uuid.New()
	const base = "https://app.example.com/"
	withSaving := func(saved domain.SavedPaymentMethod, saving ...*domain.Invoice) *PaymentMethodUpdatePolicy {
		return NewPaymentMethodUpdatePolicy(fakeSavedMethods{m: saved}, &fakeCustomerInvoices{cardSaving: saving}, routeSAR, domain.GatewayMoyasar)
	}
	cases := []struct {
		name      string
		policy    *PaymentMethodUpdatePolicy
		invoiceID uuid.UUID
		want      string
	}{
		{"self-serve customer goes to the portal", withSaving(stripeCard), given, "https://app.example.com/portal/login"},
		{"given invoice's checkout", withSaving(gatewayCard, open), given, "https://app.example.com/checkout/" + given.String()},
		{"card-saving invoice's checkout", withSaving(gatewayCard, open), uuid.Nil, "https://app.example.com/checkout/" + open.ID.String()},
		{"nothing that saves the card falls back to the portal", withSaving(gatewayCard), uuid.Nil, "https://app.example.com/portal/login"},
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
