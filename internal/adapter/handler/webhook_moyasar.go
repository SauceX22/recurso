package handler

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
	"github.com/recurso-dev/recurso/internal/service"
)

// maxMoyasarWebhookBytes caps an inbound Moyasar delivery. The envelope wraps
// one payment object, a few KB; anything larger is not a Moyasar webhook.
const maxMoyasarWebhookBytes = 1 << 20

// moyasarWebhookEnvelope is the subset of the Moyasar webhook envelope the
// handler reads. Only data.id is taken from the payment object: everything
// that decides settlement comes from re-fetching that payment server-side.
type moyasarWebhookEnvelope struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	SecretToken string `json:"secret_token"`
	Data        struct {
		ID string `json:"id"`
	} `json:"data"`
}

// SetMoyasar wires the Moyasar webhook: the payment read-back, the shared
// settlement path, and the secret Moyasar echoes as secret_token. Left unset
// (or with an empty secret) the route fails closed with 503.
func (h *WebhookHandler) SetMoyasar(fetcher paymentReader, settler paymentSettler, secret string) {
	h.moyasarFetcher = fetcher
	h.moyasarSettler = settler
	h.moyasarWebhookSecret = secret
}

// HandleMoyasar processes Moyasar webhook deliveries (POST /webhooks/moyasar).
//
// Moyasar sends no signature header; the shared secret it echoes in the body
// is compared in constant time. Settlement still trusts only the server-side
// read-back, bound to the invoice by metadata, currency and amount, so a
// leaked secret can replay real payments (idempotent) but never forge one.
func (h *WebhookHandler) HandleMoyasar(c *gin.Context) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxMoyasarWebhookBytes+1))
	if err != nil {
		respondError(c, http.StatusBadRequest, codeValidationFailed, "failed to read request body")
		return
	}
	if len(body) > maxMoyasarWebhookBytes {
		respondError(c, http.StatusRequestEntityTooLarge, codeValidationFailed, "webhook payload too large")
		return
	}

	// Fail closed: without the secret and the read-back nothing can be
	// authenticated or settled, and acknowledging would drop the event.
	if h.moyasarWebhookSecret == "" || h.moyasarFetcher == nil || h.moyasarSettler == nil {
		h.logger.Error("Moyasar webhook not configured — rejecting (fail closed)", "ip", c.ClientIP())
		respondError(c, http.StatusServiceUnavailable, codeInternalError, "webhook verification not configured")
		return
	}

	var ev moyasarWebhookEnvelope
	if err := json.Unmarshal(body, &ev); err != nil {
		respondError(c, http.StatusBadRequest, codeValidationFailed, "invalid webhook payload")
		return
	}

	if ev.SecretToken == "" ||
		subtle.ConstantTimeCompare([]byte(ev.SecretToken), []byte(h.moyasarWebhookSecret)) != 1 {
		// Never log the presented token or the payload.
		h.logger.Warn("invalid Moyasar webhook secret", "ip", c.ClientIP(), "event_id", ev.ID, "type", ev.Type)
		respondError(c, http.StatusUnauthorized, codeUnauthorized, "invalid webhook secret")
		return
	}

	if h.alreadyProcessed(c, string(domain.GatewayMoyasar), ev.ID) {
		return
	}

	switch ev.Type {
	case "payment_paid":
		h.handleMoyasarPaymentPaid(c, ev)
	case "payment_failed":
		// A failed checkout attempt leaves the invoice open; nothing to settle.
		h.logger.Info("Moyasar payment failed", "event_id", ev.ID, "payment_id", ev.Data.ID)
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	case "payment_refunded", "payment_voided":
		h.handleMoyasarPaymentReversed(c, ev)
	default:
		h.logger.Info("Moyasar webhook event ignored", "event_id", ev.ID, "type", ev.Type)
		c.JSON(http.StatusOK, gin.H{"status": "ignored"})
	}

	// Record only on a 2xx, so a failed delivery is redelivered and retried.
	if status := c.Writer.Status(); status >= 200 && status < 300 {
		h.markProcessed(c.Request.Context(), string(domain.GatewayMoyasar), ev.ID, ev.Type)
	}
}

// loadMoyasarInvoice fetches the event's payment and the invoice its metadata
// names. It writes the response and returns ok=false when there is nothing to
// act on.
//
// Cross-tenant lookup: the webhook carries no tenant, so the invoice is loaded
// with the unscoped GetByIDPublic by the id in the fetched payment's metadata.
// The browser sets that metadata, so it is only a lookup key: the payment is
// then bound to the invoice by currency and amount, and every write runs
// under the invoice's own tenant.
func (h *WebhookHandler) loadMoyasarInvoice(c *gin.Context, ev moyasarWebhookEnvelope) (*port.GatewayPayment, *domain.Invoice, bool) {
	if ev.Data.ID == "" {
		h.logger.Info("Moyasar event ignored — no payment id", "event_id", ev.ID, "type", ev.Type)
		c.JSON(http.StatusOK, gin.H{"status": "ignored", "reason": "no payment id"})
		return nil, nil, false
	}
	ctx := c.Request.Context()
	p, err := h.moyasarFetcher.FetchPayment(ctx, ev.Data.ID)
	if err != nil || p == nil {
		// Transient: not marked processed, so Moyasar redelivers.
		h.logger.Error("failed to fetch Moyasar payment", "event_id", ev.ID, "payment_id", ev.Data.ID, "error", err)
		respondError(c, http.StatusBadGateway, codeGatewayUnavailable, "failed to fetch payment from gateway; retry")
		return nil, nil, false
	}
	invoiceID, err := uuid.Parse(p.InvoiceID)
	if err != nil {
		h.logger.Warn("Moyasar payment carries no valid invoice_id — ignoring", "event_id", ev.ID, "payment_id", p.ID)
		c.JSON(http.StatusOK, gin.H{"status": "ignored", "reason": "no invoice_id"})
		return nil, nil, false
	}
	inv, err := h.invoiceRepo.GetByIDPublic(ctx, invoiceID)
	if err != nil {
		h.logger.Error("failed to load invoice for Moyasar payment", "invoice_id", invoiceID, "payment_id", p.ID, "error", err)
		respondError(c, http.StatusInternalServerError, codeInternalError, "failed to load invoice")
		return nil, nil, false
	}
	if inv == nil {
		h.logger.Warn("Moyasar payment references unknown invoice — ignoring", "invoice_id", invoiceID, "payment_id", p.ID)
		c.JSON(http.StatusOK, gin.H{"status": "ignored", "reason": "unknown invoice_id"})
		return nil, nil, false
	}
	return p, inv, true
}

// handleMoyasarPaymentPaid settles the invoice a paid Moyasar payment belongs
// to and writes the HTTP response.
func (h *WebhookHandler) handleMoyasarPaymentPaid(c *gin.Context, ev moyasarWebhookEnvelope) {
	p, inv, ok := h.loadMoyasarInvoice(c, ev)
	if !ok {
		return
	}
	if err := service.BindPaymentToInvoice(p, inv); err != nil {
		h.logger.Warn("Moyasar payment not bound to invoice — not settling",
			"invoice_id", inv.ID, "payment_id", p.ID, "reason", err.Error())
		c.JSON(http.StatusOK, gin.H{"status": "ignored", "reason": err.Error()})
		return
	}
	switch p.Status {
	case "paid":
	case "initiated":
		// The read-back can lag the event; a 5xx keeps the event unprocessed so
		// Moyasar redelivers it once the payment reads as paid.
		h.logger.Warn("Moyasar payment_paid but the payment still reads initiated — asking for redelivery",
			"event_id", ev.ID, "payment_id", p.ID, "invoice_id", inv.ID)
		respondErrorStatus(c, http.StatusServiceUnavailable, "payment not yet confirmed by the gateway; retry")
		return
	default:
		h.logger.Warn("Moyasar payment_paid but the payment is not paid — not settling",
			"event_id", ev.ID, "payment_id", p.ID, "status", p.Status)
		c.JSON(http.StatusOK, gin.H{"status": "ignored", "reason": "payment not paid"})
		return
	}

	transitioned, err := h.moyasarSettler.Settle(c.Request.Context(), inv, p)
	if errors.Is(err, service.ErrInvoiceNotPayable) {
		// Void or draft: settlement flagged the payment for a refund, and a
		// redelivery would change nothing.
		c.JSON(http.StatusOK, gin.H{"status": "ignored", "reason": "invoice not payable"})
		return
	}
	if err != nil {
		h.logger.Error("failed to settle invoice from Moyasar payment", "invoice_id", inv.ID, "payment_id", p.ID, "error", err)
		respondInternalError(c, err)
		return
	}
	h.logger.Info("invoice settled via Moyasar webhook", "invoice_id", inv.ID, "payment_id", p.ID, "transitioned", transitioned)
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// handleMoyasarPaymentReversed flags a refund or void of a payment on a
// settled invoice. Refunds issued from Recurso credit notes also arrive here;
// one made in the Moyasar dashboard has no credit note and leaves the books
// overstating cash until someone reconciles it. Moyasar refunds have no id,
// so a refund whose outcome was unknown when issued is resolved from the
// payment's refunded total read back here.
func (h *WebhookHandler) handleMoyasarPaymentReversed(c *gin.Context, ev moyasarWebhookEnvelope) {
	p, inv, ok := h.loadMoyasarInvoice(c, ev)
	if !ok {
		return
	}
	if ev.Type == "payment_refunded" && h.creditNoteService != nil {
		if err := h.creditNoteService.ResolveUnknownRefunds(c.Request.Context(), inv, p.ID, p.RefundedAmount); err != nil {
			// Not recorded as processed, so Moyasar redelivers.
			h.logger.Error("failed to resolve Moyasar refunds of unknown outcome", "invoice_id", inv.ID, "payment_id", p.ID, "error", err)
			respondInternalError(c, err)
			return
		}
	}
	if inv.Status == domain.InvoiceStatusPaid {
		h.logger.Warn("Moyasar payment refunded or voided on a settled invoice — confirm a matching credit note exists",
			"event_type", ev.Type, "invoice_id", inv.ID, "payment_id", p.ID, "recorded_payment_id", inv.GatewayPaymentID,
			"payment_status", p.Status, "amount", p.Amount, "refunded_amount", p.RefundedAmount, "currency", p.Currency)
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
