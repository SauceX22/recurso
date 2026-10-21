package service

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
)

// cardUpdateInvoiceReader is the bounded invoice reads the card-update policy
// makes: the customer's latest invoice (its billing currency) and the
// invoices whose payment would save the card. Satisfied by
// *db.InvoiceRepository.
type cardUpdateInvoiceReader interface {
	GetByCustomerIDPaged(ctx context.Context, customerID uuid.UUID, limit, offset int) ([]*domain.Invoice, error)
	ListCardSavingInvoices(ctx context.Context, customerID uuid.UUID, limit int) ([]*domain.Invoice, error)
}

// cardSavingCandidates bounds the card-saving scan; a customer rarely has more
// than one open renewal invoice.
const cardSavingCandidates = 10

// PaymentMethodUpdatePolicy decides how a customer replaces the card their
// renewals charge. The portal's self-serve flow saves a Stripe card; for a
// gateway without a setup flow that would silently move the customer's
// renewals off their gateway, so such customers replace the card by paying
// an open renewal invoice on the hosted checkout instead.
type PaymentMethodUpdatePolicy struct {
	methods     savedMethodLookup
	invoices    cardUpdateInvoiceReader
	providerFor func(currency string) domain.GatewayProvider
	noSelfServe map[domain.GatewayProvider]bool
}

// NewPaymentMethodUpdatePolicy builds the policy. providerFor reports the
// gateway a currency routes to (nil: unknown); noSelfServe lists the gateways
// whose saved cards cannot be replaced from the portal. With none listed
// every customer is self-serve.
func NewPaymentMethodUpdatePolicy(methods savedMethodLookup, invoices cardUpdateInvoiceReader, providerFor func(currency string) domain.GatewayProvider, noSelfServe ...domain.GatewayProvider) *PaymentMethodUpdatePolicy {
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
	if currency == "" && p.invoices != nil {
		if latest, err := p.invoices.GetByCustomerIDPaged(ctx, customerID, 1, 0); err == nil && len(latest) > 0 {
			currency = latest[0].Currency
		}
	}
	if currency != "" && p.providerFor != nil {
		if routed := p.providerFor(currency); !p.SelfServe(routed) {
			return routed
		}
	}
	if saved != "" {
		return saved
	}
	return domain.GatewayStripe
}

// CardUpdateInvoice returns the invoice whose payment replaces the card for a
// customer governed by provider: the oldest open renewal invoice collected
// on provider. uuid.Nil when provider is self-serve or no invoice would save
// the card (the customer is asked for it at the next renewal).
func (p *PaymentMethodUpdatePolicy) CardUpdateInvoice(ctx context.Context, customerID uuid.UUID, provider domain.GatewayProvider) uuid.UUID {
	if p.SelfServe(provider) || p.invoices == nil {
		return uuid.Nil
	}
	candidates, err := p.invoices.ListCardSavingInvoices(ctx, customerID, cardSavingCandidates)
	if err != nil {
		return uuid.Nil
	}
	for _, inv := range candidates {
		if p.providerFor == nil || p.providerFor(inv.Currency) == provider {
			return inv.ID
		}
	}
	return uuid.Nil
}

// UpdatePaymentURL is the "update payment method" link for a recovery or
// card-expiry email: the portal for self-serve customers, otherwise the
// hosted checkout of invoiceID (or the invoice CardUpdateInvoice picks),
// whose payment saves the new card. Falls back to the portal, which explains
// what happens, when there is no such invoice.
func (p *PaymentMethodUpdatePolicy) UpdatePaymentURL(ctx context.Context, baseURL string, customerID uuid.UUID, currency string, invoiceID uuid.UUID) string {
	base := strings.TrimRight(baseURL, "/")
	portal := base + "/portal/login"
	provider := p.Provider(ctx, customerID, currency)
	if p.SelfServe(provider) {
		return portal
	}
	if invoiceID == uuid.Nil {
		invoiceID = p.CardUpdateInvoice(ctx, customerID, provider)
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
	return m.Provider
}
