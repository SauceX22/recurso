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
// the Moyasar renewal CHARGE and SETTLEMENT path.
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

// TestRenewalMoyasar_SettlesLedger_Postgres renews a SAR subscription whose
// customer saved a Moyasar token: the renewal claim picks it up (no gateway
// subscription id, since Moyasar never returns one), the provider path charges
// the token with a fake Moyasar charger, and the REAL
// SubscriptionService.MarkInvoicePaid settles it. The reconciliation oracle
// then proves the invoice and payment legs are posted and the books are
// audit-grade.
func TestRenewalMoyasar_SettlesLedger_Postgres(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping postgres-backed Moyasar renewal test")
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

	// SAR plan, customer with a saved Moyasar token, and a due subscription
	// with NO gateway subscription id (Moyasar's CreateSubscription errors and
	// returns "", so nothing is stored).
	planID := uuid.New()
	mustExec(t, conn, `INSERT INTO plans (id, tenant_id, name, code, interval_unit, interval_count, active) VALUES ($1,$2,'sar-plan',$3,'month',1,TRUE)`,
		planID, h.tenantID, "sar-plan-"+h.run)
	mustExec(t, conn, `INSERT INTO prices (id, plan_id, currency, amount, type) VALUES ($1,$2,'SAR',11500,'recurring')`, uuid.New(), planID)

	customerID := uuid.New()
	mustExec(t, conn, `INSERT INTO customers (id, tenant_id, email, name, country, tax_type, ledger_account_id, created_at, updated_at)
		VALUES ($1,$2,$3,'SAR Cust','Saudi Arabia','individual',$4,NOW(),NOW())`,
		customerID, h.tenantID, "sar-"+h.run+"@t.com", uuid.New())
	custRepo := db.NewCustomerRepository(dbx)
	if err := custRepo.SetGatewayPaymentMethod(ctx, h.tenantID, customerID, domain.SavedMethodMoyasar, "token_sar_"+h.run, "mada", "1010"); err != nil {
		t.Fatalf("SetGatewayPaymentMethod: %v", err)
	}

	subID := uuid.New()
	mustExec(t, conn, `INSERT INTO subscriptions (id, tenant_id, customer_id, plan_id, status, current_period_start, current_period_end, billing_anchor, created_at, updated_at)
		VALUES ($1,$2,$3,$4,'active', NOW() - INTERVAL '1 month', NOW() - INTERVAL '1 minute', NOW() - INTERVAL '1 month', NOW(), NOW())`,
		subID, h.tenantID, customerID, planID)

	subRepo := db.NewSubscriptionRepository(conn).(*db.SubscriptionRepository)
	claimed, err := subRepo.ClaimDueForRenewal(ctx, 15*time.Minute, 1000)
	if err != nil {
		t.Fatalf("ClaimDueForRenewal: %v", err)
	}
	var sub *domain.Subscription
	for _, s := range claimed {
		if s.ID == subID {
			sub = s
		}
	}
	if sub == nil {
		t.Fatal("SAR subscription without a gateway subscription id was not claimed for renewal")
	}

	invoicer := &pgRenewalInvoicer{h: h, amount: 11500, currency: "SAR"}
	svc := NewRenewalService(subRepo, db.NewPlanRepository(conn), invoicer)
	// Stripe mocked in this deployment: no Stripe charger, settler wired.
	svc.SetSavedMethodCharging(nil, custRepo, h.subSvc)
	moyasar := &recordingSavedCharger{result: &port.PaymentResult{Success: true, PaymentID: uuid.NewString()}}
	pm := NewProviderSavedMethods(custRepo)
	pm.Register(domain.SavedMethodMoyasar, moyasar)
	svc.SetProviderSavedMethods(pm)

	// The plan lookup is tenant-scoped, so renew under the tenant ctx.
	if err := svc.RenewSubscription(h.tctx, sub); err != nil {
		t.Fatalf("RenewSubscription: %v", err)
	}
	inv := invoicer.invoice
	if inv == nil {
		t.Fatal("no renewal invoice generated")
	}
	if len(moyasar.calls) != 1 {
		t.Fatalf("moyasar charges = %d, want 1", len(moyasar.calls))
	}
	want := savedChargeCall{"", "token_sar_" + h.run, 11500, "SAR", inv.ID.String(), "renewal-" + inv.ID.String()}
	if moyasar.calls[0] != want {
		t.Fatalf("charge = %+v, want %+v", moyasar.calls[0], want)
	}

	var status string
	var amountPaid int64
	if err := conn.QueryRowContext(ctx, `SELECT status, amount_paid FROM invoices WHERE id = $1`, inv.ID).Scan(&status, &amountPaid); err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	if status != string(domain.InvoiceStatusPaid) || amountPaid != 11500 {
		t.Fatalf("invoice status=%s amount_paid=%d, want paid/11500", status, amountPaid)
	}
	var legs int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(DISTINCT code) FROM ledger_transactions WHERE reference_id = $1`, inv.ID).Scan(&legs); err != nil {
		t.Fatalf("count ledger legs: %v", err)
	}
	if legs < 2 {
		t.Fatalf("ledger transactions for the renewal invoice = %d, want invoice + payment legs", legs)
	}

	h.assertAuditGrade("moyasar SAR renewal settled")
}
