package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/recurso-dev/recurso/internal/core/port"
)

// MoyasarGateway is the Saudi card gateway (Mada, Visa, Mastercard) via the
// Moyasar Payments API — docs/spec_moyasar.md. The first payment is taken by
// Moyasar's embedded form in the browser (so CreateOrder makes no API call);
// the adapter verifies and reads back payments, charges saved card tokens
// off-session, and refunds. Gateway-managed subscriptions, UPI mandates and
// virtual accounts return not-supported errors.
//
// EXPERIMENTAL: built against the Moyasar API v1 documentation; sandbox
// (test-mode) verification is founder-gated.
type MoyasarGateway struct {
	secretKey       string
	checkoutBaseURL string
	baseURL         string
	httpClient      *http.Client
}

var _ port.PaymentGateway = (*MoyasarGateway)(nil)

const moyasarBaseURL = "https://api.moyasar.com/v1"

// moyasarNamespace seeds the name-based UUIDs used as given_id. It is fixed
// forever: changing it would map an in-flight idempotency key to a new payment
// id and let a replay charge the customer twice.
var moyasarNamespace = uuid.MustParse("6f1c2a8e-3b7d-4e0a-9c51-2d8f4b6e7a90")

// moyasarHardDeclines are ISO 8583 response codes after which retrying the
// same card cannot succeed (lost, stolen, expired, invalid, fraud,
// restricted). Moyasar does not classify codes itself; this grouping is
// Recurso's (spec "Decline mapping").
var moyasarHardDeclines = map[string]bool{
	"04": true, "07": true, "14": true, "15": true, "33": true, "34": true,
	"35": true, "36": true, "37": true, "38": true, "41": true, "43": true,
	"54": true, "56": true, "57": true, "59": true, "62": true, "63": true,
	"67": true, "79": true, "93": true,
}

// NewMoyasarGateway builds the gateway. The secret key alone selects test or
// live mode (sk_test_ / sk_live_). checkoutBaseURL is the dashboard origin used
// to build the callback_url that Moyasar requires for token charges.
func NewMoyasarGateway(secretKey, checkoutBaseURL string) *MoyasarGateway {
	return &MoyasarGateway{
		secretKey:       secretKey,
		checkoutBaseURL: strings.TrimRight(checkoutBaseURL, "/"),
		baseURL:         moyasarBaseURL,
		httpClient:      &http.Client{Timeout: 30 * time.Second},
	}
}

// moyasarAPIError is an HTTP >= 400 answer in Moyasar's {type, message,
// errors} shape. It never carries request headers, so the secret key cannot
// leak through an error string.
type moyasarAPIError struct {
	Path       string
	StatusCode int
	Type       string
	Message    string
}

func (e *moyasarAPIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("moyasar %s: %s (%s)", e.Path, e.Message, e.Type)
	}
	return fmt.Sprintf("moyasar %s: HTTP %d", e.Path, e.StatusCode)
}

func (g *MoyasarGateway) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.SetBasicAuth(g.secretKey, "")
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("moyasar request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode >= 400 {
		var me struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(payload, &me)
		return &moyasarAPIError{Path: path, StatusCode: resp.StatusCode, Type: me.Type, Message: me.Message}
	}
	if out != nil {
		if err := json.Unmarshal(payload, out); err != nil {
			return fmt.Errorf("moyasar %s: bad response: %w", path, err)
		}
	}
	return nil
}

// moyasarPayment is the subset of Moyasar's payment object the adapter reads.
type moyasarPayment struct {
	ID       string            `json:"id"`
	Status   string            `json:"status"`
	Amount   int64             `json:"amount"`
	Refunded int64             `json:"refunded"`
	Currency string            `json:"currency"`
	Metadata map[string]string `json:"metadata"`
	Source   struct {
		Type         string  `json:"type"`
		Company      string  `json:"company"`
		Number       string  `json:"number"`
		Token        string  `json:"token"`
		Message      string  `json:"message"`
		ResponseCode *string `json:"response_code"` // ISO 8583; null before the acquirer
	} `json:"source"`
}

func (p *moyasarPayment) toGatewayPayment() *port.GatewayPayment {
	return &port.GatewayPayment{
		ID:             p.ID,
		Status:         p.Status,
		Amount:         p.Amount,
		RefundedAmount: p.Refunded,
		Currency:       strings.ToUpper(p.Currency),
		InvoiceID:      p.Metadata["invoice_id"],
		CardToken:      p.Source.Token,
		CardBrand:      p.Source.Company,
		CardLast4:      lastFourDigits(p.Source.Number),
		Message:        p.Source.Message,
	}
}

// lastFourDigits extracts the last four digits of a masked card number such
// as "XXXX-XXXX-XXXX-1111" or "411111******1111"; "" if there are fewer.
func lastFourDigits(masked string) string {
	digits := make([]byte, 0, len(masked))
	for i := 0; i < len(masked); i++ {
		if masked[i] >= '0' && masked[i] <= '9' {
			digits = append(digits, masked[i])
		}
	}
	if len(digits) < 4 {
		return ""
	}
	return string(digits[len(digits)-4:])
}

func (g *MoyasarGateway) fetch(ctx context.Context, paymentID string) (*moyasarPayment, error) {
	if paymentID == "" {
		return nil, errors.New("moyasar: payment id is required")
	}
	var out moyasarPayment
	if err := g.do(ctx, http.MethodGet, "/payments/"+url.PathEscape(paymentID), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// FetchPayment reads one payment back with the secret key. Webhooks and the
// checkout return path settle only on this read-back, never on client data.
func (g *MoyasarGateway) FetchPayment(ctx context.Context, paymentID string) (*port.GatewayPayment, error) {
	p, err := g.fetch(ctx, paymentID)
	if err != nil {
		return nil, err
	}
	return p.toGatewayPayment(), nil
}

// moyasarGivenID derives the payment id Moyasar dedupes on from Recurso's
// idempotency key, so every replay of the same key targets the same payment.
func moyasarGivenID(idempotencyKey string) string {
	return uuid.NewSHA1(moyasarNamespace, []byte(idempotencyKey)).String()
}

// ChargeSavedPaymentMethod charges a saved card token off-session (renewal,
// dunning retry, wallet top-up). CustomerRef is unused: Moyasar has no
// customer object, the token alone identifies the card. Idempotency rides on
// given_id; a declined charge returns Success=false with a nil error, and
// Permanent marks a hard decline.
func (g *MoyasarGateway) ChargeSavedPaymentMethod(ctx context.Context, req port.SavedMethodCharge) (*port.PaymentResult, error) {
	var invalid string
	switch {
	case req.IdempotencyKey == "":
		invalid = "idempotency key is required (refusing to charge without one)"
	case req.MethodRef == "":
		invalid = "saved card token is required"
	case req.Amount <= 0:
		invalid = fmt.Sprintf("amount must be positive, got %d", req.Amount)
	}
	if invalid != "" {
		err := errors.New("moyasar charge: " + invalid)
		return &port.PaymentResult{Success: false, ErrorCode: "invalid_request", ErrorMsg: err.Error()}, err
	}

	givenID := moyasarGivenID(req.IdempotencyKey)
	var out moyasarPayment
	err := g.do(ctx, http.MethodPost, "/payments", map[string]any{
		"given_id":     givenID,
		"amount":       req.Amount,
		"currency":     strings.ToUpper(req.Currency),
		"description":  "Recurso invoice " + req.InvoiceID,
		"callback_url": g.checkoutBaseURL + "/checkout/" + req.InvoiceID,
		"metadata":     map[string]string{"invoice_id": req.InvoiceID},
		"source":       map[string]string{"type": "token", "token": req.MethodRef},
	}, &out)

	var apiErr *moyasarAPIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest &&
		strings.Contains(strings.ToLower(apiErr.Message), "already created") {
		// An earlier attempt with this key already created the payment:
		// read it back instead of creating a second one.
		existing, ferr := g.fetch(ctx, givenID)
		if ferr != nil {
			return &port.PaymentResult{Success: false, ErrorCode: "gateway_error", ErrorMsg: ferr.Error()}, ferr
		}
		return mapMoyasarCharge(existing), nil
	}
	if errors.As(err, &apiErr) && moyasarRejected(apiErr.StatusCode) {
		return &port.PaymentResult{Success: false, ErrorCode: "invalid_payment_method", ErrorMsg: apiErr.Error(), Permanent: true}, nil
	}
	if err != nil {
		return &port.PaymentResult{Success: false, ErrorCode: "gateway_error", ErrorMsg: err.Error()}, err
	}
	return mapMoyasarCharge(&out), nil
}

// moyasarRejected reports whether an HTTP status means Moyasar refused this
// charge request itself (bad or inactive token, invalid request): retrying it
// unchanged cannot succeed. Auth failures and throttling are not about the
// charge and stay errors, as do 5xx.
func moyasarRejected(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
		return false
	}
	return status >= 400 && status < 500
}

// mapMoyasarCharge applies the spec's decline-mapping table.
func mapMoyasarCharge(p *moyasarPayment) *port.PaymentResult {
	switch p.Status {
	case "paid":
		return &port.PaymentResult{Success: true, PaymentID: p.ID}
	case "failed":
		code := ""
		if p.Source.ResponseCode != nil {
			code = *p.Source.ResponseCode
		}
		res := &port.PaymentResult{Success: false, PaymentID: p.ID, ErrorCode: "card_declined", ErrorMsg: p.Source.Message}
		if code != "" {
			res.ErrorCode = "card_declined_" + code
			res.Permanent = moyasarHardDeclines[code]
		}
		return res
	case "initiated":
		// The issuer wants 3DS: an off-session token charge cannot complete
		// without the customer, who pays via the checkout link instead.
		return &port.PaymentResult{
			Success:   false,
			PaymentID: p.ID,
			ErrorCode: "authentication_required",
			ErrorMsg:  "issuer requires 3-D Secure; customer must pay via checkout",
		}
	default:
		return &port.PaymentResult{
			Success:   false,
			PaymentID: p.ID,
			ErrorCode: "unexpected_status_" + p.Status,
			ErrorMsg:  p.Source.Message,
		}
	}
}

// RetryPayment cannot charge anything: without a saved token there is no
// off-session payment, so it reports the missing method and dunning keeps
// emailing the pay link. No HTTP call.
func (g *MoyasarGateway) RetryPayment(ctx context.Context, invoiceID string, amount int64, currency string) (*port.PaymentResult, error) {
	return &port.PaymentResult{
		Success:   false,
		ErrorCode: port.ErrCodeNoSavedMethod,
		ErrorMsg:  "moyasar charges need a saved card token",
	}, nil
}

// CreateOrder makes no API call: Moyasar's embedded form creates the payment
// in the browser with the publishable key, bound to the invoice via metadata.
func (g *MoyasarGateway) CreateOrder(ctx context.Context, amount int64, currency string, receipt string, invoiceID string) (*port.PaymentOrder, error) {
	return &port.PaymentOrder{
		ID:       invoiceID,
		Amount:   amount,
		Currency: currency,
		Receipt:  receipt,
		Gateway:  "moyasar",
	}, nil
}

// VerifyPayment reads the payment back and accepts it only when it is paid
// and bound to the invoice (orderID is the invoice id; there is no signature).
func (g *MoyasarGateway) VerifyPayment(ctx context.Context, orderID, paymentID, _ string) error {
	p, err := g.FetchPayment(ctx, paymentID)
	if err != nil {
		return err
	}
	if p.Status != "paid" {
		return fmt.Errorf("moyasar payment %s is %q, not paid", p.ID, p.Status)
	}
	if p.InvoiceID != orderID {
		return fmt.Errorf("moyasar payment %s belongs to invoice %q, not %q", p.ID, p.InvoiceID, orderID)
	}
	return nil
}

// Refund refunds (part of) a payment. Moyasar has no refund idempotency key,
// so the adapter makes exactly one call and never retries: a blind retry after
// a timeout could refund twice. The refund has no id of its own; the payment
// id is returned.
func (g *MoyasarGateway) Refund(ctx context.Context, paymentID string, amount int64, currency string) (*port.RefundResult, error) {
	if paymentID == "" {
		return nil, errors.New("moyasar refund: payment id is required")
	}
	if amount <= 0 {
		return nil, fmt.Errorf("moyasar refund: amount must be positive, got %d", amount)
	}
	var out moyasarPayment
	if err := g.do(ctx, http.MethodPost, "/payments/"+url.PathEscape(paymentID)+"/refund",
		map[string]any{"amount": amount}, &out); err != nil {
		return nil, err
	}
	return &port.RefundResult{RefundID: paymentID, Status: "refunded"}, nil
}

// OwnsPaymentID reports whether id looks like a Moyasar payment id (a bare
// UUID), for the smart router's verify fallback.
func (g *MoyasarGateway) OwnsPaymentID(id string) bool {
	_, err := uuid.Parse(id)
	return err == nil
}

// --- Not supported ---

// CreateSubscription must never return an id: a non-empty id is stored as the
// gateway subscription id and the renewal claim then skips the subscription.
func (g *MoyasarGateway) CreateSubscription(ctx context.Context, planID string, totalCount int, customerEmail string, startAt *int64, currency string) (string, error) {
	return "", errors.New("gateway-managed subscriptions are not supported by moyasar (Recurso owns the cycle)")
}

func (g *MoyasarGateway) CancelSubscription(ctx context.Context, subscriptionID string) error {
	return nil // Recurso owns the billing cycle; nothing exists gateway-side
}

func (g *MoyasarGateway) CreateMandate(ctx context.Context, customerEmail, customerContact, vpa string, maxAmount int64, frequency, _ string) (*port.MandateResult, error) {
	return nil, errors.New("UPI mandates are not supported by moyasar")
}

func (g *MoyasarGateway) ExecuteMandateDebit(ctx context.Context, req port.MandateDebitRequest) (*port.PaymentResult, error) {
	return nil, errors.New("UPI mandate debits are not supported by moyasar")
}

func (g *MoyasarGateway) RevokeMandate(ctx context.Context, customerID, tokenID, _ string) error {
	return errors.New("UPI mandates are not supported by moyasar")
}

func (g *MoyasarGateway) CreateVirtualAccount(ctx context.Context, customerID, invoiceID string, amount int64, description string) (*port.VirtualAccountResult, error) {
	return nil, errors.New("virtual accounts are not supported by moyasar")
}
