package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
)

// stubGateway records its name on every order it creates so tests can assert
// which gateway a call routed to. Only the methods exercised here are
// meaningful; the rest satisfy the interface.
type stubGateway struct{ name string }

func (s *stubGateway) CreateOrder(_ context.Context, amount int64, currency, receipt, invoiceID string) (*port.PaymentOrder, error) {
	return &port.PaymentOrder{ID: "order_x", Amount: amount, Currency: currency, Gateway: s.name}, nil
}
func (s *stubGateway) VerifyPayment(context.Context, string, string, string) error { return nil }
func (s *stubGateway) CreateSubscription(context.Context, string, int, string, *int64, string) (string, error) {
	return "", nil
}
func (s *stubGateway) RetryPayment(context.Context, string, int64, string) (*port.PaymentResult, error) {
	return &port.PaymentResult{PaymentID: s.name}, nil
}
func (s *stubGateway) CreateMandate(context.Context, string, string, string, int64, string, string) (*port.MandateResult, error) {
	return &port.MandateResult{TokenID: s.name}, nil
}
func (s *stubGateway) ExecuteMandateDebit(context.Context, port.MandateDebitRequest) (*port.PaymentResult, error) {
	return &port.PaymentResult{PaymentID: s.name}, nil
}
func (s *stubGateway) RevokeMandate(context.Context, string, string, string) error { return nil }
func (s *stubGateway) CreateVirtualAccount(context.Context, string, string, int64, string) (*port.VirtualAccountResult, error) {
	return &port.VirtualAccountResult{VAID: s.name}, nil
}
func (s *stubGateway) CancelSubscription(context.Context, string) error { return nil }
func (s *stubGateway) Refund(context.Context, string, int64, string) (*port.RefundResult, error) {
	return &port.RefundResult{RefundID: s.name}, nil
}

// fakeVault is an in-memory ConnectionVault. Secrets are stored plaintext for
// the test; OpenSecret just returns them.
type fakeVault struct {
	conns map[uuid.UUID][]*domain.GatewayConnection
}

func (v *fakeVault) List(_ context.Context, tenantID uuid.UUID) ([]*domain.GatewayConnection, error) {
	return v.conns[tenantID], nil
}
func (v *fakeVault) OpenSecret(conn *domain.GatewayConnection) (string, error) {
	return conn.SecretKeyEnc, nil // plaintext in the fake
}

// newTestResolver wires a resolver whose env slots and per-tenant builds all
// produce named stubs, so routing is observable without any SDK/network.
func newTestResolver(vault ConnectionVault) *GatewayResolver {
	env := NewSmartRouter(&stubGateway{name: "env-razorpay"}, &stubGateway{name: "env-stripe"})
	r := NewGatewayResolver(vault, env)
	r.buildRazorpay = func(keyID, secret string) port.PaymentGateway { return &stubGateway{name: "byo-razorpay:" + secret} }
	r.buildStripe = func(secret string) port.PaymentGateway { return &stubGateway{name: "byo-stripe:" + secret} }
	return r
}

func orderGateway(t *testing.T, gw port.PaymentGateway, ctx context.Context, currency string) string {
	t.Helper()
	o, err := gw.CreateOrder(ctx, 100, currency, "rcpt", "inv")
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	return o.Gateway
}

func TestTenantGatewayFallsBackWithoutTenant(t *testing.T) {
	vault := &fakeVault{conns: map[uuid.UUID][]*domain.GatewayConnection{}}
	tg := NewTenantGateway(newTestResolver(vault), &stubGateway{name: "env-direct"})

	// No tenant in ctx -> env fallback (the wrapper's own env, not the router).
	if got := orderGateway(t, tg, context.Background(), "USD"); got != "env-direct" {
		t.Fatalf("no-tenant: got %q want env-direct", got)
	}
}

func TestTenantGatewayFallsBackWhenNoConnection(t *testing.T) {
	tenant := uuid.New()
	vault := &fakeVault{conns: map[uuid.UUID][]*domain.GatewayConnection{}} // tenant has none
	resolver := newTestResolver(vault)
	tg := NewTenantGateway(resolver, resolver.env)

	ctx := context.WithValue(context.Background(), domain.TenantIDKey, tenant)
	// Falls back to the env router: USD -> env-stripe, INR -> env-razorpay.
	if got := orderGateway(t, tg, ctx, "USD"); got != "env-stripe" {
		t.Fatalf("USD no-conn: got %q want env-stripe", got)
	}
	if got := orderGateway(t, tg, ctx, "INR"); got != "env-razorpay" {
		t.Fatalf("INR no-conn: got %q want env-razorpay", got)
	}
}

func TestTenantGatewayRoutesToTenantConnection(t *testing.T) {
	tenant := uuid.New()
	vault := &fakeVault{conns: map[uuid.UUID][]*domain.GatewayConnection{
		tenant: {
			{ID: uuid.New(), Provider: domain.GatewayStripe, PublicKey: "pk", SecretKeyEnc: "sksecret", Active: true, UpdatedAt: time.Unix(1, 0)},
		},
	}}
	resolver := newTestResolver(vault)
	tg := NewTenantGateway(resolver, resolver.env)
	ctx := context.WithValue(context.Background(), domain.TenantIDKey, tenant)

	// USD routes to the tenant's own Stripe...
	if got := orderGateway(t, tg, ctx, "USD"); got != "byo-stripe:sksecret" {
		t.Fatalf("USD: got %q want byo-stripe:sksecret", got)
	}
	// ...but INR, which the tenant hasn't connected (no Razorpay), reuses the
	// env Razorpay slot.
	if got := orderGateway(t, tg, ctx, "INR"); got != "env-razorpay" {
		t.Fatalf("INR: got %q want env-razorpay (env fallback for un-connected slot)", got)
	}
}

func TestResolverCachesAndInvalidatesOnChange(t *testing.T) {
	tenant := uuid.New()
	conn := &domain.GatewayConnection{ID: uuid.New(), Provider: domain.GatewayStripe, SecretKeyEnc: "v1", Active: true, UpdatedAt: time.Unix(1, 0)}
	vault := &fakeVault{conns: map[uuid.UUID][]*domain.GatewayConnection{tenant: {conn}}}
	resolver := newTestResolver(vault)
	ctx := context.WithValue(context.Background(), domain.TenantIDKey, tenant)

	r1 := resolver.For(ctx, tenant)
	r2 := resolver.For(ctx, tenant)
	if r1 != r2 {
		t.Fatal("expected the cached router to be reused for an unchanged connection set")
	}

	// Re-key (new secret + updated_at) must bust the cache and rebuild.
	conn.SecretKeyEnc = "v2"
	conn.UpdatedAt = time.Unix(2, 0)
	r3 := resolver.For(ctx, tenant)
	if r3 == r1 {
		t.Fatal("expected a rebuilt router after the connection changed")
	}
	tg := NewTenantGateway(resolver, resolver.env)
	if got := orderGateway(t, tg, ctx, "USD"); got != "byo-stripe:v2" {
		t.Fatalf("after re-key: got %q want byo-stripe:v2", got)
	}
}

func TestStripeForAndRazorpayFor(t *testing.T) {
	tenant := uuid.New()
	other := uuid.New()
	vault := &fakeVault{conns: map[uuid.UUID][]*domain.GatewayConnection{
		tenant: {
			{ID: uuid.New(), Provider: domain.GatewayStripe, SecretKeyEnc: "sk", Active: true, UpdatedAt: time.Unix(1, 0)},
		},
	}}
	resolver := newTestResolver(vault)
	ctx := context.Background()

	// Connected tenant -> their own Stripe; un-connected Razorpay slot -> env.
	if got := resolver.StripeFor(ctx, tenant).(*stubGateway).name; got != "byo-stripe:sk" {
		t.Fatalf("StripeFor(connected): got %q", got)
	}
	if got := resolver.RazorpayFor(ctx, tenant).(*stubGateway).name; got != "env-razorpay" {
		t.Fatalf("RazorpayFor(un-connected slot): got %q want env-razorpay", got)
	}
	// Tenant with no connections at all -> env for both.
	if got := resolver.StripeFor(ctx, other).(*stubGateway).name; got != "env-stripe" {
		t.Fatalf("StripeFor(no conn): got %q want env-stripe", got)
	}
}

func TestNilResolverAlwaysEnv(t *testing.T) {
	tg := NewTenantGateway(nil, &stubGateway{name: "env-direct"})
	ctx := context.WithValue(context.Background(), domain.TenantIDKey, uuid.New())
	if got := orderGateway(t, tg, ctx, "USD"); got != "env-direct" {
		t.Fatalf("nil resolver: got %q want env-direct", got)
	}
}

func mandateGateway(t *testing.T, gw port.PaymentGateway, ctx context.Context, currency string) string {
	t.Helper()
	m, err := gw.CreateMandate(ctx, "a@b.c", "", "", 5000, "monthly", currency)
	if err != nil {
		t.Fatalf("CreateMandate(%s): %v", currency, err)
	}
	return m.TokenID
}

func TestResolverBuildsTenantGoCardless(t *testing.T) {
	tenant := uuid.New()
	vault := &fakeVault{conns: map[uuid.UUID][]*domain.GatewayConnection{
		tenant: {
			{ID: uuid.New(), Provider: domain.GatewayGoCardless, Mode: domain.GatewayModeTest, SecretKeyEnc: "sandbox_tok", Active: true, UpdatedAt: time.Unix(1, 0)},
		},
	}}
	resolver := newTestResolver(vault)
	resolver.buildGoCardless = func(token, environment string) port.PaymentGateway {
		return &stubGateway{name: "byo-gocardless:" + token + ":" + environment}
	}
	tg := NewTenantGateway(resolver, resolver.env)
	ctx := context.WithValue(context.Background(), domain.TenantIDKey, tenant)

	// EUR and GBP mandates route to the tenant's GoCardless, in the sandbox
	// environment because the connection is in test mode.
	for _, c := range []string{"EUR", "GBP"} {
		if got := mandateGateway(t, tg, ctx, c); got != "byo-gocardless:sandbox_tok:sandbox" {
			t.Fatalf("%s mandate: got %q want tenant gocardless", c, got)
		}
	}
	// INR mandates keep the Razorpay default.
	if got := mandateGateway(t, tg, ctx, "INR"); got != "env-razorpay" {
		t.Fatalf("INR mandate: got %q want env-razorpay", got)
	}
	// Card orders are untouched by a GoCardless connection.
	if got := orderGateway(t, tg, ctx, "USD"); got != "env-stripe" {
		t.Fatalf("USD order: got %q want env-stripe", got)
	}
}

func TestResolverGoCardlessLiveModeAndEnvIsolation(t *testing.T) {
	tenant := uuid.New()
	vault := &fakeVault{conns: map[uuid.UUID][]*domain.GatewayConnection{
		tenant: {
			{ID: uuid.New(), Provider: domain.GatewayGoCardless, Mode: domain.GatewayModeLive, SecretKeyEnc: "live_tok", Active: true, UpdatedAt: time.Unix(1, 0)},
		},
	}}
	resolver := newTestResolver(vault)
	resolver.buildGoCardless = func(token, environment string) port.PaymentGateway {
		return &stubGateway{name: "byo-gocardless:" + environment}
	}
	tg := NewTenantGateway(resolver, resolver.env)
	ctx := context.WithValue(context.Background(), domain.TenantIDKey, tenant)

	if got := mandateGateway(t, tg, ctx, "EUR"); got != "byo-gocardless:live" {
		t.Fatalf("EUR mandate: got %q want live-mode tenant gocardless", got)
	}

	// The env router must be untouched by the tenant build: no leaked EUR
	// override, no leaked extra gateway (a second tenant without GoCardless
	// would otherwise debit through this tenant's account).
	if _, leaked := resolver.env.currencyOverrides["EUR"]; leaked {
		t.Fatal("tenant build leaked EUR override into the shared env router")
	}
	if _, leaked := resolver.env.Extra["gocardless"]; leaked {
		t.Fatal("tenant build leaked gocardless gateway into the shared env router")
	}
	// And a mandate for a different tenant still routes to env Razorpay.
	other := context.WithValue(context.Background(), domain.TenantIDKey, uuid.New())
	if got := mandateGateway(t, tg, other, "EUR"); got != "env-razorpay" {
		t.Fatalf("other-tenant EUR mandate: got %q want env-razorpay", got)
	}
}

func TestResolverKeepsExplicitEnvOverride(t *testing.T) {
	tenant := uuid.New()
	vault := &fakeVault{conns: map[uuid.UUID][]*domain.GatewayConnection{
		tenant: {
			{ID: uuid.New(), Provider: domain.GatewayGoCardless, Mode: domain.GatewayModeTest, SecretKeyEnc: "tok", Active: true, UpdatedAt: time.Unix(1, 0)},
		},
	}}
	resolver := newTestResolver(vault)
	resolver.buildGoCardless = func(token, environment string) port.PaymentGateway {
		return &stubGateway{name: "byo-gocardless"}
	}
	// Operator explicitly routes EUR elsewhere (e.g. Adyen): that wins.
	resolver.env.RegisterGateway("adyen", &stubGateway{name: "env-adyen"})
	resolver.env.currencyOverrides = map[string]string{"EUR": "adyen"}

	tg := NewTenantGateway(resolver, resolver.env)
	ctx := context.WithValue(context.Background(), domain.TenantIDKey, tenant)

	if got := mandateGateway(t, tg, ctx, "EUR"); got != "env-adyen" {
		t.Fatalf("EUR mandate: got %q want env-adyen (explicit override wins)", got)
	}
	// GBP has no explicit override, so it defaults to the tenant's GoCardless.
	if got := mandateGateway(t, tg, ctx, "GBP"); got != "byo-gocardless" {
		t.Fatalf("GBP mandate: got %q want byo-gocardless", got)
	}
}

// A tenant that brought its own Stripe collects card payments into its own
// account. A platform-only gateway routed by an env override (SAR=moyasar,
// the platform's Moyasar merchant account) must not take that tenant's SAR:
// it stays on the tenant's Stripe, as before Moyasar existed. A tenant
// without its own Stripe has no account of its own to protect and keeps the
// platform routing, and the shared env router is never changed.
func TestResolverBYOStripeTenantKeepsSAROffPlatformMoyasar(t *testing.T) {
	byoStripe, byoRazorpay := uuid.New(), uuid.New()
	vault := &fakeVault{conns: map[uuid.UUID][]*domain.GatewayConnection{
		byoStripe:   {{ID: uuid.New(), Provider: domain.GatewayStripe, SecretKeyEnc: "sk", Active: true, UpdatedAt: time.Unix(1, 0)}},
		byoRazorpay: {{ID: uuid.New(), Provider: domain.GatewayRazorpay, PublicKey: "rzp", SecretKeyEnc: "rs", Active: true, UpdatedAt: time.Unix(1, 0)}},
	}}
	resolver := newTestResolver(vault)
	resolver.env.RegisterGateway("moyasar", &stubGateway{name: "env-moyasar"})
	if err := resolver.env.SetCurrencyOverrides("SAR=moyasar"); err != nil {
		t.Fatal(err)
	}
	tg := NewTenantGateway(resolver, resolver.env)

	ctx := context.WithValue(context.Background(), domain.TenantIDKey, byoStripe)
	if got := orderGateway(t, tg, ctx, "SAR"); got != "byo-stripe:sk" {
		t.Fatalf("BYO-Stripe tenant SAR: got %q want byo-stripe:sk", got)
	}
	if got := resolver.For(ctx, byoStripe).ProviderFor("SAR"); got != domain.GatewayStripe {
		t.Fatalf("BYO-Stripe tenant SAR provider: got %q want stripe", got)
	}

	other := context.WithValue(context.Background(), domain.TenantIDKey, byoRazorpay)
	if got := orderGateway(t, tg, other, "SAR"); got != "env-moyasar" {
		t.Fatalf("tenant without its own Stripe, SAR: got %q want env-moyasar", got)
	}
	if got := resolver.env.ProviderFor("SAR"); got != domain.GatewayMoyasar {
		t.Fatalf("env router SAR provider: got %q want moyasar (unchanged)", got)
	}
}

// The tenant-aware ProviderFor is what the saved-card guard, the card-update
// policy and the checkout paid-payment check route on: a BYO-Stripe tenant's
// SAR stays on Stripe, a tenant without a connection (or no tenant in ctx)
// follows the env routing.
func TestResolverProviderForUsesTheTenantInContext(t *testing.T) {
	byoStripe, plain := uuid.New(), uuid.New()
	vault := &fakeVault{conns: map[uuid.UUID][]*domain.GatewayConnection{
		byoStripe: {{ID: uuid.New(), Provider: domain.GatewayStripe, SecretKeyEnc: "sk", Active: true, UpdatedAt: time.Unix(1, 0)}},
	}}
	resolver := newTestResolver(vault)
	resolver.env.RegisterGateway("moyasar", &stubGateway{name: "env-moyasar"})
	if err := resolver.env.SetCurrencyOverrides("SAR=moyasar"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want domain.GatewayProvider
	}{
		{"BYO Stripe tenant", context.WithValue(context.Background(), domain.TenantIDKey, byoStripe), domain.GatewayStripe},
		{"tenant without a connection", context.WithValue(context.Background(), domain.TenantIDKey, plain), domain.GatewayMoyasar},
		{"no tenant", context.Background(), domain.GatewayMoyasar},
	} {
		if got := resolver.ProviderFor(tc.ctx, "SAR"); got != tc.want {
			t.Errorf("%s: ProviderFor(SAR) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A tenant that connects its own Stripe after taking Moyasar payments must
// still refund those payments on Moyasar: the payment id (a UUID, which no
// other gateway issues) names the gateway, not the currency's current route.
func TestResolverBYOStripeTenantRefundsMoyasarPaymentOnMoyasar(t *testing.T) {
	byoStripe := uuid.New()
	vault := &fakeVault{conns: map[uuid.UUID][]*domain.GatewayConnection{
		byoStripe: {{ID: uuid.New(), Provider: domain.GatewayStripe, SecretKeyEnc: "sk", Active: true, UpdatedAt: time.Unix(1, 0)}},
	}}
	resolver := newTestResolver(vault)
	resolver.env.RegisterGateway("moyasar", &stubGateway{name: "env-moyasar"})
	if err := resolver.env.SetCurrencyOverrides("SAR=moyasar"); err != nil {
		t.Fatal(err)
	}
	tg := NewTenantGateway(resolver, resolver.env)
	ctx := context.WithValue(context.Background(), domain.TenantIDKey, byoStripe)

	res, err := tg.Refund(ctx, uuid.NewString(), 100, "SAR")
	if err != nil || res.RefundID != "env-moyasar" {
		t.Fatalf("UUID payment refund = %+v, %v; want env-moyasar", res, err)
	}
	// Stripe ids still go to the tenant's own Stripe, whatever the currency.
	res, err = tg.Refund(ctx, "pi_123", 100, "SAR")
	if err != nil || res.RefundID != "byo-stripe:sk" {
		t.Fatalf("pi_ refund = %+v, %v; want byo-stripe:sk", res, err)
	}
}
