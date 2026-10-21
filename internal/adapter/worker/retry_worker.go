package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
	"github.com/recurso-dev/recurso/internal/service"
)

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

// SetSettler routes successful retries through the ledger-posting
// MarkInvoicePaid, so recovered payments get amount_paid, recovery
// attribution, a ledger entry, and a rev-rec schedule like every other
// payment. Without it a successful retry is not settled.
func (w *RetryWorker) SetSettler(s invoiceSettler) {
	w.settler = s
}

// SetSavedMethodCharging wires off-session collection with the customer's saved
// method, charged on the gateway router picks for it. When either dependency is
// nil, retries use the interactive gateway path.
func (w *RetryWorker) SetSavedMethodCharging(lookup customerPaymentLookup, router savedCardChargerRouter) {
	w.customerLookup = lookup
	w.chargerRouter = router
}

// errCodeDeclinedMethodUnchanged is the worker's own result for a saved method
// that already hard-declined this invoice: it is not charged again.
const errCodeDeclinedMethodUnchanged = "declined_method_unchanged"

// chargeInvoice attempts collection, preferring the customer's saved payment
// method (charged off-session) when one exists, and falling back to the
// interactive gateway retry otherwise. methodRef is the saved method charged,
// "" for the interactive retry.
func (w *RetryWorker) chargeInvoice(ctx context.Context, inv *domain.Invoice) (result *port.PaymentResult, methodRef string, err error) {
	// Collect only the OUTSTANDING balance — Total net of any wallet drain and
	// account credit already applied (mirrors renewal.go's amountDue). Charging
	// the full inv.Total here over-charged the customer's card by the wallet/
	// credit portion already settled at invoice generation.
	amountDue := inv.Total - inv.AmountPaid - inv.CreditApplied
	if amountDue <= 0 {
		// Fully covered by wallet/credit — nothing left to collect, so treat it as
		// paid rather than charging the card for an amount that isn't owed.
		return &port.PaymentResult{Success: true, PaymentID: noChargeDue}, "", nil
	}

	charger, m, err := w.savedMethodCharger(ctx, inv)
	if err != nil {
		return nil, "", err
	}
	if charger != nil {
		// The scheduler hands the invoice back after each email; a card that
		// hard-declined (lost, stolen, closed) must not be charged again until
		// the customer saves another one.
		if inv.DeclinedMethodRef != "" && inv.DeclinedMethodRef == m.PaymentMethodRef {
			return &port.PaymentResult{Success: false, ErrorCode: errCodeDeclinedMethodUnchanged}, m.PaymentMethodRef, nil
		}
		// Idempotent per attempt: a worker re-run for the same attempt won't
		// double-charge.
		result, err := charger.ChargeSavedPaymentMethod(ctx, port.SavedMethodCharge{
			CustomerRef:    m.CustomerRef,
			MethodRef:      m.PaymentMethodRef,
			Amount:         amountDue,
			Currency:       inv.Currency,
			InvoiceID:      inv.ID.String(),
			IdempotencyKey: fmt.Sprintf("retry-%s-%d", inv.ID, inv.RetryCount),
		})
		return result, m.PaymentMethodRef, err
	}
	result, err = w.gateway.RetryPayment(ctx, inv.ID.String(), amountDue, inv.Currency)
	return result, "", err
}

// savedMethodCharger resolves the customer's saved method and the charger for
// it. A nil charger with a nil error means there is nothing to charge
// off-session: no method, or one that can't be charged for this invoice
// (another gateway, a connection that is gone) — never the wrong account. An
// error means the method couldn't be resolved right now (lookup or connection
// store failure); the caller retries rather than concluding there is no card.
func (w *RetryWorker) savedMethodCharger(ctx context.Context, inv *domain.Invoice) (service.SavedCardCharger, domain.SavedPaymentMethod, error) {
	if w.customerLookup == nil || w.chargerRouter == nil {
		return nil, domain.SavedPaymentMethod{}, nil
	}
	m, err := w.customerLookup.GetSavedPaymentMethod(ctx, inv.CustomerID)
	if err != nil {
		return nil, domain.SavedPaymentMethod{}, fmt.Errorf("look up saved payment method: %w", err)
	}
	if !m.Chargeable() {
		return nil, domain.SavedPaymentMethod{}, nil
	}
	charger, err := w.chargerRouter.ChargerFor(ctx, m, inv.Currency)
	if errors.Is(err, service.ErrSavedMethodNotChargeable) {
		slog.Warn("retry: saved payment method can't be charged; using interactive retry",
			"invoice_id", inv.ID, "provider", m.Provider, "error", err)
		return nil, domain.SavedPaymentMethod{}, nil
	}
	if err != nil {
		return nil, domain.SavedPaymentMethod{}, fmt.Errorf("resolve saved payment method gateway: %w", err)
	}
	return charger, m, nil
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

// Infrastructure errors (gateway outage, revoked key, rate limit, lookup
// failure) back off exponentially from 5 minutes, and after maxInfraFailures
// in a row the invoice goes to email dunning: a key that stays broken must
// not call the gateway every few minutes forever.
const (
	infraRetryBase   = 5 * time.Minute
	infraRetryMax    = 6 * time.Hour
	maxInfraFailures = 6
)

func infraRetryDelay(failures int) time.Duration {
	d := infraRetryBase
	for i := 1; i < failures && d < infraRetryMax; i++ {
		d *= 2
	}
	return min(d, infraRetryMax)
}

func (w *RetryWorker) processInvoice(ctx context.Context, inv *domain.Invoice) {
	slog.Info("retry worker: retrying invoice", "invoice_number", inv.InvoiceNumber, "attempt", inv.RetryCount+1)

	// 1. Attempt payment — via the customer's saved method (off-session) when
	// available, else the interactive gateway path.
	result, methodRef, err := w.chargeInvoice(ctx, inv)
	if err != nil {
		w.retryAfterError(ctx, inv, err)
		return
	}

	// The saved method already hard-declined this invoice: leave it to the
	// email ladder, which keeps its count, until the customer changes cards.
	if !result.Success && result.ErrorCode == errCodeDeclinedMethodUnchanged {
		slog.Info("retry worker: saved method hard-declined before and is unchanged; not charging it again",
			"invoice_number", inv.InvoiceNumber)
		w.handToEmailDunning(ctx, inv, inv.LastPaymentError, inv.DeclinedMethodRef)
		return
	}

	// Nothing chargeable off-session (any gateway reporting
	// no_saved_payment_method, Stripe included): a retry can't collect, so
	// stop worker retries; the scheduler's email ladder and pay link take
	// over. The bandit learns nothing about retry timing from this, so no
	// outcome.
	if !result.Success && result.ErrorCode == port.ErrCodeNoSavedMethod {
		slog.Info("retry worker: no saved payment method, handing invoice to email dunning", "invoice_number", inv.InvoiceNumber)
		w.handToEmailDunning(ctx, inv, result.ErrorCode, "")
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
		w.settle(ctx, inv, result.PaymentID)
		return
	}

	// 4. Payment failed
	errorCode := result.ErrorCode
	if errorCode == "" {
		errorCode = "GENERIC_FAILURE"
	}
	slog.Warn("retry worker: payment declined",
		"invoice_id", inv.ID, "payment_id", result.PaymentID, "error_code", result.ErrorCode,
		"permanent", result.Permanent, "message", result.ErrorMsg)

	// Hard decline: retrying the same method cannot succeed, so stop charging
	// it and let the email ladder (pay link, escalation) take over. The
	// scheduler counts its own emails; counting this attempt too would skip
	// one of them.
	if result.Permanent {
		slog.Warn("retry worker: hard decline, handing invoice to email dunning", "invoice_number", inv.InvoiceNumber, "error_code", errorCode)
		w.handToEmailDunning(ctx, inv, errorCode, methodRef)
		return
	}

	inv.RetryCount++
	inv.LastPaymentError = errorCode
	decision := w.retryService.DecideRetry(ctx, inv, errorCode)
	if decision == nil {
		slog.Warn("retry worker: max retries reached, marking uncollectible", "invoice_number", inv.InvoiceNumber)
		w.writeRetryState(ctx, inv, port.InvoiceRetryState{
			Status:           domain.InvoiceStatusUncollectible,
			RetryCount:       inv.RetryCount,
			LastPaymentError: errorCode,
		})
		return
	}

	// 5. Schedule next retry with RL decision tracking
	slog.Warn("retry worker: payment failed, scheduling next retry", "error_code", errorCode, "action_id", decision.Action.ID, "next_retry_at", decision.NextRetryAt)
	next := decision.NextRetryAt
	w.writeRetryState(ctx, inv, port.InvoiceRetryState{
		Status:            domain.InvoiceStatusPastDue,
		NextRetryAt:       &next,
		RetryCount:        inv.RetryCount,
		DunningActionID:   decision.Action.ID,
		DunningContextKey: decision.ContextKey,
		LastPaymentError:  errorCode,
	})
}

// retryAfterError schedules a backed-off retry after an attempt that got no
// answer. It is not an attempt: no outcome, no retry count, the dunning action
// stays.
func (w *RetryWorker) retryAfterError(ctx context.Context, inv *domain.Invoice, err error) {
	failures := inv.RetryInfraFailures + 1
	if failures > maxInfraFailures {
		slog.Error("retry worker: gateway keeps failing; handing invoice to email dunning — check the gateway credentials and status",
			"invoice_number", inv.InvoiceNumber, "invoice_id", inv.ID, "failures", failures, "error", err)
		w.handToEmailDunning(ctx, inv, "gateway_error", inv.DeclinedMethodRef)
		return
	}
	delay := infraRetryDelay(failures)
	slog.Warn("retry worker: gateway infra error, scheduling a backed-off retry",
		"invoice_number", inv.InvoiceNumber, "failures", failures, "retry_in", delay, "error", err)
	next := time.Now().Add(delay)
	w.writeRetryState(ctx, inv, port.InvoiceRetryState{
		Status:             inv.Status,
		NextRetryAt:        &next,
		RetryCount:         inv.RetryCount,
		DunningActionID:    inv.DunningActionID,
		DunningContextKey:  inv.DunningContextKey,
		LastPaymentError:   inv.LastPaymentError,
		DeclinedMethodRef:  inv.DeclinedMethodRef,
		RetryInfraFailures: failures,
	})
}

// handToEmailDunning ends worker-driven retries of inv: past_due, no next
// retry, no pending dunning action, and the dunning scheduler as owner, so its
// claim emails the pay link right away and escalates to uncollectible on
// schedule. declinedRef records a hard-declined saved method.
func (w *RetryWorker) handToEmailDunning(ctx context.Context, inv *domain.Invoice, lastError, declinedRef string) {
	w.writeRetryState(ctx, inv, port.InvoiceRetryState{
		Status:            domain.InvoiceStatusPastDue,
		RetryCount:        inv.RetryCount,
		LastPaymentError:  lastError,
		DunningManagedBy:  dunningManagedByScheduler,
		DeclinedMethodRef: declinedRef,
	})
}

// writeRetryState writes the attempt's bookkeeping unless the invoice was
// settled or closed while the attempt ran.
func (w *RetryWorker) writeRetryState(ctx context.Context, inv *domain.Invoice, s port.InvoiceRetryState) {
	written, err := w.invoiceRepo.UpdateRetryState(ctx, inv.TenantID, inv.ID, s)
	switch {
	case err != nil:
		slog.Error("retry worker: failed to update invoice", "invoice_id", inv.ID, "status", s.Status, "error", err)
	case !written:
		slog.Info("retry worker: invoice was settled or closed during the attempt; leaving it as is", "invoice_id", inv.ID)
	}
}

// settle marks inv paid after a successful charge through the ledger path —
// the same idempotent method the checkout and payment webhooks use, so
// amount_paid, recovery attribution, the ledger entry, and the rev-rec
// schedule all happen here rather than in a racing webhook that would no-op on
// an already-paid invoice.
func (w *RetryWorker) settle(ctx context.Context, inv *domain.Invoice, paymentID string) {
	slog.Info("retry worker: payment succeeded", "invoice_number", inv.InvoiceNumber, "payment_id", paymentID)
	if w.settler == nil {
		slog.Error("retry worker: no settler wired; the charged invoice is not settled", "invoice_id", inv.ID, "payment_id", paymentID)
		return
	}
	// The worker's ctx carries no tenant, so inject the invoice's own (same
	// as the webhook handlers).
	tenantCtx := context.WithValue(ctx, domain.TenantIDKey, inv.TenantID)
	transitioned, err := w.settler.MarkInvoicePaid(tenantCtx, inv.ID)
	if err != nil {
		// Leave dunning state untouched: the next poll retries under the same
		// idempotency key and the settle is idempotent, so nothing is lost or
		// double-posted.
		slog.Error("retry worker: failed to settle invoice", "invoice_id", inv.ID, "error", err)
		return
	}
	if paymentID != noChargeDue {
		service.RecordSettlingPayment(tenantCtx, w.invoiceRepo, inv, transitioned, paymentID)
	}
	if err := w.invoiceRepo.ClearRetryState(tenantCtx, inv.TenantID, inv.ID); err != nil {
		slog.Error("retry worker: failed to clear dunning state", "invoice_id", inv.ID, "error", err)
	}

	// Mark dunning campaign as recovered
	if w.dunningCampaignService != nil {
		if err := w.dunningCampaignService.MarkRecovered(ctx, inv.ID); err != nil {
			slog.Error("retry worker: failed to mark dunning campaign recovered", "invoice_id", inv.ID, "error", err)
		}
	}
}

// noChargeDue is the PaymentID of a "success" where nothing was charged.
const noChargeDue = "no_charge_due"

// dunningManagedByScheduler puts an invoice on the dunning scheduler's email
// ladder (ClaimOverdueForDunning) instead of the worker's retry claim.
const dunningManagedByScheduler = "scheduler"

// getDurationSeconds maps action ID to its interval in seconds
func getDurationSeconds(actionID string) int64 {
	for _, a := range domain.DefaultDunningActions {
		if a.ID == actionID {
			return int64(a.Interval.Seconds())
		}
	}
	return 86400 // default 24h
}
