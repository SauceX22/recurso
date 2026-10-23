package service

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/recurso-dev/recurso/internal/adapter/db"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
)

// pgCardSaver adapts the real CustomerRepository to the settlement's card
// saver. It accepts either repository signature while the expiry-carrying one
// lands; storesExpiry says which one ran.
type pgCardSaver struct {
	repo         any
	storesExpiry bool
}

func (a *pgCardSaver) SetGatewayPaymentMethod(ctx context.Context, tenantID, customerID uuid.UUID, provider domain.GatewayProvider, ref, brand, last4 string, expMonth, expYear int) error {
	switch r := a.repo.(type) {
	case gatewayMethodSaver:
		a.storesExpiry = true
		return r.SetGatewayPaymentMethod(ctx, tenantID, customerID, provider, ref, brand, last4, expMonth, expYear)
	case interface {
		SetGatewayPaymentMethod(ctx context.Context, tenantID, customerID uuid.UUID, provider, ref, brand, last4 string) error
	}:
		return r.SetGatewayPaymentMethod(ctx, tenantID, customerID, string(provider), ref, brand, last4)
	default:
		return fmt.Errorf("customer repository has no SetGatewayPaymentMethod")
	}
}

// TestGatewaySettlement_SettlesLedger_Postgres drives the shared settlement
// against the real invoice, subscription and customer repositories and the
// real MarkInvoicePaid: the invoice and payment ledger legs post once, the
// payment id and card are recorded, a redelivery changes nothing, and a
// second payment neither moves the refund target nor replaces the card.
func TestGatewaySettlement_SettlesLedger_Postgres(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping postgres-backed gateway settlement test")
	}
	if err := db.RunMigrations(dbURL); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	dbx, err := sqlx.Open("postgres", dbURL)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = dbx.Close() })
	conn := dbx.DB
	h := newInvariantHarness(t, conn, dbx)
	ctx := context.Background()

	const total = int64(11500)
	planID, customerID, subID, invID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExec(t, conn, `INSERT INTO plans (id, tenant_id, name, code, interval_unit, interval_count, active) VALUES ($1,$2,'sar-plan',$3,'month',1,TRUE)`,
		planID, h.tenantID, "sar-settle-"+h.run)
	mustExec(t, conn, `INSERT INTO customers (id, tenant_id, email, name, country, tax_type, ledger_account_id, created_at, updated_at)
		VALUES ($1,$2,$3,'SAR Cust','Saudi Arabia','individual',$4,NOW(),NOW())`,
		customerID, h.tenantID, "sar-settle-"+h.run+"@t.com", uuid.New())
	mustExec(t, conn, `INSERT INTO subscriptions (id, tenant_id, customer_id, plan_id, status, current_period_start, current_period_end, billing_anchor, created_at, updated_at)
		VALUES ($1,$2,$3,$4,'active', NOW(), NOW() + INTERVAL '1 month', NOW(), NOW(), NOW())`,
		subID, h.tenantID, customerID, planID)
	mustExec(t, conn, `INSERT INTO invoices (id, tenant_id, customer_id, subscription_id, currency, subtotal, total, amount_paid, status, invoice_number, created_at, due_date)
		VALUES ($1,$2,$3,$4,'SAR',$5,$5,0,'open',$6,NOW(),NOW())`,
		invID, h.tenantID, customerID, subID, total, "INV-SETTLE-"+h.run)
	t.Cleanup(func() {
		for _, del := range []struct {
			q  string
			id uuid.UUID
		}{
			{`DELETE FROM revenue_schedules WHERE invoice_id = $1`, invID},
			{`DELETE FROM ledger_transactions WHERE reference_id = $1`, invID},
			{`DELETE FROM invoices WHERE id = $1`, invID},
			{`DELETE FROM subscriptions WHERE id = $1`, subID},
			{`DELETE FROM customers WHERE id = $1`, customerID},
			{`DELETE FROM plans WHERE id = $1`, planID},
		} {
			if _, err := conn.ExecContext(ctx, del.q, del.id); err != nil {
				t.Errorf("cleanup %q: %v", del.q, err)
			}
		}
	})

	invRepo := db.NewInvoiceRepository(conn).(*db.InvoiceRepository)
	seeded := &domain.Invoice{ID: invID, TenantID: h.tenantID, CustomerID: customerID, SubscriptionID: &subID,
		Status: domain.InvoiceStatusOpen, Total: total, Subtotal: total, Currency: "SAR"}
	if err := h.ledger.RecordInvoice(ctx, seeded); err != nil {
		t.Fatalf("RecordInvoice: %v", err)
	}

	cards := &pgCardSaver{repo: db.NewCustomerRepository(dbx)}
	svc := NewGatewaySettlementService(domain.GatewayMoyasar, h.subSvc, invRepo, db.NewSubscriptionRepository(conn))
	svc.SetCardSaving(cards, &fakeTokenReader{month: 9, year: 2031})

	load := func() *domain.Invoice {
		t.Helper()
		inv, err := invRepo.GetByIDPublic(ctx, invID)
		if err != nil || inv == nil {
			t.Fatalf("load invoice: %v", err)
		}
		return inv
	}
	payment := func(id, token string) *port.GatewayPayment {
		return &port.GatewayPayment{ID: id, Status: "paid", Amount: total, Currency: "SAR", InvoiceID: invID.String(),
			CardToken: token, CardBrand: "mada", CardLast4: "1010"}
	}
	payA, payB := uuid.NewString(), uuid.NewString()

	// The background ctx carries no tenant: Settle must supply the invoice's.
	if transitioned, err := svc.Settle(ctx, load(), payment(payA, "token_a_"+h.run)); err != nil || !transitioned {
		t.Fatalf("Settle = %v, %v; want transitioned", transitioned, err)
	}

	type invoiceRow struct {
		status     string
		amountPaid int64
		paymentID  sql.NullString
	}
	readInvoice := func() invoiceRow {
		t.Helper()
		var r invoiceRow
		if err := conn.QueryRowContext(ctx, `SELECT status, amount_paid, gateway_payment_id FROM invoices WHERE id = $1`, invID).
			Scan(&r.status, &r.amountPaid, &r.paymentID); err != nil {
			t.Fatalf("read invoice: %v", err)
		}
		return r
	}
	type cardRow struct {
		provider, ref     sql.NullString
		expMonth, expYear sql.NullInt64
	}
	readCard := func() cardRow {
		t.Helper()
		var r cardRow
		if err := conn.QueryRowContext(ctx, `SELECT pm_provider, default_payment_method, card_exp_month, card_exp_year FROM customers WHERE id = $1`, customerID).
			Scan(&r.provider, &r.ref, &r.expMonth, &r.expYear); err != nil {
			t.Fatalf("read customer: %v", err)
		}
		return r
	}
	ledgerLegs := func() map[int]int64 {
		t.Helper()
		rows, err := conn.QueryContext(ctx, `SELECT code, SUM(amount) FROM ledger_transactions WHERE reference_id = $1 GROUP BY code`, invID)
		if err != nil {
			t.Fatalf("read ledger: %v", err)
		}
		defer func() { _ = rows.Close() }()
		legs := map[int]int64{}
		for rows.Next() {
			var code int
			var amt int64
			if err := rows.Scan(&code, &amt); err != nil {
				t.Fatalf("scan ledger: %v", err)
			}
			legs[code] = amt
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("ledger rows: %v", err)
		}
		return legs
	}

	if r := readInvoice(); r.status != string(domain.InvoiceStatusPaid) || r.amountPaid != total || r.paymentID.String != payA {
		t.Fatalf("invoice = %+v, want paid/%d/%s", r, total, payA)
	}
	c := readCard()
	if c.provider.String != string(domain.GatewayMoyasar) || c.ref.String != "token_a_"+h.run {
		t.Fatalf("customer card = %+v, want moyasar token_a", c)
	}
	if cards.storesExpiry && (c.expMonth.Int64 != 9 || c.expYear.Int64 != 2031) {
		t.Fatalf("card expiry = %v/%v, want 9/2031", c.expMonth, c.expYear)
	}
	legs := ledgerLegs()
	const invoiceCode, paymentCode = 1, 3
	if legs[invoiceCode] != total || legs[paymentCode] != total {
		t.Fatalf("ledger legs = %v, want invoice and payment legs of %d", legs, total)
	}
	h.assertAuditGrade("gateway payment settled")

	// Redelivery of the same payment (the checkout return after the webhook).
	if transitioned, err := svc.Settle(ctx, load(), payment(payA, "token_a_"+h.run)); err != nil || transitioned {
		t.Fatalf("redelivery Settle = %v, %v; want not transitioned", transitioned, err)
	}
	// A second, different payment for the already-settled invoice.
	if transitioned, err := svc.Settle(ctx, load(), payment(payB, "token_b_"+h.run)); err != nil || transitioned {
		t.Fatalf("second payment Settle = %v, %v; want not transitioned", transitioned, err)
	}
	if r := readInvoice(); r.paymentID.String != payA {
		t.Fatalf("refund target = %q, want first payment %s kept", r.paymentID.String, payA)
	}
	if c := readCard(); c.ref.String != "token_a_"+h.run {
		t.Fatalf("saved card = %q, want the first payment's card kept", c.ref.String)
	}
	if after := ledgerLegs(); after[invoiceCode] != total || after[paymentCode] != total || len(after) != len(legs) {
		t.Fatalf("ledger changed on redelivery: before %v after %v", legs, after)
	}
	h.assertAuditGrade("gateway payment redelivered")
}
