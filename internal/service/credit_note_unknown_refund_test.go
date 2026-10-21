package service

import (
	"context"
	"testing"
	"time"

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

func processedRefundNote(f *cnFixture, refundID string, amount int64) *domain.CreditNote {
	return &domain.CreditNote{
		ID: uuid.New(), TenantID: f.tenantID, CustomerID: f.customerID, InvoiceID: &f.invoiceID,
		Type: domain.CreditNoteTypeRefund, Status: domain.CreditNoteStatusIssued, Amount: amount,
		RefundStatus: domain.RefundStatusProcessed, RefundID: &refundID,
	}
}

func resolveUnknown(t *testing.T, f *cnFixture, refundedTotal int64) {
	t.Helper()
	inv, _ := f.invRepo.GetByIDPublic(context.Background(), f.invoiceID)
	if err := f.svc.ResolveUnknownRefunds(context.Background(), inv, "pay_1", refundedTotal); err != nil {
		t.Fatal(err)
	}
}

// The unambiguous case resolves: one unknown refund, and the gateway's total
// beyond the processed refunds is exactly its amount. It becomes processed,
// keeps its refund id, and is booked then.
func TestCreditNote_ResolveUnknownRefunds(t *testing.T) {
	f := newCNFixture(paidInvoice(1000, "SAR", "pay_1"))
	ledger := withRecordingLedger(f)
	processed := processedRefundNote(f, "pay_1:300", 300)
	applied := unknownRefundNote(f, "pay_1", 200)
	otherPayment := unknownRefundNote(f, "pay_2", 50)
	f.repo.created = []*domain.CreditNote{processed, applied, otherPayment}

	resolveUnknown(t, f, 500)

	if len(f.repo.updates) != 1 || f.repo.updates[0].id != applied.ID || f.repo.updates[0].status != domain.RefundStatusProcessed {
		t.Fatalf("updates = %+v, want only the 200 processed", f.repo.updates)
	}
	if u := f.repo.updates[0]; u.refundID == nil || *u.refundID != *applied.RefundID {
		t.Fatalf("update %+v changed the refund id", u)
	}
	if b := ledger.refundedFor(applied.ID); b != 200 || len(ledger.txs) != 1 {
		t.Fatalf("booked %d in %d transactions, want 200 in 1", b, len(ledger.txs))
	}
}

// M1 (review4): Moyasar's refunded total is one number with no refund ids, so
// it cannot say which of several refunds happened. Every case it cannot
// settle unambiguously leaves the notes pending and unbooked for an operator
// to confirm on the dashboard, and a total alone never fails a note.
func TestCreditNote_ResolveUnknownRefundsLeavesAmbiguousCasesPending(t *testing.T) {
	cases := []struct {
		name     string
		notes    func(f *cnFixture) []*domain.CreditNote
		refunded int64
	}{
		{
			// The review's probe: the older 30 never applied, the newer 40
			// did. Oldest first booked the 30 and failed the 40.
			name: "two unknown refunds",
			notes: func(f *cnFixture) []*domain.CreditNote {
				older := unknownRefundNote(f, "pay_1", 30)
				older.CreatedAt = time.Now().Add(-time.Hour)
				newer := unknownRefundNote(f, "pay_1", 40)
				newer.CreatedAt = time.Now()
				return []*domain.CreditNote{older, newer}
			},
			refunded: 40,
		},
		{
			// A dashboard refund has no credit note but is in the total.
			name: "total includes a dashboard refund",
			notes: func(f *cnFixture) []*domain.CreditNote {
				return []*domain.CreditNote{unknownRefundNote(f, "pay_1", 30)}
			},
			refunded: 70,
		},
		{
			// A refund being issued now (no refund id stored yet) may already
			// be in the total, so the total may be its money.
			name: "another refund in flight",
			notes: func(f *cnFixture) []*domain.CreditNote {
				inFlight := &domain.CreditNote{
					ID: uuid.New(), TenantID: f.tenantID, CustomerID: f.customerID, InvoiceID: &f.invoiceID,
					Type: domain.CreditNoteTypeRefund, Status: domain.CreditNoteStatusIssued, Amount: 30,
					RefundStatus: domain.RefundStatusPending,
				}
				return []*domain.CreditNote{unknownRefundNote(f, "pay_1", 30), inFlight}
			},
			refunded: 30,
		},
		{
			// Nothing beyond the processed refunds: the unknown one may still
			// be landing, so the total alone does not fail it.
			name: "total covers only processed refunds",
			notes: func(f *cnFixture) []*domain.CreditNote {
				return []*domain.CreditNote{processedRefundNote(f, "pay_1:300", 300), unknownRefundNote(f, "pay_1", 200)}
			},
			refunded: 300,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCNFixture(paidInvoice(1000, "SAR", "pay_1"))
			ledger := withRecordingLedger(f)
			f.repo.created = tc.notes(f)

			resolveUnknown(t, f, tc.refunded)

			if len(f.repo.updates) != 0 || len(ledger.txs) != 0 {
				t.Fatalf("updates = %+v, ledger transactions = %d, want every note left pending and unbooked", f.repo.updates, len(ledger.txs))
			}
		})
	}
}

// M2 (review4): two deliveries of the webhook (a retry of a slow one, or two
// payment_refunded events together) both read the note as pending. Only the
// one that wins the pending -> processed transition books it, so the ledger
// legs and the rev-rec unwind happen once.
func TestCreditNote_ResolveUnknownRefundsBooksOnceUnderRedelivery(t *testing.T) {
	f := newCNFixture(paidInvoice(1000, "SAR", "pay_1"))
	ledger := withRecordingLedger(f)
	note := unknownRefundNote(f, "pay_1", 200)
	f.repo.created = []*domain.CreditNote{note}
	f.repo.listed = []domain.CreditNote{*note} // both deliveries read it pending

	resolveUnknown(t, f, 200)
	resolveUnknown(t, f, 200)

	if b := ledger.refundedFor(note.ID); b != 200 {
		t.Fatalf("booked %d for the refund across two deliveries, want 200 once", b)
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
