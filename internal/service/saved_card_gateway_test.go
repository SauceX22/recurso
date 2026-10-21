package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
)

type fakeConnOpener struct {
	conn   *domain.GatewayConnection
	getErr error
	secret string
	secErr error
}

func (f *fakeConnOpener) GetByID(_ context.Context, _ uuid.UUID) (*domain.GatewayConnection, error) {
	return f.conn, f.getErr
}
func (f *fakeConnOpener) OpenSecret(_ *domain.GatewayConnection) (string, error) {
	return f.secret, f.secErr
}

// namedCharger records which gateway a charge was routed to.
type namedCharger struct{ name string }

func (c *namedCharger) ChargeSavedPaymentMethod(_ context.Context, _ port.SavedMethodCharge) (*port.PaymentResult, error) {
	return &port.PaymentResult{Success: true}, nil
}

func stripeMethod(connID *uuid.UUID) domain.SavedPaymentMethod {
	return domain.SavedPaymentMethod{Provider: domain.GatewayStripe, CustomerRef: "cus_1", PaymentMethodRef: "pm_1", GatewayConnectionID: connID}
}

func moyasarMethod() domain.SavedPaymentMethod {
	return domain.SavedPaymentMethod{Provider: domain.GatewayMoyasar, PaymentMethodRef: "token_1"}
}

func TestSavedCardGatewayRouter_ChargerFor(t *testing.T) {
	platform := &namedCharger{name: "platform"}
	build := func(secret string) SavedCardCharger { return &namedCharger{name: "byo:" + secret} }

	t.Run("nil connection -> platform", func(t *testing.T) {
		r := NewSavedCardGatewayRouter(&fakeConnOpener{}, build, platform)
		got, err := r.ChargerFor(context.Background(), stripeMethod(nil), "USD")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got != SavedCardCharger(platform) {
			t.Fatalf("nil connection should route to platform, got %#v", got)
		}
	})

	t.Run("stripe connection -> byo built from secret", func(t *testing.T) {
		connID := uuid.New()
		opener := &fakeConnOpener{
			conn:   &domain.GatewayConnection{ID: connID, Provider: domain.GatewayStripe, SecretKeyEnc: "sealed"},
			secret: "sk_byo",
		}
		r := NewSavedCardGatewayRouter(opener, build, platform)
		got, err := r.ChargerFor(context.Background(), stripeMethod(&connID), "USD")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		nc, ok := got.(*namedCharger)
		if !ok || nc.name != "byo:sk_byo" {
			t.Fatalf("stripe connection should build BYO charger from its secret, got %#v", got)
		}
	})

	t.Run("connection load error -> error (dunning)", func(t *testing.T) {
		connID := uuid.New()
		r := NewSavedCardGatewayRouter(&fakeConnOpener{getErr: errors.New("db down")}, build, platform)
		if _, err := r.ChargerFor(context.Background(), stripeMethod(&connID), "USD"); err == nil {
			t.Fatal("expected error when the connection can't be loaded")
		}
	})

	t.Run("missing connection -> error", func(t *testing.T) {
		connID := uuid.New()
		r := NewSavedCardGatewayRouter(&fakeConnOpener{conn: nil}, build, platform)
		if _, err := r.ChargerFor(context.Background(), stripeMethod(&connID), "USD"); err == nil {
			t.Fatal("expected error when the connection is gone")
		}
	})

	t.Run("connection of another provider -> error", func(t *testing.T) {
		connID := uuid.New()
		opener := &fakeConnOpener{conn: &domain.GatewayConnection{ID: connID, Provider: domain.GatewayRazorpay, SecretKeyEnc: "sealed"}, secret: "s"}
		r := NewSavedCardGatewayRouter(opener, build, platform)
		if _, err := r.ChargerFor(context.Background(), stripeMethod(&connID), "USD"); err == nil {
			t.Fatal("expected error for a stripe method on a razorpay connection")
		}
	})

	t.Run("connection provider without a builder -> error", func(t *testing.T) {
		connID := uuid.New()
		opener := &fakeConnOpener{conn: &domain.GatewayConnection{ID: connID, Provider: domain.GatewayRazorpay, SecretKeyEnc: "sealed"}, secret: "s"}
		r := NewSavedCardGatewayRouter(opener, build, platform)
		m := domain.SavedPaymentMethod{Provider: domain.GatewayRazorpay, PaymentMethodRef: "tok", GatewayConnectionID: &connID}
		if _, err := r.ChargerFor(context.Background(), m, "INR"); err == nil {
			t.Fatal("expected error when no BYO builder exists for the provider")
		}
	})

	t.Run("secret open failure -> error", func(t *testing.T) {
		connID := uuid.New()
		opener := &fakeConnOpener{conn: &domain.GatewayConnection{ID: connID, Provider: domain.GatewayStripe, SecretKeyEnc: "sealed"}, secErr: errors.New("vault down")}
		r := NewSavedCardGatewayRouter(opener, build, platform)
		if _, err := r.ChargerFor(context.Background(), stripeMethod(&connID), "USD"); err == nil {
			t.Fatal("expected error when the secret can't be opened")
		}
	})
}

func TestSavedCardGatewayRouter_DispatchesOnProvider(t *testing.T) {
	stripe := &namedCharger{name: "stripe"}
	moyasar := &namedCharger{name: "moyasar"}
	r := NewSavedCardGatewayRouter(nil, nil, stripe)
	r.RegisterProvider(domain.GatewayMoyasar, moyasar)

	if got, err := r.ChargerFor(context.Background(), moyasarMethod(), "SAR"); err != nil || got != SavedCardCharger(moyasar) {
		t.Fatalf("moyasar method routed to %#v (%v), want the moyasar charger", got, err)
	}
	if got, err := r.ChargerFor(context.Background(), stripeMethod(nil), "USD"); err != nil || got != SavedCardCharger(stripe) {
		t.Fatalf("stripe method routed to %#v (%v), want the stripe charger", got, err)
	}
}

// A method whose provider has no charger must never fall back to another
// provider's charger: a Moyasar token sent to Stripe always fails, and the
// reverse too.
func TestSavedCardGatewayRouter_UnregisteredProviderRefused(t *testing.T) {
	stripe := &namedCharger{name: "stripe"}
	r := NewSavedCardGatewayRouter(nil, nil, stripe)
	if got, err := r.ChargerFor(context.Background(), moyasarMethod(), "SAR"); err == nil {
		t.Fatalf("unregistered moyasar method resolved to %#v, want an error", got)
	}

	noStripe := NewSavedCardGatewayRouter(nil, nil, nil)
	noStripe.RegisterProvider(domain.GatewayMoyasar, &namedCharger{name: "moyasar"})
	if got, err := noStripe.ChargerFor(context.Background(), stripeMethod(nil), "USD"); err == nil {
		t.Fatalf("stripe method with no stripe charger resolved to %#v, want an error", got)
	}
}

// fixedRoutes routes every currency in the map to its gateway, and anything
// else to Stripe.
type fixedRoutes map[string]domain.GatewayProvider

func (f fixedRoutes) ProviderFor(currency string) domain.GatewayProvider {
	if p, ok := f[currency]; ok {
		return p
	}
	return domain.GatewayStripe
}

// A saved method is charged only on the gateway its charge's currency routes
// to: a customer with one slot can't have their USD renewal sent to Moyasar,
// or their SAR renewal sent to Stripe. A Stripe card keeps its historical
// reach into currencies whose gateway has no saved-card charger (INR on
// Razorpay, EUR on GoCardless).
func TestSavedCardGatewayRouter_CurrencyGuard(t *testing.T) {
	stripe := &namedCharger{name: "stripe"}
	moyasar := &namedCharger{name: "moyasar"}
	r := NewSavedCardGatewayRouter(nil, nil, stripe)
	r.RegisterProvider(domain.GatewayMoyasar, moyasar)
	r.SetCurrencyRouting(fixedRoutes{"SAR": domain.GatewayMoyasar, "INR": domain.GatewayRazorpay, "EUR": domain.GatewayGoCardless})

	for _, tc := range []struct {
		name     string
		m        domain.SavedPaymentMethod
		currency string
		want     SavedCardCharger
	}{
		{"moyasar token for SAR", moyasarMethod(), "SAR", moyasar},
		{"stripe card for USD", stripeMethod(nil), "USD", stripe},
		{"stripe card for SAR", stripeMethod(nil), "SAR", nil},
		{"moyasar token for USD", moyasarMethod(), "USD", nil},
		{"stripe card for INR", stripeMethod(nil), "INR", stripe},
		{"stripe card for EUR", stripeMethod(nil), "EUR", stripe},
		{"moyasar token for INR", moyasarMethod(), "INR", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.ChargerFor(context.Background(), tc.m, tc.currency)
			if tc.want == nil {
				if !errors.Is(err, ErrSavedMethodNotRoutable) || got != nil {
					t.Fatalf("got (%#v, %v), want ErrSavedMethodNotRoutable", got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got (%#v, %v), want %#v", got, err, tc.want)
			}
		})
	}
}

// The guard runs before any BYO connection is loaded or decrypted.
func TestSavedCardGatewayRouter_CurrencyGuardBeforeConnection(t *testing.T) {
	connID := uuid.New()
	opener := &fakeConnOpener{getErr: errors.New("must not be called")}
	r := NewSavedCardGatewayRouter(opener, func(string) SavedCardCharger { return &namedCharger{} }, nil)
	r.RegisterProvider(domain.GatewayMoyasar, &namedCharger{name: "moyasar"})
	r.SetCurrencyRouting(fixedRoutes{"SAR": domain.GatewayMoyasar})
	if _, err := r.ChargerFor(context.Background(), stripeMethod(&connID), "SAR"); !errors.Is(err, ErrSavedMethodNotRoutable) {
		t.Fatalf("err = %v, want ErrSavedMethodNotRoutable", err)
	}
}

func TestSavedCardGatewayRouter_NilChargerNotRegistered(t *testing.T) {
	r := NewSavedCardGatewayRouter(nil, nil, nil)
	r.RegisterProvider(domain.GatewayMoyasar, nil)
	if _, err := r.ChargerFor(context.Background(), moyasarMethod(), "SAR"); err == nil {
		t.Fatal("a nil charger must not be registered")
	}
}

func TestSavedCardGatewayRouter_BYOConnectionWithoutStoreRefused(t *testing.T) {
	connID := uuid.New()
	r := NewSavedCardGatewayRouter(nil, nil, &namedCharger{name: "platform"})
	if got, err := r.ChargerFor(context.Background(), stripeMethod(&connID), "USD"); err == nil {
		t.Fatalf("BYO method without a connection store resolved to %#v; it must never charge the platform account", got)
	}
}

// Callers retry a transient failure but give up on a method that can never be
// charged as configured, so ChargerFor must say which one it is. A Moyasar
// token pointing at a Stripe connection is never charged, and no charger is
// built for it.
func TestSavedCardGatewayRouter_ErrorKinds(t *testing.T) {
	connID := uuid.New()
	stripeConn := &domain.GatewayConnection{ID: connID, Provider: domain.GatewayStripe}
	moyasarOnStripeConn := moyasarMethod()
	moyasarOnStripeConn.GatewayConnectionID = &connID
	for _, tc := range []struct {
		name          string
		opener        *fakeConnOpener
		m             domain.SavedPaymentMethod
		notChargeable bool
	}{
		{"connection store down", &fakeConnOpener{getErr: errors.New("timeout")}, stripeMethod(&connID), false},
		{"secret can't be opened", &fakeConnOpener{conn: stripeConn, secErr: errors.New("vault sealed")}, stripeMethod(&connID), false},
		{"connection gone", &fakeConnOpener{}, stripeMethod(&connID), true},
		{"connection of another provider", &fakeConnOpener{conn: stripeConn, secret: "sk_x"}, moyasarOnStripeConn, true},
		{"provider without a platform charger", &fakeConnOpener{}, moyasarMethod(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			built := false
			r := NewSavedCardGatewayRouter(tc.opener, func(string) SavedCardCharger { built = true; return &namedCharger{} }, &namedCharger{name: "stripe"})
			got, err := r.ChargerFor(context.Background(), tc.m, "USD")
			if err == nil || got != nil {
				t.Fatalf("got (%#v, %v), want an error", got, err)
			}
			if errors.Is(err, ErrSavedMethodNotChargeable) != tc.notChargeable {
				t.Fatalf("errors.Is(%v, ErrSavedMethodNotChargeable) = %v, want %v", err, !tc.notChargeable, tc.notChargeable)
			}
			if built {
				t.Fatal("a charger was built although ChargerFor failed")
			}
		})
	}
	if !errors.Is(ErrSavedMethodNotRoutable, ErrSavedMethodNotChargeable) {
		t.Fatal("a method on another gateway than the currency's must count as not chargeable")
	}
}
