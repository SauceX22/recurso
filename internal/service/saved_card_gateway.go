package service

import (
	"context"
	"errors"
	"fmt"

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

// ErrSavedMethodNotRoutable means the saved method lives on a different
// gateway than the one the charge's currency routes to. The charge is skipped
// and the invoice is left to dunning and the pay link.
var ErrSavedMethodNotRoutable = errors.New("saved payment method is not on the gateway for this currency")

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
// when currency routes to a different gateway than the one m lives on.
func (r *SavedCardGatewayRouter) ChargerFor(ctx context.Context, m domain.SavedPaymentMethod, currency string) (SavedCardCharger, error) {
	if r.routes != nil {
		if want := r.routes.ProviderFor(currency); want != m.Provider {
			return nil, fmt.Errorf("%w: %s method, %s routes to %s", ErrSavedMethodNotRoutable, m.Provider, currency, want)
		}
	}
	if m.GatewayConnectionID == nil {
		c := r.platform[m.Provider]
		if c == nil {
			return nil, fmt.Errorf("no platform charger configured for %s saved methods", m.Provider)
		}
		return c, nil
	}
	connID := *m.GatewayConnectionID
	if r.conns == nil {
		return nil, fmt.Errorf("saved-card gateway connection %s: no connection store configured", connID)
	}
	conn, err := r.conns.GetByID(ctx, connID)
	if err != nil {
		return nil, fmt.Errorf("load saved-card gateway connection %s: %w", connID, err)
	}
	if conn == nil {
		return nil, fmt.Errorf("saved-card gateway connection %s not found", connID)
	}
	if conn.Provider != m.Provider {
		return nil, fmt.Errorf("saved-card gateway connection %s is %s, not %s", connID, conn.Provider, m.Provider)
	}
	build := r.build[conn.Provider]
	if build == nil {
		return nil, fmt.Errorf("saved-card gateway connection %s: %s connections can't charge saved methods", connID, conn.Provider)
	}
	secret, err := r.conns.OpenSecret(conn)
	if err != nil || secret == "" {
		return nil, fmt.Errorf("open saved-card gateway secret for %s: %w", connID, err)
	}
	return build(secret), nil
}

// platformRouter is the router used when none is wired: platform Stripe
// methods charge on c, everything else is refused.
func platformRouter(c SavedCardCharger) savedChargerRouter {
	return NewSavedCardGatewayRouter(nil, nil, c)
}
