package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
	"github.com/recurso-dev/recurso/internal/service"
)

// RecoveryRecorder records recovered-revenue attribution when a retried
// payment succeeds. Implemented by service.DunningRecoveryService.
type RecoveryRecorder interface {
	RecordIfRecovered(ctx context.Context, inv *domain.Invoice) bool
}

// customerPaymentLookup resolves a customer's saved payment method. Implemented
// by *db.CustomerRepository.
type customerPaymentLookup interface {
	GetSavedPaymentMethod(ctx context.Context, customerID uuid.UUID) (domain.SavedPaymentMethod, error)
}

// invoiceSettler marks an invoice paid through the ledger path (amount_paid,
// recovery attribution, ledger entry, rev-rec schedule) — the same idempotent
// method the checkout and payment webhooks use. Implemented by
// *service.SubscriptionService.
type invoiceSettler interface {
	MarkInvoicePaid(ctx context.Context, invoiceID uuid.UUID) (bool, error)
}

// savedCardChargerRouter routes an off-session charge to the gateway a method
// was saved on. Implemented by *service.SavedCardGatewayRouter.
type savedCardChargerRouter interface {
	ChargerFor(ctx context.Context, m domain.SavedPaymentMethod, currency string) (service.SavedCardCharger, error)
}

// retryDecider schedules the next retry and learns from outcomes. Implemented
// by *service.SmartRetryService.
type retryDecider interface {
	DecideRetry(ctx context.Context, inv *domain.Invoice, errorCode string) *service.RetryDecision
	RecordOutcome(ctx context.Context, history domain.DunningHistory) error
}

type RetryWorker struct {
	invoiceRepo            port.InvoiceRepository
	retryService           retryDecider
	gateway                port.PaymentGateway
	notifier               port.Notifier
	dunningCampaignService *service.DunningCampaignService
	recoveryRecorder       RecoveryRecorder
	savedCharger           service.SavedCardCharger
	customerLookup         customerPaymentLookup
	chargerRouter          savedCardChargerRouter
	settler                invoiceSettler
}

func NewRetryWorker(
	invoiceRepo port.InvoiceRepository,
	retryService retryDecider,
	gateway port.PaymentGateway,
	notifier port.Notifier,
) *RetryWorker {
	return &RetryWorker{
		invoiceRepo:  invoiceRepo,
		retryService: retryService,
		gateway:      gateway,
		notifier:     notifier,
	}
}

func (w *RetryWorker) SetDunningCampaignService(svc *service.DunningCampaignService) {
	w.dunningCampaignService = svc
}

func (w *RetryWorker) SetRecoveryRecorder(rr RecoveryRecorder) {
	w.recoveryRecorder = rr
}

// SetSettler routes successful retries through the ledger-posting
// MarkInvoicePaid instead of a bare status update, so recovered payments get
// amount_paid, a ledger entry, and a rev-rec schedule like every other payment.
func (w *RetryWorker) SetSettler(s invoiceSettler) {
	w.settler = s
}

// SetSavedMethodCharging wires off-session collection with the customer's saved
// card (ENG-5 Phase 2). When either dependency is nil, retries fall back to the
// interactive gateway path — behavior is unchanged for customers without a
// saved payment method.
func (w *RetryWorker) SetSavedMethodCharging(charger service.SavedCardCharger, lookup customerPaymentLookup) {
	w.savedCharger = charger
	w.customerLookup = lookup
}

// SetChargerRouter wires per-gateway routing: the retry charges on the gateway
// the method was saved with. nil-safe — without it, only platform Stripe
// methods charge, on `savedCharger`.
func (w *RetryWorker) SetChargerRouter(router savedCardChargerRouter) {
	w.chargerRouter = router
}

// chargeInvoice attempts collection, preferring the customer's saved payment
// method (charged off-session) when one exists, and falling back to the
// interactive gateway retry otherwise.
func (w *RetryWorker) chargeInvoice(ctx context.Context, inv *domain.Invoice) (*port.PaymentResult, error) {
	// Collect only the OUTSTANDING balance — Total net of any wallet drain and
	// account credit already applied (mirrors renewal.go's amountDue). Charging
	// the full inv.Total here over-charged the customer's card by the wallet/
	// credit portion already settled at invoice generation.
	amountDue := inv.Total - inv.AmountPaid - inv.CreditApplied
	if amountDue <= 0 {
		// Fully covered by wallet/credit — nothing left to collect, so treat it as
		// paid rather than charging the card for an amount that isn't owed.
		return &port.PaymentResult{Success: true, PaymentID: noChargeDue}, nil
	}

	if charger, m, ok := w.savedMethodCharger(ctx, inv); ok {
		// Idempotent per attempt: a worker re-run for the same attempt won't
		// double-charge.
		return charger.ChargeSavedPaymentMethod(ctx, port.SavedMethodCharge{
			CustomerRef:    m.CustomerRef,
			MethodRef:      m.PaymentMethodRef,
			Amount:         amountDue,
			Currency:       inv.Currency,
			InvoiceID:      inv.ID.String(),
			IdempotencyKey: fmt.Sprintf("retry-%s-%d", inv.ID, inv.RetryCount),
		})
	}
	return w.gateway.RetryPayment(ctx, inv.ID.String(), amountDue, inv.Currency)
}

// savedMethodCharger resolves the customer's saved method and the charger for
// it. ok is false when there is nothing to charge off-session: no method, a
// lookup error, or a method that can't be charged for this invoice (another
// gateway, an unreachable BYO connection) — never the wrong account.
func (w *RetryWorker) savedMethodCharger(ctx context.Context, inv *domain.Invoice) (service.SavedCardCharger, domain.SavedPaymentMethod, bool) {
	if w.customerLookup == nil || (w.savedCharger == nil && w.chargerRouter == nil) {
		return nil, domain.SavedPaymentMethod{}, false
	}
	m, err := w.customerLookup.GetSavedPaymentMethod(ctx, inv.CustomerID)
	if err != nil || !m.Chargeable() {
		return nil, domain.SavedPaymentMethod{}, false
	}
	var router savedCardChargerRouter = w.chargerRouter
	if router == nil {
		router = service.NewSavedCardGatewayRouter(nil, nil, w.savedCharger)
	}
	charger, err := router.ChargerFor(ctx, m, inv.Currency)
	if err != nil {
		slog.Warn("retry: saved payment method can't be charged; using interactive retry",
			"invoice_id", inv.ID, "provider", m.Provider, "error", err)
		return nil, domain.SavedPaymentMethod{}, false
	}
	return charger, m, true
}

// Start runs the worker loop.
func (w *RetryWorker) Start(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second) // Poll every 10s for demo
	defer ticker.Stop()

	slog.Info("retry worker started")

	for {
		select {
		case <-ctx.Done():
			slog.Info("retry worker stopping")
			return
		case <-ticker.C:
			w.processRetries(ctx)
		}
	}
}

func (w *RetryWorker) processRetries(ctx context.Context) {
	// Atomically claim (lease) the due invoices so a second worker instance —
	// Cloud Run scales to many, and the Locker is a no-op without Redis — can't
	// pick up the same rows this cycle (ADR-003). The lease exceeds the poll
	// interval so a row can't lapse back to due while we're still charging it.
	invoices, err := w.invoiceRepo.ClaimDueForRetry(ctx, 10*time.Minute, 10)
	if err != nil {
		slog.Error("retry worker: failed to claim retry invoices", "error", err)
		return
	}

	if len(invoices) > 0 {
		slog.Info("retry worker: found invoices to retry", "count", len(invoices))
	}

	for _, inv := range invoices {
		w.processInvoice(ctx, inv)
	}
}

func (w *RetryWorker) processInvoice(ctx context.Context, inv *domain.Invoice) {
	slog.Info("retry worker: retrying invoice", "invoice_number", inv.InvoiceNumber, "attempt", inv.RetryCount+1)

	// 1. Attempt payment — via the customer's saved method (off-session) when
	// available, else the interactive gateway path.
	result, err := w.chargeInvoice(ctx, inv)
	if err != nil {
		// Infrastructure error (network, etc.) — schedule short retry, don't record outcome
		slog.Warn("retry worker: gateway infra error, scheduling 5min retry", "invoice_number", inv.InvoiceNumber, "error", err)
		shortRetry := time.Now().Add(5 * time.Minute)
		inv.NextRetryAt = &shortRetry
		if updateErr := w.invoiceRepo.Update(ctx, inv); updateErr != nil {
			slog.Error("retry worker: failed to update invoice", "invoice_id", inv.ID, "error", updateErr)
		}
		return
	}

	// Nothing chargeable off-session: a retry can't collect, so neither
	// schedule one nor stop dunning — the email ladder and pay link take over.
	// The bandit learns nothing about retry timing from this, so no outcome.
	if !result.Success && result.ErrorCode == port.ErrCodeNoSavedMethod {
		slog.Info("retry worker: no saved payment method, handing invoice to email dunning", "invoice_number", inv.InvoiceNumber)
		inv.LastPaymentError = result.ErrorCode
		w.stopRetries(ctx, inv, domain.InvoiceStatusPastDue, dunningManagedByScheduler)
		return
	}

	// 2. Record outcome for the PREVIOUS action (if one was tracked)
	if inv.DunningActionID != "" && inv.DunningContextKey != "" {
		outcome := "failure"
		reward := 0.0
		if result.Success {
			outcome = "success"
			reward = 1.0
		}

		historyErr := w.retryService.RecordOutcome(ctx, domain.DunningHistory{
			ID:            uuid.New(),
			TenantID:      inv.TenantID,
			InvoiceID:     inv.ID,
			ContextKey:    inv.DunningContextKey,
			ActionID:      inv.DunningActionID,
			RetryInterval: getDurationSeconds(inv.DunningActionID),
			Outcome:       outcome,
			Reward:        reward,
			CreatedAt:     time.Now(),
		})
		if historyErr != nil {
			slog.Error("retry worker: failed to record outcome", "invoice_number", inv.InvoiceNumber, "error", historyErr)
		} else {
			slog.Info("retry worker: recorded outcome", "outcome", outcome, "action_id", inv.DunningActionID, "context_key", inv.DunningContextKey)
		}
	}

	// 3. Handle result
	if result.Success {
		slog.Info("retry worker: payment succeeded", "invoice_number", inv.InvoiceNumber, "payment_id", result.PaymentID)
		if w.settler != nil {
			// Settle through the ledger path — the same idempotent method the
			// checkout and payment webhooks use, so amount_paid, recovery
			// attribution, the ledger entry, and the rev-rec schedule all
			// happen here rather than in a racing webhook that would no-op on
			// an already-paid invoice. The worker's ctx carries no tenant, so
			// inject the invoice's own (same as the webhook handlers).
			tenantCtx := context.WithValue(ctx, domain.TenantIDKey, inv.TenantID)
			transitioned, err := w.settler.MarkInvoicePaid(tenantCtx, inv.ID)
			if err != nil {
				// Leave dunning state untouched: the next poll retries and the
				// settle is idempotent, so nothing is lost or double-posted.
				slog.Error("retry worker: failed to settle invoice", "invoice_id", inv.ID, "error", err)
				return
			}
			w.recordSettlingPayment(tenantCtx, inv, transitioned, result.PaymentID)
			// Re-read the settled row before clearing dunning state — Update
			// writes the full struct and would clobber paid_at/amount_paid.
			settled, err := w.invoiceRepo.GetByID(tenantCtx, inv.ID)
			if err != nil || settled == nil {
				slog.Error("retry worker: failed to reload settled invoice", "invoice_id", inv.ID, "error", err)
				return
			}
			settled.NextRetryAt = nil
			settled.DunningActionID = ""
			settled.DunningContextKey = ""
			settled.LastPaymentError = ""
			if updateErr := w.invoiceRepo.Update(ctx, settled); updateErr != nil {
				slog.Error("retry worker: failed to clear dunning state", "invoice_id", inv.ID, "error", updateErr)
			}
		} else {
			// No settler wired (tests / minimal setups): the legacy direct
			// update, which skips the ledger and rev-rec.
			now := time.Now()
			inv.Status = domain.InvoiceStatusPaid
			inv.PaidAt = &now
			inv.AmountPaid = inv.Total
			// Snapshot before dunning fields are cleared — recovery attribution
			// needs retry_count and the action that was in effect at payment time.
			recoverySnapshot := *inv
			inv.NextRetryAt = nil
			inv.DunningActionID = ""
			inv.DunningContextKey = ""
			inv.LastPaymentError = ""
			if updateErr := w.invoiceRepo.Update(ctx, inv); updateErr != nil {
				slog.Error("retry worker: failed to mark invoice as paid", "invoice_id", inv.ID, "error", updateErr)
			} else if w.recoveryRecorder != nil {
				// Recovered revenue attribution (idempotent, non-fatal)
				w.recoveryRecorder.RecordIfRecovered(ctx, &recoverySnapshot)
			}
		}

		// Mark dunning campaign as recovered
		if w.dunningCampaignService != nil {
			if err := w.dunningCampaignService.MarkRecovered(ctx, inv.ID); err != nil {
				slog.Error("retry worker: failed to mark dunning campaign recovered", "invoice_id", inv.ID, "error", err)
			}
		}
		return
	}

	// 4. Payment failed — decide next retry
	inv.RetryCount++
	errorCode := result.ErrorCode
	if errorCode == "" {
		errorCode = "GENERIC_FAILURE"
	}
	inv.LastPaymentError = errorCode
	slog.Warn("retry worker: payment declined",
		"invoice_id", inv.ID, "payment_id", result.PaymentID, "error_code", result.ErrorCode,
		"permanent", result.Permanent, "message", result.ErrorMsg)

	// Hard decline: retrying the same method cannot succeed, so stop charging
	// it and let the email ladder (pay link, escalation) take over.
	if result.Permanent {
		slog.Warn("retry worker: hard decline, handing invoice to email dunning", "invoice_number", inv.InvoiceNumber, "error_code", errorCode)
		w.stopRetries(ctx, inv, domain.InvoiceStatusPastDue, dunningManagedByScheduler)
		return
	}

	decision := w.retryService.DecideRetry(ctx, inv, errorCode)
	if decision == nil {
		slog.Warn("retry worker: max retries reached, marking uncollectible", "invoice_number", inv.InvoiceNumber)
		w.stopRetries(ctx, inv, domain.InvoiceStatusUncollectible, "")
		return
	}

	// 5. Schedule next retry with RL decision tracking
	slog.Warn("retry worker: payment failed, scheduling next retry", "error_code", errorCode, "action_id", decision.Action.ID, "next_retry_at", decision.NextRetryAt)
	inv.NextRetryAt = &decision.NextRetryAt
	inv.Status = domain.InvoiceStatusPastDue
	inv.DunningActionID = decision.Action.ID
	inv.DunningContextKey = decision.ContextKey

	if updateErr := w.invoiceRepo.Update(ctx, inv); updateErr != nil {
		slog.Error("retry worker: failed to update invoice", "invoice_id", inv.ID, "error", updateErr)
	}
}

// noChargeDue is the PaymentID of a "success" where nothing was charged.
const noChargeDue = "no_charge_due"

// recordSettlingPayment stores the payment that settled inv as its refund
// target, so refunds don't depend on the payment webhook. A charge on an
// already-settled invoice may be a second payment, which needs review.
func (w *RetryWorker) recordSettlingPayment(ctx context.Context, inv *domain.Invoice, transitioned bool, paymentID string) {
	if paymentID == "" || paymentID == noChargeDue {
		return
	}
	if !transitioned {
		slog.Warn("retry worker: charge succeeded on an invoice that was already settled; review for refund if it was another payment",
			"invoice_id", inv.ID, "payment_id", paymentID)
		return
	}
	if err := w.invoiceRepo.SetGatewayPaymentID(ctx, inv.TenantID, inv.ID, paymentID); err != nil {
		slog.Error("retry worker: failed to record the settling payment id; refunds need it",
			"invoice_id", inv.ID, "payment_id", paymentID, "error", err)
	}
}

// dunningManagedByScheduler puts an invoice on the dunning scheduler's email
// ladder (ClaimOverdueForDunning) instead of the worker's retry claim.
const dunningManagedByScheduler = "scheduler"

// stopRetries ends worker-driven retries of inv: no next retry and no pending
// dunning action. A non-empty managedBy hands the invoice to that dunning
// owner; the scheduler then keeps emailing and escalates it to uncollectible
// on schedule.
func (w *RetryWorker) stopRetries(ctx context.Context, inv *domain.Invoice, status domain.InvoiceStatus, managedBy string) {
	inv.Status = status
	inv.NextRetryAt = nil
	inv.DunningActionID = ""
	inv.DunningContextKey = ""
	if managedBy != "" {
		inv.DunningManagedBy = managedBy
	}
	if err := w.invoiceRepo.Update(ctx, inv); err != nil {
		slog.Error("retry worker: failed to stop retries", "invoice_id", inv.ID, "status", status, "error", err)
	}
}

// getDurationSeconds maps action ID to its interval in seconds
func getDurationSeconds(actionID string) int64 {
	for _, a := range domain.DefaultDunningActions {
		if a.ID == actionID {
			return int64(a.Interval.Seconds())
		}
	}
	return 86400 // default 24h
}
