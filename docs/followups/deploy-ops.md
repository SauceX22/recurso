# Follow-ups: deploy, ops and tests

Back to the [index](README.md).

## Open

### OPS-1: Mock gateways settle real invoices on a production deploy with no Stripe/Razorpay key

- **Status:** open
- **Severity:** high
- **What's wrong:** with `STRIPE_SECRET_KEY` (or `RAZORPAY_KEY_ID`) unset, that
  gateway is a `MockGateway`, even with `APP_ENV=production`. The mock's
  `RetryPayment` succeeds at random. The dunning worker falls back to it for an
  invoice with no saved method, routed by currency.
- **Evidence:** `cmd/api/main.go:241,250` (`NewMockGateway()` when the key is
  empty; no production guard); `internal/adapter/gateway/mock.go:129-136`
  (`rand.Float64() < 0.4` ⇒ `Success: true`);
  `internal/adapter/worker/retry_worker.go:140` (`w.gateway.RetryPayment`),
  which then settles and posts the cash leg.
- **Consequence:** on the expected Saudi setup (Moyasar only), overdue USD or
  INR invoices, and SAR ones if `GATEWAY_CURRENCY_OVERRIDES=SAR=moyasar` is
  missing, are randomly marked paid with no money collected. The books
  overstate cash.
- **Proposed fix:** refuse mock gateways outside demo/development (fail boot,
  or use a "not configured" gateway that always errors); warn or fail at boot
  when a Moyasar key is set without the SAR override
  (`cmd/api/gateway_config.go`).
- **Source:** `review_parity.md` "Adjacent blockers"; `review_ops.md` H1.

### OPS-2: The image build no longer runs the Go tests, and the fork has no CI runs

- **Status:** open
- **Severity:** high
- **What's wrong:** teammate commit `27aaf12f` removed
  `RUN CGO_ENABLED=1 go test ./...` from the Dockerfile. The tests pass in
  Alpine; the likely cause is build-server memory or time. CI only runs on
  pushes and PRs to `main`, and the fork has no recorded workflow runs
  (`gh run list -R SauceX22/recurso` is empty).
- **Evidence:** `git show 27aaf12f -- Dockerfile`; `.github/workflows/ci.yml:3-7`.
- **Consequence:** a commit that fails tests can be built and deployed to
  Coolify with nothing stopping it.
- **Proposed fix:** gate on tests in CI instead of the image build (enable
  Actions on the fork, add `feat/moyasar-gateway` to the triggers, or make
  Coolify deploy only from a branch CI has passed). Or make the step opt-in:
  `ARG RUN_TESTS=0` and `RUN if [ "$RUN_TESTS" = 1 ]; then go test ./...; fi`.
- **Source:** commit `27aaf12f` (Kareem Ebrahim, 2026-10-22).

### OPS-3: `docker-compose.prod.yml` publishes the frontend on 5168

- **Status:** open (decision: confirm and document)
- **Severity:** low
- **What's wrong:** the same commit changed the frontend port from `80:8080`
  to `5168:8080`. This is the teammate's choice for their host; nothing
  documents it.
- **Evidence:** `docker-compose.prod.yml:11`; `git show 27aaf12f`.
- **Consequence:** anyone following the docs expects port 80; a reverse proxy
  has to point at 5168. It also diverges from upstream on merge.
- **Proposed fix:** make it `${FRONTEND_PORT:-80}:8080` and note it in
  `docs/deployment.md`, or keep 5168 and document why.
- **Source:** commit `27aaf12f`.

### OPS-4: Frontend tests time out under heavy machine load

- **Status:** open
- **Severity:** low
- **What's wrong:** some page suites (CouponPage, MeterPage, ~220s) time out
  when the machine is loaded; they pass when it is idle. vitest uses the
  default timeouts.
- **Evidence:** `frontend/vite.config.js` (no `testTimeout`/`hookTimeout`);
  `frontend/src/pages/__tests__/CouponPage.test.jsx`, `MeterPage.test.jsx`.
- **Consequence:** flaky red runs that hide real failures.
- **Proposed fix:** set `testTimeout`/`hookTimeout` for page suites, or run
  the slow suites in their own pool (`poolMatchGlobs`); look at why those two
  suites are slow (unmocked endpoints waiting on retries).
- **Source:** local runs during the SAR UX fixes.

### OPS-5: Money anomalies are only log lines; no Moyasar health check

- **Status:** open
- **Severity:** medium
- **What's wrong:** "review for refund" conditions (payment on a void invoice,
  overpayment, duplicate charge) are WARN logs only. Health checks cover
  Postgres, Redis and TigerBeetle, not the Moyasar API.
- **Evidence:** `internal/service/gateway_settlement.go:255,268,304,314,317`;
  `cmd/api/main.go:1219-1239`.
- **Consequence:** double charges, overpayments and a revoked Moyasar key are
  found by grepping logs or from customer complaints.
- **Proposed fix:** persist these as reconciliation discrepancies (or send to
  `ALERT_WEBHOOK_URL`); add a cheap Moyasar probe to `/health`.
- **Source:** `review_money_security.md` M3/M5; `review2_money_ops.md` M3/M5;
  `review_ops.md` M2.

### OPS-6: Migration 000176 validates under lock; its down migration deletes saved SAR cards

- **Status:** open
- **Severity:** low
- **What's wrong:** the comment says the CHECK is validated separately so the
  scan doesn't block writes, but VALIDATE runs in the same migration. The
  down migration nulls every non-Stripe saved method. That is deliberate (old
  code would charge them on Stripe) but irreversible.
- **Evidence:** `internal/adapter/db/migrations/000176_customer_pm_provider.up.sql:4-12`;
  `000176_customer_pm_provider.down.sql:1-6`.
- **Consequence:** a lock on `customers` during the scan on a big table; a
  rollback permanently loses every saved Moyasar card.
- **Proposed fix:** fix the comment or move VALIDATE to its own migration, and
  set `lock_timeout`; copy the affected rows to a backup table in the down
  migration.
- **Source:** `review2_money_ops.md` B14; `review_ops.md` M4.

### OPS-7: A renewal batch can outlive its claim lease on multi-replica deploys

- **Status:** open
- **Severity:** low
- **What's wrong:** 200 renewals per batch, processed serially with up to 30s
  per Moyasar call, under a 15-minute lease.
- **Evidence:** `internal/service/renewal.go:96,99`;
  `internal/adapter/gateway/moyasar.go:83` (client timeout).
- **Consequence:** with more than one replica, the tail of a slow batch can be
  claimed again. That duplicates the invoice and charges it under a new key.
  Single-instance self-hosting is not affected.
- **Proposed fix:** a smaller batch, a per-sweep deadline below the lease, or
  extend the lease while the sweep runs.
- **Source:** `review_ops.md` M5 (suspected).

### OPS-8: Test hygiene left over from the reviews

- **Status:** open
- **Severity:** low
- **What's wrong:** the handback test copies the claim SQL instead of calling
  it; a pg test has no cleanup; two tests block on bare channel receives; a
  dead test adapter; a known-flaky e-invoice claim test.
- **Evidence:** `internal/adapter/worker/retry_worker_handback_pg_test.go:85`;
  `internal/adapter/db/customer_gateway_method_pg_test.go` (no `t.Cleanup`);
  `internal/service/background_tenant_ctx_test.go:81,84`;
  `internal/service/gateway_settlement_pg_test.go:18-21`;
  `internal/service/einvoice_claim_pg_test.go` (`TestClaimFailedEInvoices`).
- **Consequence:** a change to the claim SQL wouldn't fail its test; a
  regression hangs instead of failing; false reds.
- **Proposed fix:** call `ClaimOverdueForDunning`; add cleanup; `select` with a
  timeout; delete the dead adapter; fix or quarantine the flake.
- **Source:** `review2_tests.md` Part C; `review2_money_ops.md` B19;
  `review_tests.md` #22.

## Done

None yet.
