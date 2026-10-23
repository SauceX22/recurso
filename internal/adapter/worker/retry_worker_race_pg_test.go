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
	"github.com/recurso-dev/recurso/internal/service"
)

// seedWorkerInvoice inserts a tenant, a customer and one past-due SAR invoice
// owned by the retry worker, and deletes them after the test.
func seedWorkerInvoice(t *testing.T) (conn *sql.DB, tenantID, invoiceID uuid.UUID) {
	t.Helper()
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
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec: %v", err)
		}
	}
	tenantID, customerID, invoiceID := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO tenants (id, name, email, created_at, updated_at) VALUES ($1,$2,$3,NOW(),NOW())`,
		tenantID, "RWR-"+tenantID.String()[:8], tenantID.String()[:8]+"@t.com")
	exec(`INSERT INTO customers (id, tenant_id, email, ledger_account_id, created_at) VALUES ($1,$2,$3,$4,NOW())`,
		customerID, tenantID, customerID.String()[:8]+"@c.com", uuid.New())
	exec(`INSERT INTO invoices (id, tenant_id, customer_id, currency, subtotal, total, amount_paid, credit_applied,
			status, invoice_number, retry_count, next_retry_at, dunning_managed_by, created_at, due_date)
		VALUES ($1,$2,$3,'SAR',11500,11500,0,0,'past_due',$4,1, NOW() - INTERVAL '1 minute','worker', NOW(), NOW() - INTERVAL '3 days')`,
		invoiceID, tenantID, customerID, "INV-RWR-"+invoiceID.String()[:8])
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = conn.ExecContext(bg, `DELETE FROM invoices WHERE id = $1`, invoiceID)
		_, _ = conn.ExecContext(bg, `DELETE FROM customers WHERE id = $1`, customerID)
		_, _ = conn.ExecContext(bg, `DELETE FROM entities WHERE tenant_id = $1`, tenantID)
		_, _ = conn.ExecContext(bg, `DELETE FROM tenants WHERE id = $1`, tenantID)
		_ = conn.Close()
	})
	return conn, tenantID, invoiceID
}

// payingCharger settles the invoice through another path (a checkout return,
// a webhook) while the worker's charge is in flight, then answers.
type payingCharger struct {
	repo     *db.InvoiceRepository
	tenantID uuid.UUID
	result   *port.PaymentResult
	err      error
	t        *testing.T
}

func (c *payingCharger) ChargeSavedPaymentMethod(ctx context.Context, req port.SavedMethodCharge) (*port.PaymentResult, error) {
	id := uuid.MustParse(req.InvoiceID)
	if ok, err := c.repo.MarkPaid(ctx, c.tenantID, id, time.Now()); err != nil || !ok {
		c.t.Fatalf("concurrent settlement: ok=%v err=%v", ok, err)
	}
	return c.result, c.err
}

// countingSettler is MarkInvoicePaid's transition guard: the ledger legs post
// only when it reports a transition.
type countingSettler struct {
	repo        *db.InvoiceRepository
	tenantID    uuid.UUID
	transitions int
}

func (s *countingSettler) MarkInvoicePaid(ctx context.Context, id uuid.UUID) (bool, error) {
	ok, err := s.repo.MarkPaid(ctx, s.tenantID, id, time.Now())
	if ok {
		s.transitions++
	}
	return ok, err
}

// The worker charges between its claim and its write-back. If the invoice is
// paid by another path in that window, whatever the charge answered, the
// worker's bookkeeping must not put it back to unpaid (the next settlement
// would post the payment twice) and must not settle it a second time.
func TestRetryWorker_ConcurrentPaymentStaysPaid_Postgres(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result *port.PaymentResult
		err    error
	}{
		{"soft decline", &port.PaymentResult{Success: false, ErrorCode: "insufficient_funds"}, nil},
		{"hard decline", &port.PaymentResult{Success: false, ErrorCode: "lost_or_stolen", Permanent: true}, nil},
		{"gateway error", nil, context.DeadlineExceeded},
		{"charge succeeded too", &port.PaymentResult{Success: true, PaymentID: "pay_retry"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, tenantID, invID := seedWorkerInvoice(t)
			repo := db.NewInvoiceRepository(conn).(*db.InvoiceRepository)
			charger := &payingCharger{repo: repo, tenantID: tenantID, result: tc.result, err: tc.err, t: t}
			router := service.NewSavedCardGatewayRouter(nil, nil, nil)
			router.RegisterProvider(domain.GatewayMoyasar, charger)
			settler := &countingSettler{repo: repo, tenantID: tenantID}

			w := NewRetryWorker(oneInvoiceRetryClaim{InvoiceRepository: repo, id: invID, tenantID: tenantID},
				newTestRetryService(), &mockGateway{}, &mockNotifier{})
			w.SetSavedMethodCharging(&fakeCustomerLookup{provider: domain.GatewayMoyasar, ref: "token_r"}, router)
			w.SetSettler(settler)
			w.processRetries(context.Background())

			var status string
			var amountPaid int64
			var paidAt sql.NullTime
			if err := conn.QueryRow(`SELECT status, amount_paid, paid_at FROM invoices WHERE id = $1`, invID).
				Scan(&status, &amountPaid, &paidAt); err != nil {
				t.Fatal(err)
			}
			if status != "paid" || amountPaid != 11500 || !paidAt.Valid {
				t.Fatalf("invoice is %s, amount_paid %d, paid_at %v after a concurrent payment; want paid/11500/set", status, amountPaid, paidAt)
			}
			if settler.transitions != 0 {
				t.Fatalf("the worker settled the invoice %d more time(s)", settler.transitions)
			}
		})
	}
}
