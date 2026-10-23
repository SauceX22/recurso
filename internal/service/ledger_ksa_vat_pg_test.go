package service

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/recurso-dev/recurso/internal/adapter/db"
	"github.com/recurso-dev/recurso/internal/core/domain"
)

// TestKSAVATInvoice_PostsTaxPayable_Postgres proves the KSA VAT computed by the
// resolver reaches the books like any other output tax (ADR-002): a SAR
// invoice from a Saudi seller posts Code-1 AR → Revenue at the gross and a
// Code-6 Revenue → Tax Payable reclass at exactly the 15% VAT, in halalas, and
// the reconciler finds no missing or mismatched tax leg.
func TestKSAVATInvoice_PostsTaxPayable_Postgres(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping postgres-backed KSA VAT ledger test")
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
	tenantID := seedRevRecTenant(t, conn)

	customerID := uuid.New()
	mustExec(t, conn, `INSERT INTO customers (id, tenant_id, email, name, country, tax_type, ledger_account_id, created_at, updated_at)
		VALUES ($1,$2,$3,'KSA Cust','Saudi Arabia','individual',$4,NOW(),NOW())`,
		customerID, tenantID, "ksa-"+customerID.String()[:8]+"@t.com", uuid.New())
	customer := &domain.Customer{ID: customerID, BillingAddress: domain.BillingAddress{Country: "Saudi Arabia"}}

	// A Saudi seller, a non-round SAR amount: 1,234.57 SAR → 185.19 SAR VAT.
	const net = int64(123457)
	res := NewTaxResolver(nil, "SA", "").ResolveInvoiceTax(ctx, tenantID, customer, "SAR", net, "")
	if res.Total != 18519 || res.TaxType != "vat" {
		t.Fatalf("resolved tax = %d %q, want 18519 vat", res.Total, res.TaxType)
	}

	invID := uuid.New()
	invNo := "INV-KSA-" + invID.String()[:8]
	total := net + res.Total
	mustExec(t, conn, `INSERT INTO invoices (id, tenant_id, customer_id, currency, subtotal, total, tax_amount, amount_paid, status, invoice_number, created_at, due_date)
		VALUES ($1,$2,$3,'SAR',$4,$5,$6,0,'open',$7,NOW(),NOW())`,
		invID, tenantID, customerID, net, total, res.Total, invNo)

	ledger := NewLedgerService(nil, db.NewLedgerRepository(conn))
	if err := ledger.RecordInvoice(ctx, &domain.Invoice{
		ID: invID, TenantID: tenantID, CustomerID: customerID, InvoiceNumber: invNo,
		Subtotal: net, TaxAmount: res.Total, Total: total, Currency: "SAR",
	}); err != nil {
		t.Fatalf("RecordInvoice: %v", err)
	}

	legAmount := func(code int) int64 {
		var amt int64
		if err := conn.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(amount),0) FROM ledger_transactions WHERE reference_id = $1 AND code = $2`,
			invID, code).Scan(&amt); err != nil {
			t.Fatalf("leg code %d: %v", code, err)
		}
		return amt
	}
	if got := legAmount(1); got != total {
		t.Errorf("Code-1 AR→Revenue = %d, want gross %d", got, total)
	}
	if got := legAmount(int(domain.LedgerCodeOutputTax)); got != 18519 {
		t.Errorf("Code-6 Revenue→Tax Payable = %d, want 18519 (15%% KSA VAT)", got)
	}

	// The Tax Payable account holds exactly the VAT.
	var taxPayable int64
	if err := conn.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(t.amount),0) FROM ledger_transactions t
		   JOIN ledger_accounts a ON a.id = t.credit_account_id
		  WHERE t.reference_id = $1 AND a.code = $2`,
		invID, domain.AccountCodeTaxPayable).Scan(&taxPayable); err != nil {
		t.Fatalf("tax payable: %v", err)
	}
	if taxPayable != 18519 {
		t.Errorf("Tax Payable credited %d, want 18519", taxPayable)
	}

	report, err := NewReconciliationService(db.NewLedgerRepository(conn), nil).Run(ctx, tenantID)
	if err != nil {
		t.Fatalf("reconciliation: %v", err)
	}
	for _, d := range report.Discrepancies {
		switch d.Type {
		case DiscrepancyMissingInvoiceTx, DiscrepancyInvoiceAmountMismatch,
			DiscrepancyMissingTaxTx, DiscrepancyTaxAmountMismatch,
			DiscrepancyLedgerUnbalanced, DiscrepancyAbnormalBalance:
			t.Errorf("ledger not audit-grade: %s %+v", d.Type, d)
		}
	}
}
