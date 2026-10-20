-- Records which gateway issued customers.default_payment_method, so off-session
-- charges (renewal, dunning retry, wallet auto-recharge) go to that gateway.
-- NULL = Stripe: every row saved before this column existed is a Stripe pm_*.
-- Nullable with no default, so the ADD is catalog-only. The CHECK is added
-- NOT VALID and validated separately so the scan doesn't block writes.
ALTER TABLE customers ADD COLUMN IF NOT EXISTS pm_provider TEXT;

ALTER TABLE customers DROP CONSTRAINT IF EXISTS customers_pm_provider_check;
ALTER TABLE customers ADD CONSTRAINT customers_pm_provider_check
    CHECK (pm_provider IS NULL OR pm_provider IN ('stripe', 'moyasar')) NOT VALID;
ALTER TABLE customers VALIDATE CONSTRAINT customers_pm_provider_check;

COMMENT ON COLUMN customers.pm_provider IS
    'Gateway that issued default_payment_method; NULL means Stripe.';
