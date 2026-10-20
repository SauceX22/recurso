package db

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/recurso-dev/recurso/internal/core/domain"
)

// TestSetGatewayPaymentMethod_RoundTripAndTenantFilter proves the
// provider-neutral saved-method columns: a Moyasar token round-trips through
// GetSavedGatewayMethod, a wrong-tenant write is refused without touching the
// row, and a later Stripe save (SetDefaultPaymentMethod) resets the provider.
func TestSetGatewayPaymentMethod_RoundTripAndTenantFilter(t *testing.T) {
	dbx := openCustomerPMTestDB(t)
	conn := dbx.DB
	repo := NewCustomerRepository(dbx)
	ctx := context.Background()

	ownerTenant, custID := seedPMTenantCustomer(t, conn)
	otherTenant, _ := seedPMTenantCustomer(t, conn)

	// Start from a saved Stripe card with an expiry.
	if _, err := conn.ExecContext(ctx,
		`UPDATE customers SET stripe_customer_id = 'cus_1', default_payment_method = 'pm_1',
		        card_brand = 'visa', card_last4 = '4242', card_exp_month = 12, card_exp_year = 2030
		  WHERE id = $1`, custID); err != nil {
		t.Fatalf("seed stripe card: %v", err)
	}
	m, err := repo.GetSavedGatewayMethod(ctx, custID)
	if err != nil {
		t.Fatalf("GetSavedGatewayMethod: %v", err)
	}
	if m.Provider != "" || m.ProviderOrStripe() != domain.SavedMethodStripe || m.CustomerRef != "cus_1" || m.PaymentMethodRef != "pm_1" || m.GatewayConnectionID != nil {
		t.Fatalf("legacy stripe row = %+v", m)
	}

	// Wrong tenant: refused, row unchanged.
	err = repo.SetGatewayPaymentMethod(ctx, otherTenant, custID, domain.SavedMethodMoyasar, "token_evil", "mada", "0000")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant SetGatewayPaymentMethod: want sql.ErrNoRows, got %v", err)
	}
	m, err = repo.GetSavedGatewayMethod(ctx, custID)
	if err != nil {
		t.Fatal(err)
	}
	if m.PaymentMethodRef != "pm_1" || m.Provider != "" {
		t.Fatalf("cross-tenant write mutated the row: %+v", m)
	}

	// Owner: Moyasar token saved.
	if err := repo.SetGatewayPaymentMethod(ctx, ownerTenant, custID, domain.SavedMethodMoyasar, "token_abc", "mada", "1010"); err != nil {
		t.Fatalf("owner SetGatewayPaymentMethod: %v", err)
	}
	m, err = repo.GetSavedGatewayMethod(ctx, custID)
	if err != nil {
		t.Fatal(err)
	}
	if m.Provider != domain.SavedMethodMoyasar || m.PaymentMethodRef != "token_abc" || m.CustomerRef != "cus_1" || m.GatewayConnectionID != nil {
		t.Fatalf("after moyasar save = %+v", m)
	}
	var brand, last4 sql.NullString
	var expMonth, expYear sql.NullInt64
	if err := conn.QueryRowContext(ctx,
		`SELECT card_brand, card_last4, card_exp_month, card_exp_year FROM customers WHERE id = $1`, custID).
		Scan(&brand, &last4, &expMonth, &expYear); err != nil {
		t.Fatal(err)
	}
	if brand.String != "mada" || last4.String != "1010" || expMonth.Valid || expYear.Valid {
		t.Fatalf("display fields = %v %v %v %v, want mada/1010 and NULL expiry", brand, last4, expMonth, expYear)
	}

	// Tenant-scoped read: wrong tenant in ctx sees nothing; right tenant sees it.
	if _, err := repo.GetSavedGatewayMethod(context.WithValue(ctx, domain.TenantIDKey, otherTenant), custID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant read: want sql.ErrNoRows, got %v", err)
	}
	if m, err := repo.GetSavedGatewayMethod(context.WithValue(ctx, domain.TenantIDKey, ownerTenant), custID); err != nil || m.PaymentMethodRef != "token_abc" {
		t.Fatalf("owner-scoped read = %+v, %v", m, err)
	}

	// Saving a Stripe card again routes the customer back to Stripe.
	if err := repo.SetDefaultPaymentMethod(ctx, custID, "pm_2", "visa", "4242", 1, 2031, nil); err != nil {
		t.Fatal(err)
	}
	m, err = repo.GetSavedGatewayMethod(ctx, custID)
	if err != nil {
		t.Fatal(err)
	}
	if m.Provider != "" || m.PaymentMethodRef != "pm_2" || !m.Chargeable() {
		t.Fatalf("after stripe re-save = %+v", m)
	}
}
