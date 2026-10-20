package service

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/recurso-dev/recurso/internal/adapter/db"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
)

// pgRenewalInvoicer stands in for InvoiceService.GenerateInvoice: it books an
// open renewal invoice for the subscription's plan price and posts its
// invoice-time ledger legs (ADR-002), exactly what the real generator persists
// for a tax-free flat-fee renewal. Driving the full InvoiceService (tax,
// usage rating, credit/wallet appliers) adds nothing to what this test proves:
// the saved-method renewal CHARGE and SETTLEMENT path.
type pgRenewalInvoicer struct {
	h        *invariantHarness
	amount   int64
	currency string
	invoice  *domain.Invoice
}

func (p *pgRenewalInvoicer) GenerateInvoice(_ context.Context, sub *domain.Subscription) (*domain.Invoice, error) {
	h := p.h
	h.n++
	invID := uuid.New()
	invNo := fmt.Sprintf("INV-%s-SAR-%d", h.run, h.n)
	mustExec(h.t, h.conn, `INSERT INTO invoices (id, tenant_id, customer_id, subscription_id, currency, subtotal, total, amount_paid, status, invoice_number, created_at, due_date)
		VALUES ($1,$2,$3,$4,$5,$6,$6,0,'open',$7,NOW(),NOW())`,
		invID, sub.TenantID, sub.CustomerID, sub.ID, p.currency, p.amount, invNo)
	subID := sub.ID
	inv := &domain.Invoice{
		ID: invID, TenantID: sub.TenantID, CustomerID: sub.CustomerID, SubscriptionID: &subID,
		InvoiceNumber: invNo, Status: domain.InvoiceStatusOpen, Total: p.amount, Subtotal: p.amount, Currency: p.currency,
	}
	if err := h.ledger.RecordInvoice(h.ctx, inv); err != nil {
		return nil, fmt.Errorf("RecordInvoice: %w", err)
	}
	p.invoice = inv
	return inv, nil
}

func (p *pgRenewalInvoicer) GenerateFinalUsageInvoice(context.Context, *domain.Subscription, time.Time) (*domain.Invoice, error) {
	return nil, fmt.Errorf("not used")
}

// TestRenewalSavedMethod_SettlesLedger_Postgres renews a SAR subscription
// whose customer saved a Moyasar token, on the unattended sweep's tenantless
// context: the router charges the token on a fake Moyasar charger, and the
// REAL SubscriptionService.MarkInvoicePaid settles it. The reconciliation
// oracle then proves the invoice and payment legs are posted and the books are
// audit-grade.
func TestRenewalSavedMethod_SettlesLedger_Postgres(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping postgres-backed saved-method renewal test")
	}
	if err := db.RunMigrations(dbURL); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	dbx, err := sqlx.Open("postgres", dbURL)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = dbx.Close() }()
	conn := dbx.DB

	h := newInvariantHarness(t, conn, dbx)
	ctx := h.ctx

	planID := uuid.New()
	mustExec(t, conn, `INSERT INTO plans (id, tenant_id, name, code, interval_unit, interval_count, active) VALUES ($1,$2,'sar-plan',$3,'month',1,TRUE)`,
		planID, h.tenantID, "sar-plan-"+h.run)
	mustExec(t, conn, `INSERT INTO prices (id, plan_id, currency, amount, type) VALUES ($1,$2,'SAR',11500,'recurring')`, uuid.New(), planID)

	customerID := uuid.New()
	mustExec(t, conn, `INSERT INTO customers (id, tenant_id, email, name, country, tax_type, ledger_account_id, created_at, updated_at)
		VALUES ($1,$2,$3,'SAR Cust','Saudi Arabia','individual',$4,NOW(),NOW())`,
		customerID, h.tenantID, "sar-"+h.run+"@t.com", uuid.New())
	custRepo := db.NewCustomerRepository(dbx)
	if err := custRepo.SetGatewayPaymentMethod(ctx, h.tenantID, customerID, domain.GatewayMoyasar, "token_sar_"+h.run, "mada", "1010", 0, 0); err != nil {
		t.Fatalf("SetGatewayPaymentMethod: %v", err)
	}

	// Due, with no gateway subscription id (Moyasar never returns one). The
	// renewal is driven directly rather than through the global claim, which
	// would lease other tests' due subscriptions; it is canceled afterwards so
	// a failed run never leaves a due row behind.
	subID := uuid.New()
	mustExec(t, conn, `INSERT INTO subscriptions (id, tenant_id, customer_id, plan_id, status, current_period_start, current_period_end, billing_anchor, created_at, updated_at)
		VALUES ($1,$2,$3,$4,'active', NOW() - INTERVAL '1 month', NOW() - INTERVAL '1 minute', NOW() - INTERVAL '1 month', NOW(), NOW())`,
		subID, h.tenantID, customerID, planID)
	t.Cleanup(func() {
		_, _ = conn.ExecContext(context.Background(), `UPDATE subscriptions SET status = 'canceled' WHERE id = $1`, subID)
	})
	subRepo := db.NewSubscriptionRepository(conn).(*db.SubscriptionRepository)
	sub, err := subRepo.GetByID(h.tctx, subID)
	if err != nil || sub == nil {
		t.Fatalf("load subscription: %v", err)
	}

	invoicer := &pgRenewalInvoicer{h: h, amount: 11500, currency: "SAR"}
	svc := NewRenewalService(subRepo, db.NewPlanRepository(conn), invoicer)
	// Stripe mocked in this deployment: no Stripe charger, settler wired.
	svc.SetSavedMethodCharging(nil, custRepo, h.subSvc)
	moyasar := &fakeRenewalCharger{result: &port.PaymentResult{Success: true, PaymentID: uuid.NewString()}}
	router := NewSavedCardGatewayRouter(nil, nil, nil)
	router.RegisterProvider(domain.GatewayMoyasar, moyasar)
	svc.SetChargerRouter(router)

	// The unattended sweep's context carries no tenant; renewal must supply
	// the subscription's own for the plan lookup and the settlement.
	if err := svc.RenewSubscription(context.Background(), sub); err != nil {
		t.Fatalf("RenewSubscription: %v", err)
	}
	inv := invoicer.invoice
	if inv == nil {
		t.Fatal("no renewal invoice generated")
	}
	if len(moyasar.reqs) != 1 {
		t.Fatalf("moyasar charges = %d, want 1", len(moyasar.reqs))
	}
	want := port.SavedMethodCharge{MethodRef: "token_sar_" + h.run, Amount: 11500, Currency: "SAR", InvoiceID: inv.ID.String(), IdempotencyKey: "renewal-" + inv.ID.String()}
	if moyasar.reqs[0] != want {
		t.Fatalf("charge = %+v, want %+v", moyasar.reqs[0], want)
	}

	var status string
	var amountPaid int64
	if err := conn.QueryRowContext(ctx, `SELECT status, amount_paid FROM invoices WHERE id = $1`, inv.ID).Scan(&status, &amountPaid); err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	if status != string(domain.InvoiceStatusPaid) || amountPaid != 11500 {
		t.Fatalf("invoice status=%s amount_paid=%d, want paid/11500", status, amountPaid)
	}
	var invoiceLegs, paymentLegs int
	var paymentAmount int64
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FILTER (WHERE code = 1), COUNT(*) FILTER (WHERE code = 3), COALESCE(SUM(amount) FILTER (WHERE code = 3), 0)
		   FROM ledger_transactions WHERE reference_id = $1`, inv.ID).Scan(&invoiceLegs, &paymentLegs, &paymentAmount); err != nil {
		t.Fatalf("read ledger legs: %v", err)
	}
	if invoiceLegs != 1 || paymentLegs != 1 || paymentAmount != 11500 {
		t.Fatalf("ledger legs: invoice=%d payment=%d payment amount=%d, want 1/1/11500", invoiceLegs, paymentLegs, paymentAmount)
	}

	h.assertAuditGrade("saved-method SAR renewal settled")
}
