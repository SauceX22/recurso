package service

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/recurso-dev/recurso/internal/adapter/db"
	"github.com/recurso-dev/recurso/internal/core/domain"
)

// TestWalletRechargeKeyAdvances_Postgres: the auto-recharge key follows the
// wallet row's updated_at, so it must move on every top-up and on a recorded
// declined attempt, and only for the wallet's own tenant.
func TestWalletRechargeKeyAdvances_Postgres(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping postgres-backed wallet test")
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

	tenantID := uuid.New()
	mustExec(t, conn, `INSERT INTO tenants (id, name, email, created_at, updated_at) VALUES ($1,$2,$3,NOW(),NOW())`,
		tenantID, "WRK-"+tenantID.String()[:8], tenantID.String()[:8]+"@t.com")
	customerID := uuid.New()
	mustExec(t, conn, `INSERT INTO customers (id, tenant_id, name, email, ledger_account_id, created_at) VALUES ($1,$2,$3,$4,$5,NOW())`,
		customerID, tenantID, "WRK Cust", "wrk-"+customerID.String()[:8]+"@example.com", uuid.New())
	var entityID uuid.UUID
	if err := conn.QueryRowContext(ctx, `SELECT id FROM entities WHERE tenant_id=$1 AND is_primary`, tenantID).Scan(&entityID); err != nil {
		t.Fatalf("primary entity: %v", err)
	}

	repo := db.NewWalletRepository(conn)
	now := time.Now().UTC()
	wallet := &domain.Wallet{ID: uuid.New(), TenantID: tenantID, EntityID: entityID, CustomerID: customerID, Currency: "SAR", CreatedAt: now, UpdatedAt: now}
	if err := repo.Create(ctx, wallet); err != nil {
		t.Fatalf("create wallet: %v", err)
	}
	updatedAt := func() time.Time {
		t.Helper()
		w, err := repo.GetByID(ctx, tenantID, wallet.ID)
		if err != nil || w == nil {
			t.Fatalf("load wallet: %v", err)
		}
		return w.UpdatedAt
	}

	before := updatedAt()
	if err := repo.MarkRechargeAttempted(ctx, uuid.New(), wallet.ID); err != nil {
		t.Fatal(err)
	}
	if got := updatedAt(); !got.Equal(before) {
		t.Fatalf("another tenant's call moved updated_at %v -> %v", before, got)
	}
	if err := repo.MarkRechargeAttempted(ctx, tenantID, wallet.ID); err != nil {
		t.Fatal(err)
	}
	afterDecline := updatedAt()
	if !afterDecline.After(before) {
		t.Fatalf("updated_at %v did not advance past %v after a declined attempt", afterDecline, before)
	}
	if _, err := repo.TopUp(ctx, &domain.WalletTransaction{
		ID: uuid.New(), TenantID: tenantID, WalletID: wallet.ID, Type: domain.WalletTxTopUp,
		Source: domain.WalletSourceManual, Amount: 100, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if got := updatedAt(); !got.After(afterDecline) {
		t.Fatalf("updated_at %v did not advance past %v after a top-up", got, afterDecline)
	}
}
