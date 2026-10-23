-- Code from before this column treats every default_payment_method as a Stripe
-- pm_*, so drop the non-Stripe methods rather than let it charge them on Stripe.
UPDATE customers
   SET default_payment_method = NULL, card_brand = NULL, card_last4 = NULL,
       card_exp_month = NULL, card_exp_year = NULL
 WHERE pm_provider IS NOT NULL AND pm_provider <> 'stripe';

ALTER TABLE customers DROP CONSTRAINT IF EXISTS customers_pm_provider_check;
ALTER TABLE customers DROP COLUMN IF EXISTS pm_provider;
