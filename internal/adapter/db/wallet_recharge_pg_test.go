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

// seedRechargeWallet creates a SAR wallet below its auto-recharge threshold
// for a fresh tenant and customer, removed after the test.
func seedRechargeWallet(t *testing.T, conn *sql.DB) (*WalletRepository, *domain.Wallet) {
	t.Helper()
	tenantID, customerID := seedPMTenantCustomer(t, conn)
	cleanupPMTenant(t, conn, tenantID, customerID)
	var entityID uuid.UUID
	if err := conn.QueryRow(`SELECT id FROM entities WHERE tenant_id = $1 AND is_primary`, tenantID).Scan(&entityID); err != nil {
		t.Fatalf("primary entity: %v", err)
	}
	repo := NewWalletRepository(conn).(*WalletRepository)
	now := time.Now().UTC()
	th, amt := int64(1000), int64(5000)
	w := &domain.Wallet{ID: uuid.New(), TenantID: tenantID, EntityID: entityID, CustomerID: customerID, Currency: "SAR",
		AutoRechargeThreshold: &th, AutoRechargeAmount: &amt, CreatedAt: now, UpdatedAt: now}
	if err := repo.Create(context.Background(), w); err != nil {
		t.Fatalf("create wallet: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(`DELETE FROM wallet_transactions WHERE wallet_id = $1`, w.ID)
		_, _ = conn.Exec(`DELETE FROM wallets WHERE id = $1`, w.ID)
	})
	return repo, w
}

func isDue(t *testing.T, repo *WalletRepository, id uuid.UUID) bool {
	t.Helper()
	// The sweep is global; look for this wallet in a page large enough to
	// hold every due wallet a shared test database accumulates.
	due, err := repo.ListDueForRecharge(context.Background(), 100000)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range due {
		if w.ID == id {
			return true
		}
	}
	return false
}

// The recharge key's attempt number advances only on an answered attempt and
// only from the attempt that was charged; a backoff or a hard-declined method
// keeps the wallet out of the sweep, and a newly saved method lets it back.
func TestWalletRechargeOutcome_Postgres(t *testing.T) {
	dbx := openCustomerPMTestDB(t)
	conn := dbx.DB
	repo, w := seedRechargeWallet(t, conn)
	ctx := context.Background()
	load := func() *domain.Wallet {
		t.Helper()
		got, err := repo.GetByID(ctx, w.TenantID, w.ID)
		if err != nil || got == nil {
			t.Fatalf("load wallet: %v", err)
		}
		return got
	}
	if !isDue(t, repo, w.ID) {
		t.Fatal("a wallet below its threshold is not due")
	}

	// Another tenant, or a stale attempt number, changes nothing.
	if err := repo.RecordRechargeOutcome(ctx, uuid.New(), w.ID, 0, port.RechargeOutcome{Answered: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordRechargeOutcome(ctx, w.TenantID, w.ID, 5, port.RechargeOutcome{Answered: true}); err != nil {
		t.Fatal(err)
	}
	if got := load(); got.RechargeAttempt != 0 {
		t.Fatalf("attempt = %d, want 0", got.RechargeAttempt)
	}

	// A lost response keeps the attempt (same key next time) but backs off.
	later := time.Now().Add(time.Hour)
	if err := repo.RecordRechargeOutcome(ctx, w.TenantID, w.ID, 0, port.RechargeOutcome{Failures: 1, RetryAt: &later}); err != nil {
		t.Fatal(err)
	}
	if got := load(); got.RechargeAttempt != 0 || got.RechargeFailures != 1 {
		t.Fatalf("attempt/failures = %d/%d, want 0/1", got.RechargeAttempt, got.RechargeFailures)
	}
	if isDue(t, repo, w.ID) {
		t.Fatal("a wallet backing off is listed as due")
	}

	// A hard decline advances the attempt and blocks the declined method.
	if _, err := conn.Exec(`UPDATE customers SET default_payment_method = 'token_lost' WHERE id = $1`, w.CustomerID); err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordRechargeOutcome(ctx, w.TenantID, w.ID, 0, port.RechargeOutcome{Answered: true, Failures: 2, BlockedMethodRef: "token_lost"}); err != nil {
		t.Fatal(err)
	}
	if got := load(); got.RechargeAttempt != 1 || got.RechargeFailures != 2 {
		t.Fatalf("attempt/failures = %d/%d, want 1/2", got.RechargeAttempt, got.RechargeFailures)
	}
	if isDue(t, repo, w.ID) {
		t.Fatal("a wallet whose saved method hard-declined is listed as due")
	}
	if _, err := conn.Exec(`UPDATE customers SET default_payment_method = 'token_new' WHERE id = $1`, w.CustomerID); err != nil {
		t.Fatal(err)
	}
	if !isDue(t, repo, w.ID) {
		t.Fatal("a wallet whose customer saved a new method is not due again")
	}
}

// updated_at orders the recharge queue and must only move forward, even when
// the row's value is ahead of this database's clock (another writer's clock
// ran ahead), on a recorded attempt and on a top-up alike.
func TestWalletUpdatedAtNeverMovesBackwards_Postgres(t *testing.T) {
	dbx := openCustomerPMTestDB(t)
	conn := dbx.DB
	repo, w := seedRechargeWallet(t, conn)
	ctx := context.Background()
	read := func() time.Time {
		t.Helper()
		got, err := repo.GetByID(ctx, w.TenantID, w.ID)
		if err != nil || got == nil {
			t.Fatalf("load wallet: %v", err)
		}
		return got.UpdatedAt
	}
	for _, step := range []struct {
		name string
		run  func() error
	}{
		{"recorded attempt", func() error {
			return repo.RecordRechargeOutcome(ctx, w.TenantID, w.ID, 0, port.RechargeOutcome{Answered: true})
		}},
		{"top-up", func() error {
			_, err := repo.TopUp(ctx, &domain.WalletTransaction{ID: uuid.New(), TenantID: w.TenantID, WalletID: w.ID,
				Type: domain.WalletTxTopUp, Source: domain.WalletSourceManual, Amount: 100, CreatedAt: time.Now().UTC()})
			return err
		}},
	} {
		if _, err := conn.Exec(`UPDATE wallets SET updated_at = NOW() + INTERVAL '1 hour' WHERE id = $1`, w.ID); err != nil {
			t.Fatal(err)
		}
		before := read()
		if err := step.run(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if after := read(); !after.After(before) {
			t.Fatalf("%s: updated_at went %v -> %v", step.name, before, after)
		}
	}
}
