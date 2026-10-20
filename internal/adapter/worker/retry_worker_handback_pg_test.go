package worker

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/recurso-dev/recurso/internal/adapter/db"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
)

// oneInvoiceRetryClaim hands the worker only the invoice under test, read
// through the real repository, instead of running the tenant-agnostic claim
// that would lease other tests' rows on a shared database.
type oneInvoiceRetryClaim struct {
	port.InvoiceRepository
	id, tenantID uuid.UUID
}

func (c oneInvoiceRetryClaim) ClaimDueForRetry(ctx context.Context, _ time.Duration, _ int) ([]*domain.Invoice, error) {
	inv, err := c.GetByID(context.WithValue(ctx, domain.TenantIDKey, c.tenantID), c.id)
	if err != nil {
		return nil, err
	}
	return []*domain.Invoice{inv}, nil
}

// TestRetryWorker_HardDeclineStaysInEmailDunning_Postgres: after a hard
// decline the invoice must still match ClaimOverdueForDunning's predicate, so
// the scheduler keeps emailing it and eventually writes it off. The predicate
// is checked for this row only (the claim itself is global and leases every
// overdue row in the database).
func TestRetryWorker_HardDeclineStaysInEmailDunning_Postgres(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping postgres-backed retry worker test")
	}
	if err := db.RunMigrations(dbURL); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	conn, err := sql.Open("postgres", dbURL)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = conn.Close() }()
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec: %v", err)
		}
	}

	tenantID := uuid.New()
	exec(`INSERT INTO tenants (id, name, email, created_at, updated_at) VALUES ($1,$2,$3,NOW(),NOW())`,
		tenantID, "RWH-"+tenantID.String()[:8], tenantID.String()[:8]+"@t.com")
	customerID := uuid.New()
	exec(`INSERT INTO customers (id, tenant_id, email, ledger_account_id, created_at) VALUES ($1,$2,$3,$4,NOW())`,
		customerID, tenantID, customerID.String()[:8]+"@c.com", uuid.New())
	invID := uuid.New()
	exec(`INSERT INTO invoices (id, tenant_id, customer_id, currency, subtotal, total, amount_paid, credit_applied,
			status, invoice_number, retry_count, next_retry_at, dunning_managed_by, created_at, due_date)
		VALUES ($1,$2,$3,'SAR',11500,11500,0,0,'past_due',$4,1, NOW() - INTERVAL '1 minute','worker', NOW(), NOW() - INTERVAL '3 days')`,
		invID, tenantID, customerID, "INV-RWH-"+invID.String()[:8])
	t.Cleanup(func() {
		_, _ = conn.ExecContext(context.Background(), `UPDATE invoices SET status = 'void' WHERE id = $1`, invID)
	})

	repo := oneInvoiceRetryClaim{InvoiceRepository: db.NewInvoiceRepository(conn), id: invID, tenantID: tenantID}
	gw := &mockGateway{result: &port.PaymentResult{Success: false, ErrorCode: "lost_or_stolen", Permanent: true}}
	w := NewRetryWorker(repo, &countingDecider{}, gw, &mockNotifier{})
	w.processRetries(ctx)

	var claimable bool
	var managedBy sql.NullString
	if err := conn.QueryRowContext(ctx, `
		SELECT status IN ('open', 'past_due')
		       AND due_date < NOW()
		       AND (next_retry_at IS NULL OR next_retry_at <= NOW())
		       AND (dunning_managed_by = 'scheduler' OR dunning_managed_by IS NULL)
		       AND NOT dunning_paused,
		       dunning_managed_by
		  FROM invoices WHERE id = $1`, invID).Scan(&claimable, &managedBy); err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	if !claimable {
		t.Fatalf("invoice is not claimable by the dunning scheduler after a hard decline (managed_by=%v): orphaned", managedBy)
	}
}
