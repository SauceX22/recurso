package db

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
)

func seedRetryInvoice(t *testing.T, conn *sql.DB, tenantID, customerID uuid.UUID, status string) uuid.UUID {
	t.Helper()
	invID := uuid.New()
	if _, err := conn.Exec(`INSERT INTO invoices (id, tenant_id, customer_id, currency, subtotal, total, amount_paid, status, invoice_number,
			retry_count, dunning_managed_by, created_at, due_date)
		VALUES ($1,$2,$3,'SAR',100,100,0,$4,$5,1,'worker',NOW(),NOW())`,
		invID, tenantID, customerID, status, "INV-RS-"+invID.String()[:8]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = conn.Exec(`DELETE FROM invoices WHERE id = $1`, invID) })
	return invID
}

func cleanupPMTenant(t *testing.T, conn *sql.DB, tenantID, customerID uuid.UUID) {
	t.Cleanup(func() {
		_, _ = conn.Exec(`DELETE FROM customers WHERE id = $1`, customerID)
		_, _ = conn.Exec(`DELETE FROM entities WHERE tenant_id = $1`, tenantID)
		_, _ = conn.Exec(`DELETE FROM tenants WHERE id = $1`, tenantID)
	})
}

// The retry worker's write-back is conditional on the invoice still being
// unpaid, so a settlement that lands while the worker charges is never
// reverted; it writes every bookkeeping field and keeps the owner when none
// is given.
func TestUpdateRetryState_Postgres(t *testing.T) {
	dbx := openCustomerPMTestDB(t)
	tenantID, custID := seedPMTenantCustomer(t, dbx.DB)
	cleanupPMTenant(t, dbx.DB, tenantID, custID)
	repo := NewInvoiceRepository(dbx.DB).(*InvoiceRepository)
	ctx := context.Background()
	next := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)

	open := seedRetryInvoice(t, dbx.DB, tenantID, custID, "past_due")
	state := port.InvoiceRetryState{
		Status: domain.InvoiceStatusPastDue, NextRetryAt: &next, RetryCount: 2,
		DunningActionID: "24h", DunningContextKey: "SAR:card_declined", LastPaymentError: "card_declined",
		DeclinedMethodRef: "token_x", RetryInfraFailures: 3,
	}
	if ok, err := repo.UpdateRetryState(ctx, uuid.New(), open, state); err != nil || ok {
		t.Fatalf("another tenant's write = %v, %v; want false", ok, err)
	}
	if ok, err := repo.UpdateRetryState(ctx, tenantID, open, state); err != nil || !ok {
		t.Fatalf("write = %v, %v; want true", ok, err)
	}
	got, err := repo.GetByIDPublic(ctx, open)
	if err != nil {
		t.Fatal(err)
	}
	if got.NextRetryAt == nil || !got.NextRetryAt.Equal(next) || got.RetryCount != 2 || got.DunningActionID != "24h" ||
		got.DunningContextKey != "SAR:card_declined" || got.LastPaymentError != "card_declined" ||
		got.DunningManagedBy != "worker" || got.DeclinedMethodRef != "token_x" || got.RetryInfraFailures != 3 {
		t.Fatalf("stored %+v", got)
	}

	if err := repo.ClearRetryState(ctx, tenantID, open); err != nil {
		t.Fatal(err)
	}
	got, _ = repo.GetByIDPublic(ctx, open)
	if got.NextRetryAt != nil || got.DunningActionID != "" || got.LastPaymentError != "" || got.DeclinedMethodRef != "" ||
		got.RetryInfraFailures != 0 || got.Status != domain.InvoiceStatusPastDue {
		t.Fatalf("after clear %+v", got)
	}

	paid := seedRetryInvoice(t, dbx.DB, tenantID, custID, "open")
	if ok, err := repo.MarkPaid(ctx, tenantID, paid, time.Now()); err != nil || !ok {
		t.Fatalf("mark paid: %v %v", ok, err)
	}
	if ok, err := repo.UpdateRetryState(ctx, tenantID, paid, state); err != nil || ok {
		t.Fatalf("write on a paid invoice = %v, %v; want false", ok, err)
	}
	got, _ = repo.GetByIDPublic(ctx, paid)
	if got.Status != domain.InvoiceStatusPaid || got.AmountPaid != 100 || got.PaidAt == nil || got.RetryCount != 1 {
		t.Fatalf("paid invoice changed: %+v", got)
	}
}

// The refund target is claimed, not overwritten: a redelivery of the settling
// payment re-claims it, a second payment or another tenant never does.
func TestClaimGatewayPaymentID_Postgres(t *testing.T) {
	dbx := openCustomerPMTestDB(t)
	tenantID, custID := seedPMTenantCustomer(t, dbx.DB)
	cleanupPMTenant(t, dbx.DB, tenantID, custID)
	invID := seedRetryInvoice(t, dbx.DB, tenantID, custID, "open")
	repo := NewInvoiceRepository(dbx.DB).(*InvoiceRepository)
	ctx := context.Background()
	for _, step := range []struct {
		tenant uuid.UUID
		id     string
		want   bool
	}{
		{uuid.New(), "pay_A", false}, // other tenant
		{tenantID, "pay_A", true},    // first claim
		{tenantID, "pay_A", true},    // same payment again (redelivery)
		{tenantID, "pay_B", false},   // a second payment
	} {
		got, err := repo.ClaimGatewayPaymentID(ctx, step.tenant, invID, step.id)
		if err != nil || got != step.want {
			t.Fatalf("claim %s under %s = %v, %v; want %v", step.id, step.tenant, got, err, step.want)
		}
	}
}
