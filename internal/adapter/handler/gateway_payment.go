package handler

import (
	"context"

	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
	"github.com/recurso-dev/recurso/internal/service"
)

// paymentReader reads a gateway payment back with the secret key; checkout
// returns and webhooks settle only on this read, never on what the browser or
// the webhook body claims. Satisfied by *gateway.MoyasarGateway.
type paymentReader interface {
	FetchPayment(ctx context.Context, paymentID string) (*port.GatewayPayment, error)
}

// paymentSettler is the one settlement path shared by checkout verify and the
// webhook. Satisfied by *service.GatewaySettlementService.
type paymentSettler interface {
	Settle(ctx context.Context, inv *domain.Invoice, p *port.GatewayPayment) (bool, error)
	// SettleExistingPayment settles inv from a payment the gateway already
	// captured for it and reports whether inv is now paid.
	SettleExistingPayment(ctx context.Context, inv *domain.Invoice) (bool, error)
	// CardOnFile reports whether paying inv saves the card for its renewals
	// and, when it does, the terms the page must disclose (nil if incomplete).
	CardOnFile(ctx context.Context, inv *domain.Invoice) (bool, *service.CardOnFileTerms)
}
