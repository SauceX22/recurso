package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/recurso-dev/recurso/internal/core/port"
)

// --- Test harness: record every request, answer per "METHOD path" ---

type moyasarCall struct {
	method, path, rawPath, auth, contentType string
	query                                    url.Values
	body                                     map[string]any
}

type moyasarReply struct {
	status int
	body   string
}

type moyasarRecorder struct {
	mu    sync.Mutex
	calls []moyasarCall
}

func (r *moyasarRecorder) all() []moyasarCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]moyasarCall(nil), r.calls...)
}

func (r *moyasarRecorder) count(method string) int {
	n := 0
	for _, c := range r.all() {
		if c.method == method {
			n++
		}
	}
	return n
}

// to returns the calls made with method to path, in order.
func (r *moyasarRecorder) to(method, path string) []moyasarCall {
	var out []moyasarCall
	for _, c := range r.all() {
		if c.method == method && c.path == path {
			out = append(out, c)
		}
	}
	return out
}

const moyasarNoPayments = `{"payments":[],"meta":{"current_page":1,"next_page":null,"total_count":0}}`

// moyasarTestServer serves canned replies keyed by "METHOD /path"; an
// unrouted request gets a 404 so a stray call fails the test loudly. The
// payment list an invoice charge consults first answers "none" unless a
// test routes it.
func moyasarTestServer(t *testing.T, routes map[string]moyasarReply) (*httptest.Server, *moyasarRecorder) {
	t.Helper()
	rec := &moyasarRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := moyasarCall{
			method:      r.Method,
			path:        r.URL.Path,
			rawPath:     r.URL.EscapedPath(),
			auth:        r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"),
			query:       r.URL.Query(),
		}
		_ = json.NewDecoder(r.Body).Decode(&c.body)
		rec.mu.Lock()
		rec.calls = append(rec.calls, c)
		rec.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		reply, ok := routes[r.Method+" "+r.URL.Path]
		if !ok && r.Method == http.MethodGet && r.URL.Path == "/payments" {
			reply, ok = moyasarReply{http.StatusOK, moyasarNoPayments}, true
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"type":"api_error","message":"Object not found","errors":null}`))
			return
		}
		w.WriteHeader(reply.status)
		_, _ = w.Write([]byte(reply.body))
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

const moyasarTestSecret = "sk_test_x"

func testMoyasar(srvURL string) *MoyasarGateway {
	return NewMoyasarGateway(moyasarTestSecret, "https://app.recurso.dev/", srvURL)
}

// moyasarPaymentJSON builds a payment object; responseCode "null" renders a
// JSON null, anything else a quoted string.
func moyasarPaymentJSON(id, status, responseCode, message string) string {
	code := "null"
	if responseCode != "null" {
		code = `"` + responseCode + `"`
	}
	return `{"id":"` + id + `","status":"` + status + `","amount":10000,"refunded":0,"currency":"SAR",
		"metadata":{"invoice_id":"inv-1"},
		"source":{"type":"token","company":"mada","number":"4201-32XX-XXXX-1010",
		"token":"token_abc","message":"` + message + `","response_code":` + code + `}}`
}

const moyasarPaymentID = "760878ec-d1d3-5f72-9056-191683f55872"

// --- Constructor and auth ---

func TestMoyasarNewGatewayDefaults(t *testing.T) {
	g := NewMoyasarGateway("sk_live_y", "https://app.recurso.dev//", "")
	if g.baseURL != "https://api.moyasar.com/v1" {
		t.Fatalf("baseURL = %q, want Moyasar v1 API", g.baseURL)
	}
	if g.checkoutBaseURL != "https://app.recurso.dev" {
		t.Fatalf("checkoutBaseURL = %q, trailing slashes must be trimmed or callback_url gets //", g.checkoutBaseURL)
	}
	if g.httpClient == nil || g.httpClient.Timeout == 0 {
		t.Fatal("http client must have a timeout so a hung gateway cannot block renewals")
	}
}

func TestMoyasarBasicAuthSecretKeyEmptyPassword(t *testing.T) {
	srv, rec := moyasarTestServer(t, map[string]moyasarReply{
		"GET /payments/" + moyasarPaymentID: {http.StatusOK, moyasarPaymentJSON(moyasarPaymentID, "paid", "00", "APPROVED")},
	})
	g := testMoyasar(srv.URL)

	if _, err := g.FetchPayment(context.Background(), moyasarPaymentID); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("sk_test_x:"))
	if got := rec.all()[0].auth; got != want {
		t.Fatalf("Authorization = %q, want %q (secret key as username, empty password)", got, want)
	}
}

// --- ChargeSavedPaymentMethod ---

func TestMoyasarChargeSavedPaymentMethod(t *testing.T) {
	givenID := moyasarGivenID("renewal-inv-1")
	srv, rec := moyasarTestServer(t, map[string]moyasarReply{
		"POST /payments": {http.StatusCreated, moyasarPaymentJSON(givenID, "paid", "00", "APPROVED")},
	})
	g := testMoyasar(srv.URL)

	res, err := g.ChargeSavedPaymentMethod(context.Background(), port.SavedMethodCharge{MethodRef: "token_abc", Amount: 10000, Currency: "sar", InvoiceID: "inv-1", IdempotencyKey: "renewal-inv-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Success || res.PaymentID != givenID || res.Permanent {
		t.Fatalf("res = %+v, want success with PaymentID = given_id %s", res, givenID)
	}

	calls := rec.to(http.MethodPost, "/payments")
	if len(calls) != 1 || len(rec.all()) != 2 {
		t.Fatalf("calls = %+v, want the paid-payment lookup then exactly one POST /payments", rec.all())
	}
	c := calls[0]
	if c.contentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", c.contentType)
	}
	b := c.body
	if b["given_id"] != givenID {
		t.Fatalf("given_id = %v, want %s derived from the idempotency key", b["given_id"], givenID)
	}
	if b["amount"].(float64) != 10000 {
		t.Fatalf("amount = %v, want 10000 minor units", b["amount"])
	}
	if b["currency"] != "SAR" {
		t.Fatalf("currency = %v, want upper-case SAR", b["currency"])
	}
	if b["description"] != "Invoice inv-1" {
		t.Fatalf("description = %v", b["description"])
	}
	if b["callback_url"] != "https://app.recurso.dev/checkout/inv-1" {
		t.Fatalf("callback_url = %v, Moyasar requires it for token sources", b["callback_url"])
	}
	if md := b["metadata"].(map[string]any); md["invoice_id"] != "inv-1" || len(md) != 1 {
		t.Fatalf("metadata = %v, want only invoice_id (webhook settlement binds on it)", md)
	}
	src := b["source"].(map[string]any)
	if src["type"] != "token" || src["token"] != "token_abc" || len(src) != 2 {
		t.Fatalf("source = %v, want {type:token, token:token_abc}", src)
	}
	if len(b) != 7 {
		t.Fatalf("body has %d fields, want exactly 7: %v", len(b), b)
	}
}

func TestMoyasarGivenIDDeterministic(t *testing.T) {
	a1, a2 := moyasarGivenID("renewal-inv-1"), moyasarGivenID("renewal-inv-1")
	b := moyasarGivenID("retry-inv-1-2")
	if a1 != a2 {
		t.Fatalf("same idempotency key gave %s and %s; a replay would create a second payment", a1, a2)
	}
	if a1 == b {
		t.Fatal("different idempotency keys must map to different payment ids")
	}
	if _, err := uuid.Parse(a1); err != nil {
		t.Fatalf("given_id %q is not a UUID: %v", a1, err)
	}
	// Pinned: a changed namespace maps an in-flight key to a new payment id,
	// so a replay after the change would charge the customer again.
	if want := "8575d574-fa2e-569f-a060-2fd2f3c06217"; a1 != want {
		t.Fatalf("given_id(renewal-inv-1) = %s, want %s", a1, want)
	}
}

func TestMoyasarChargeSameKeySameGivenIDOnTheWire(t *testing.T) {
	srv, rec := moyasarTestServer(t, map[string]moyasarReply{
		"POST /payments": {http.StatusCreated, moyasarPaymentJSON(moyasarPaymentID, "paid", "00", "")},
	})
	g := testMoyasar(srv.URL)
	ctx := context.Background()
	for _, key := range []string{"renewal-inv-1", "renewal-inv-1", "retry-inv-1-1"} {
		if _, err := g.ChargeSavedPaymentMethod(ctx, port.SavedMethodCharge{MethodRef: "token_abc", Amount: 100, Currency: "SAR", InvoiceID: "inv-1", IdempotencyKey: key}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	calls := rec.to(http.MethodPost, "/payments")
	if calls[0].body["given_id"] != calls[1].body["given_id"] {
		t.Fatal("replaying the same key must send the same given_id")
	}
	if calls[0].body["given_id"] == calls[2].body["given_id"] {
		t.Fatal("a different key must send a different given_id")
	}
}

func TestMoyasarChargeDeclineMapping(t *testing.T) {
	// ErrorCode keys the dunning bandit's context, so it is a small fixed
	// vocabulary; the raw ISO 8583 code goes into ErrorMsg.
	cases := []struct {
		name, status, code, message string
		wantCode                    string
		wantPermanent               bool
		wantMsg                     string
	}{
		{"insufficient funds", "failed", "51", "INSUFFICIENT FUNDS", "insufficient_funds", false, "INSUFFICIENT FUNDS (response code 51)"},
		{"withdrawal limit", "failed", "61", "EXCEEDS WITHDRAWAL LIMIT", "insufficient_funds", false, "EXCEEDS WITHDRAWAL LIMIT (response code 61)"},
		{"do not honour", "failed", "05", "DO NOT HONOUR", "card_declined", false, "DO NOT HONOUR (response code 05)"},
		{"stolen card", "failed", "43", "STOLEN CARD", "lost_or_stolen", true, "STOLEN CARD (response code 43)"},
		{"lost card", "failed", "41", "LOST CARD", "lost_or_stolen", true, "LOST CARD (response code 41)"},
		{"expired card", "failed", "54", "EXPIRED CARD", "expired_card", true, "EXPIRED CARD (response code 54)"},
		{"other hard decline", "failed", "62", "RESTRICTED CARD", "card_declined", true, "RESTRICTED CARD (response code 62)"},
		{"null response code", "failed", "null", "3-D Secure failed", "card_declined", false, "3-D Secure failed"},
		{"3ds required", "initiated", "null", "", "authentication_required", false, "issuer requires 3-D Secure; customer must pay via checkout"},
		{"unexpected status", "authorized", "00", "", "processing_error", false, `unexpected payment status "authorized"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := moyasarTestServer(t, map[string]moyasarReply{
				"POST /payments": {http.StatusCreated, moyasarPaymentJSON(moyasarPaymentID, tc.status, tc.code, tc.message)},
			})
			g := testMoyasar(srv.URL)

			res, err := g.ChargeSavedPaymentMethod(context.Background(), port.SavedMethodCharge{MethodRef: "token_abc", Amount: 10000, Currency: "SAR", InvoiceID: "inv-1", IdempotencyKey: "renewal-inv-1"})
			if err != nil {
				t.Fatalf("a decline is a result, not an error: %v", err)
			}
			if res.Success {
				t.Fatalf("res = %+v, status %q must not be a success", res, tc.status)
			}
			if res.ErrorCode != tc.wantCode {
				t.Fatalf("ErrorCode = %q, want %q", res.ErrorCode, tc.wantCode)
			}
			if res.Permanent != tc.wantPermanent {
				t.Fatalf("Permanent = %v, want %v (only hard declines stop dunning)", res.Permanent, tc.wantPermanent)
			}
			if res.ErrorMsg != tc.wantMsg {
				t.Fatalf("ErrorMsg = %q, want %q", res.ErrorMsg, tc.wantMsg)
			}
			if res.PaymentID != moyasarPaymentID {
				t.Fatalf("PaymentID = %q, want the declined payment's id for traceability", res.PaymentID)
			}
		})
	}
}

// Every hard code stops dunning on the card; dropping one from the list would
// keep re-charging lost, stolen or closed cards, which card networks forbid.
// The list is the spec's decline table.
func TestClassifyMoyasarDecline(t *testing.T) {
	for _, rc := range []string{"04", "07", "14", "15", "33", "34", "35", "36", "37", "38", "41", "43", "54", "56", "57", "59", "62", "63", "67", "79", "93"} {
		if _, hard := classifyMoyasarDecline(rc); !hard {
			t.Errorf("%s must be a hard decline", rc)
		}
	}
	for _, rc := range []string{"05", "51", "61", "65", "91", "96", ""} {
		if _, hard := classifyMoyasarDecline(rc); hard {
			t.Errorf("%s must be soft", rc)
		}
	}
}

// MOYASAR_API_URL points the adapter at a stub for the e2e suite.
func TestMoyasarAPIURLOverride(t *testing.T) {
	srv, rec := moyasarTestServer(t, map[string]moyasarReply{
		"GET /payments/" + moyasarPaymentID: {http.StatusOK, moyasarPaymentJSON(moyasarPaymentID, "paid", "00", "APPROVED")},
	})
	g := NewMoyasarGateway(moyasarTestSecret, "https://app.recurso.dev", srv.URL+"/")
	if _, err := g.FetchPayment(context.Background(), moyasarPaymentID); err != nil {
		t.Fatalf("fetch through the override: %v", err)
	}
	if len(rec.all()) != 1 {
		t.Fatalf("calls = %+v, want the one fetch on the override URL", rec.all())
	}
}

func TestMoyasarChargeAlreadyCreatedFetchesInsteadOfRecreating(t *testing.T) {
	givenID := moyasarGivenID("renewal-inv-1")
	srv, rec := moyasarTestServer(t, map[string]moyasarReply{
		"POST /payments": {http.StatusBadRequest,
			`{"type":"invalid_request_error","message":"Payment is already created.","errors":null}`},
		"GET /payments/" + givenID: {http.StatusOK, moyasarPaymentJSON(givenID, "paid", "00", "APPROVED")},
	})
	g := testMoyasar(srv.URL)

	res, err := g.ChargeSavedPaymentMethod(context.Background(), port.SavedMethodCharge{MethodRef: "token_abc", Amount: 10000, Currency: "SAR", InvoiceID: "inv-1", IdempotencyKey: "renewal-inv-1"})
	if err != nil {
		t.Fatalf("an already-created payment must resolve via read-back, got %v", err)
	}
	if !res.Success || res.PaymentID != givenID {
		t.Fatalf("res = %+v, want the existing paid payment %s", res, givenID)
	}
	if n := rec.count(http.MethodPost); n != 1 {
		t.Fatalf("POST count = %d, want exactly 1: the adapter must never create a second payment", n)
	}
	calls := rec.all()
	if last := calls[len(calls)-1]; last.method != http.MethodGet || last.path != "/payments/"+givenID {
		t.Fatalf("follow-up = %s %s, want GET /payments/%s", last.method, last.path, givenID)
	}
}

// Replay detection must not depend on the wording of Moyasar's message, which
// changes and is localized: any 400 on a create that carried given_id is
// checked by reading that id back.
func TestMoyasarChargeReplayDetectedByReadBackNotMessage(t *testing.T) {
	givenID := moyasarGivenID("renewal-inv-1")
	srv, rec := moyasarTestServer(t, map[string]moyasarReply{
		"POST /payments": {http.StatusBadRequest,
			`{"type":"invalid_request_error","message":"تم إنشاء عملية الدفع مسبقاً","errors":null}`},
		"GET /payments/" + givenID: {http.StatusOK, moyasarPaymentJSON(givenID, "paid", "00", "APPROVED")},
	})
	g := testMoyasar(srv.URL)

	res, err := g.ChargeSavedPaymentMethod(context.Background(), port.SavedMethodCharge{MethodRef: "token_abc", Amount: 10000, Currency: "SAR", InvoiceID: "inv-1", IdempotencyKey: "renewal-inv-1"})
	if err != nil || !res.Success || res.PaymentID != givenID {
		t.Fatalf("res = %+v, err = %v; want the existing paid payment %s", res, err, givenID)
	}
	if n := rec.count(http.MethodPost); n != 1 {
		t.Fatalf("POST count = %d, want 1", n)
	}
}

// A 400 whose given_id was never created is a genuine rejection of the
// request, reported with the create's error.
func TestMoyasarChargeBadRequestWithNoExistingPaymentIsRejected(t *testing.T) {
	givenID := moyasarGivenID("renewal-inv-1")
	srv, rec := moyasarTestServer(t, map[string]moyasarReply{
		"POST /payments": {http.StatusBadRequest,
			`{"type":"invalid_request_error","message":"Validation Failed","errors":{"amount":["must be greater than 100"]}}`},
	})
	g := testMoyasar(srv.URL)

	res, err := g.ChargeSavedPaymentMethod(context.Background(), port.SavedMethodCharge{MethodRef: "token_abc", Amount: 50, Currency: "SAR", InvoiceID: "inv-1", IdempotencyKey: "renewal-inv-1"})
	if err == nil || res == nil || res.Success || res.Permanent {
		t.Fatalf("res = %+v, err = %v; want the original rejection as an error", res, err)
	}
	if want := "moyasar /payments: Validation Failed (invalid_request_error)"; !strings.Contains(res.ErrorMsg, want) {
		t.Fatalf("ErrorMsg = %q, want the create's error %q, not the read-back's", res.ErrorMsg, want)
	}
	calls := rec.all()
	if len(calls) != 3 || calls[1].method != http.MethodPost || calls[2].method != http.MethodGet || calls[2].path != "/payments/"+givenID {
		t.Fatalf("calls = %+v, want lookup, POST, then GET /payments/%s", calls, givenID)
	}
}

// A replayed key must not settle on a payment for a different amount or
// currency: that is a reused key, not the same charge.
func TestMoyasarChargeReplayMustMatchAmountAndCurrency(t *testing.T) {
	givenID := moyasarGivenID("renewal-inv-1")
	for _, tc := range []struct{ name, amount, currency string }{
		{"amount", "5000", "SAR"},
		{"currency", "10000", "USD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := moyasarTestServer(t, map[string]moyasarReply{
				"POST /payments": {http.StatusBadRequest, `{"type":"invalid_request_error","message":"Payment is already created.","errors":null}`},
				"GET /payments/" + givenID: {http.StatusOK, `{"id":"` + givenID + `","status":"paid","amount":` + tc.amount +
					`,"currency":"` + tc.currency + `","metadata":{"invoice_id":"inv-1"},"source":{"type":"token","response_code":"00"}}`},
			})
			g := testMoyasar(srv.URL)

			res, err := g.ChargeSavedPaymentMethod(context.Background(), port.SavedMethodCharge{MethodRef: "token_abc", Amount: 10000, Currency: "SAR", InvoiceID: "inv-1", IdempotencyKey: "renewal-inv-1"})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Success || res.ErrorCode != "processing_error" || res.PaymentID != givenID {
				t.Fatalf("res = %+v, want a processing_error failure naming %s", res, givenID)
			}
		})
	}
}

// Only a 400 can mean "already created"; the same words on a 5xx are an outage.
func TestMoyasarChargeAlreadyCreatedOnlyOnBadRequest(t *testing.T) {
	srv, rec := moyasarTestServer(t, map[string]moyasarReply{
		"POST /payments": {http.StatusInternalServerError, `{"type":"api_error","message":"Payment is already created.","errors":null}`},
	})
	g := testMoyasar(srv.URL)

	res, err := g.ChargeSavedPaymentMethod(context.Background(), port.SavedMethodCharge{MethodRef: "token_abc", Amount: 10000, Currency: "SAR", InvoiceID: "inv-1", IdempotencyKey: "renewal-inv-1"})
	if err == nil || res.ErrorCode != "gateway_error" {
		t.Fatalf("res = %+v, err = %v; want gateway_error", res, err)
	}
	if n := len(rec.to(http.MethodGet, "/payments/"+moyasarGivenID("renewal-inv-1"))); n != 0 {
		t.Fatalf("given_id read-backs = %d, want 0", n)
	}
}

// A wallet top-up is not an invoice: it must not carry an invoice_id the
// webhook would try to settle, nor a checkout callback that leads nowhere.
func TestMoyasarChargeWalletTopUpIsLabelledAsWallet(t *testing.T) {
	givenID := moyasarGivenID("wallet-recharge-w-1")
	srv, rec := moyasarTestServer(t, map[string]moyasarReply{
		"POST /payments": {http.StatusCreated, moyasarPaymentJSON(givenID, "paid", "00", "APPROVED")},
	})
	g := testMoyasar(srv.URL)

	res, err := g.ChargeSavedPaymentMethod(context.Background(), port.SavedMethodCharge{MethodRef: "token_abc", Amount: 10000, Currency: "SAR", WalletID: "wallet-9", IdempotencyKey: "wallet-recharge-w-1"})
	if err != nil || !res.Success {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	posts := rec.to(http.MethodPost, "/payments")
	if len(posts) != 1 || len(rec.all()) != 1 {
		t.Fatalf("calls = %+v, want one POST and no invoice lookup", rec.all())
	}
	b := posts[0].body
	if md := b["metadata"].(map[string]any); md["wallet_id"] != "wallet-9" || len(md) != 1 {
		t.Fatalf("metadata = %v, want only wallet_id", md)
	}
	if b["description"] != "Wallet top-up wallet-9" {
		t.Fatalf("description = %v", b["description"])
	}
	if b["callback_url"] != "https://app.recurso.dev" {
		t.Fatalf("callback_url = %v, want the dashboard origin, not an invoice checkout", b["callback_url"])
	}
}

// --- Already-paid invoices ---

// A charge whose response was lost (timeout, crash) can have paid the invoice
// under a different key than the next attempt uses (renewal-<inv> then
// retry-<inv>-1), and without the webhook nothing settled it. Charging again
// would take the money twice.
func TestMoyasarChargeInvoiceAlreadyPaidIsNotChargedAgain(t *testing.T) {
	const earlier = "11111111-2222-4333-8444-555555555555"
	srv, rec := moyasarTestServer(t, map[string]moyasarReply{
		"GET /payments": {http.StatusOK, `{"payments":[` + moyasarPaymentJSON(earlier, "paid", "00", "APPROVED") + `],"meta":{"total_count":1}}`},
	})
	g := testMoyasar(srv.URL)

	res, err := g.ChargeSavedPaymentMethod(context.Background(), port.SavedMethodCharge{MethodRef: "token_abc", Amount: 10000, Currency: "SAR", InvoiceID: "inv-1", IdempotencyKey: "retry-inv-1-1"})
	if err != nil || !res.Success || res.PaymentID != earlier {
		t.Fatalf("res = %+v, err = %v; want success with the earlier payment %s", res, err, earlier)
	}
	if n := rec.count(http.MethodPost); n != 0 {
		t.Fatalf("POST count = %d, want 0: the invoice is already paid", n)
	}
	q := rec.to(http.MethodGet, "/payments")[0].query
	if q.Get("metadata[invoice_id]") != "inv-1" || q.Get("status") != "paid" {
		t.Fatalf("lookup query = %v, want metadata[invoice_id]=inv-1&status=paid", q)
	}
}

// Only a paid payment of this invoice that still covers this charge in its
// currency counts. The list query filters by invoice and status too, but if
// Moyasar ignored those filters, any other payment returned here would settle
// the invoice without charging anyone.
func TestMoyasarChargeIgnoresPaidPaymentsThatDontCover(t *testing.T) {
	for _, tc := range []struct{ name, payment string }{
		{"smaller amount", `{"id":"other","status":"paid","amount":5000,"refunded":0,"currency":"SAR","metadata":{"invoice_id":"inv-1"},"source":{}}`},
		{"other currency", `{"id":"other","status":"paid","amount":10000,"refunded":0,"currency":"USD","metadata":{"invoice_id":"inv-1"},"source":{}}`},
		{"other invoice", `{"id":"other","status":"paid","amount":10000,"refunded":0,"currency":"SAR","metadata":{"invoice_id":"inv-2"},"source":{}}`},
		{"fully refunded", `{"id":"other","status":"paid","amount":10000,"refunded":10000,"currency":"SAR","metadata":{"invoice_id":"inv-1"},"source":{}}`},
		{"not paid", `{"id":"other","status":"failed","amount":10000,"refunded":0,"currency":"SAR","metadata":{"invoice_id":"inv-1"},"source":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			givenID := moyasarGivenID("retry-inv-1-1")
			srv, rec := moyasarTestServer(t, map[string]moyasarReply{
				"GET /payments":  {http.StatusOK, `{"payments":[` + tc.payment + `]}`},
				"POST /payments": {http.StatusCreated, moyasarPaymentJSON(givenID, "paid", "00", "APPROVED")},
			})
			g := testMoyasar(srv.URL)

			res, err := g.ChargeSavedPaymentMethod(context.Background(), port.SavedMethodCharge{MethodRef: "token_abc", Amount: 10000, Currency: "SAR", InvoiceID: "inv-1", IdempotencyKey: "retry-inv-1-1"})
			if err != nil || !res.Success || res.PaymentID != givenID {
				t.Fatalf("res = %+v, err = %v; want a new charge %s", res, err, givenID)
			}
			if n := rec.count(http.MethodPost); n != 1 {
				t.Fatalf("POST count = %d, want 1", n)
			}
		})
	}
}

// If Moyasar can't say whether the invoice is paid, don't charge blind.
func TestMoyasarChargeLookupFailureDoesNotCharge(t *testing.T) {
	srv, rec := moyasarTestServer(t, map[string]moyasarReply{
		"GET /payments": {http.StatusBadGateway, ``},
	})
	g := testMoyasar(srv.URL)

	res, err := g.ChargeSavedPaymentMethod(context.Background(), port.SavedMethodCharge{MethodRef: "token_abc", Amount: 10000, Currency: "SAR", InvoiceID: "inv-1", IdempotencyKey: "retry-inv-1-1"})
	if err == nil || res == nil || res.Success {
		t.Fatalf("res = %+v, err = %v; want an error", res, err)
	}
	if n := rec.count(http.MethodPost); n != 0 {
		t.Fatalf("POST count = %d, want 0", n)
	}
}

// The read-back of a replay goes through the same decline mapping.
func TestMoyasarChargeReplayOfHardDeclineIsPermanent(t *testing.T) {
	givenID := moyasarGivenID("renewal-inv-1")
	srv, _ := moyasarTestServer(t, map[string]moyasarReply{
		"POST /payments":           {http.StatusBadRequest, `{"type":"invalid_request_error","message":"Payment is already created.","errors":null}`},
		"GET /payments/" + givenID: {http.StatusOK, moyasarPaymentJSON(givenID, "failed", "54", "EXPIRED CARD")},
	})
	g := testMoyasar(srv.URL)

	res, err := g.ChargeSavedPaymentMethod(context.Background(), port.SavedMethodCharge{MethodRef: "token_abc", Amount: 10000, Currency: "SAR", InvoiceID: "inv-1", IdempotencyKey: "renewal-inv-1"})
	if err != nil || res.Success || !res.Permanent {
		t.Fatalf("res = %+v, err = %v; want a permanent decline", res, err)
	}
}

func TestMoyasarChargeAlreadyCreatedFetchFails(t *testing.T) {
	givenID := moyasarGivenID("renewal-inv-1")
	srv, rec := moyasarTestServer(t, map[string]moyasarReply{
		"POST /payments": {http.StatusBadRequest,
			`{"type":"invalid_request_error","message":"Payment is already created.","errors":null}`},
		"GET /payments/" + givenID: {http.StatusBadGateway, ``},
	})
	g := testMoyasar(srv.URL)

	res, err := g.ChargeSavedPaymentMethod(context.Background(), port.SavedMethodCharge{MethodRef: "token_abc", Amount: 10000, Currency: "SAR", InvoiceID: "inv-1", IdempotencyKey: "renewal-inv-1"})
	if err == nil {
		t.Fatal("a failed read-back must surface an error, not a guessed result")
	}
	if res == nil || res.Success || res.ErrorCode != "gateway_error" {
		t.Fatalf("res = %+v, want gateway_error", res)
	}
	if n := rec.count(http.MethodPost); n != 1 {
		t.Fatalf("POST count = %d, want 1", n)
	}
}

// Moyasar rejecting the saved card itself (an inactive, expired or deleted
// token) is an answer about the card: a permanent decline, not an error, so
// dunning stops charging it instead of retrying forever.
func TestMoyasarChargeTokenRejectedIsPermanentDecline(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"token field invalid", http.StatusBadRequest,
			`{"type":"invalid_request_error","message":"Validation Failed","errors":{"source.token":["is invalid"]}}`},
		{"source rejected", http.StatusUnprocessableEntity,
			`{"type":"invalid_request_error","message":"Validation Failed","errors":{"source":["token is inactive"]}}`},
		{"token named in the message", http.StatusBadRequest,
			`{"type":"invalid_request_error","message":"Token is expired","errors":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := moyasarTestServer(t, map[string]moyasarReply{
				"POST /payments": {tc.status, tc.body},
			})
			g := testMoyasar(srv.URL)

			res, err := g.ChargeSavedPaymentMethod(context.Background(), port.SavedMethodCharge{MethodRef: "token_abc", Amount: 10000, Currency: "SAR", InvoiceID: "inv-1", IdempotencyKey: "renewal-inv-1"})
			if err != nil {
				t.Fatalf("a rejected card is a result, not an error: %v", err)
			}
			if res.Success || !res.Permanent || res.ErrorCode != "invalid_payment_method" {
				t.Fatalf("res = %+v, want a permanent invalid_payment_method failure", res)
			}
			if !strings.Contains(res.ErrorMsg, "moyasar /payments") {
				t.Fatalf("ErrorMsg = %q, want Moyasar's answer for diagnosis", res.ErrorMsg)
			}
		})
	}
}

// A 4xx caused by our own request or configuration (a callback_url built from
// a bad DASHBOARD_URL, an amount below the minimum, an account not activated
// for live) says nothing about the card. Reported as a hard decline it would
// hand every SAR invoice to email dunning as "card declined"; it is an error,
// which the retry worker backs off and escalates.
func TestMoyasarChargeRequestRejectedIsAnErrorNotADecline(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"callback url invalid", http.StatusBadRequest,
			`{"type":"invalid_request_error","message":"Validation Failed","errors":{"callback_url":["must be a valid URL"]}}`},
		{"amount below minimum", http.StatusBadRequest,
			`{"type":"invalid_request_error","message":"Validation Failed","errors":{"amount":["must be greater than 100"]}}`},
		{"not activated for live", http.StatusMethodNotAllowed, `{"type":"account_inactive_error","message":"Not activated","errors":null}`},
		{"unexpected not found", http.StatusNotFound, `{"type":"api_error","message":"Object not found","errors":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := moyasarTestServer(t, map[string]moyasarReply{
				"POST /payments": {tc.status, tc.body},
			})
			g := testMoyasar(srv.URL)

			res, err := g.ChargeSavedPaymentMethod(context.Background(), port.SavedMethodCharge{MethodRef: "token_abc", Amount: 10000, Currency: "SAR", InvoiceID: "inv-1", IdempotencyKey: "renewal-inv-1"})
			if err == nil || !strings.Contains(err.Error(), "moyasar /payments") {
				t.Fatalf("err = %v, want Moyasar's rejection as an error", err)
			}
			if res == nil || res.Success || res.Permanent {
				t.Fatalf("res = %+v, want a non-permanent failure", res)
			}
		})
	}
}

// Throttling is transient and says nothing about the card: keep it an error
// so the worker retries the same attempt shortly.
func TestMoyasarChargeRateLimitedIsError(t *testing.T) {
	srv, _ := moyasarTestServer(t, map[string]moyasarReply{
		"POST /payments": {http.StatusTooManyRequests, `{"type":"rate_limit_error","message":"Too many requests","errors":null}`},
	})
	g := testMoyasar(srv.URL)

	if _, err := g.ChargeSavedPaymentMethod(context.Background(), port.SavedMethodCharge{MethodRef: "token_abc", Amount: 10000, Currency: "SAR", InvoiceID: "inv-1", IdempotencyKey: "renewal-inv-1"}); err == nil {
		t.Fatal("429 must be an error")
	}
}

func TestMoyasarChargeUnauthorizedNeverLeaksSecret(t *testing.T) {
	srv, _ := moyasarTestServer(t, map[string]moyasarReply{
		"POST /payments": {http.StatusUnauthorized,
			`{"type":"authentication_error","message":"Invalid authorization credentials","errors":null}`},
	})
	g := testMoyasar(srv.URL)

	res, err := g.ChargeSavedPaymentMethod(context.Background(), port.SavedMethodCharge{MethodRef: "token_abc", Amount: 10000, Currency: "SAR", InvoiceID: "inv-1", IdempotencyKey: "renewal-inv-1"})
	if err == nil {
		t.Fatal("401 must be an error")
	}
	if res == nil || res.ErrorCode != "gateway_error" || res.Success {
		t.Fatalf("res = %+v, want gateway_error", res)
	}
	basic := base64.StdEncoding.EncodeToString([]byte(moyasarTestSecret + ":"))
	for _, s := range []string{err.Error(), res.ErrorMsg} {
		if strings.Contains(s, moyasarTestSecret) || strings.Contains(s, basic) {
			t.Fatalf("error %q leaks the secret key", s)
		}
	}
	if !strings.Contains(err.Error(), "authentication_error") {
		t.Fatalf("error = %q, want the Moyasar error type for diagnosis", err)
	}
}

func TestMoyasarChargeHTTPErrorWithoutBody(t *testing.T) {
	srv, _ := moyasarTestServer(t, map[string]moyasarReply{
		"POST /payments": {http.StatusBadGateway, ``},
	})
	g := testMoyasar(srv.URL)

	_, err := g.ChargeSavedPaymentMethod(context.Background(), port.SavedMethodCharge{MethodRef: "token_abc", Amount: 10000, Currency: "SAR", InvoiceID: "inv-1", IdempotencyKey: "renewal-inv-1"})
	if err == nil || err.Error() != "moyasar /payments: HTTP 502" {
		t.Fatalf("err = %v, want status fallback when the body is not Moyasar's error shape", err)
	}
}

func TestMoyasarChargeTransportError(t *testing.T) {
	srv, _ := moyasarTestServer(t, nil)
	g := testMoyasar(srv.URL)
	srv.Close() // connection refused

	res, err := g.ChargeSavedPaymentMethod(context.Background(), port.SavedMethodCharge{MethodRef: "token_abc", Amount: 10000, Currency: "SAR", InvoiceID: "inv-1", IdempotencyKey: "renewal-inv-1"})
	if err == nil {
		t.Fatal("transport failure must be an error")
	}
	if res == nil || res.Success || res.ErrorCode != "gateway_error" || res.Permanent {
		t.Fatalf("res = %+v, want retryable gateway_error", res)
	}
}

func TestMoyasarChargeMalformedResponse(t *testing.T) {
	srv, _ := moyasarTestServer(t, map[string]moyasarReply{
		"POST /payments": {http.StatusCreated, `not json`},
	})
	g := testMoyasar(srv.URL)

	res, err := g.ChargeSavedPaymentMethod(context.Background(), port.SavedMethodCharge{MethodRef: "token_abc", Amount: 10000, Currency: "SAR", InvoiceID: "inv-1", IdempotencyKey: "renewal-inv-1"})
	if err == nil || res.Success || res.ErrorCode != "gateway_error" {
		t.Fatalf("res = %+v, err = %v; an unreadable answer must not be treated as paid", res, err)
	}
}

func TestMoyasarChargeRefusesBlindCharges(t *testing.T) {
	cases := []struct {
		name, token, key string
		amount           int64
	}{
		{"empty idempotency key", "token_abc", "", 10000},
		{"empty token", "", "renewal-inv-1", 10000},
		{"zero amount", "token_abc", "renewal-inv-1", 0},
		{"negative amount", "token_abc", "renewal-inv-1", -5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, rec := moyasarTestServer(t, nil)
			g := testMoyasar(srv.URL)

			res, err := g.ChargeSavedPaymentMethod(context.Background(), port.SavedMethodCharge{MethodRef: tc.token, Amount: tc.amount, Currency: "SAR", InvoiceID: "inv-1", IdempotencyKey: tc.key})
			if err == nil {
				t.Fatal("invalid charge input must be refused with an error")
			}
			if res == nil || res.Success {
				t.Fatalf("res = %+v, must not report success", res)
			}
			if n := len(rec.all()); n != 0 {
				t.Fatalf("made %d HTTP calls, want 0: never charge without a key, token and amount", n)
			}
		})
	}
}

// --- FetchPayment ---

func TestMoyasarFetchPaymentMapping(t *testing.T) {
	srv, rec := moyasarTestServer(t, map[string]moyasarReply{
		"GET /payments/" + moyasarPaymentID: {http.StatusOK, `{"id":"` + moyasarPaymentID + `","status":"paid",
			"amount":60000,"refunded":2000,"currency":"sar","metadata":{"invoice_id":"inv-7","other":"x"},
			"source":{"type":"creditcard","company":"visa","name":"Saleh Ali","number":"XXXX-XXXX-XXXX-1111",
			"token":"token_x6okRgkZ","message":"APPROVED","response_code":"00"}}`},
	})
	g := testMoyasar(srv.URL)

	p, err := g.FetchPayment(context.Background(), moyasarPaymentID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := port.GatewayPayment{
		ID: moyasarPaymentID, Status: "paid", Amount: 60000, RefundedAmount: 2000,
		Currency: "SAR", InvoiceID: "inv-7", CardToken: "token_x6okRgkZ",
		CardBrand: "visa", CardLast4: "1111", Message: "APPROVED",
	}
	if *p != want {
		t.Fatalf("payment = %+v, want %+v", *p, want)
	}
	if c := rec.all()[0]; c.method != http.MethodGet || c.path != "/payments/"+moyasarPaymentID {
		t.Fatalf("request = %s %s, want GET /payments/{id}", c.method, c.path)
	}
}

func TestMoyasarFetchPaymentEscapesID(t *testing.T) {
	srv, rec := moyasarTestServer(t, nil)
	g := testMoyasar(srv.URL)

	_, _ = g.FetchPayment(context.Background(), "../refund")
	if c := rec.all()[0]; c.rawPath != "/payments/..%2Frefund" || c.method != http.MethodGet {
		t.Fatalf("request = %s %s, want the id path-escaped into one segment", c.method, c.rawPath)
	}
}

func TestMoyasarLastFourDigits(t *testing.T) {
	for in, want := range map[string]string{
		"XXXX-XXXX-XXXX-1111": "1111",
		"420132******1010":    "1010",
		"12":                  "",
		"":                    "",
	} {
		if got := lastFourDigits(in); got != want {
			t.Fatalf("lastFourDigits(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMoyasarFetchPaymentRejectsEmptyID(t *testing.T) {
	srv, rec := moyasarTestServer(t, nil)
	g := testMoyasar(srv.URL)

	if _, err := g.FetchPayment(context.Background(), ""); err == nil {
		t.Fatal("empty id must be rejected (GET /payments/ would list payments)")
	}
	if len(rec.all()) != 0 {
		t.Fatal("empty id must not reach the network")
	}
}

func TestMoyasarFetchPaymentNotFound(t *testing.T) {
	srv, _ := moyasarTestServer(t, nil)
	g := testMoyasar(srv.URL)

	_, err := g.FetchPayment(context.Background(), moyasarPaymentID)
	if err == nil || !strings.Contains(err.Error(), "Object not found (api_error)") {
		t.Fatalf("err = %v, want Moyasar's not-found message", err)
	}
}

// VerifyPayment can't bind amount or currency, so it must never accept a
// payment; settlement reads payments back with FetchPayment instead.
func TestMoyasarVerifyPaymentNotSupported(t *testing.T) {
	srv, rec := moyasarTestServer(t, map[string]moyasarReply{
		"GET /payments/paid-1": {http.StatusOK, moyasarPaymentJSON("paid-1", "paid", "00", "")},
	})
	g := testMoyasar(srv.URL)

	if err := g.VerifyPayment(context.Background(), "inv-1", "paid-1", ""); err == nil {
		t.Fatal("VerifyPayment must refuse")
	}
	if len(rec.all()) != 0 {
		t.Fatal("VerifyPayment must not call Moyasar")
	}
}

// --- Refund ---

// moyasarRefundJSON is the payment object Moyasar returns for a fetch or a
// refund, with refunded set.
func moyasarRefundJSON(refunded int64) string {
	return fmt.Sprintf(`{"id":%q,"status":"paid","amount":10000,"refunded":%d,"currency":"SAR"}`, moyasarPaymentID, refunded)
}

// refundServer answers GET /payments/{id} with the refunded total, which the
// refund POST raises by its amount when applied; refundReply decides the
// POST's answer (hangUp closes the connection, as a timeout would).
type refundServer struct {
	mu       sync.Mutex
	refunded int64
	posts    int
	gets     int
	getFails bool
	// failGetsAfter > 0 fails every GET after that many succeeded.
	failGetsAfter int
	apply         bool
	reply         int
	hangUp        bool
}

func (s *refundServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/payments/"+moyasarPaymentID:
			s.gets++
			if s.getFails || (s.failGetsAfter > 0 && s.gets > s.failGetsAfter) {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_, _ = w.Write([]byte(moyasarRefundJSON(s.refunded)))
		case r.Method == http.MethodPost && r.URL.Path == "/payments/"+moyasarPaymentID+"/refund":
			s.posts++
			var body struct {
				Amount int64 `json:"amount"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if s.apply {
				s.refunded += body.Amount
			}
			if s.hangUp {
				conn, _, _ := w.(http.Hijacker).Hijack()
				_ = conn.Close()
				return
			}
			w.WriteHeader(s.reply)
			if s.reply == http.StatusOK {
				_, _ = w.Write([]byte(moyasarRefundJSON(s.refunded)))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestMoyasarRefund(t *testing.T) {
	srv, rec := moyasarTestServer(t, map[string]moyasarReply{
		"GET /payments/" + moyasarPaymentID:              {http.StatusOK, moyasarRefundJSON(1000)},
		"POST /payments/" + moyasarPaymentID + "/refund": {http.StatusOK, moyasarRefundJSON(3500)},
	})
	g := testMoyasar(srv.URL)

	res, err := g.Refund(context.Background(), moyasarPaymentID, 2500, "SAR")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Moyasar refunds have no id of their own; payment id + refunded total
	// after this refund is unique per partial refund of the payment.
	if res.RefundID != moyasarPaymentID+":3500" || res.Status != "refunded" {
		t.Fatalf("res = %+v, want RefundID %s:3500, Status refunded", res, moyasarPaymentID)
	}
	posts := rec.to(http.MethodPost, "/payments/"+moyasarPaymentID+"/refund")
	if len(posts) != 1 {
		t.Fatalf("refund POSTs = %d, want exactly 1 (no refund idempotency key: never retry)", len(posts))
	}
	if b := posts[0].body; b["amount"].(float64) != 2500 || len(b) != 1 {
		t.Fatalf("refund body = %v, want exactly {amount: 2500}", b)
	}
}

func TestMoyasarRefundFailureMakesOneCall(t *testing.T) {
	s := &refundServer{reply: http.StatusInternalServerError}
	g := testMoyasar(s.start(t).URL)

	res, err := g.Refund(context.Background(), moyasarPaymentID, 2500, "SAR")
	if err != nil || res.Status != "pending" {
		t.Fatalf("res = %+v, err = %v; a 500 may still apply the refund, so its outcome is pending", res, err)
	}
	if s.posts != 1 {
		t.Fatalf("refund POSTs = %d, want 1: a blind refund retry could refund twice", s.posts)
	}
}

// A refund whose answer was lost may still have been applied, or still be in
// flight. Reporting it as failed invites a re-issue that refunds the customer
// twice, so the outcome is read from the payment's refunded total; when the
// read-back doesn't show it (not applied yet, or the read-back itself failed)
// the refund is reported pending: it holds the refundable amount and an
// operator confirms it against Moyasar.
func TestMoyasarRefundLostAnswerReadBack(t *testing.T) {
	for _, tc := range []struct {
		name        string
		hangUp      bool
		reply       int
		apply       bool
		readFails   bool
		wantPending bool
	}{
		{"timeout, applied", true, 0, true, false, false},
		{"5xx, applied", false, http.StatusBadGateway, true, false, false},
		{"timeout, not visible yet", true, 0, false, false, true},
		{"5xx, not visible yet", false, http.StatusBadGateway, false, false, true},
		{"timeout, read-back fails", true, 0, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &refundServer{refunded: 1000, hangUp: tc.hangUp, reply: tc.reply, apply: tc.apply}
			srv := s.start(t)
			if tc.readFails {
				s.failGetsAfter = 1
			}
			g := testMoyasar(srv.URL)

			res, err := g.Refund(context.Background(), moyasarPaymentID, 2500, "SAR")
			if err != nil {
				t.Fatalf("err = %v; a lost refund answer must never read as failed", err)
			}
			if tc.wantPending {
				if res.Status != "pending" || !strings.HasPrefix(res.RefundID, moyasarPaymentID+":") || res.RefundID == moyasarPaymentID+":3500" {
					t.Fatalf("res = %+v, want pending with an id of its own", res)
				}
			} else if res.RefundID != moyasarPaymentID+":3500" || res.Status != "refunded" {
				t.Fatalf("res = %+v; want the applied refund", res)
			}
			if s.posts != 1 || s.gets != 2 {
				t.Fatalf("POSTs=%d GETs=%d, want 1 refund, a baseline and one read-back", s.posts, s.gets)
			}
		})
	}
}

// The read-back after a lost answer must run even when the caller's request
// has been cancelled meanwhile; otherwise an applied refund reads as unknown.
func TestMoyasarRefundReadBackSurvivesCallerCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	refunded := int64(1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = io.Copy(io.Discard, r.Body)
			mu.Lock()
			refunded += 2500
			mu.Unlock()
			cancel() // the caller gives up while the refund is applied
			<-r.Context().Done()
			return
		}
		mu.Lock()
		cur := refunded
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(moyasarRefundJSON(cur)))
	}))
	defer srv.Close()

	res, err := testMoyasar(srv.URL).Refund(ctx, moyasarPaymentID, 2500, "SAR")
	if err != nil || res.RefundID != moyasarPaymentID+":3500" || res.Status != "refunded" {
		t.Fatalf("res=%+v err=%v; a refund applied while the caller cancelled must read back as refunded", res, err)
	}
}

// Two partial refunds of one payment must not overlap: if one's answer is
// lost while the other applies, its read-back would see the other's amount
// and report a refund that never happened. Here the first refund's answer is
// lost (nothing applied) and the server waits briefly for a second refund to
// arrive meanwhile; run one after the other, exactly one is refunded and the
// lost one stays pending.
func TestMoyasarRefundsOfOnePaymentDoNotOverlap(t *testing.T) {
	var mu sync.Mutex
	refunded, posts := int64(0), 0
	secondPost := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var body struct {
				Amount int64 `json:"amount"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			posts++
			first := posts == 1
			mu.Unlock()
			if first {
				select { // overlapping refunds would arrive here
				case <-secondPost:
				case <-time.After(300 * time.Millisecond):
				}
				conn, _, _ := w.(http.Hijacker).Hijack()
				_ = conn.Close() // answer lost, nothing applied
				return
			}
			close(secondPost)
			mu.Lock()
			refunded += body.Amount
			mu.Unlock()
		}
		mu.Lock()
		cur := refunded
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(moyasarRefundJSON(cur)))
	}))
	defer srv.Close()
	g := testMoyasar(srv.URL)

	var wg sync.WaitGroup
	statuses := make([]string, 2)
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := g.Refund(context.Background(), moyasarPaymentID, 100, "SAR")
			if err != nil {
				t.Errorf("refund %d: %v", i, err)
				return
			}
			statuses[i] = res.Status
		}(i)
	}
	wg.Wait()
	if !(statuses[0] == "refunded" && statuses[1] == "pending") && !(statuses[0] == "pending" && statuses[1] == "refunded") {
		t.Fatalf("statuses = %v, want one refunded and the lost one pending", statuses)
	}
}

// Without a baseline the outcome of a lost answer can't be told, so no
// refund is attempted.
func TestMoyasarRefundBaselineFailureRefundsNothing(t *testing.T) {
	s := &refundServer{getFails: true, reply: http.StatusOK}
	g := testMoyasar(s.start(t).URL)

	if _, err := g.Refund(context.Background(), moyasarPaymentID, 2500, "SAR"); err == nil {
		t.Fatal("want an error when the payment can't be read")
	}
	if s.posts != 0 {
		t.Fatalf("refund POSTs = %d, want 0", s.posts)
	}
}

// Moyasar refusing the refund (e.g. more than is left) is definitive.
func TestMoyasarRefundRejectedNoReadBack(t *testing.T) {
	s := &refundServer{refunded: 9000, reply: http.StatusBadRequest}
	g := testMoyasar(s.start(t).URL)

	if _, err := g.Refund(context.Background(), moyasarPaymentID, 2500, "SAR"); err == nil {
		t.Fatal("a rejected refund must be an error")
	}
	if s.posts != 1 || s.gets != 1 {
		t.Fatalf("POSTs=%d GETs=%d, want the baseline and one refund only", s.posts, s.gets)
	}
}

func TestMoyasarRefundRejectsInvalidInput(t *testing.T) {
	srv, rec := moyasarTestServer(t, nil)
	g := testMoyasar(srv.URL)
	ctx := context.Background()

	if _, err := g.Refund(ctx, "", 100, "SAR"); err == nil {
		t.Fatal("empty payment id must be rejected")
	}
	if _, err := g.Refund(ctx, moyasarPaymentID, 0, "SAR"); err == nil {
		t.Fatal("zero amount must be rejected (Moyasar would refund in full)")
	}
	if len(rec.all()) != 0 {
		t.Fatal("invalid refunds must not reach the network")
	}
}

// --- No-HTTP methods ---

func TestMoyasarCreateOrderNoHTTP(t *testing.T) {
	srv, rec := moyasarTestServer(t, nil)
	g := testMoyasar(srv.URL)

	order, err := g.CreateOrder(context.Background(), 10000, "SAR", "rcpt-1", "inv-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := port.PaymentOrder{ID: "inv-1", Amount: 10000, Currency: "SAR", Receipt: "rcpt-1", Gateway: "moyasar"}
	if *order != want {
		t.Fatalf("order = %+v, want %+v", *order, want)
	}
	if len(rec.all()) != 0 {
		t.Fatal("CreateOrder must not call Moyasar: the embedded form creates the payment")
	}
}

func TestMoyasarRetryPaymentReportsNoSavedMethod(t *testing.T) {
	srv, rec := moyasarTestServer(t, nil)
	g := testMoyasar(srv.URL)

	res, err := g.RetryPayment(context.Background(), "inv-1", 10000, "SAR")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Not a decline: the customer may still save a card or pay the link, so
	// dunning must keep emailing rather than stop.
	if res.Success || res.Permanent || res.ErrorCode != port.ErrCodeNoSavedMethod {
		t.Fatalf("res = %+v, want a non-permanent %s failure", res, port.ErrCodeNoSavedMethod)
	}
	if len(rec.all()) != 0 {
		t.Fatal("RetryPayment must not call Moyasar")
	}
}

func TestMoyasarCreateSubscriptionNeverReturnsID(t *testing.T) {
	g := testMoyasar("http://unused")
	id, err := g.CreateSubscription(context.Background(), "plan", 12, "c@example.com", nil, "SAR")
	if err == nil {
		t.Fatal("gateway-managed subscriptions must be not-supported")
	}
	if id != "" {
		t.Fatalf("id = %q: a non-empty id makes the renewal claim skip the subscription", id)
	}
}

func TestMoyasarUnsupportedSurfaces(t *testing.T) {
	g := testMoyasar("http://unused")
	ctx := context.Background()
	if err := g.CancelSubscription(ctx, "sub"); err != nil {
		t.Fatalf("CancelSubscription must be a no-op, got %v", err)
	}
	if _, err := g.CreateMandate(ctx, "e", "c", "v", 1, "monthly", "SAR"); err == nil {
		t.Fatal("CreateMandate must be not-supported")
	}
	if _, err := g.ExecuteMandateDebit(ctx, port.MandateDebitRequest{}); err == nil {
		t.Fatal("ExecuteMandateDebit must be not-supported")
	}
	if err := g.RevokeMandate(ctx, "c", "t", "SAR"); err == nil {
		t.Fatal("RevokeMandate must be not-supported")
	}
	if _, err := g.CreateVirtualAccount(ctx, "c", "i", 1, "d"); err == nil {
		t.Fatal("CreateVirtualAccount must be not-supported")
	}
}

// --- Tokens ---

func TestMoyasarFetchTokenExpiry(t *testing.T) {
	for _, tc := range []struct {
		name, year string
		wantYear   int
	}{
		{"four-digit year", "2029", 2029},
		{"two-digit year", "29", 2029},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, rec := moyasarTestServer(t, map[string]moyasarReply{
				"GET /tokens/token_abc": {http.StatusOK, `{"id":"token_abc","status":"active","brand":"mada","funding":"debit",
					"country":"SA","month":7,"year":` + tc.year + `,"name":"Saleh Ali","last_four":"1010"}`},
			})
			g := testMoyasar(srv.URL)

			month, year, err := g.FetchToken(context.Background(), "token_abc")
			if err != nil || month != 7 || year != tc.wantYear {
				t.Fatalf("FetchToken = %d/%d, %v; want 7/%d", month, year, err, tc.wantYear)
			}
			if c := rec.all()[0]; c.method != http.MethodGet || c.auth == "" {
				t.Fatalf("request = %+v, want an authenticated GET", c)
			}
		})
	}
}

func TestMoyasarFetchTokenErrors(t *testing.T) {
	srv, rec := moyasarTestServer(t, nil)
	g := testMoyasar(srv.URL)

	if _, _, err := g.FetchToken(context.Background(), ""); err == nil || len(rec.all()) != 0 {
		t.Fatalf("empty id: err = %v, calls = %d; want an error and no request", err, len(rec.all()))
	}
	if _, _, err := g.FetchToken(context.Background(), "token_gone"); err == nil {
		t.Fatal("an unknown token must be an error")
	}
	if _, _, err := g.FetchToken(context.Background(), "../payments"); err == nil || rec.all()[1].rawPath != "/tokens/..%2Fpayments" {
		t.Fatalf("token id must be path-escaped, got %q", rec.all()[1].rawPath)
	}
}
