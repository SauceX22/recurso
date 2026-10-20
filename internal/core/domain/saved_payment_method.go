package domain

import "github.com/google/uuid"

// Saved-payment-method providers. Empty means Stripe: every row saved before
// provider tracking existed is a Stripe pm_* on a cus_* customer.
const (
	SavedMethodStripe  = "stripe"
	SavedMethodMoyasar = "moyasar"
)

// SavedPaymentMethod is the customer's reusable method for off-session
// charges (renewal, dunning retry, wallet auto-recharge), independent of the
// gateway that issued it.
type SavedPaymentMethod struct {
	// Provider is the gateway the method lives on; "" or "stripe" for Stripe.
	Provider string
	// CustomerRef is the gateway-side customer (Stripe cus_*). Moyasar tokens
	// are standalone, so it is empty for them.
	CustomerRef string
	// PaymentMethodRef is the chargeable reference (Stripe pm_*, Moyasar token_*).
	PaymentMethodRef string
	// GatewayConnectionID is the BYO connection the method was saved on; nil
	// means the platform gateway.
	GatewayConnectionID *uuid.UUID
}

// ProviderOrStripe normalizes the legacy empty provider to "stripe".
func (m SavedPaymentMethod) ProviderOrStripe() string {
	if m.Provider == "" {
		return SavedMethodStripe
	}
	return m.Provider
}

// Chargeable reports whether an off-session charge can be attempted: Stripe
// needs both the customer and the payment method; other providers need only
// the method reference.
func (m SavedPaymentMethod) Chargeable() bool {
	if m.PaymentMethodRef == "" {
		return false
	}
	if m.ProviderOrStripe() == SavedMethodStripe {
		return m.CustomerRef != ""
	}
	return true
}
