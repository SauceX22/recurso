-- wallets.recharge_amount: the amount auto-recharge attempt recharge_attempt
-- charges, frozen before the charge and cleared once the attempt is answered.
-- A replay after a lost response charges the same amount under the same key,
-- even when auto_recharge_amount changed in between. Nullable, catalog-only.
ALTER TABLE wallets ADD COLUMN IF NOT EXISTS recharge_amount BIGINT;
