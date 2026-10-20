-- Provider-neutral saved payment methods (docs/spec_moyasar.md). Records which
-- gateway issued customers.default_payment_method so off-session charges
-- (renewal, dunning retry, wallet auto-recharge) route to it.
--
-- NULL = Stripe (every row saved before this column existed is a Stripe pm_*),
-- so existing customers keep charging exactly as before. 'moyasar' marks a
-- Moyasar token_* (no gateway customer object; stripe_customer_id untouched).
ALTER TABLE customers ADD COLUMN IF NOT EXISTS pm_provider TEXT;

COMMENT ON COLUMN customers.pm_provider IS
    'Gateway that issued default_payment_method; NULL means Stripe.';
