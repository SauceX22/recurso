package scheduler

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/recurso-dev/recurso/internal/core/domain"
)

// writeOffDunningRepo is the minimal dunning repo for the write-off branch:
// only MarkAsUncollectible is reachable there.
type writeOffDunningRepo struct {
	InvoiceRepoForDunning
	marked []uuid.UUID
}

func (r *writeOffDunningRepo) MarkAsUncollectible(_ context.Context, id uuid.UUID) error {
	r.marked = append(r.marked, id)
	return nil
}

// strictTenantInvoiceReader mirrors db.InvoiceRepository.GetByID: it fails
// closed when ctx carries no tenant, and scopes the read to that tenant.
type strictTenantInvoiceReader struct {
	invoice *domain.Invoice
}

func (r *strictTenantInvoiceReader) GetByID(ctx context.Context, id uuid.UUID) (*domain.Invoice, error) {
	tenantID, ok := ctx.Value(domain.TenantIDKey).(uuid.UUID)
	if !ok {
		return nil, fmt.Errorf("tenant_id missing from context")
	}
	if tenantID != r.invoice.TenantID || id != r.invoice.ID {
		return nil, fmt.Errorf("invoice not found")
	}
	return r.invoice, nil
}

type recordingWriteOffPoster struct {
	posted []*domain.Invoice
}

func (p *recordingWriteOffPoster) RecordInvoiceWriteOff(_ context.Context, inv *domain.Invoice) error {
	p.posted = append(p.posted, inv)
	return nil
}

// TestDunning_AutoWriteOff_PostsLedgerReversalOnBackgroundCtx: the dunning
// sweep runs on context.Background(). When it auto-writes-off an invoice it
// must still load the full invoice through the tenant-scoped reader and post
// the ledger reversal; otherwise AR, Deferred and Tax Payable stay overstated.
func TestDunning_AutoWriteOff_PostsLedgerReversalOnBackgroundCtx(t *testing.T) {
	tenantID := uuid.New()
	inv := &domain.Invoice{ID: uuid.New(), TenantID: tenantID, InvoiceNumber: "INV-WO-1"}
	repo := &writeOffDunningRepo{}
	poster := &recordingWriteOffPoster{}

	s := &DunningScheduler{invoiceRepo: repo, config: DefaultDunningConfig()}
	s.SetWriteOffLedger(poster, &strictTenantInvoiceReader{invoice: inv})

	s.processInvoice(context.Background(), domain.OverdueInvoice{
		ID:            inv.ID,
		TenantID:      tenantID,
		InvoiceNumber: inv.InvoiceNumber,
		DueDate:       time.Now().AddDate(0, 0, -60), // past CancelAfterDays
	})

	if len(repo.marked) != 1 || repo.marked[0] != inv.ID {
		t.Fatalf("invoice not marked uncollectible: %v", repo.marked)
	}
	if len(poster.posted) != 1 || poster.posted[0].ID != inv.ID {
		t.Fatalf("write-off ledger reversal not posted (posted=%d) — invoice read ran without the invoice tenant", len(poster.posted))
	}
}
