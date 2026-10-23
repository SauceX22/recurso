# Follow-ups: upstream bugs

Back to the [index](README.md).

Bugs that exist on upstream `main` and aren't Moyasar- or SAR-specific.
Candidates for small upstream PRs.

## Open

### UP-1: Coupon codes are unique across all tenants

- **Status:** open
- **Severity:** medium
- **What's wrong:** the original `UNIQUE` on `coupons.code` was never replaced
  by a per-tenant one; the migration that meant to do it has the statements
  commented out.
- **Evidence:** `internal/adapter/db/migrations/000006_create_coupons_table.up.sql:3`
  (`code VARCHAR(50) NOT NULL UNIQUE`);
  `000016_add_tenant_id_to_coupons.up.sql:9-11` (DROP/ADD commented out).
- **Consequence:** two tenants can't both have a coupon `WELCOME10`; the
  second gets a conflict error, and the error leaks that the code exists
  elsewhere.
- **Proposed fix:** new migration: drop `coupons_code_key`, add
  `UNIQUE (tenant_id, code)`; check the coupon lookup is tenant-scoped.
- **Source:** fork PR #2 section D.

### UP-2: The portal's "login (dev only)" button doesn't log in

- **Status:** open
- **Severity:** low
- **What's wrong:** the dev link is the SPA path `/portal/verify?token=…`. The
  button `fetch`es it (getting the SPA's HTML, 200) and navigates to the
  dashboard without a session.
- **Evidence:** `internal/adapter/handler/portal_api.go:242` (`_dev_link`);
  `frontend/src/pages/portal/PortalLogin.jsx:64-82` (`fetch(API_BASE + devLink)`
  then `navigate("/portal/dashboard")`).
- **Consequence:** local portal testing bounces back to login.
- **Proposed fix:** `navigate(devLink)` so the SPA verify page posts the token
  (`POST /portal/auth/verify`).
- **Source:** fork PR #2 section D.

### UP-3: BYO-Stripe tenants' currencies overridden to Adyen collect into the platform's Adyen

- **Status:** open (decision pending)
- **Severity:** medium
- **What's wrong:** the rule that drops a platform-only override for a tenant
  with its own gateway lists only Moyasar. Adyen has the same exposure (no BYO
  connection type, payments land in the platform's account) but keeps its
  routing.
- **Evidence:** `internal/adapter/gateway/resolver.go:142-152,174-178`
  (`platformOnlyGateways = {GatewayMoyasar: true}`; comment: "Adyen has the
  same exposure but predates this rule").
- **Consequence:** a BYO-Stripe tenant's Adyen-overridden currency is
  collected into the platform's Adyen account with no payout to them.
- **Proposed fix:** add Adyen to `platformOnlyGateways` (registered as
  `"adyen"`, `cmd/api/main.go:273`; there is no `domain.GatewayProvider`
  constant for it yet, so add one). A one-entry change once someone confirms
  no deployment relies on the current behaviour.
- **Source:** fork PR #2 review (H3 follow-up).

## Done

None yet.
