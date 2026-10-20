package db

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
)

// TestSetGatewayPaymentMethod_RoundTripAndTenantFilter: a non-Stripe method
// round-trips with its provider, a wrong-tenant write is refused without
// touching the row, the BYO connection of the previous card is cleared, and a
// later Stripe save (SetDefaultPaymentMethod) routes the customer back to Stripe.
func TestSetGatewayPaymentMethod_RoundTripAndTenantFilter(t *testing.T) {
	dbx := openCustomerPMTestDB(t)
	conn := dbx.DB
	repo := NewCustomerRepository(dbx)
	ctx := context.Background()

	ownerTenant, custID := seedPMTenantCustomer(t, conn)
	otherTenant, _ := seedPMTenantCustomer(t, conn)

	// Start from a Stripe card saved on a BYO connection, with an expiry.
	connID := uuid.New()
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO gateway_connections (id, tenant_id, provider) VALUES ($1, $2, 'stripe')`,
		connID, ownerTenant); err != nil {
		t.Fatalf("seed connection: %v", err)
	}
	if _, err := conn.ExecContext(ctx,
		`UPDATE customers SET stripe_customer_id = 'cus_1', default_payment_method = 'pm_1',
		        card_brand = 'visa', card_last4 = '4242', card_exp_month = 12, card_exp_year = 2030,
		        pm_gateway_connection_id = $2
		  WHERE id = $1`, custID, connID); err != nil {
		t.Fatalf("seed stripe card: %v", err)
	}
	m, err := repo.GetSavedPaymentMethod(ctx, custID)
	if err != nil {
		t.Fatalf("GetSavedPaymentMethod: %v", err)
	}
	if m.Provider != domain.GatewayStripe || m.CustomerRef != "cus_1" || m.PaymentMethodRef != "pm_1" ||
		m.GatewayConnectionID == nil || *m.GatewayConnectionID != connID {
		t.Fatalf("NULL pm_provider must read as Stripe with its connection, got %+v", m)
	}

	// Wrong tenant: refused, row unchanged.
	err = repo.SetGatewayPaymentMethod(ctx, otherTenant, custID, domain.GatewayMoyasar, "token_evil", "mada", "0000", 1, 2031)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant SetGatewayPaymentMethod: want sql.ErrNoRows, got %v", err)
	}
	if m, err = repo.GetSavedPaymentMethod(ctx, custID); err != nil || m.PaymentMethodRef != "pm_1" || m.Provider != domain.GatewayStripe {
		t.Fatalf("cross-tenant write mutated the row: %+v, %v", m, err)
	}

	// Owner, expiry unknown: token saved, expiry and BYO connection cleared.
	if err := repo.SetGatewayPaymentMethod(ctx, ownerTenant, custID, domain.GatewayMoyasar, "token_abc", "mada", "1010", 0, 0); err != nil {
		t.Fatalf("owner SetGatewayPaymentMethod: %v", err)
	}
	m, err = repo.GetSavedPaymentMethod(ctx, custID)
	if err != nil {
		t.Fatal(err)
	}
	if m.Provider != domain.GatewayMoyasar || m.PaymentMethodRef != "token_abc" || m.CustomerRef != "cus_1" || m.GatewayConnectionID != nil {
		t.Fatalf("after moyasar save = %+v", m)
	}
	brand, last4, expMonth, expYear := readCardDisplay(t, conn, custID)
	if brand.String != "mada" || last4.String != "1010" || expMonth.Valid || expYear.Valid {
		t.Fatalf("display fields = %v %v %v %v, want mada/1010 and NULL expiry", brand, last4, expMonth, expYear)
	}

	// Owner, expiry known: stored so the card-expiry notifier can see it.
	if err := repo.SetGatewayPaymentMethod(ctx, ownerTenant, custID, domain.GatewayMoyasar, "token_def", "visa", "1111", 7, 2029); err != nil {
		t.Fatal(err)
	}
	_, _, expMonth, expYear = readCardDisplay(t, conn, custID)
	if expMonth.Int64 != 7 || expYear.Int64 != 2029 {
		t.Fatalf("expiry = %v/%v, want 7/2029", expMonth, expYear)
	}

	// Saving a Stripe card again routes the customer back to Stripe.
	if err := repo.SetDefaultPaymentMethod(ctx, custID, "pm_2", "visa", "4242", 1, 2031, nil); err != nil {
		t.Fatal(err)
	}
	m, err = repo.GetSavedPaymentMethod(ctx, custID)
	if err != nil {
		t.Fatal(err)
	}
	if m.Provider != domain.GatewayStripe || m.PaymentMethodRef != "pm_2" || !m.Chargeable() {
		t.Fatalf("after stripe re-save = %+v", m)
	}
}

func TestPMProviderRejectsUnknownProvider(t *testing.T) {
	dbx := openCustomerPMTestDB(t)
	repo := NewCustomerRepository(dbx)
	tenantID, custID := seedPMTenantCustomer(t, dbx.DB)
	err := repo.SetGatewayPaymentMethod(context.Background(), tenantID, custID, domain.GatewayProvider("typo"), "x", "", "", 0, 0)
	if err == nil {
		t.Fatal("an unknown pm_provider must be rejected by the CHECK constraint")
	}
}

func readCardDisplay(t *testing.T, conn *sql.DB, custID uuid.UUID) (brand, last4 sql.NullString, expMonth, expYear sql.NullInt64) {
	t.Helper()
	if err := conn.QueryRowContext(context.Background(),
		`SELECT card_brand, card_last4, card_exp_month, card_exp_year FROM customers WHERE id = $1`, custID).
		Scan(&brand, &last4, &expMonth, &expYear); err != nil {
		t.Fatal(err)
	}
	return
}
