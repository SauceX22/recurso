#!/bin/bash
# -o pipefail: a failing command inside a pipeline (curl | jq) must fail the
# step, not be masked by the last stage succeeding.
set -eo pipefail

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
NC='\033[0m'

API_URL="${API_URL:-http://localhost:8080}"

echo "Starting End-to-End Verification..."

# Helper function for JSON extraction using jq (more robust)
get_json_value() {
  echo "$1" | jq -r ".$2"
}

# post_json <path> <json-body> [api-key]
# Sends a JSON POST without ever placing the body or the API key on the curl
# argv (which would leak them via `ps aux`). The body is piped over stdin with
# `--data @-`; the Authorization header (when an api-key is given) is passed
# through a `curl -K` config file supplied on stdin-adjacent fd via process
# substitution, so the bearer token never appears in the process list.
post_json() {
  local path="$1" body="$2" api_key="${3:-}"
  if [ -n "$api_key" ]; then
    curl -s -X POST "$path" \
      -H "Content-Type: application/json" \
      -K <(printf 'header = "Authorization: Bearer %s"\n' "$api_key") \
      --data @- <<<"$body"
  else
    curl -s -X POST "$path" \
      -H "Content-Type: application/json" \
      --data @- <<<"$body"
  fi
}

# get_json <path> <api-key> — authenticated GET, key kept off argv.
get_json() {
  local path="$1" api_key="$2"
  curl -s "$path" \
    -K <(printf 'header = "Authorization: Bearer %s"\n' "$api_key")
}

# put_json <path> <json-body> <api-key>
put_json() {
  local path="$1" body="$2" api_key="$3"
  curl -s -X PUT "$path" \
    -H "Content-Type: application/json" \
    -K <(printf 'header = "Authorization: Bearer %s"\n' "$api_key") \
    --data @- <<<"$body"
}

# 1. Setup Tenant
TIMESTAMP=$(date +%s)
EMAIL="admin_e2e_${TIMESTAMP}@example.com"
# Read the registration password from the environment so it never sits on an
# argv. Falls back to the historical test value when E2E_PASSWORD is unset.
E2E_PASSWORD="${E2E_PASSWORD:-password123}"
echo "Step 1: Creating Tenant ($EMAIL)..."
TENANT_RES=$(post_json "$API_URL/auth/register" "$(jq -n \
  --arg email "$EMAIL" \
  --arg password "$E2E_PASSWORD" \
  '{name: "E2E Tech Inc", email: $email, password: $password, company_name: "E2E Corp"}')")

if echo "$TENANT_RES" | grep -q "api_key"; then
  API_KEY=$(get_json_value "$TENANT_RES" "api_key")
  TENANT_ID=$(get_json_value "$TENANT_RES" "tenant.id")
  echo -e "${GREEN}✅ Tenant Created (ID: $TENANT_ID)${NC}"
else
  echo -e "${RED}❌ Failed to create tenant${NC}"
  echo "Response: $TENANT_RES"
  exit 1
fi

# 2. Test INR Flow (Razorpay + GST)
echo "Step 2: Testing INR/Razorpay Flow (India Stack)..."

# Create INR Customer
CUST_INR_RES=$(post_json "$API_URL/v1/customers" '{
    "name": "Ramesh Gupta",
    "email": "ramesh.gupta@example.com",
    "billing_address": {
      "country": "IN",
      "state": "MH",
      "postal_code": "400001"
    },
    "tax_id": "27DQBPS8356J1Z1"
  }' "$API_KEY")
CUST_ID_INR=$(get_json_value "$CUST_INR_RES" "data.id")
echo "  > Customer Created: $CUST_ID_INR"

# Create INR Plan
PLAN_INR_RES=$(post_json "$API_URL/v1/plans" '{
    "code": "BASIC-INR",
    "name": "Basic Plan INR",
    "interval_unit": "month",
    "interval_count": 1,
    "amount": 50000,
    "currency": "INR"
  }' "$API_KEY")
PLAN_ID_INR=$(get_json_value "$PLAN_INR_RES" "data.id")
echo "  > Plan Created: $PLAN_ID_INR"

# Create INR Subscription
SUB_INR_RES=$(post_json "$API_URL/v1/subscriptions" "$(jq -n \
  --arg customer_id "$CUST_ID_INR" \
  --arg plan_id "$PLAN_ID_INR" \
  '{customer_id: $customer_id, plan_id: $plan_id, start_date: "2025-01-01T00:00:00Z"}')" \
  "$API_KEY")
SUB_ID_INR=$(get_json_value "$SUB_INR_RES" "data.id")
RAZORPAY_ID=$(get_json_value "$SUB_INR_RES" "data.razorpay_subscription_id")

if [[ "$RAZORPAY_ID" == sub_* ]]; then
    echo -e "${GREEN}  ✅ INR Subscription routing verified (Razorpay ID: $RAZORPAY_ID)${NC}"
else
    echo -e "${RED}  ❌ INR Subscription failed to populate Razorpay ID${NC}"
    echo "Response: $SUB_INR_RES"
    exit 1
fi


# 3. Test USD Flow (Stripe + Global)
echo "Step 3: Testing USD/Stripe Flow (Global)..."

# Create USD Customer
CUST_USD_RES=$(post_json "$API_URL/v1/customers" '{
    "name": "Jane User",
    "email": "jane@example.com",
    "billing_address": {
      "country": "US",
      "state": "CA",
      "postal_code": "90210"
    }
  }' "$API_KEY")
CUST_ID_USD=$(get_json_value "$CUST_USD_RES" "data.id")
echo "  > Customer Created: $CUST_ID_USD"

# Create USD Plan
PLAN_USD_RES=$(post_json "$API_URL/v1/plans" '{
    "code": "PRO-USD",
    "name": "Pro Plan USD",
    "interval_unit": "month",
    "interval_count": 1,
    "amount": 2900,
    "currency": "USD"
  }' "$API_KEY")
PLAN_ID_USD=$(get_json_value "$PLAN_USD_RES" "data.id")
echo "  > Plan Created: $PLAN_ID_USD"

# Create USD Subscription
SUB_USD_RES=$(post_json "$API_URL/v1/subscriptions" "$(jq -n \
  --arg customer_id "$CUST_ID_USD" \
  --arg plan_id "$PLAN_ID_USD" \
  '{customer_id: $customer_id, plan_id: $plan_id, start_date: "2025-01-01T00:00:00Z"}')" \
  "$API_KEY")
SUB_ID_USD=$(get_json_value "$SUB_USD_RES" "data.id")
STRIPE_ID=$(get_json_value "$SUB_USD_RES" "data.stripe_subscription_id")

if [[ "$STRIPE_ID" == sub_* ]]; then
    echo -e "${GREEN}  ✅ USD Subscription routing verified (Stripe ID: $STRIPE_ID)${NC}"
else
    echo -e "${RED}  ❌ USD Subscription failed to populate Stripe ID${NC}"
    echo "Response: $SUB_USD_RES"
    exit 1
fi

# 4. Coupon correctness: a 20% coupon must discount the first invoice by 20%,
# not by 20 minor units (the percent/amount enum bug class).
echo "Step 4: Testing coupon math (percent, not minor units)..."
COUPON_RES=$(post_json "$API_URL/v1/coupons" '{
    "code": "E2E-20OFF",
    "discount_type": "percent",
    "discount_value": 20,
    "duration": "once"
  }' "$API_KEY")

CUST_C_RES=$(post_json "$API_URL/v1/customers" '{
    "name": "Coupon Carl",
    "email": "carl@example.com",
    "billing_address": {"country": "US", "state": "CA", "postal_code": "90210"}
  }' "$API_KEY")
CUST_ID_C=$(get_json_value "$CUST_C_RES" "data.id")

SUB_C_RES=$(post_json "$API_URL/v1/subscriptions" "$(jq -n \
  --arg customer_id "$CUST_ID_C" \
  --arg plan_id "$PLAN_ID_USD" \
  '{customer_id: $customer_id, plan_id: $plan_id, coupon_code: "E2E-20OFF"}')" \
  "$API_KEY")
SUB_ID_C=$(get_json_value "$SUB_C_RES" "data.id")

# The USD plan is 2900; 20% off => 2320 on the first invoice.
INVOICES_RES=$(get_json "$API_URL/v1/invoices" "$API_KEY")
COUPON_TOTAL=$(echo "$INVOICES_RES" | jq -r --arg sid "$SUB_ID_C" \
  '[.data[] | select(.subscription_id == $sid)][0].total')
if [ "$COUPON_TOTAL" = "2320" ]; then
    echo -e "${GREEN}  ✅ Coupon applied as 20% (invoice total 2320)${NC}"
else
    echo -e "${RED}  ❌ Coupon math wrong: first invoice total = $COUPON_TOTAL, want 2320${NC}"
    echo "Response: $INVOICES_RES"
    exit 1
fi

# 5. Usage ingestion round-trip: an ingested event must be visible on the raw
# event stream endpoint.
echo "Step 5: Testing usage ingest -> raw stream visibility..."
post_json "$API_URL/v1/usage/events" "$(jq -n \
  --arg sub "$SUB_ID_USD" --arg cust "$CUST_ID_USD" \
  '{subscription_id: $sub, customer_id: $cust, dimension: "e2e_api_calls", quantity: 42, transaction_id: "e2e-txn-1"}')" \
  "$API_KEY" > /dev/null

EVENTS_RES=$(get_json "$API_URL/v1/usage/events?dimension=e2e_api_calls" "$API_KEY")
EVENT_QTY=$(echo "$EVENTS_RES" | jq -r '.data[0].quantity')
if [ "$EVENT_QTY" = "42" ]; then
    echo -e "${GREEN}  ✅ Ingested event visible on the raw stream (qty 42)${NC}"
else
    echo -e "${RED}  ❌ Ingested event not visible${NC}"
    echo "Response: $EVENTS_RES"
    exit 1
fi

# 6. Webhook endpoint lifecycle: create -> pause -> verify -> delete.
echo "Step 6: Testing webhook endpoint pause/resume..."
WH_RES=$(post_json "$API_URL/v1/webhooks" '{
    "url": "https://example.com/e2e-hook",
    "events": ["invoice.paid"]
  }' "$API_KEY")
WH_ID=$(get_json_value "$WH_RES" "data.id")

put_json "$API_URL/v1/webhooks/$WH_ID/status" '{"status":"inactive"}' "$API_KEY" > /dev/null
WH_LIST=$(get_json "$API_URL/v1/webhooks" "$API_KEY")
WH_STATUS=$(echo "$WH_LIST" | jq -r --arg id "$WH_ID" '.data[] | select(.id == $id) | .status')
if [ "$WH_STATUS" = "inactive" ]; then
    echo -e "${GREEN}  ✅ Endpoint paused (status inactive)${NC}"
else
    echo -e "${RED}  ❌ Endpoint pause failed (status: $WH_STATUS)${NC}"
    exit 1
fi

# 6b. OAuth callback must be PUBLIC: it arrives as a bare browser redirect
# from the provider (no session, no API key) — the HMAC-signed state is its
# authentication. Regression for the wiring bug where it sat behind auth
# middleware and 401'd every real handoff.
echo "Step 6b: OAuth callback reachable without credentials..."
CB_CODE=$(curl -s -o /dev/null -w "%{http_code}" "$API_URL/v1/accounting/callback/xero?code=fake&state=forged")
if [ "$CB_CODE" = "302" ] || [ "$CB_CODE" = "303" ]; then
    echo -e "${GREEN}  ✅ Callback is public (redirects with $CB_CODE, not 401)${NC}"
else
    echo -e "${RED}  ❌ Callback returned $CB_CODE — must redirect (302), never 401${NC}"
    exit 1
fi

# 6c. GoCardless webhook chain: signature verification must fail closed, and a
# signed billing_requests.fulfilled delivery must activate the mandate and swap
# the stored billing-request id for the gateway mandate id (the id every future
# debit references). Regression for the "mandate stays created forever" gap.
echo "Step 6c: GoCardless webhook chain..."
GC_SECRET="e2e_gc_webhook_secret"

BAD_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$API_URL/webhooks/gocardless"     -H 'Content-Type: application/json' -H 'Webhook-Signature: deadbeef' -d '{"events":[]}')
if [ "$BAD_CODE" = "401" ]; then
    echo -e "${GREEN}  ✅ Bad webhook signature rejected (401)${NC}"
else
    echo -e "${RED}  ❌ Bad signature returned $BAD_CODE, want 401${NC}"
    exit 1
fi

# An EUR mandate needs no VPA/phone (bank debit); the gateway (mock in CI,
# sandbox locally) returns the token the webhook will reference.
MANDATE_RES=$(post_json "$API_URL/v1/mandates" "$(jq -n \
  --arg customer_id "$CUST_ID_USD" \
  '{customer_id: $customer_id, currency: "EUR", max_amount: 5000, frequency: "monthly"}')" \
  "$API_KEY")
MANDATE_ID=$(echo "$MANDATE_RES" | jq -r '.data.mandate.id')
MANDATE_TOKEN=$(echo "$MANDATE_RES" | jq -r '.data.mandate.razorpay_token_id')
if [ -z "$MANDATE_ID" ] || [ "$MANDATE_ID" = "null" ]; then
    echo -e "${RED}  ❌ EUR mandate creation failed${NC}"
    echo "Response: $MANDATE_RES"
    exit 1
fi

GC_BODY=$(jq -nc --arg brq "$MANDATE_TOKEN" \
  '{events: [{id: "EV_E2E_FULFIL", resource_type: "billing_requests", action: "fulfilled", links: {billing_request: $brq, mandate_request_mandate: "MD_E2E_1"}}]}')
GC_SIG=$(printf '%s' "$GC_BODY" | openssl dgst -sha256 -hmac "$GC_SECRET" -hex | sed 's/^.*= //')
GC_RES=$(curl -s -X POST "$API_URL/webhooks/gocardless"     -H 'Content-Type: application/json' -H "Webhook-Signature: $GC_SIG" -d "$GC_BODY")
PROCESSED=$(echo "$GC_RES" | jq -r '.processed')
if [ "$PROCESSED" != "1" ]; then
    echo -e "${RED}  ❌ Fulfilment webhook not processed: $GC_RES${NC}"
    exit 1
fi

MANDATE_AFTER=$(get_json "$API_URL/v1/mandates/$MANDATE_ID" "$API_KEY")
M_STATUS=$(echo "$MANDATE_AFTER" | jq -r '.data.status')
M_TOKEN=$(echo "$MANDATE_AFTER" | jq -r '.data.razorpay_token_id')
if [ "$M_STATUS" = "active" ] && [ "$M_TOKEN" = "MD_E2E_1" ]; then
    echo -e "${GREEN}  ✅ Fulfilment webhook activated mandate and swapped token to MD_E2E_1${NC}"
else
    echo -e "${RED}  ❌ Mandate after webhook: status=$M_STATUS token=$M_TOKEN (want active / MD_E2E_1)${NC}"
    exit 1
fi

# 6d. Moyasar (SAR) end to end against scripts/moyasar_stub.py, which the API
# reaches through the test-only MOYASAR_API_URL (docker-compose.yml). Covers:
# the pay response, server-side verify (in flight, then paid), webhook secret
# enforcement, webhook settlement, a credit-note refund through the gateway,
# and (step 7) a clean ledger afterwards.
echo "Step 6d: Moyasar checkout, webhook and refund (stubbed API)..."
MOYASAR_STUB_PORT="${MOYASAR_STUB_PORT:-8091}"
MOYASAR_STUB="http://localhost:$MOYASAR_STUB_PORT"
MOYASAR_WEBHOOK_SECRET="${MOYASAR_WEBHOOK_SECRET:-e2e_moyasar_webhook_secret}"
python3 -I "$(dirname "$0")/moyasar_stub.py" --port "$MOYASAR_STUB_PORT" &
MOYASAR_STUB_PID=$!
trap 'kill $MOYASAR_STUB_PID 2>/dev/null' EXIT
for _ in $(seq 1 25); do
  curl -s -o /dev/null "$MOYASAR_STUB/v1/payments" && break
  sleep 0.2
done

fail_6d() {
  echo -e "${RED}  ❌ $1${NC}"
  [ -n "${2:-}" ] && echo "Response: $2"
  exit 1
}

CUST_SAR_RES=$(post_json "$API_URL/v1/customers" '{
    "name": "Sara Alharbi",
    "email": "sara.sar@example.com",
    "billing_address": {"country": "SA", "postal_code": "11564"}
  }' "$API_KEY")
CUST_ID_SAR=$(get_json_value "$CUST_SAR_RES" "data.id")
PLAN_SAR_RES=$(post_json "$API_URL/v1/plans" '{
    "code": "PRO-SAR",
    "name": "Pro Plan SAR",
    "interval_unit": "month",
    "interval_count": 1,
    "amount": 11500,
    "currency": "SAR"
  }' "$API_KEY")
PLAN_ID_SAR=$(get_json_value "$PLAN_SAR_RES" "data.id")
SUB_SAR_RES=$(post_json "$API_URL/v1/subscriptions" "$(jq -n \
  --arg customer_id "$CUST_ID_SAR" --arg plan_id "$PLAN_ID_SAR" \
  '{customer_id: $customer_id, plan_id: $plan_id}')" "$API_KEY")
SUB_ID_SAR=$(get_json_value "$SUB_SAR_RES" "data.id")
INV_SAR=$(get_json "$API_URL/v1/invoices" "$API_KEY" | jq -r --arg sid "$SUB_ID_SAR" \
  '[.data[] | select(.subscription_id == $sid)][0].id')
[ -n "$INV_SAR" ] && [ "$INV_SAR" != "null" ] || fail_6d "SAR subscription produced no invoice" "$SUB_SAR_RES"

PAY_RES=$(post_json "$API_URL/checkout/$INV_SAR/pay" '{}')
SAR_DUE=$(echo "$PAY_RES" | jq -r '.data.amount')
if [ "$(echo "$PAY_RES" | jq -r '.data.gateway')" != "moyasar" ] || [ "$(echo "$PAY_RES" | jq -r '.data.save_card')" != "true" ]; then
  fail_6d "SAR /pay did not return the Moyasar form for a renewing subscription" "$PAY_RES"
fi
echo -e "${GREEN}  ✅ SAR /pay returns the Moyasar form (amount $SAR_DUE, save_card)${NC}"

# The buyer's payment, still waiting on 3DS: verify must not settle it.
PAYMENT_ID=$(python3 -c 'import uuid; print(uuid.uuid4())')
stub_payment() {
  curl -s -X PUT "$MOYASAR_STUB/_stub/payments/$PAYMENT_ID" -H 'Content-Type: application/json' \
    --data @- > /dev/null <<<"$(jq -n --arg inv "$INV_SAR" --arg status "$1" --argjson amount "$SAR_DUE" \
      '{status: $status, amount: $amount, currency: "SAR", metadata: {invoice_id: $inv},
        source: {type: "creditcard", company: "visa", number: "4111-11XX-XXXX-1111", token: "token_e2e_sar"}}')"
}
stub_payment initiated
VERIFY_RES=$(post_json "$API_URL/checkout/$INV_SAR/moyasar/verify" "$(jq -n --arg id "$PAYMENT_ID" '{payment_id: $id}')")
[ "$(echo "$VERIFY_RES" | jq -r '.data.status')" = "processing" ] || fail_6d "verify of an initiated payment must report processing" "$VERIFY_RES"
echo -e "${GREEN}  ✅ Verify of an unfinished payment reports processing, settles nothing${NC}"

stub_payment paid
moyasar_webhook() {
  curl -s -o /dev/null -w "%{http_code}" -X POST "$API_URL/webhooks/moyasar" -H 'Content-Type: application/json' \
    --data @- <<<"$(jq -n --arg secret "$1" --arg pid "$PAYMENT_ID" --arg ev "$2" \
      '{id: $ev, type: "payment_paid", secret_token: $secret, live: false, data: {id: $pid}}')"
}
WH_BAD=$(moyasar_webhook "not-the-secret" "ev_e2e_bad_$TIMESTAMP")
[ "$WH_BAD" = "401" ] || fail_6d "Moyasar webhook with a wrong secret returned $WH_BAD, want 401"
WH_OK=$(moyasar_webhook "$MOYASAR_WEBHOOK_SECRET" "ev_e2e_paid_$TIMESTAMP")
[ "$WH_OK" = "200" ] || fail_6d "Moyasar payment_paid webhook returned $WH_OK, want 200"
INV_SAR_RES=$(get_json "$API_URL/v1/invoices/$INV_SAR" "$API_KEY")
[ "$(echo "$INV_SAR_RES" | jq -r '.data.status')" = "paid" ] || fail_6d "invoice not paid after the webhook" "$INV_SAR_RES"
echo -e "${GREEN}  ✅ Wrong webhook secret 401; payment_paid webhook settled the invoice${NC}"

VERIFY_RES=$(post_json "$API_URL/checkout/$INV_SAR/moyasar/verify" "$(jq -n --arg id "$PAYMENT_ID" '{payment_id: $id}')")
[ "$(echo "$VERIFY_RES" | jq -r '.data.status')" = "paid" ] || fail_6d "verify after settlement must report paid" "$VERIFY_RES"

CN_RES=$(post_json "$API_URL/v1/credit-notes" "$(jq -n --arg c "$CUST_ID_SAR" --arg i "$INV_SAR" \
  '{customer_id: $c, invoice_id: $i, amount: 1000, currency: "SAR", type: "refund", reason: "e2e partial refund"}')" "$API_KEY")
CN_REFUND=$(echo "$CN_RES" | jq -r '.data.refund_status')
STUB_REFUNDED=$(curl -s -u sk_test_e2e: "$MOYASAR_STUB/v1/payments/$PAYMENT_ID" | jq -r '.refunded')
if [ "$CN_REFUND" != "processed" ] || [ "$STUB_REFUNDED" != "1000" ]; then
  fail_6d "credit-note refund: refund_status=$CN_REFUND, Moyasar refunded=$STUB_REFUNDED (want processed / 1000)" "$CN_RES"
fi
echo -e "${GREEN}  ✅ Credit-note refund went through Moyasar (refunded 1000)${NC}"

# 6e. Saved-card renewal against the Moyasar stub. A subscription created with
# a start_date one interval back is already due, so the billing-cycle sweep
# (BILLING_CYCLE_INTERVAL, 5s in docker-compose.yml) renews it within the run
# and charges the card saved by a checkout payment: POST /v1/payments with the
# token and a given_id derived from "renewal-<invoice id>".
echo "Step 6e: Moyasar saved-card renewal, paid and hard-declined (stubbed API)..."
BACKDATED_START=$(python3 -c 'import datetime as d; print((d.datetime.now(d.timezone.utc) - d.timedelta(days=32)).strftime("%Y-%m-%dT%H:%M:%SZ"))')

# backdated_sar_sub <customer id> — prints the new subscription's id.
backdated_sar_sub() {
  post_json "$API_URL/v1/subscriptions" "$(jq -n --arg c "$1" --arg p "$PLAN_ID_SAR" --arg s "$BACKDATED_START" \
    '{customer_id: $c, plan_id: $p, start_date: $s}')" "$API_KEY" | jq -r '.data.id'
}

# renewal_invoice <subscription id> — the subscription's renewal
# (subscription_cycle) invoice, "null" when there is none yet.
renewal_invoice() {
  get_json "$API_URL/v1/invoices?per_page=250" "$API_KEY" | jq -c --arg sid "$1" \
    '[.data[] | select(.subscription_id == $sid and .billing_reason == "subscription_cycle")] | first'
}

# stub_charges <invoice id> — every payment the stub holds for the invoice.
stub_charges() {
  curl -s -u sk_test_e2e: -G "$MOYASAR_STUB/v1/payments" --data-urlencode "metadata[invoice_id]=$1" | jq -c '.payments'
}

# wait_renewal_charge <subscription id> — polls up to 60s for the renewal
# invoice and a stub charge for it; prints the invoice id ("" on timeout).
wait_renewal_charge() {
  local inv_id
  for _ in $(seq 1 60); do
    inv_id=$(renewal_invoice "$1" | jq -r '.id // empty')
    if [ -n "$inv_id" ] && stub_charges "$inv_id" | jq -e 'length > 0' > /dev/null; then
      sleep 2 # the sweep settles (or gives up on) the charge right after it
      echo "$inv_id"
      return
    fi
    sleep 1
  done
}

# Paid: Sara's card was saved by step 6d's checkout payment (token_e2e_sar).
SUB_RENEW_OK=$(backdated_sar_sub "$CUST_ID_SAR")
[ -n "$SUB_RENEW_OK" ] && [ "$SUB_RENEW_OK" != "null" ] || fail_6d "backdated SAR subscription not created"
RENEW_OK_ID=$(wait_renewal_charge "$SUB_RENEW_OK")
[ -n "$RENEW_OK_ID" ] || fail_6d "no charged renewal invoice for backdated subscription $SUB_RENEW_OK within 60s" "$(renewal_invoice "$SUB_RENEW_OK")"
RENEW_OK=$(get_json "$API_URL/v1/invoices/$RENEW_OK_ID" "$API_KEY" | jq -c '.data')
WANT_GIVEN_ID=$(python3 -c 'import sys, uuid; print(uuid.uuid5(uuid.UUID("6f1c2a8e-3b7d-4e0a-9c51-2d8f4b6e7a90"), "renewal-" + sys.argv[1]))' "$RENEW_OK_ID")
OK_CHARGES=$(stub_charges "$RENEW_OK_ID")
if [ "$(echo "$RENEW_OK" | jq -r '.status')" != "paid" ] \
  || [ "$(echo "$RENEW_OK" | jq -r '.gateway_payment_id')" != "$WANT_GIVEN_ID" ] \
  || [ "$(echo "$OK_CHARGES" | jq 'length')" != "1" ] \
  || [ "$(echo "$OK_CHARGES" | jq -r --argjson total "$(echo "$RENEW_OK" | jq '.total')" \
        '.[0] | "\(.id) \(.status) \(.source.token) \(.amount == $total)"')" != "$WANT_GIVEN_ID paid token_e2e_sar true" ]; then
  fail_6d "renewal did not settle through one token charge of token_e2e_sar (want gateway_payment_id $WANT_GIVEN_ID); stub charges: $OK_CHARGES" "$RENEW_OK"
fi
echo -e "${GREEN}  ✅ Renewal charged the saved Moyasar token once and settled (payment $WANT_GIVEN_ID)${NC}"

# Hard decline: Dina's checkout payment saves a card whose token the stub
# declines with ISO 8583 54 (expired card), a permanent decline.
CUST_DECL_RES=$(post_json "$API_URL/v1/customers" '{
    "name": "Dina Alqahtani",
    "email": "dina.sar@example.com",
    "billing_address": {"country": "SA", "postal_code": "11564"}
  }' "$API_KEY")
CUST_ID_DECL=$(get_json_value "$CUST_DECL_RES" "data.id")
SUB_DECL_FIRST=$(post_json "$API_URL/v1/subscriptions" "$(jq -n --arg c "$CUST_ID_DECL" --arg p "$PLAN_ID_SAR" \
  '{customer_id: $c, plan_id: $p}')" "$API_KEY" | jq -r '.data.id')
INV_DECL_FIRST=$(get_json "$API_URL/v1/invoices?per_page=250" "$API_KEY" | jq -r --arg sid "$SUB_DECL_FIRST" \
  '[.data[] | select(.subscription_id == $sid)][0].id')
DECL_DUE=$(post_json "$API_URL/checkout/$INV_DECL_FIRST/pay" '{}' | jq -r '.data.amount')
DECL_PAYMENT_ID=$(python3 -c 'import uuid; print(uuid.uuid4())')
curl -s -X PUT "$MOYASAR_STUB/_stub/payments/$DECL_PAYMENT_ID" -H 'Content-Type: application/json' \
  --data @- > /dev/null <<<"$(jq -n --arg inv "$INV_DECL_FIRST" --argjson amount "$DECL_DUE" \
    '{status: "paid", amount: $amount, currency: "SAR", metadata: {invoice_id: $inv},
      source: {type: "creditcard", company: "mada", number: "4201-32XX-XXXX-1010", token: "token_e2e_decline_hard"}}')"
DECL_VERIFY=$(post_json "$API_URL/checkout/$INV_DECL_FIRST/moyasar/verify" "$(jq -n --arg id "$DECL_PAYMENT_ID" '{payment_id: $id}')")
[ "$(echo "$DECL_VERIFY" | jq -r '.data.status')" = "paid" ] || fail_6d "checkout verify did not settle Dina's first invoice" "$DECL_VERIFY"

SUB_RENEW_DECL=$(backdated_sar_sub "$CUST_ID_DECL")
[ -n "$SUB_RENEW_DECL" ] && [ "$SUB_RENEW_DECL" != "null" ] || fail_6d "backdated SAR subscription (decline) not created"
RENEW_DECL_ID=$(wait_renewal_charge "$SUB_RENEW_DECL")
[ -n "$RENEW_DECL_ID" ] || fail_6d "no charged renewal invoice for backdated subscription $SUB_RENEW_DECL within 60s" "$(renewal_invoice "$SUB_RENEW_DECL")"
RENEW_DECL=$(get_json "$API_URL/v1/invoices/$RENEW_DECL_ID" "$API_KEY" | jq -c '.data')
DECL_CHARGES=$(stub_charges "$RENEW_DECL_ID")
# A declined renewal leaves the invoice open and unpaid for dunning
# (renewal.go attemptPayment): no payment recorded, nothing settled.
if [ "$(echo "$RENEW_DECL" | jq -r '.status')" != "open" ] \
  || [ "$(echo "$RENEW_DECL" | jq -r '.amount_paid')" != "0" ] \
  || [ "$(echo "$RENEW_DECL" | jq -r '.gateway_payment_id // ""')" != "" ] \
  || [ "$(echo "$DECL_CHARGES" | jq 'length')" != "1" ] \
  || [ "$(echo "$DECL_CHARGES" | jq -r '.[0] | "\(.status) \(.source.token) \(.source.response_code)"')" != "failed token_e2e_decline_hard 54" ]; then
  fail_6d "hard-declined renewal: want one failed charge (code 54) and an open, unpaid invoice; stub charges: $DECL_CHARGES" "$RENEW_DECL"
fi
echo -e "${GREEN}  ✅ Hard-declined renewal charged the token once (code 54), invoice left open and unpaid${NC}"

# 7. Audit-grade gate: after everything above, the ledger must reconcile with
# ZERO discrepancies — the end-to-end version of the invariant harness. Any
# invoice-creating flow that forgets its ledger leg fails the whole run here.
echo "Step 7: Reconciliation must be clean..."
RECON_RES=$(get_json "$API_URL/v1/finance/reconciliation" "$API_KEY")
DISCREPANCIES=$(echo "$RECON_RES" | jq -r '.data.discrepancies | length')
if [ "$DISCREPANCIES" = "0" ]; then
    echo -e "${GREEN}  ✅ Ledger reconciles clean (0 discrepancies)${NC}"
else
    echo -e "${RED}  ❌ Reconciliation found $DISCREPANCIES discrepancies${NC}"
    echo "Response: $RECON_RES"
    exit 1
fi

echo -e "${GREEN}✅ All Scenario Checks Passed!${NC}"
# Sentinel asserted by CI: proves the script ran to its last check.
echo "E2E_VERIFICATION_COMPLETE"
