package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
	"github.com/recurso-dev/recurso/internal/core/port/porttest"
)

// recordingLedgerRepo records the ledger transactions a refund posts.
type recordingLedgerRepo struct {
	porttest.UnimplementedLedgerRepository
	txs []*domain.LedgerTransaction
}

func (r *recordingLedgerRepo) GetAccountByTenantAndCode(_ context.Context, tenantID uuid.UUID, code int) (*domain.LedgerAccount, error) {
	return &domain.LedgerAccount{ID: uuid.New(), TenantID: tenantID, Code: code}, nil
}

func (r *recordingLedgerRepo) CreateTransaction(_ context.Context, tx *domain.LedgerTransaction) error {
	r.txs = append(r.txs, tx)
	return nil
}

// refundedFor sums the Refund-code transfers posted for a credit note.
func (r *recordingLedgerRepo) refundedFor(cnID uuid.UUID) int64 {
	var sum int64
	for _, tx := range r.txs {
		if tx.ReferenceID == cnID && tx.Code == 4 {
			sum += int64(tx.Amount)
		}
	}
	return sum
}

func withRecordingLedger(f *cnFixture) *recordingLedgerRepo {
	repo := &recordingLedgerRepo{}
	f.svc.SetLedgerService(NewLedgerService(nil, repo))
	return repo
}

// M3: a refund whose outcome the gateway could not confirm (the answer was
// lost and the read-back did not show it) may never have happened. It stays
// pending and is not booked: no DR Refunds / CR Cash while the customer may
// not have been refunded.
func TestCreditNote_UnknownRefundOutcomeIsNotBooked(t *testing.T) {
	f := newCNFixture(paidInvoice(1000, "SAR", "pay_1"))
	ledger := withRecordingLedger(f)
	f.gateway.result = &port.RefundResult{RefundID: "pay_1:unknown:" + uuid.NewString(), Status: "pending"}

	cn, err := f.svc.Create(context.Background(), f.tenantID, uuid.Nil, "", refundRequest(f, 400, "SAR"))
	if err != nil {
		t.Fatal(err)
	}
	if cn.RefundStatus != domain.RefundStatusPending {
		t.Fatalf("refund status = %s, want pending", cn.RefundStatus)
	}
	if len(ledger.txs) != 0 {
		t.Fatalf("unknown refund booked %d ledger transactions, want none", len(ledger.txs))
	}
}

// Stripe's pending is a real gateway state that its webhook resolves; it is
// booked when issued, as before.
func TestCreditNote_GatewayPendingRefundIsStillBooked(t *testing.T) {
	f := newCNFixture(paidInvoice(1000, "USD", "pi_1"))
	ledger := withRecordingLedger(f)
	f.gateway.result = &port.RefundResult{RefundID: "re_1", Status: "pending"}

	cn, err := f.svc.Create(context.Background(), f.tenantID, uuid.Nil, "", refundRequest(f, 400, "USD"))
	if err != nil {
		t.Fatal(err)
	}
	if got := ledger.refundedFor(cn.ID); got != 400 {
		t.Fatalf("booked refund = %d, want 400", got)
	}
}

func unknownRefundNote(f *cnFixture, paymentID string, amount int64) *domain.CreditNote {
	id := paymentID + ":unknown:" + uuid.NewString()
	return &domain.CreditNote{
		ID: uuid.New(), TenantID: f.tenantID, CustomerID: f.customerID, InvoiceID: &f.invoiceID,
		Type: domain.CreditNoteTypeRefund, Status: domain.CreditNoteStatusIssued, Amount: amount,
		RefundStatus: domain.RefundStatusPending, RefundID: &id,
	}
}

// The gateway's refunded total resolves unknown refunds, oldest first: what
// it covers beyond the refunds already processed was applied (now booked);
// the rest never was (failed, nothing booked, and the refundable amount is
// released).
func TestCreditNote_ResolveUnknownRefunds(t *testing.T) {
	f := newCNFixture(paidInvoice(1000, "SAR", "pay_1"))
	ledger := withRecordingLedger(f)
	processedID := "pay_1:300"
	processed := &domain.CreditNote{
		ID: uuid.New(), TenantID: f.tenantID, CustomerID: f.customerID, InvoiceID: &f.invoiceID,
		Type: domain.CreditNoteTypeRefund, Status: domain.CreditNoteStatusIssued, Amount: 300,
		RefundStatus: domain.RefundStatusProcessed, RefundID: &processedID,
	}
	applied := unknownRefundNote(f, "pay_1", 200)
	notApplied := unknownRefundNote(f, "pay_1", 100)
	otherPayment := unknownRefundNote(f, "pay_2", 50)
	f.repo.created = []*domain.CreditNote{processed, applied, notApplied, otherPayment}

	inv, _ := f.invRepo.GetByIDPublic(context.Background(), f.invoiceID)
	if err := f.svc.ResolveUnknownRefunds(context.Background(), inv, "pay_1", 500); err != nil {
		t.Fatal(err)
	}

	got := map[uuid.UUID]domain.CreditNoteRefundStatus{}
	for _, u := range f.repo.updates {
		got[u.id] = u.status
		if u.refundID == nil || (*u.refundID != *applied.RefundID && *u.refundID != *notApplied.RefundID) {
			t.Fatalf("update %+v changed the refund id", u)
		}
	}
	if len(f.repo.updates) != 2 || got[applied.ID] != domain.RefundStatusProcessed || got[notApplied.ID] != domain.RefundStatusFailed {
		t.Fatalf("updates = %+v, want the 200 processed and the 100 failed", f.repo.updates)
	}
	if b := ledger.refundedFor(applied.ID); b != 200 {
		t.Fatalf("booked for the applied refund = %d, want 200", b)
	}
	if len(ledger.txs) != 1 {
		t.Fatalf("ledger transactions = %d, want only the applied refund's", len(ledger.txs))
	}
}

// A payment that is not the invoice's recorded one (a duplicate refunded on
// the dashboard) resolves nothing.
func TestCreditNote_ResolveUnknownRefundsIgnoresOtherPayments(t *testing.T) {
	f := newCNFixture(paidInvoice(1000, "SAR", "pay_1"))
	f.repo.created = []*domain.CreditNote{unknownRefundNote(f, "pay_1", 200)}
	inv, _ := f.invRepo.GetByIDPublic(context.Background(), f.invoiceID)
	if err := f.svc.ResolveUnknownRefunds(context.Background(), inv, "pay_9", 1000); err != nil {
		t.Fatal(err)
	}
	if len(f.repo.updates) != 0 {
		t.Fatalf("updates = %+v, want none", f.repo.updates)
	}
}
