package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
)

// Binding failures: the fetched payment is genuine but must not settle this
// invoice.
var (
	ErrPaymentWrongInvoice  = errors.New("payment does not belong to this invoice")
	ErrPaymentWrongCurrency = errors.New("payment currency does not match invoice")
	ErrPaymentShortAmount   = errors.New("payment amount is less than the amount due")
	// ErrInvoiceNotPayable: the invoice is void or draft, so nothing is owed
	// and a payment for it must not settle it.
	ErrInvoiceNotPayable = errors.New("invoice is not payable")
)

// InvoicePayable reports whether inv is owed and may take a payment: open,
// past due, or written off (uncollectible). Paid, void and draft invoices
// are not.
func InvoicePayable(inv *domain.Invoice) bool {
	switch inv.Status {
	case domain.InvoiceStatusOpen, domain.InvoiceStatusPastDue, domain.InvoiceStatusUncollectible:
		return true
	}
	return false
}

// BindPaymentToInvoice checks that a payment read back from the gateway was
// made for inv: its metadata names the invoice, the currency matches, and the
// amount (net of refunds) covers the balance due. inv must come from the
// invoice repository, which derives AmountDue on every load.
func BindPaymentToInvoice(p *port.GatewayPayment, inv *domain.Invoice) error {
	if p.InvoiceID != inv.ID.String() {
		return ErrPaymentWrongInvoice
	}
	if !strings.EqualFold(p.Currency, inv.Currency) {
		return ErrPaymentWrongCurrency
	}
	if p.Amount-p.RefundedAmount < inv.AmountDue {
		return ErrPaymentShortAmount
	}
	return nil
}

type paidInvoiceMarker interface {
	MarkInvoicePaid(ctx context.Context, invoiceID uuid.UUID) (bool, error)
}

// gatewayPaymentClaimer records the payment that settled an invoice unless a
// different one is already recorded, and says whether none was recorded
// before. Satisfied by *db.InvoiceRepository.
type gatewayPaymentClaimer interface {
	ClaimGatewayPaymentID(ctx context.Context, tenantID, invoiceID uuid.UUID, paymentID string) (claimed, wasEmpty bool, err error)
}

type settlementSubscriptionReader interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Subscription, error)
}

// settlementPlanReader reads a plan under the tenant in ctx. Satisfied by
// *db.PlanRepository.
type settlementPlanReader interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Plan, error)
}

// gatewayMethodSaver stores a reusable gateway card as the customer's
// off-session method. Satisfied by *db.CustomerRepository.
type gatewayMethodSaver interface {
	SetGatewayPaymentMethod(ctx context.Context, tenantID, customerID uuid.UUID, provider domain.GatewayProvider, ref, brand, last4 string, expMonth, expYear int) error
}

// cardTokenReader reads a saved card's expiry from the gateway, which the
// payment read-back does not carry.
type cardTokenReader interface {
	FetchToken(ctx context.Context, tokenID string) (expMonth, expYear int, err error)
}

// paidPaymentFinder finds a payment the gateway already captured for an
// invoice in currency covering amount (net of refunds), or nil.
type paidPaymentFinder interface {
	FindPaidPayment(ctx context.Context, invoiceID, currency string, amount int64) (*port.GatewayPayment, error)
}

type dunningRecoveryMarker interface {
	MarkRecovered(ctx context.Context, invoiceID uuid.UUID) error
}

type dunningOutcomeRecorder interface {
	RecordOutcome(ctx context.Context, history domain.DunningHistory) error
}

// GatewaySettlementService settles an invoice from a server-side read-back of
// a gateway payment. The checkout return and the gateway webhook both call it,
// so the order of effects and the duplicate-payment rules are the same no
// matter which arrives first.
type GatewaySettlementService struct {
	provider  domain.GatewayProvider
	settler   paidInvoiceMarker
	payments  gatewayPaymentClaimer
	subs      settlementSubscriptionReader
	cards     gatewayMethodSaver
	tokens    cardTokenReader
	recovery  *DunningSuccessRecorder
	paid      paidPaymentFinder
	routesTo  func(currency string) domain.GatewayProvider
	plans     settlementPlanReader
	merchants TenantLookup
	logger    *slog.Logger
}

// NewGatewaySettlementService builds the settlement path for one gateway.
// provider is recorded on any card the payments save.
func NewGatewaySettlementService(provider domain.GatewayProvider, settler paidInvoiceMarker, payments gatewayPaymentClaimer, subs settlementSubscriptionReader) *GatewaySettlementService {
	return &GatewaySettlementService{
		provider: provider,
		settler:  settler,
		payments: payments,
		subs:     subs,
		logger:   slog.Default().With("component", "gateway_settlement", "provider", string(provider)),
	}
}

// SetCardSaving wires saving the card a payment tokenized. tokens may be nil;
// the card is then saved without an expiry.
func (s *GatewaySettlementService) SetCardSaving(cards gatewayMethodSaver, tokens cardTokenReader) {
	s.cards = cards
	s.tokens = tokens
}

// SetDunningRecovery wires the once-per-settlement dunning outcome: the
// campaign is closed as recovered and the retry bandit is rewarded.
func (s *GatewaySettlementService) SetDunningRecovery(campaigns dunningRecoveryMarker, outcomes dunningOutcomeRecorder) {
	s.recovery = NewDunningSuccessRecorder(campaigns, outcomes)
}

// SetPaidPaymentLookup wires the check for a payment the gateway already
// captured. routesTo names the gateway a currency is collected on; only
// invoices routed to this service's gateway are looked up.
func (s *GatewaySettlementService) SetPaidPaymentLookup(paid paidPaymentFinder, routesTo func(currency string) domain.GatewayProvider) {
	s.paid = paid
	s.routesTo = routesTo
}

// SettleExistingPayment settles inv from a payment the gateway already
// captured for it, so a buyer whose earlier payment (a renewal whose answer
// was lost, or a missed webhook) is not yet recorded is never asked to pay
// again. It reports whether inv is now paid. A lookup error is returned: the
// caller must not take a new payment it could not check against.
func (s *GatewaySettlementService) SettleExistingPayment(ctx context.Context, inv *domain.Invoice) (bool, error) {
	if s == nil || s.paid == nil || s.routesTo == nil ||
		!InvoicePayable(inv) || inv.AmountDue <= 0 || s.routesTo(inv.Currency) != s.provider {
		return false, nil
	}
	p, err := s.paid.FindPaidPayment(ctx, inv.ID.String(), inv.Currency, inv.AmountDue)
	if err != nil || p == nil {
		return false, err
	}
	s.logger.InfoContext(ctx, "invoice has a captured payment not yet recorded; settling it", "invoice_id", inv.ID, "payment_id", p.ID)
	if _, err := s.Settle(ctx, inv, p); err != nil {
		return false, err
	}
	return true, nil
}

// SavesCard reports whether paying inv may store the card for off-session
// charges: only an invoice of a subscription that will renew again has
// consented future charges. One-off invoices never save.
func (s *GatewaySettlementService) SavesCard(ctx context.Context, inv *domain.Invoice) bool {
	return s.renewingSubscription(ctx, inv) != nil
}

// renewingSubscription returns inv's subscription when paying inv may save
// the card for its renewals, else nil.
func (s *GatewaySettlementService) renewingSubscription(ctx context.Context, inv *domain.Invoice) *domain.Subscription {
	if s == nil || s.cards == nil || s.subs == nil || inv.SubscriptionID == nil {
		return nil
	}
	sub, err := s.subs.GetByID(context.WithValue(ctx, domain.TenantIDKey, inv.TenantID), *inv.SubscriptionID)
	if err != nil || sub == nil || sub.Status == domain.SubscriptionStatusCanceled || sub.CancelAtPeriodEnd {
		return nil
	}
	return sub
}

// CardOnFileTerms is the stored-credential agreement a checkout shows before
// a payment that saves the card: who charges, how much, and how often, until
// the customer cancels. Amount is this invoice's total in minor units, what
// a renewal of the same plan charges; usage, discounts and plan changes can
// move later renewals.
type CardOnFileTerms struct {
	Merchant      string `json:"merchant"`
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	Interval      string `json:"interval"`
	IntervalCount int    `json:"interval_count"`
}

// SetCardOnFileTerms wires the plan and merchant reads the consent terms
// need. Unset, checkouts save cards with the page's generic consent text.
func (s *GatewaySettlementService) SetCardOnFileTerms(plans settlementPlanReader, merchants TenantLookup) {
	s.plans = plans
	s.merchants = merchants
}

// CardOnFile reports whether paying inv saves the card (as SavesCard) and,
// when it does, the terms to disclose. terms is nil when any part of them
// cannot be read: the page then shows its generic consent rather than a
// half-filled agreement.
func (s *GatewaySettlementService) CardOnFile(ctx context.Context, inv *domain.Invoice) (bool, *CardOnFileTerms) {
	sub := s.renewingSubscription(ctx, inv)
	if sub == nil {
		return false, nil
	}
	if s.plans == nil || s.merchants == nil {
		return true, nil
	}
	plan, err := s.plans.GetByID(context.WithValue(ctx, domain.TenantIDKey, inv.TenantID), sub.PlanID)
	if err != nil || plan == nil || plan.IntervalUnit == "" {
		return true, nil
	}
	tenant, err := s.merchants.GetByID(ctx, inv.TenantID)
	if err != nil || tenant == nil || tenant.Name == "" {
		return true, nil
	}
	return true, &CardOnFileTerms{
		Merchant:      tenant.Name,
		Amount:        inv.Total,
		Currency:      strings.ToUpper(inv.Currency),
		Interval:      string(plan.IntervalUnit),
		IntervalCount: max(plan.IntervalCount, 1),
	}
}

// Settle marks inv paid by p. The caller has already confirmed with the
// gateway that p is captured. It returns whether this call performed the paid
// transition; an error means the invoice was not settled. A payment for a
// void or draft invoice is refused with ErrInvoiceNotPayable and flagged for
// a refund; an already-paid invoice goes through, so a duplicate is caught.
func (s *GatewaySettlementService) Settle(ctx context.Context, inv *domain.Invoice, p *port.GatewayPayment) (bool, error) {
	if err := BindPaymentToInvoice(p, inv); err != nil {
		return false, err
	}
	if inv.Status != domain.InvoiceStatusPaid && !InvoicePayable(inv) {
		s.logger.WarnContext(ctx, "captured payment for an invoice that is not payable — not settled, review for refund",
			"invoice_id", inv.ID, "invoice_status", inv.Status, "payment_id", p.ID, "amount", p.Amount, "currency", p.Currency)
		return false, ErrInvoiceNotPayable
	}
	tenantCtx := context.WithValue(ctx, domain.TenantIDKey, inv.TenantID)
	transitioned, err := s.settler.MarkInvoicePaid(tenantCtx, inv.ID)
	if err != nil {
		return false, err
	}
	log := s.logger.With("invoice_id", inv.ID, "payment_id", p.ID)
	log.InfoContext(ctx, "invoice settled from gateway payment", "transitioned", transitioned)

	if excess := p.Amount - p.RefundedAmount - inv.AmountDue; transitioned && excess > 0 {
		log.WarnContext(ctx, "payment exceeds the amount due; the excess is not booked — review for refund",
			"excess", excess, "currency", inv.Currency)
	}

	if recordSettlingPayment(ctx, log, s.payments, inv, transitioned, p.ID) {
		s.saveCard(ctx, tenantCtx, inv, p)
	}

	if transitioned {
		s.recovery.Record(tenantCtx, inv)
	}
	return transitioned, nil
}

// RecordSettlingPayment records paymentID, a payment that just succeeded on
// inv, as the invoice's refund target; transitioned is whether settling inv
// for it marked it paid. Renewal, the retry worker and Settle share it, so
// every path applies the same duplicate-charge rule.
func RecordSettlingPayment(ctx context.Context, claimer gatewayPaymentClaimer, inv *domain.Invoice, transitioned bool, paymentID string) {
	recordSettlingPayment(ctx, slog.With("invoice_id", inv.ID, "payment_id", paymentID), claimer, inv, transitioned, paymentID)
}

// recordSettlingPayment claims the invoice's refund target for paymentID and
// reports whether paymentID is the payment that settled inv. The claim is
// conditional, so it never overwrites another payment. It logs a possible
// duplicate charge, for an operator to review for a refund, when the claim
// loses to another payment, or when the invoice was already settled some
// other way (offline, manually) with no payment recorded. Finding paymentID
// already recorded (the webhook settled it first) is not a duplicate. Without
// a claimer, or when the claim fails, only the paid transition decides.
func recordSettlingPayment(ctx context.Context, log *slog.Logger, claimer gatewayPaymentClaimer, inv *domain.Invoice, transitioned bool, paymentID string) bool {
	if paymentID == "" {
		return transitioned
	}
	if claimer == nil {
		if !transitioned {
			log.WarnContext(ctx, "invoice was already settled before this payment — possible duplicate charge, review for refund")
		}
		return transitioned
	}
	claimed, wasEmpty, err := claimer.ClaimGatewayPaymentID(ctx, inv.TenantID, inv.ID, paymentID)
	switch {
	case err != nil:
		log.ErrorContext(ctx, "failed to record the settling payment id; refunds need it", "error", err)
		return transitioned
	case !claimed:
		log.WarnContext(ctx, "invoice already settled by another payment — duplicate charge, review for refund")
		return false
	case !transitioned && wasEmpty:
		log.WarnContext(ctx, "invoice was settled another way before this payment — possible duplicate charge, review for refund")
		return false
	}
	return true
}

func (s *GatewaySettlementService) saveCard(ctx, tenantCtx context.Context, inv *domain.Invoice, p *port.GatewayPayment) {
	if !s.SavesCard(ctx, inv) {
		return
	}
	log := s.logger.With("invoice_id", inv.ID, "payment_id", p.ID)
	if p.CardToken == "" {
		// Renewals of this subscription cannot charge off-session; usually the
		// gateway account has card tokenization disabled.
		log.WarnContext(ctx, "paid payment carries no reusable card token; renewals will not auto-charge")
		return
	}
	var expMonth, expYear int
	if s.tokens != nil {
		m, y, err := s.tokens.FetchToken(ctx, p.CardToken)
		if err != nil {
			log.WarnContext(ctx, "could not read card expiry; saving the card without it", "error", err)
		} else {
			expMonth, expYear = m, y
		}
	}
	if err := s.cards.SetGatewayPaymentMethod(tenantCtx, inv.TenantID, inv.CustomerID, s.provider, p.CardToken, p.CardBrand, p.CardLast4, expMonth, expYear); err != nil {
		log.ErrorContext(ctx, "failed to save card for off-session charges", "error", err)
	}
}
