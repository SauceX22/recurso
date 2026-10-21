package service

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
)

// customerInvoiceLister lists a customer's invoices, newest first. Satisfied
// by *db.InvoiceRepository.
type customerInvoiceLister interface {
	GetByCustomerID(ctx context.Context, customerID uuid.UUID) ([]*domain.Invoice, error)
}

// PaymentMethodUpdatePolicy decides how a customer replaces the card their
// renewals charge. The portal's self-serve flow saves a Stripe card; for a
// gateway without a setup flow that would silently move the customer's
// renewals off their gateway, so such customers replace the card by paying
// an open invoice on the hosted checkout instead.
type PaymentMethodUpdatePolicy struct {
	methods     savedMethodLookup
	invoices    customerInvoiceLister
	providerFor func(currency string) domain.GatewayProvider
	noSelfServe map[domain.GatewayProvider]bool
}

// NewPaymentMethodUpdatePolicy builds the policy. providerFor reports the
// gateway a currency routes to (nil: unknown); noSelfServe lists the gateways
// whose saved cards cannot be replaced from the portal.
func NewPaymentMethodUpdatePolicy(methods savedMethodLookup, invoices customerInvoiceLister, providerFor func(currency string) domain.GatewayProvider, noSelfServe ...domain.GatewayProvider) *PaymentMethodUpdatePolicy {
	p := &PaymentMethodUpdatePolicy{methods: methods, invoices: invoices, providerFor: providerFor, noSelfServe: map[domain.GatewayProvider]bool{}}
	for _, g := range noSelfServe {
		p.noSelfServe[g] = true
	}
	return p
}

// SelfServe reports whether provider's saved cards can be replaced from the
// portal.
func (p *PaymentMethodUpdatePolicy) SelfServe(provider domain.GatewayProvider) bool {
	return p == nil || !p.noSelfServe[provider]
}

// Provider returns the gateway that governs the customer's card update: the
// gateway of the saved method or the one the billing currency routes to,
// whichever lacks self-serve, else the saved method's gateway (Stripe when
// nothing is saved). currency "" means the latest invoice's currency.
func (p *PaymentMethodUpdatePolicy) Provider(ctx context.Context, customerID uuid.UUID, currency string) domain.GatewayProvider {
	if p == nil {
		return domain.GatewayStripe
	}
	saved := p.savedProvider(ctx, customerID)
	if saved != "" && !p.SelfServe(saved) {
		return saved
	}
	if currency == "" {
		if inv := p.latestInvoice(ctx, customerID, false); inv != nil {
			currency = inv.Currency
		}
	}
	if currency != "" && p.providerFor != nil {
		if routed := p.providerFor(currency); routed != "" && !p.SelfServe(routed) {
			return routed
		}
	}
	if saved != "" {
		return saved
	}
	return domain.GatewayStripe
}

// UpdatePaymentURL is the "update payment method" link for a recovery or
// card-expiry email: the portal for self-serve customers, otherwise the
// hosted checkout of invoiceID (or the customer's latest open invoice), whose
// payment saves the new card. Falls back to the portal when there is no
// invoice to pay.
func (p *PaymentMethodUpdatePolicy) UpdatePaymentURL(ctx context.Context, baseURL string, customerID uuid.UUID, currency string, invoiceID uuid.UUID) string {
	base := strings.TrimRight(baseURL, "/")
	portal := base + "/portal/login"
	if p.SelfServe(p.Provider(ctx, customerID, currency)) {
		return portal
	}
	if invoiceID == uuid.Nil {
		if inv := p.latestInvoice(ctx, customerID, true); inv != nil {
			invoiceID = inv.ID
		}
	}
	if invoiceID == uuid.Nil {
		return portal
	}
	return base + "/checkout/" + invoiceID.String()
}

func (p *PaymentMethodUpdatePolicy) savedProvider(ctx context.Context, customerID uuid.UUID) domain.GatewayProvider {
	if p.methods == nil {
		return ""
	}
	m, err := p.methods.GetSavedPaymentMethod(ctx, customerID)
	if err != nil || m.PaymentMethodRef == "" {
		return ""
	}
	if provider := m.Provider; provider != "" {
		return provider
	}
	return domain.GatewayStripe
}

func (p *PaymentMethodUpdatePolicy) latestInvoice(ctx context.Context, customerID uuid.UUID, payableOnly bool) *domain.Invoice {
	if p.invoices == nil {
		return nil
	}
	invoices, err := p.invoices.GetByCustomerID(ctx, customerID)
	if err != nil {
		return nil
	}
	for _, inv := range invoices {
		switch {
		case inv.Status == domain.InvoiceStatusVoid || inv.Status == domain.InvoiceStatusDraft:
		case !payableOnly:
			return inv
		case inv.Status == domain.InvoiceStatusOpen || inv.Status == domain.InvoiceStatusPastDue:
			return inv
		}
	}
	return nil
}
