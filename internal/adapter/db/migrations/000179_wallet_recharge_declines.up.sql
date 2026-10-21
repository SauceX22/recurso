-- wallets.recharge_declines: answered declines in a row on the saved method
-- recharge_declines_ref names. It drives the soft-decline cap and the
-- "declines started" email; it restarts when the saved method changes, and
-- timeouts or lookup failures never count (they only back off through
-- recharge_failures). Constant default and a nullable column: catalog-only.
ALTER TABLE wallets ADD COLUMN IF NOT EXISTS recharge_declines INTEGER NOT NULL DEFAULT 0;
ALTER TABLE wallets ADD COLUMN IF NOT EXISTS recharge_declines_ref TEXT;
