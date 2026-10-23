package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/recurso-dev/recurso/internal/validate"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/adapter/db"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
)

// WalletService owns prepaid wallets (Lago-parity B1): creation, top-ups
// (paid or promotional, with optional expiry), the invoice-time drain, the
// expiry sweep, and auto-recharge. Every money movement posts balanced
// ledger legs; nil-safe on the ledger for test contexts.

// WalletValidationError marks invalid caller input (maps to HTTP 400).
type WalletValidationError string

func (e WalletValidationError) Error() string { return string(e) }

var (
	ErrWalletNotFound     = errors.New("wallet not found")
	ErrWalletExists       = errors.New("a wallet for this customer and currency already exists")
	ErrWalletCustomerGone = errors.New("customer not found")
)

// walletLedger is the slice of LedgerService wallets post through. Wallets are
// entity-scoped (Multi-Entity Books): top-up/drain legs post on the wallet's
// own entity ledger.
type walletLedger interface {
	RecordWalletTopUp(ctx context.Context, tenantID uuid.UUID, entityID *uuid.UUID, walletTxID uuid.UUID, amount int64, description string) (uuid.UUID, error)
	RecordWalletDrain(ctx context.Context, tenantID uuid.UUID, entityID *uuid.UUID, customerID, invoiceID uuid.UUID, amount int64, description string) (uuid.UUID, error)
	RecordAdjustmentCreditIssued(ctx context.Context, tenantID uuid.UUID, entityID *uuid.UUID, creditNoteID uuid.UUID, amount int64, description string) (uuid.UUID, error)
	RecordWalletRefund(ctx context.Context, tenantID uuid.UUID, entityID *uuid.UUID, walletTxID uuid.UUID, amount int64, description string) (uuid.UUID, error)
	RecordWalletForfeit(ctx context.Context, tenantID uuid.UUID, entityID *uuid.UUID, walletTxID uuid.UUID, amount int64, description string) (uuid.UUID, error)
	RecordWalletExpiry(ctx context.Context, tenantID uuid.UUID, entityID *uuid.UUID, walletTxID uuid.UUID, amount int64, description string) (uuid.UUID, error)
}

// walletEntityReader resolves the legal entity a new wallet belongs to
// (Multi-Entity Books). nil-safe: without it wallets are created with a nil
// entity (single-entity/test contexts), and the DB default handles primary.
type walletEntityReader interface {
	GetByID(ctx context.Context, id, tenantID uuid.UUID) (*domain.Entity, error)
	GetPrimary(ctx context.Context, tenantID uuid.UUID) (*domain.Entity, error)
}

type WalletService struct {
	wallets   port.WalletRepository
	customers port.CustomerRepository
	ledger    walletLedger
	entities  walletEntityReader // nil-safe: entity resolution for new wallets
	notifier  port.Notifier      // auto-recharge failure notices; nil-safe

	// Saved-method charging; nil-safe: without it auto-recharge does nothing.
	// chargerRouter routes the recharge to the gateway the method was saved on.
	lookup        savedMethodLookup
	chargerRouter savedChargerRouter

	now func() time.Time
}

func NewWalletService(wallets port.WalletRepository, customers port.CustomerRepository, ledger walletLedger) *WalletService {
	return &WalletService{
		wallets:   wallets,
		customers: customers,
		ledger:    ledger,
		now:       func() time.Time { return time.Now().UTC() },
	}
}

// SetEntityReader wires multi-entity resolution for new wallets (nil-safe).
func (s *WalletService) SetEntityReader(r walletEntityReader) { s.entities = r }

// SetNotifier wires auto-recharge failure notifications (nil-safe).
func (s *WalletService) SetNotifier(n port.Notifier) { s.notifier = n }

// SetSavedMethodCharging wires gateway charging for auto-recharge (nil-safe):
// the recharge charges on the gateway router picks for the saved method.
func (s *WalletService) SetSavedMethodCharging(lookup savedMethodLookup, router savedChargerRouter) {
	s.lookup = lookup
	s.chargerRouter = router
}

// CreateWalletInput creates a wallet, optionally with auto-recharge.
type CreateWalletInput struct {
	CustomerID string `json:"customer_id" binding:"required"`
	Currency   string `json:"currency" binding:"required"`
	// EntityID scopes the wallet to a legal entity (Multi-Entity Books). Empty
	// = the tenant's primary entity.
	EntityID              string `json:"entity_id" binding:"omitempty,uuid"`
	AutoRechargeThreshold *int64 `json:"auto_recharge_threshold"`
	AutoRechargeAmount    *int64 `json:"auto_recharge_amount"`
}

func (s *WalletService) CreateWallet(ctx context.Context, tenantID uuid.UUID, in CreateWalletInput) (*domain.Wallet, error) {
	customerID, err := uuid.Parse(in.CustomerID)
	if err != nil {
		return nil, WalletValidationError("invalid customer_id")
	}
	currency := strings.ToUpper(strings.TrimSpace(in.Currency))
	if !validate.Currency(currency) {
		return nil, WalletValidationError("currency must be a valid ISO 4217 code")
	}
	if err := validateAutoRecharge(in.AutoRechargeThreshold, in.AutoRechargeAmount); err != nil {
		return nil, err
	}
	customer, err := s.customers.GetByID(ctx, customerID)
	if err != nil || customer == nil || customer.TenantID != tenantID {
		return nil, ErrWalletCustomerGone
	}

	entityID, err := s.resolveWalletEntity(ctx, tenantID, in.EntityID)
	if err != nil {
		return nil, err
	}

	now := s.now()
	w := &domain.Wallet{
		ID:                    uuid.New(),
		TenantID:              tenantID,
		EntityID:              entityID,
		CustomerID:            customerID,
		Currency:              currency,
		AutoRechargeThreshold: in.AutoRechargeThreshold,
		AutoRechargeAmount:    in.AutoRechargeAmount,
		CreatedAt:             now,
		UpdatedAt:             now,
	}
	if err := s.wallets.Create(ctx, w); err != nil {
		if db.IsUniqueViolation(err) {
			return nil, ErrWalletExists
		}
		return nil, err
	}
	return w, nil
}

// resolveWalletEntity turns the optional entity_id input into the concrete
// entity id the wallet is scoped to. Empty input → the tenant's primary
// entity. Without an entity reader wired (single-entity/test contexts) it
// returns the nil UUID and leaves entity resolution to the DB default.
func (s *WalletService) resolveWalletEntity(ctx context.Context, tenantID uuid.UUID, entityIDStr string) (uuid.UUID, error) {
	if s.entities == nil {
		return uuid.Nil, nil
	}
	if entityIDStr != "" {
		entityID, err := uuid.Parse(entityIDStr)
		if err != nil {
			return uuid.Nil, WalletValidationError("invalid entity_id")
		}
		e, err := s.entities.GetByID(ctx, entityID, tenantID)
		if err != nil {
			return uuid.Nil, err
		}
		if e == nil {
			return uuid.Nil, WalletValidationError("entity not found")
		}
		return e.ID, nil
	}
	e, err := s.entities.GetPrimary(ctx, tenantID)
	if err != nil {
		return uuid.Nil, err
	}
	if e == nil {
		return uuid.Nil, nil
	}
	return e.ID, nil
}

func validateAutoRecharge(threshold, amount *int64) error {
	if (threshold == nil) != (amount == nil) {
		return WalletValidationError("auto_recharge_threshold and auto_recharge_amount must be set together")
	}
	if threshold != nil && (*threshold <= 0 || *amount <= 0) {
		return WalletValidationError("auto-recharge threshold and amount must be positive")
	}
	return nil
}

func (s *WalletService) GetWallet(ctx context.Context, tenantID, id uuid.UUID) (*domain.Wallet, error) {
	w, err := s.wallets.GetByID(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if w == nil {
		return nil, ErrWalletNotFound
	}
	return w, nil
}

// CloseWalletResult reports what a closure settled.
type CloseWalletResult struct {
	Refunded  int64 `json:"refunded"`  // paid balance to return to the customer (minor units)
	Forfeited int64 `json:"forfeited"` // promotional balance written off (minor units)
}

// CloseWallet closes a wallet and settles its remaining balance: paid residue is
// refunded to the customer, promotional residue is forfeited (non-refundable),
// and the ledger legs are posted (DR Customer Credit / CR Cash for the refund;
// DR Customer Credit / CR Credits for the forfeit). A closed wallet accepts no
// further top-ups or drains. The actual money return is handled out of band
// (like a manual refund) — the returned `refunded` amount is what's owed back.
func (s *WalletService) CloseWallet(ctx context.Context, tenantID, walletID uuid.UUID) (*CloseWalletResult, error) {
	w, err := s.GetWallet(ctx, tenantID, walletID)
	if err != nil {
		return nil, err
	}
	if w.ClosedAt != nil {
		return nil, WalletValidationError("wallet is already closed")
	}

	res, err := s.wallets.Close(ctx, tenantID, walletID, s.now())
	if err != nil {
		if errors.Is(err, port.ErrWalletAlreadyClosed) {
			return nil, WalletValidationError("wallet is already closed")
		}
		return nil, err
	}

	if s.ledger != nil {
		eid := entityPtr(w.EntityID)
		if res.Refunded > 0 {
			if _, lErr := s.ledger.RecordWalletRefund(ctx, tenantID, eid, res.RefundTxID, res.Refunded, "Wallet closed — paid balance refunded"); lErr != nil {
				slog.ErrorContext(ctx, "wallet refund ledger posting failed", "wallet_id", walletID, "error", lErr)
			}
		}
		if res.Forfeited > 0 {
			if _, lErr := s.ledger.RecordWalletForfeit(ctx, tenantID, eid, res.ForfeitTxID, res.Forfeited, "Wallet closed — promotional balance forfeited"); lErr != nil {
				slog.ErrorContext(ctx, "wallet forfeit ledger posting failed", "wallet_id", walletID, "error", lErr)
			}
		}
	}
	return &CloseWalletResult{Refunded: res.Refunded, Forfeited: res.Forfeited}, nil
}

// ListWallets returns the tenant's wallets, most recently active first.
func (s *WalletService) ListWallets(ctx context.Context, tenantID uuid.UUID, limit int) ([]domain.Wallet, error) {
	wallets, err := s.wallets.ListByTenant(ctx, tenantID, limit)
	if err != nil {
		return nil, err
	}
	if wallets == nil {
		wallets = []domain.Wallet{}
	}
	return wallets, nil
}

func (s *WalletService) ListCustomerWallets(ctx context.Context, tenantID, customerID uuid.UUID) ([]domain.Wallet, error) {
	wallets, err := s.wallets.ListByCustomer(ctx, tenantID, customerID)
	if err != nil {
		return nil, err
	}
	if wallets == nil {
		wallets = []domain.Wallet{}
	}
	return wallets, nil
}

// TopUpInput adds balance to a wallet. Source "manual" records money
// already received (bank transfer / offline); "promotional" grants credit
// with no money movement and may carry an expiry.
type TopUpInput struct {
	Amount    int64      `json:"amount" binding:"required,gt=0"`
	Source    string     `json:"source"`
	ExpiresAt *time.Time `json:"expires_at"`
}

func (s *WalletService) TopUp(ctx context.Context, tenantID, walletID uuid.UUID, in TopUpInput) (*domain.WalletTransaction, error) {
	if in.Amount <= 0 {
		return nil, WalletValidationError("amount must be greater than zero")
	}
	source := in.Source
	if source == "" {
		source = domain.WalletSourceManual
	}
	if source != domain.WalletSourceManual && source != domain.WalletSourcePromotional {
		return nil, WalletValidationError("source must be manual or promotional")
	}
	if in.ExpiresAt != nil && source != domain.WalletSourcePromotional {
		return nil, WalletValidationError("only promotional top-ups may expire")
	}
	if in.ExpiresAt != nil && !in.ExpiresAt.After(s.now()) {
		return nil, WalletValidationError("expires_at must be in the future")
	}

	w, err := s.GetWallet(ctx, tenantID, walletID)
	if err != nil {
		return nil, err
	}
	// A closed wallet's residue was already settled (refund/forfeit) at close;
	// value added after that could never drain (the invoice-drain lookup skips
	// closed wallets) and would sit as a stranded Customer-Credit liability.
	if w.ClosedAt != nil {
		return nil, WalletValidationError("wallet is closed")
	}

	wtx := &domain.WalletTransaction{
		ID:        uuid.New(),
		TenantID:  tenantID,
		WalletID:  w.ID,
		Type:      domain.WalletTxTopUp,
		Source:    source,
		Amount:    in.Amount,
		ExpiresAt: in.ExpiresAt,
		CreatedAt: s.now(),
	}
	if _, err := s.wallets.TopUp(ctx, wtx); err != nil {
		return nil, err
	}

	s.postTopUpLedger(ctx, tenantID, w.EntityID, wtx, source)
	return wtx, nil
}

// postTopUpLedger books the top-up: paid credit moves cash, promotional
// credit books an expense. Best-effort — reconciliation catches misses.
// entityID scopes the postings to the wallet's legal entity ledger.
func (s *WalletService) postTopUpLedger(ctx context.Context, tenantID, entityID uuid.UUID, wtx *domain.WalletTransaction, source string) {
	if s.ledger == nil {
		return
	}
	var err error
	if source == domain.WalletSourcePromotional {
		_, err = s.ledger.RecordAdjustmentCreditIssued(ctx, tenantID, entityPtr(entityID), wtx.ID, wtx.Amount, "Promotional wallet credit")
	} else {
		_, err = s.ledger.RecordWalletTopUp(ctx, tenantID, entityPtr(entityID), wtx.ID, wtx.Amount, fmt.Sprintf("Wallet top-up (%s)", source))
	}
	if err != nil {
		slog.ErrorContext(ctx, "wallet top-up ledger posting failed", "wallet_tx_id", wtx.ID, "error", err)
	}
}

func (s *WalletService) ListTransactions(ctx context.Context, tenantID, walletID uuid.UUID, limit int) ([]domain.WalletTransaction, error) {
	if _, err := s.GetWallet(ctx, tenantID, walletID); err != nil {
		return nil, err
	}
	txs, err := s.wallets.ListTransactions(ctx, tenantID, walletID, limit)
	if err != nil {
		return nil, err
	}
	if txs == nil {
		txs = []domain.WalletTransaction{}
	}
	return txs, nil
}

// UpdateAutoRecharge sets or clears the wallet's auto-recharge rule.
func (s *WalletService) UpdateAutoRecharge(ctx context.Context, tenantID, walletID uuid.UUID, threshold, amount *int64) error {
	if err := validateAutoRecharge(threshold, amount); err != nil {
		return err
	}
	w, err := s.GetWallet(ctx, tenantID, walletID)
	if err != nil {
		return err
	}
	// The recharge worker already skips closed wallets; rejecting the rule
	// here too keeps the contract visible at the API instead of silently
	// configuring a recharge that will never fire.
	if w.ClosedAt != nil {
		return WalletValidationError("wallet is closed")
	}
	err = s.wallets.UpdateAutoRecharge(ctx, tenantID, walletID, threshold, amount)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrWalletNotFound
	}
	return err
}

// DrainForInvoice consumes wallet balance against a committed invoice
// (D3: wallet applies BEFORE adjustment credit notes and the gateway).
// Returns the amount drained; 0 when the customer holds no wallet in the
// invoice currency or it is empty. The ledger leg posts per drain.
func (s *WalletService) DrainForInvoice(ctx context.Context, inv *domain.Invoice) (int64, error) {
	if inv == nil || inv.Total <= 0 {
		return 0, nil
	}
	// A wallet is spendable only on its own entity's invoices — resolve the
	// invoice's concrete entity and look the wallet up scoped to it.
	entityID, err := s.resolveInvoiceEntity(ctx, inv.TenantID, inv.EntityID)
	if err != nil {
		return 0, err
	}
	w, err := s.wallets.GetByCustomerEntityAndCurrency(ctx, inv.TenantID, inv.CustomerID, entityID, inv.Currency)
	if err != nil || w == nil || w.Balance <= 0 {
		return 0, err
	}
	owed := inv.Total - inv.AmountPaid - inv.CreditApplied
	if owed <= 0 {
		return 0, nil
	}
	drained, err := s.wallets.Drain(ctx, inv.TenantID, w.ID, owed, inv.ID, s.now())
	if err != nil || drained == 0 {
		return 0, err
	}
	if s.ledger != nil {
		if _, lErr := s.ledger.RecordWalletDrain(ctx, inv.TenantID, entityPtr(w.EntityID), inv.CustomerID, inv.ID, drained, "Wallet applied to invoice"); lErr != nil {
			slog.ErrorContext(ctx, "wallet drain ledger posting failed", "invoice_id", inv.ID, "error", lErr)
		}
	}
	return drained, nil
}

// resolveInvoiceEntity turns an invoice's optional entity pointer into the
// concrete entity id the wallet lookup keys on. A non-nil pointer is trusted
// (it was validated at invoice creation); nil falls back to the primary
// entity, or the nil UUID when no entity reader is wired.
func (s *WalletService) resolveInvoiceEntity(ctx context.Context, tenantID uuid.UUID, entityID *uuid.UUID) (uuid.UUID, error) {
	if entityID != nil {
		return *entityID, nil
	}
	if s.entities == nil {
		return uuid.Nil, nil
	}
	e, err := s.entities.GetPrimary(ctx, tenantID)
	if err != nil {
		return uuid.Nil, err
	}
	if e == nil {
		return uuid.Nil, nil
	}
	return e.ID, nil
}

// entityPtr returns a pointer to id, or nil when id is the zero UUID — so
// ledger postings resolve to the primary entity in single-entity contexts.
func entityPtr(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// ExpireOverdueCredits writes off expired promotional residue (called from the
// billing-cycle sweep) and posts the discharging ledger leg for each write-off
// (DR Customer Credit / CR Credits) — without it, expired promo shrinks the
// wallet but leaves Customer Credit overstated in the GL. Returns the number of
// wallets touched.
func (s *WalletService) ExpireOverdueCredits(ctx context.Context) (int, error) {
	expiries, err := s.wallets.ExpireOverdue(ctx, s.now())
	if err != nil {
		return len(expiries), err
	}
	if s.ledger != nil {
		for _, e := range expiries {
			if _, lErr := s.ledger.RecordWalletExpiry(ctx, e.TenantID, entityPtr(e.EntityID), e.ExpiryTxID, e.Amount, "Wallet promotional credit expired"); lErr != nil {
				slog.ErrorContext(ctx, "wallet expiry ledger posting failed", "wallet_id", e.WalletID, "error", lErr)
			}
		}
	}
	return len(expiries), nil
}

// autoRechargeBatchLimit bounds one sweep.
const autoRechargeBatchLimit = 100

// ProcessAutoRecharges tops up every wallet sitting below its threshold by
// charging the customer's saved payment method. No saved method or a
// decline notifies (tenant + customer per the spec default) and moves on;
// the wallet stays low and is retried after a backoff.
func (s *WalletService) ProcessAutoRecharges(ctx context.Context) (int, error) {
	due, err := s.wallets.ListDueForRecharge(ctx, autoRechargeBatchLimit)
	if err != nil {
		return 0, err
	}
	recharged := 0
	for i := range due {
		w := due[i]
		if s.rechargeWallet(ctx, &w) {
			recharged++
		}
	}
	return recharged, nil
}

func (s *WalletService) rechargeWallet(ctx context.Context, w *domain.Wallet) bool {
	if s.lookup == nil || w.AutoRechargeAmount == nil || s.chargerRouter == nil {
		return false
	}
	// The sweep runs on a background ctx; gateway routing is per tenant.
	ctx = context.WithValue(ctx, domain.TenantIDKey, w.TenantID)
	m, err := s.lookup.GetSavedPaymentMethod(ctx, w.CustomerID)
	if err != nil {
		slog.WarnContext(ctx, "wallet auto-recharge: saved payment method lookup failed", "wallet_id", w.ID, "error", err)
		s.recordRechargeFailure(ctx, w, "")
		return false
	}
	if !m.Chargeable() {
		s.notifyRechargeFailure(ctx, w, "no saved payment method")
		s.recordRechargeFailure(ctx, w, "")
		return false
	}
	charger, err := s.chargerRouter.ChargerFor(ctx, m, w.Currency)
	if errors.Is(err, ErrSavedMethodNotChargeable) {
		// The method can never be charged as things stand (e.g. its gateway
		// no longer collects this currency for the tenant): stop on it, as
		// for a hard decline, until the customer saves another, and tell
		// them once instead of on every sweep.
		slog.WarnContext(ctx, "wallet auto-recharge: saved payment method can't be charged; auto-recharge stops on it", "wallet_id", w.ID, "provider", m.Provider, "error", err)
		s.recordRechargeFailure(ctx, w, m.PaymentMethodRef)
		s.notifyRechargeFailure(ctx, w, "payment gateway unavailable")
		return false
	}
	if err != nil {
		// Transient (connection store or vault): ours, not the customer's.
		slog.WarnContext(ctx, "wallet auto-recharge: saved payment method's gateway could not be loaded", "wallet_id", w.ID, "provider", m.Provider, "error", err)
		s.recordRechargeFailure(ctx, w, "")
		return false
	}

	// The key advances only once an attempt's outcome is known (paid or
	// declined), so a sweep after a lost response replays the same charge,
	// whatever else changed on the wallet meanwhile, and each legitimate
	// recharge gets a fresh key. Gateway keys can live forever (Moyasar's
	// given_id does), so a key that repeated across recharges would replay
	// an old payment instead of charging. The amount is frozen with the
	// attempt: a replay at another amount is refused by the gateway, and the
	// payment the lost response captured would never be credited.
	amount, current, err := s.wallets.ReserveRechargeAmount(ctx, w.TenantID, w.ID, w.RechargeAttempt, *w.AutoRechargeAmount)
	if err != nil {
		slog.WarnContext(ctx, "wallet auto-recharge: could not reserve the attempt", "wallet_id", w.ID, "error", err)
		s.recordRechargeFailure(ctx, w, "")
		return false
	}
	if !current {
		// Another sweep already recorded this attempt's outcome.
		return false
	}
	idemKey := fmt.Sprintf("wallet-recharge-%s-%d", w.ID, w.RechargeAttempt)
	result, err := charger.ChargeSavedPaymentMethod(ctx, port.SavedMethodCharge{
		CustomerRef:    m.CustomerRef,
		MethodRef:      m.PaymentMethodRef,
		Amount:         amount,
		Currency:       w.Currency,
		WalletID:       w.ID.String(),
		IdempotencyKey: idemKey,
	})
	if err != nil || result == nil {
		// No answer: the card may have been charged, so tell no one it failed.
		logChargeFailure(ctx, "wallet auto-recharge got no answer from the gateway; the same charge is replayed later", result, err, "wallet_id", w.ID, "provider", m.Provider)
		s.recordRechargeFailure(ctx, w, "")
		return false
	}
	if !result.Success {
		logChargeFailure(ctx, "wallet auto-recharge charge declined", result, nil, "wallet_id", w.ID, "provider", m.Provider)
		// The cap counts declines on this method only: a newly saved method
		// starts afresh, and no-answer or lookup failures never count.
		declines := 1
		if w.RechargeDeclinesRef == m.PaymentMethodRef {
			declines = w.RechargeDeclines + 1
		}
		blocked := ""
		if result.Permanent || declines >= maxRechargeFailures {
			blocked = m.PaymentMethodRef
		}
		failures := w.RechargeFailures + 1
		retryAt := s.now().Add(rechargeBackoff(failures))
		s.recordRechargeOutcome(ctx, w, port.RechargeOutcome{
			Answered: true, Failures: failures, RetryAt: &retryAt, BlockedMethodRef: blocked,
			Declines: declines, DeclinesRef: m.PaymentMethodRef,
		})
		// Tell the customer once when declines start, and once when
		// auto-recharge stops; not on every backed-off retry in between.
		if declines == 1 || blocked != "" {
			s.notifyRechargeFailure(ctx, w, "payment failed")
		}
		return false
	}

	wtx := &domain.WalletTransaction{
		ID:       uuid.New(),
		TenantID: w.TenantID,
		WalletID: w.ID,
		Type:     domain.WalletTxTopUp,
		Source:   domain.WalletSourceAutoRecharge,
		Amount:   amount,
		// Same key the gateway charge used: a concurrent Redis-less sweep that
		// read the same attempt produces the same key, so the second credit
		// conflicts and no-ops — the card was charged once, the wallet is
		// credited once.
		IdempotencyKey: idemKey,
		CreatedAt:      s.now(),
	}
	applied, err := s.wallets.TopUp(ctx, wtx)
	if err != nil {
		// The key stays, so the next sweep replays the paid charge and
		// credits it then.
		slog.ErrorContext(ctx, "wallet auto-recharge charged but the top-up record failed; the next sweep retries the credit",
			"wallet_id", w.ID, "amount", amount, "payment_id", result.PaymentID, "error", err)
		return false
	}
	s.recordRechargeOutcome(ctx, w, port.RechargeOutcome{Answered: true})
	if !applied {
		// Another sweep already credited this recharge — the gateway
		// idempotency key also deduped the charge, so this is a clean no-op.
		slog.InfoContext(ctx, "wallet auto-recharge already applied by a concurrent sweep — skipping", "wallet_id", w.ID)
		return false
	}
	s.postTopUpLedger(ctx, w.TenantID, w.EntityID, wtx, domain.WalletSourceManual) // cash received
	slog.InfoContext(ctx, "wallet auto-recharged", "wallet_id", w.ID, "amount", amount)
	return true
}

// maxRechargeFailures caps answered declines in a row on one saved method:
// the decline that reaches it stops auto-recharge on that method until the
// customer saves another (card networks limit reattempts on a declined card).
const maxRechargeFailures = 8

// rechargeBackoff is how long a wallet waits after its nth failed recharge in
// a row: retrying a declined card every sweep breaks card-network retry rules
// and floods the customer with failure emails.
func rechargeBackoff(failures int) time.Duration {
	switch failures {
	case 1:
		return time.Hour
	case 2:
		return 6 * time.Hour
	default:
		return 24 * time.Hour
	}
}

// recordRechargeFailure backs the wallet off after an attempt the gateway
// did not answer (no answer, nothing to charge, a lookup or routing failure):
// the attempt keeps its key and the decline count stays as it was.
// blockedRef stops auto-recharge on a method that can't be charged until the
// customer saves another.
func (s *WalletService) recordRechargeFailure(ctx context.Context, w *domain.Wallet, blockedRef string) {
	failures := w.RechargeFailures + 1
	retryAt := s.now().Add(rechargeBackoff(failures))
	s.recordRechargeOutcome(ctx, w, port.RechargeOutcome{
		Failures: failures, RetryAt: &retryAt, BlockedMethodRef: blockedRef,
		Declines: w.RechargeDeclines, DeclinesRef: w.RechargeDeclinesRef,
	})
}

func (s *WalletService) recordRechargeOutcome(ctx context.Context, w *domain.Wallet, o port.RechargeOutcome) {
	if err := s.wallets.RecordRechargeOutcome(ctx, w.TenantID, w.ID, w.RechargeAttempt, o); err != nil {
		slog.ErrorContext(ctx, "wallet auto-recharge: failed to record the attempt", "wallet_id", w.ID, "error", err)
	}
}

func (s *WalletService) notifyRechargeFailure(ctx context.Context, w *domain.Wallet, reason string) {
	if s.notifier == nil {
		return
	}
	// The recharge sweep runs on a background ctx; the tenant-scoped customer lookup fails closed without the wallet's tenant.
	ctx = context.WithValue(ctx, domain.TenantIDKey, w.TenantID)
	customer, err := s.customers.GetByID(ctx, w.CustomerID)
	if err != nil || customer == nil {
		return
	}
	subject := "Wallet auto-recharge failed"
	body := fmt.Sprintf("Auto-recharge for your %s wallet could not complete: %s. Please update your payment method.", w.Currency, reason)
	if err := s.notifier.SendEmail(ctx, customer.Email, subject, body); err != nil {
		slog.WarnContext(ctx, "wallet recharge-failure notification failed", "wallet_id", w.ID, "error", err)
	}
}
