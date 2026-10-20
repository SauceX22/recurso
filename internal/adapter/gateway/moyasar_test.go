package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/recurso-dev/recurso/internal/core/port"
)

// --- Test harness: record every request, answer per "METHOD path" ---

type moyasarCall struct {
	method, path, rawPath, auth, contentType string
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

// moyasarTestServer serves canned replies keyed by "METHOD /path"; an
// unrouted request gets a 404 so a stray call fails the test loudly.
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
		}
		_ = json.NewDecoder(r.Body).Decode(&c.body)
		rec.mu.Lock()
		rec.calls = append(rec.calls, c)
		rec.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		reply, ok := routes[r.Method+" "+r.URL.Path]
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
	g := NewMoyasarGateway(moyasarTestSecret, "https://app.recurso.dev/")
	g.baseURL = srvURL
	return g
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
	g := NewMoyasarGateway("sk_live_y", "https://app.recurso.dev//")
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

	calls := rec.all()
	if len(calls) != 1 || calls[0].method != http.MethodPost || calls[0].path != "/payments" {
		t.Fatalf("calls = %+v, want exactly one POST /payments", calls)
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
	if b["description"] != "Recurso invoice inv-1" {
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
	calls := rec.all()
	if calls[0].body["given_id"] != calls[1].body["given_id"] {
		t.Fatal("replaying the same key must send the same given_id")
	}
	if calls[0].body["given_id"] == calls[2].body["given_id"] {
		t.Fatal("a different key must send a different given_id")
	}
}

func TestMoyasarChargeDeclineMapping(t *testing.T) {
	cases := []struct {
		name, status, code, message string
		wantCode                    string
		wantPermanent               bool
	}{
		{"soft decline insufficient funds", "failed", "51", "INSUFFICIENT FUNDS", "card_declined_51", false},
		{"soft decline do not honour", "failed", "05", "DO NOT HONOUR", "card_declined_05", false},
		{"hard decline stolen card", "failed", "43", "STOLEN CARD", "card_declined_43", true},
		{"hard decline expired card", "failed", "54", "EXPIRED CARD", "card_declined_54", true},
		{"null response code", "failed", "null", "3-D Secure failed", "card_declined", false},
		{"3ds required", "initiated", "null", "", "authentication_required", false},
		{"unexpected status", "authorized", "00", "", "unexpected_status_authorized", false},
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
			if tc.status == "failed" && res.ErrorMsg != tc.message {
				t.Fatalf("ErrorMsg = %q, want source.message %q", res.ErrorMsg, tc.message)
			}
			if res.PaymentID != moyasarPaymentID {
				t.Fatalf("PaymentID = %q, want the declined payment's id for traceability", res.PaymentID)
			}
		})
	}
}

func TestMoyasarHardDeclineSetIsExactlyTheSpec(t *testing.T) {
	spec := strings.Fields("04 07 14 15 33 34 35 36 37 38 41 43 54 56 57 59 62 63 67 79 93")
	if len(moyasarHardDeclines) != len(spec) {
		t.Fatalf("hard set has %d codes, spec lists %d", len(moyasarHardDeclines), len(spec))
	}
	for _, c := range spec {
		if !moyasarHardDeclines[c] {
			t.Fatalf("code %s is hard in the spec but missing from the adapter", c)
		}
	}
	for _, soft := range []string{"00", "05", "51", "91"} {
		if moyasarHardDeclines[soft] {
			t.Fatalf("code %s must stay soft (retryable)", soft)
		}
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

func TestMoyasarChargeAlreadyCreatedFetchFails(t *testing.T) {
	srv, rec := moyasarTestServer(t, map[string]moyasarReply{
		"POST /payments": {http.StatusBadRequest,
			`{"type":"invalid_request_error","message":"Payment is already created.","errors":null}`},
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

func TestMoyasarChargeOtherBadRequestIsGatewayError(t *testing.T) {
	srv, rec := moyasarTestServer(t, map[string]moyasarReply{
		"POST /payments": {http.StatusBadRequest,
			`{"type":"invalid_request_error","message":"Validation Failed","errors":{"amount":["must be greater than 100"]}}`},
	})
	g := testMoyasar(srv.URL)

	res, err := g.ChargeSavedPaymentMethod(context.Background(), port.SavedMethodCharge{MethodRef: "token_abc", Amount: 50, Currency: "SAR", InvoiceID: "inv-1", IdempotencyKey: "renewal-inv-1"})
	if err == nil || res.ErrorCode != "gateway_error" || res.Success {
		t.Fatalf("res = %+v, err = %v; want gateway_error", res, err)
	}
	if want := "moyasar /payments: Validation Failed (invalid_request_error)"; err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
	if len(rec.all()) != 1 {
		t.Fatal("a non-idempotency 400 must not trigger a read-back")
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
	if err == nil || res.ErrorCode != "gateway_error" {
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

// --- FetchPayment / VerifyPayment ---

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

func TestMoyasarVerifyPayment(t *testing.T) {
	srv, _ := moyasarTestServer(t, map[string]moyasarReply{
		"GET /payments/paid-1":    {http.StatusOK, moyasarPaymentJSON("paid-1", "paid", "00", "")},
		"GET /payments/failed-1":  {http.StatusOK, moyasarPaymentJSON("failed-1", "failed", "51", "")},
		"GET /payments/missing-1": {http.StatusNotFound, `{"type":"api_error","message":"Object not found"}`},
	})
	g := testMoyasar(srv.URL)
	ctx := context.Background()

	if err := g.VerifyPayment(ctx, "inv-1", "paid-1", ""); err != nil {
		t.Fatalf("paid payment bound to inv-1 must verify, got %v", err)
	}
	if err := g.VerifyPayment(ctx, "inv-OTHER", "paid-1", ""); err == nil {
		t.Fatal("a paid payment for a different invoice must not settle this one")
	}
	if err := g.VerifyPayment(ctx, "inv-1", "failed-1", ""); err == nil {
		t.Fatal("an unpaid payment must not verify")
	}
	if err := g.VerifyPayment(ctx, "inv-1", "missing-1", ""); err == nil {
		t.Fatal("an unknown payment must not verify")
	}
}

// --- Refund ---

func TestMoyasarRefund(t *testing.T) {
	srv, rec := moyasarTestServer(t, map[string]moyasarReply{
		"POST /payments/" + moyasarPaymentID + "/refund": {http.StatusOK,
			`{"id":"` + moyasarPaymentID + `","status":"refunded","amount":10000,"refunded":2500,"currency":"SAR"}`},
	})
	g := testMoyasar(srv.URL)

	res, err := g.Refund(context.Background(), moyasarPaymentID, 2500, "SAR")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RefundID != moyasarPaymentID || res.Status != "refunded" {
		t.Fatalf("res = %+v, want RefundID = payment id, Status refunded", res)
	}
	calls := rec.all()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want exactly 1 (no refund idempotency key: never retry)", len(calls))
	}
	if b := calls[0].body; b["amount"].(float64) != 2500 || len(b) != 1 {
		t.Fatalf("refund body = %v, want exactly {amount: 2500}", b)
	}
}

func TestMoyasarRefundFailureMakesOneCall(t *testing.T) {
	srv, rec := moyasarTestServer(t, map[string]moyasarReply{
		"POST /payments/" + moyasarPaymentID + "/refund": {http.StatusInternalServerError, ``},
	})
	g := testMoyasar(srv.URL)

	if _, err := g.Refund(context.Background(), moyasarPaymentID, 2500, "SAR"); err == nil {
		t.Fatal("a 500 must be an error")
	}
	if n := len(rec.all()); n != 1 {
		t.Fatalf("calls = %d, want 1: a blind refund retry could refund twice", n)
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

func TestMoyasarRetryPaymentIsPermanent(t *testing.T) {
	srv, rec := moyasarTestServer(t, nil)
	g := testMoyasar(srv.URL)

	res, err := g.RetryPayment(context.Background(), "inv-1", 10000, "SAR")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Success || !res.Permanent || res.ErrorCode != "no_saved_payment_method" {
		t.Fatalf("res = %+v, want permanent no_saved_payment_method so dunning stops retrying", res)
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

func TestMoyasarOwnsPaymentID(t *testing.T) {
	g := testMoyasar("http://unused")
	if !g.OwnsPaymentID(moyasarPaymentID) {
		t.Fatal("a bare UUID is a Moyasar payment id")
	}
	for _, id := range []string{"pi_123", "order_123", "pay_123", ""} {
		if g.OwnsPaymentID(id) {
			t.Fatalf("OwnsPaymentID(%q) = true; must not claim other gateways' ids", id)
		}
	}
}
