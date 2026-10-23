package db

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
)

func mustExecDB(t *testing.T, conn *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// Only an open or past-due invoice of a subscription that will renew saves
// the card when paid; the query returns those, oldest due first, for one
// customer only.
func TestListCardSavingInvoices_Postgres(t *testing.T) {
	dbx := openCustomerPMTestDB(t)
	conn := dbx.DB
	ctx := context.Background()
	tenantID, customerID := seedPMTenantCustomer(t, conn)
	_, otherCustomer := seedPMTenantCustomer(t, conn)

	planID := uuid.New()
	mustExecDB(t, conn, `INSERT INTO plans (id, tenant_id, name, code, interval_unit, interval_count, active) VALUES ($1, $2, 'P', $3, 'month', 1, TRUE)`,
		planID, tenantID, "p-"+planID.String()[:8])
	sub := func(customer uuid.UUID, status string, cancelAtEnd bool) uuid.UUID {
		id := uuid.New()
		mustExecDB(t, conn, `INSERT INTO subscriptions (id, tenant_id, customer_id, plan_id, status, current_period_start, current_period_end, billing_anchor, cancel_at_period_end, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, NOW(), NOW() + INTERVAL '1 month', NOW(), $6, NOW(), NOW())`, id, tenantID, customer, planID, status, cancelAtEnd)
		return id
	}
	inv := func(customer uuid.UUID, subID *uuid.UUID, status string, due time.Time) uuid.UUID {
		id := uuid.New()
		mustExecDB(t, conn, `INSERT INTO invoices (id, tenant_id, customer_id, subscription_id, currency, subtotal, total, amount_paid, status, invoice_number, created_at, due_date)
			VALUES ($1, $2, $3, $4, 'SAR', 100, 100, 0, $5, $6, NOW(), $7)`, id, tenantID, customer, subID, status, "INV-CS-"+id.String()[:8], due)
		return id
	}

	now := time.Now().UTC()
	renewing := sub(customerID, "active", false)
	pastDueSub := sub(customerID, "past_due", false)
	cancelling := sub(customerID, "active", true)
	canceled := sub(customerID, "canceled", false)
	otherSub := sub(otherCustomer, "active", false)

	newer := inv(customerID, &renewing, "open", now.Add(48*time.Hour))
	older := inv(customerID, &pastDueSub, "past_due", now.Add(-48*time.Hour))
	inv(customerID, &renewing, "paid", now.Add(-72*time.Hour))
	inv(customerID, &renewing, "void", now.Add(-72*time.Hour))
	inv(customerID, &cancelling, "open", now.Add(-96*time.Hour))
	inv(customerID, &canceled, "open", now.Add(-96*time.Hour))
	inv(customerID, nil, "open", now.Add(-96*time.Hour))
	inv(otherCustomer, &otherSub, "open", now.Add(-96*time.Hour))
	t.Cleanup(func() {
		_, _ = conn.Exec(`DELETE FROM invoices WHERE tenant_id = $1`, tenantID)
		_, _ = conn.Exec(`DELETE FROM subscriptions WHERE tenant_id = $1`, tenantID)
		_, _ = conn.Exec(`DELETE FROM plans WHERE tenant_id = $1`, tenantID)
		_, _ = conn.Exec(`DELETE FROM customers WHERE id IN ($1, $2)`, customerID, otherCustomer)
	})

	repo := NewInvoiceRepository(conn).(*InvoiceRepository)
	got, err := repo.ListCardSavingInvoices(ctx, customerID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != older || got[1].ID != newer {
		ids := make([]uuid.UUID, len(got))
		for i, g := range got {
			ids[i] = g.ID
		}
		t.Fatalf("got %v, want [%s %s]", ids, older, newer)
	}
	if got[0].Currency != "SAR" || got[0].TenantID != tenantID || got[0].AmountDue != 100 {
		t.Fatalf("row not fully scanned: %+v", got[0])
	}
	if limited, err := repo.ListCardSavingInvoices(ctx, customerID, 1); err != nil || len(limited) != 1 || limited[0].ID != older {
		t.Fatalf("limit 1 = %v, %v", limited, err)
	}
}
