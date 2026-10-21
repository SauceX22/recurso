package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
)

// SavedCardCharger charges a saved payment method off-session. Satisfied by
// the Stripe, Adyen and Moyasar gateways. It is the capability the renewal,
// dunning and wallet paths need from a gateway.
type SavedCardCharger interface {
	ChargeSavedPaymentMethod(ctx context.Context, req port.SavedMethodCharge) (*port.PaymentResult, error)
}

// savedMethodLookup reads a customer's saved off-session method. Satisfied by
// *db.CustomerRepository.
type savedMethodLookup interface {
	GetSavedPaymentMethod(ctx context.Context, customerID uuid.UUID) (domain.SavedPaymentMethod, error)
}

// savedChargerRouter picks the charger for a saved method. Satisfied by
// *SavedCardGatewayRouter.
type savedChargerRouter interface {
	ChargerFor(ctx context.Context, m domain.SavedPaymentMethod, currency string) (SavedCardCharger, error)
}

// gatewayConnectionOpener loads a stored gateway connection and decrypts its
// secret. Satisfied by *GatewayConnectionService.
type gatewayConnectionOpener interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.GatewayConnection, error)
	OpenSecret(conn *domain.GatewayConnection) (string, error)
}

// currencyProviders names the gateway that collects a currency. Satisfied by
// *gateway.SmartRouter.
type currencyProviders interface {
	ProviderFor(currency string) domain.GatewayProvider
}

// ErrSavedMethodNotChargeable means the saved method can never be charged
// off-session as things stand (its gateway isn't configured, its BYO
// connection is gone or belongs to another gateway), so the caller falls back
// to dunning and the pay link. Any other ChargerFor error is transient (the
// connection store or vault failed) and worth retrying.
var ErrSavedMethodNotChargeable = errors.New("saved payment method can't be charged off-session")

// ErrSavedMethodNotRoutable means the saved method lives on a different
// gateway than the one the charge's currency routes to. It is a kind of
// ErrSavedMethodNotChargeable.
var ErrSavedMethodNotRoutable = fmt.Errorf("%w: not on the gateway for this currency", ErrSavedMethodNotChargeable)

// SavedCardGatewayRouter returns the off-session charger a saved method must
// be charged on. A method's reference is only valid on the gateway account
// that issued it, so each charge routes on (provider, connection):
//
//   - nil connection → the platform charger registered for the provider;
//   - a connection   → that tenant's BYO gateway, built from the stored
//     (decrypted) credentials.
//
// A connection that can't be loaded or opened is a hard error, not a silent
// fall back to the platform: charging a BYO-saved card on the platform account
// would always fail anyway, and the caller leaves the invoice open for dunning.
type SavedCardGatewayRouter struct {
	conns    gatewayConnectionOpener
	build    map[domain.GatewayProvider]func(secret string) SavedCardCharger
	platform map[domain.GatewayProvider]SavedCardCharger
	routes   currencyProviders
}

// NewSavedCardGatewayRouter builds the router with Stripe registered: platform
// is the env Stripe charger (nil when Stripe isn't configured), and buildStripe
// constructs a Stripe charger from a BYO connection's decrypted secret.
func NewSavedCardGatewayRouter(conns gatewayConnectionOpener, buildStripe func(secret string) SavedCardCharger, platform SavedCardCharger) *SavedCardGatewayRouter {
	r := &SavedCardGatewayRouter{
		conns:    conns,
		build:    map[domain.GatewayProvider]func(string) SavedCardCharger{},
		platform: map[domain.GatewayProvider]SavedCardCharger{},
	}
	if buildStripe != nil {
		r.build[domain.GatewayStripe] = buildStripe
	}
	r.RegisterProvider(domain.GatewayStripe, platform)
	return r
}

// RegisterProvider makes methods saved on provider's platform account
// chargeable through c. A nil charger registers nothing.
func (r *SavedCardGatewayRouter) RegisterProvider(provider domain.GatewayProvider, c SavedCardCharger) {
	if c == nil {
		return
	}
	r.platform[provider] = c
}

// SetCurrencyRouting enables the routing guard: a method is charged only when
// its provider is the gateway the charge's currency routes to. Without it the
// guard is off.
func (r *SavedCardGatewayRouter) SetCurrencyRouting(p currencyProviders) {
	r.routes = p
}

// ChargerFor returns the charger for m, refusing with ErrSavedMethodNotRoutable
// when currency routes to a different saved-card gateway than the one m lives on.
func (r *SavedCardGatewayRouter) ChargerFor(ctx context.Context, m domain.SavedPaymentMethod, currency string) (SavedCardCharger, error) {
	if r.routes != nil {
		// A Stripe card keeps reaching currencies whose gateway can't charge a
		// saved method (INR on Razorpay, EUR on GoCardless), as it always has.
		want := r.routes.ProviderFor(currency)
		_, wantCharges := r.platform[want]
		if want != m.Provider && (m.Provider != domain.GatewayStripe || wantCharges) {
			return nil, fmt.Errorf("%w: %s method, %s routes to %s", ErrSavedMethodNotRoutable, m.Provider, currency, want)
		}
	}
	if m.GatewayConnectionID == nil {
		c := r.platform[m.Provider]
		if c == nil {
			return nil, fmt.Errorf("%w: no platform charger configured for %s saved methods", ErrSavedMethodNotChargeable, m.Provider)
		}
		return c, nil
	}
	connID := *m.GatewayConnectionID
	if r.conns == nil {
		return nil, fmt.Errorf("%w: saved-card gateway connection %s: no connection store configured", ErrSavedMethodNotChargeable, connID)
	}
	conn, err := r.conns.GetByID(ctx, connID)
	if err != nil {
		return nil, fmt.Errorf("load saved-card gateway connection %s: %w", connID, err)
	}
	if conn == nil {
		return nil, fmt.Errorf("%w: saved-card gateway connection %s not found", ErrSavedMethodNotChargeable, connID)
	}
	if conn.Provider != m.Provider {
		return nil, fmt.Errorf("%w: saved-card gateway connection %s is %s, not %s", ErrSavedMethodNotChargeable, connID, conn.Provider, m.Provider)
	}
	build := r.build[conn.Provider]
	if build == nil {
		return nil, fmt.Errorf("%w: saved-card gateway connection %s: %s connections can't charge saved methods", ErrSavedMethodNotChargeable, connID, conn.Provider)
	}
	secret, err := r.conns.OpenSecret(conn)
	if err != nil {
		return nil, fmt.Errorf("open saved-card gateway secret for %s: %w", connID, err)
	}
	if secret == "" {
		return nil, fmt.Errorf("%w: saved-card gateway connection %s has no secret", ErrSavedMethodNotChargeable, connID)
	}
	return build(secret), nil
}

// logChargeFailure logs a failed off-session charge with what an operator
// needs: the error for an outage, or the decline itself (payment id, decline
// code, hard or soft, the gateway's message).
func logChargeFailure(ctx context.Context, msg string, result *port.PaymentResult, err error, attrs ...any) {
	switch {
	case err != nil:
		attrs = append(attrs, "error", err)
	case result == nil:
		attrs = append(attrs, "error", "gateway returned no result")
	default:
		attrs = append(attrs, "payment_id", result.PaymentID, "error_code", result.ErrorCode,
			"permanent", result.Permanent, "message", result.ErrorMsg)
	}
	slog.WarnContext(ctx, msg, attrs...)
}
