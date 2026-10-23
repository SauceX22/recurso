package domain

import "github.com/google/uuid"

// SavedPaymentMethod is the customer's reusable method for off-session
// charges (renewal, dunning retry, wallet auto-recharge). Provider and
// GatewayConnectionID together say where the method lives: a reference is
// only chargeable on the gateway account that issued it.
type SavedPaymentMethod struct {
	// Provider is the gateway that issued PaymentMethodRef.
	Provider GatewayProvider
	// CustomerRef is the gateway-side customer the method is attached to
	// (Stripe cus_*). Empty for gateways without customer objects.
	CustomerRef string
	// PaymentMethodRef is the chargeable reference (Stripe pm_*, Moyasar token_*).
	PaymentMethodRef string
	// GatewayConnectionID is the BYO connection the method was saved on; nil
	// means the platform (env) gateway.
	GatewayConnectionID *uuid.UUID
}

// Chargeable reports whether an off-session charge can be attempted. Stripe
// charges a payment method through its customer, so it needs both refs.
func (m SavedPaymentMethod) Chargeable() bool {
	if m.PaymentMethodRef == "" {
		return false
	}
	if m.Provider == GatewayStripe {
		return m.CustomerRef != ""
	}
	return m.Provider != ""
}
