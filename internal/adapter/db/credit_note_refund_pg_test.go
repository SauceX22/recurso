package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
)

// M2 (review4): ResolvePendingRefund moves a refund only while it is pending
// under its refund id, so of two webhook deliveries only one wins and books.
func TestResolvePendingRefund_OnlyOnce_Postgres(t *testing.T) {
	dbx := openCreditAppTestDB(t)
	defer func() { _ = dbx.Close() }()
	ctx := context.Background()
	tenantID, customerID := seedCreditAppTenantCustomer(t, dbx.DB)
	invID := seedInvoiceRow(t, dbx.DB, tenantID, customerID, 1000)

	id := uuid.New()
	refundID := "pay_1:unknown:" + uuid.NewString()
	if _, err := dbx.ExecContext(ctx,
		`INSERT INTO credit_notes (id, tenant_id, customer_id, invoice_id, amount, balance, currency, status, reason, type, refund_status, refund_id, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, 400, 0, 'USD', 'issued', 'test refund', 'refund', 'pending', $5, NOW(), NOW())`,
		id, tenantID, customerID, invID, refundID); err != nil {
		t.Fatalf("seed refund note: %v", err)
	}

	repo := NewCreditNoteRepository(dbx)
	if ok, err := repo.ResolvePendingRefund(ctx, id, "pay_1:unknown:other", domain.RefundStatusProcessed, "m"); err != nil || ok {
		t.Fatalf("other refund id: resolved=%v err=%v, want false", ok, err)
	}
	if ok, err := repo.ResolvePendingRefund(ctx, id, refundID, domain.RefundStatusProcessed, "first"); err != nil || !ok {
		t.Fatalf("first: resolved=%v err=%v, want true", ok, err)
	}
	if ok, err := repo.ResolvePendingRefund(ctx, id, refundID, domain.RefundStatusProcessed, "second"); err != nil || ok {
		t.Fatalf("second: resolved=%v err=%v, want false", ok, err)
	}

	var status, message string
	if err := dbx.QueryRowContext(ctx, `SELECT refund_status, refund_message FROM credit_notes WHERE id = $1`, id).Scan(&status, &message); err != nil {
		t.Fatal(err)
	}
	if status != string(domain.RefundStatusProcessed) || message != "first" {
		t.Fatalf("note = (%s, %q), want (processed, \"first\")", status, message)
	}
}
