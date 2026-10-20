package handler

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
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
	Live        bool   `json:"live"`
	Data        struct {
		ID string `json:"id"`
	} `json:"data"`
}

// SetMoyasar wires the Moyasar webhook: the payment read-back, the card-token
// store and the shared secret Moyasar echoes as secret_token. Nil-safe: left
// unset (or with an empty secret) the route fails closed with 503.
func (h *WebhookHandler) SetMoyasar(fetcher moyasarPaymentFetcher, saver gatewayCardSaver, secret string) {
	h.moyasarFetcher = fetcher
	h.moyasarCards = saver
	h.moyasarWebhookSecret = secret
}

// HandleMoyasar processes Moyasar webhook deliveries (POST /webhooks/moyasar).
//
// Moyasar documents no signature header; authenticity is the shared secret it
// echoes in the body as secret_token, compared here in constant time. The body
// is still never trusted for settlement: payment_paid re-fetches the payment
// with the secret key and settles only on that read-back, bound to the invoice
// by its own metadata, currency and amount. A leaked secret therefore lets an
// attacker replay real payments (idempotent) but never forge one.
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

	// Fail CLOSED: without a configured secret nothing can be authenticated.
	if h.moyasarWebhookSecret == "" {
		h.logger.Error("MOYASAR_WEBHOOK_SECRET not set — rejecting webhook (fail closed)", "ip", c.ClientIP())
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

	if h.moyasarFetcher == nil {
		h.logger.Info("Moyasar payment fetcher not configured, ignoring webhook", "event_id", ev.ID, "type", ev.Type)
		c.JSON(http.StatusOK, gin.H{"status": "ignored"})
		return
	}

	// Dedup on Moyasar's event id. A store outage answers 503 (Moyasar
	// redelivers); a duplicate is acknowledged without reprocessing.
	if h.alreadyProcessed(c, "moyasar", ev.ID) {
		return
	}

	switch ev.Type {
	case "payment_paid":
		h.handleMoyasarPaymentPaid(c, ev)
	case "payment_failed", "payment_faild": // Moyasar's own docs misspell it in places
		// Renewal and dunning record token-charge failures synchronously; a
		// failed checkout attempt leaves the invoice open. Nothing to settle.
		h.logger.Info("Moyasar payment failed", "event_id", ev.ID, "payment_id", ev.Data.ID)
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	case "payment_refunded":
		// Refunds originate from Recurso credit notes, which already post the
		// reversal when the gateway Refund call returns.
		h.logger.Info("Moyasar payment refunded", "event_id", ev.ID, "payment_id", ev.Data.ID)
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	default:
		h.logger.Info("Moyasar webhook event ignored", "event_id", ev.ID, "type", ev.Type)
		c.JSON(http.StatusOK, gin.H{"status": "ignored"})
	}

	// Record only on a 2xx, so a failed delivery is redelivered and retried.
	if status := c.Writer.Status(); status >= 200 && status < 300 {
		h.markProcessed(c.Request.Context(), "moyasar", ev.ID, ev.Type)
	}
}

// handleMoyasarPaymentPaid settles the invoice a paid Moyasar payment belongs
// to and writes the HTTP response. Only the server-side read-back of the
// payment is trusted; the webhook body contributes nothing but the payment id.
//
// Cross-tenant lookup: the webhook carries no tenant, so the invoice is loaded
// with the unscoped GetByIDPublic by the id in the FETCHED payment's metadata.
// This is safe because (1) the payment is read from Moyasar with the platform
// secret key, so its metadata is what Recurso itself set at checkout, (2)
// checkMoyasarPaymentForInvoice binds the payment to exactly that invoice
// (id, currency, amount), and (3) every write after the lookup runs under the
// invoice's own TenantID.
func (h *WebhookHandler) handleMoyasarPaymentPaid(c *gin.Context, ev moyasarWebhookEnvelope) {
	ctx := c.Request.Context()

	if ev.Data.ID == "" {
		h.logger.Info("Moyasar payment_paid ignored — no payment id", "event_id", ev.ID)
		c.JSON(http.StatusOK, gin.H{"status": "ignored", "reason": "no payment id"})
		return
	}

	p, err := h.moyasarFetcher.FetchPayment(ctx, ev.Data.ID)
	if err != nil || p == nil {
		// Transient: not marked processed, so Moyasar redelivers.
		h.logger.Error("failed to fetch Moyasar payment", "event_id", ev.ID, "payment_id", ev.Data.ID, "error", err)
		respondErrorStatus(c, http.StatusBadGateway, "failed to fetch payment from gateway; retry")
		return
	}
	if p.Status != "paid" {
		h.logger.Warn("Moyasar payment_paid but fetched payment is not paid — not settling",
			"event_id", ev.ID, "payment_id", p.ID, "status", p.Status)
		c.JSON(http.StatusOK, gin.H{"status": "ignored", "reason": "payment not paid"})
		return
	}

	invoiceID, err := uuid.Parse(p.InvoiceID)
	if err != nil {
		h.logger.Warn("Moyasar payment carries no valid invoice_id — not settling", "event_id", ev.ID, "payment_id", p.ID)
		c.JSON(http.StatusOK, gin.H{"status": "ignored", "reason": "no invoice_id"})
		return
	}

	inv, err := h.invoiceRepo.GetByIDPublic(ctx, invoiceID)
	if err != nil {
		h.logger.Error("failed to load invoice for Moyasar payment", "invoice_id", invoiceID, "payment_id", p.ID, "error", err)
		respondError(c, http.StatusInternalServerError, codeInternalError, "failed to load invoice")
		return
	}
	if inv == nil {
		h.logger.Warn("Moyasar payment references unknown invoice — not settling", "invoice_id", invoiceID, "payment_id", p.ID)
		c.JSON(http.StatusOK, gin.H{"status": "ignored", "reason": "unknown invoice_id"})
		return
	}
	if err := checkMoyasarPaymentForInvoice(p, inv); err != nil {
		h.logger.Warn("Moyasar payment not bound to invoice — not settling",
			"invoice_id", inv.ID, "payment_id", p.ID, "reason", err.Error())
		c.JSON(http.StatusOK, gin.H{"status": "ignored", "reason": err.Error()})
		return
	}

	if h.subService == nil {
		h.logger.Info("subscription service not configured, ignoring Moyasar settlement", "invoice_id", inv.ID, "payment_id", p.ID)
		c.JSON(http.StatusOK, gin.H{"status": "ignored"})
		return
	}

	tenantCtx := context.WithValue(ctx, domain.TenantIDKey, inv.TenantID)
	transitioned, err := h.subService.MarkInvoicePaid(tenantCtx, inv.ID)
	if err != nil {
		h.logger.Error("failed to mark invoice paid via Moyasar", "invoice_id", inv.ID, "payment_id", p.ID, "error", err)
		respondInternalError(c, err)
		return
	}
	h.logger.Info("invoice settled via Moyasar webhook", "invoice_id", inv.ID, "payment_id", p.ID, "transitioned", transitioned)

	// Record the payment (refund target) and its card token only when this
	// payment is the invoice's payment: it settled it now, nothing was recorded
	// yet, or it is a redelivery of the recorded one. A second payment against
	// an invoice another payment already settled must not overwrite either.
	if transitioned || inv.GatewayPaymentID == "" || inv.GatewayPaymentID == p.ID {
		if inv.GatewayPaymentID != p.ID {
			if err := h.invoiceRepo.SetGatewayPaymentID(ctx, inv.TenantID, inv.ID, p.ID); err != nil {
				h.logger.Error("failed to record Moyasar payment id", "invoice_id", inv.ID, "payment_id", p.ID, "error", err)
			}
		}
		if err := saveMoyasarCard(tenantCtx, h.moyasarCards, p, inv); err != nil {
			h.logger.Error("failed to save Moyasar card token", "invoice_id", inv.ID, "payment_id", p.ID, "error", err)
		}
	} else {
		h.logger.Warn("Moyasar payment for an invoice already settled by another payment — review for refund",
			"invoice_id", inv.ID, "payment_id", p.ID)
	}

	if transitioned {
		h.recordDunningSuccess(ctx, inv.ID)
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
