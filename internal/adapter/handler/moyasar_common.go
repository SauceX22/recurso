package handler

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
)

// moyasarPaymentFetcher reads a Moyasar payment back with the secret key.
// Satisfied by *gateway.MoyasarGateway. Both the checkout verify endpoint and
// the webhook settle only on this server-side read, never on what the browser
// or the webhook body claims.
type moyasarPaymentFetcher interface {
	FetchPayment(ctx context.Context, paymentID string) (*port.GatewayPayment, error)
}

// gatewayCardSaver records a reusable gateway card as the customer's default
// off-session method. Satisfied by *db.CustomerRepository.
type gatewayCardSaver interface {
	SetGatewayPaymentMethod(ctx context.Context, tenantID, customerID uuid.UUID, provider, paymentMethodRef, brand, last4 string) error
}

var (
	errMoyasarWrongInvoice  = errors.New("payment does not belong to this invoice")
	errMoyasarWrongCurrency = errors.New("payment currency does not match invoice")
	errMoyasarShortAmount   = errors.New("payment amount is less than the amount due")
)

// moyasarAmountDue is what checkout asks the payer for: the outstanding
// balance net of wallet drain and applied credit (mirrors renewal.go).
func moyasarAmountDue(inv *domain.Invoice) int64 {
	return inv.Total - inv.AmountPaid - inv.CreditApplied
}

// checkMoyasarPaymentForInvoice binds a fetched payment to an invoice: the
// payment's metadata must name this invoice, the currency must match, and the
// amount must cover the outstanding balance. Status is checked by the caller.
func checkMoyasarPaymentForInvoice(p *port.GatewayPayment, inv *domain.Invoice) error {
	if p.InvoiceID != inv.ID.String() {
		return errMoyasarWrongInvoice
	}
	if !strings.EqualFold(p.Currency, inv.Currency) {
		return errMoyasarWrongCurrency
	}
	if p.Amount < moyasarAmountDue(inv) {
		return errMoyasarShortAmount
	}
	return nil
}

// saveMoyasarCard stores the token a paid payment created as the invoice
// customer's default method, so renewals charge it off-session. Best-effort:
// a payment made without save_card carries no token and is a no-op.
func saveMoyasarCard(ctx context.Context, saver gatewayCardSaver, p *port.GatewayPayment, inv *domain.Invoice) error {
	if saver == nil || p.CardToken == "" {
		return nil
	}
	return saver.SetGatewayPaymentMethod(ctx, inv.TenantID, inv.CustomerID, domain.SavedMethodMoyasar, p.CardToken, p.CardBrand, p.CardLast4)
}
