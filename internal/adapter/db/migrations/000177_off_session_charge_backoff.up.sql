-- Off-session charge backoff. Constant defaults and nullable columns, so every
-- ADD is catalog-only.
--
-- invoices.retry_infra_failures: consecutive gateway errors (outage, revoked
-- key, rate limit) on the retry worker's attempts; it backs the retry off and
-- hands the invoice to email dunning after a cap.
-- invoices.declined_method_ref: the saved method a hard decline came from; the
-- worker won't charge it again until the customer's saved method changes.
ALTER TABLE invoices ADD COLUMN IF NOT EXISTS retry_infra_failures INTEGER NOT NULL DEFAULT 0;
ALTER TABLE invoices ADD COLUMN IF NOT EXISTS declined_method_ref TEXT;

-- wallets.recharge_attempt: sequence in the auto-recharge idempotency key; it
-- advances only on a confirmed outcome, so a lost response replays the same
-- charge. recharge_failures / recharge_retry_at: backoff after a failed
-- attempt. recharge_blocked_ref: the saved method a hard decline came from;
-- auto-recharge stops until the customer's saved method changes.
ALTER TABLE wallets ADD COLUMN IF NOT EXISTS recharge_attempt INTEGER NOT NULL DEFAULT 0;
ALTER TABLE wallets ADD COLUMN IF NOT EXISTS recharge_failures INTEGER NOT NULL DEFAULT 0;
ALTER TABLE wallets ADD COLUMN IF NOT EXISTS recharge_retry_at TIMESTAMPTZ;
ALTER TABLE wallets ADD COLUMN IF NOT EXISTS recharge_blocked_ref TEXT;
