ALTER TABLE wallets DROP COLUMN IF EXISTS recharge_blocked_ref;
ALTER TABLE wallets DROP COLUMN IF EXISTS recharge_retry_at;
ALTER TABLE wallets DROP COLUMN IF EXISTS recharge_failures;
ALTER TABLE wallets DROP COLUMN IF EXISTS recharge_attempt;
ALTER TABLE invoices DROP COLUMN IF EXISTS declined_method_ref;
ALTER TABLE invoices DROP COLUMN IF EXISTS retry_infra_failures;
