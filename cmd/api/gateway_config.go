package main

import (
	"errors"
	"fmt"
	"strings"
)

// gatewayModeFromEnv reports the payment mode of the configured gateways:
// "test" when any configured gateway runs in test mode, "live" when all run
// live, "none" when no real gateway is configured (mock). It drives the
// dashboard's "Test mode" chip and which API keys (live or test) the server
// accepts. overrideGateways is false when the currency-override gateways
// (GoCardless, Adyen, Moyasar) are not registered, as in demo mode.
func gatewayModeFromEnv(getenv func(string) string, overrideGateways bool) string {
	var modes []bool // true = live
	if k := getenv("STRIPE_SECRET_KEY"); k != "" {
		modes = append(modes, !strings.HasPrefix(k, "sk_test"))
	}
	if k := getenv("RAZORPAY_KEY_ID"); k != "" {
		modes = append(modes, !strings.HasPrefix(k, "rzp_test"))
	}
	if overrideGateways {
		if getenv("GOCARDLESS_ACCESS_TOKEN") != "" {
			// Same rule as NewGoCardlessGateway: live unless "sandbox".
			modes = append(modes, !strings.EqualFold(getenv("GOCARDLESS_ENV"), "sandbox"))
		}
		if getenv("ADYEN_API_KEY") != "" {
			// Same rule as NewAdyenGateway: the live endpoint needs a prefix.
			modes = append(modes, !strings.EqualFold(getenv("ADYEN_ENV"), "test") && getenv("ADYEN_LIVE_URL_PREFIX") != "")
		}
		if k := getenv("MOYASAR_SECRET_KEY"); k != "" {
			modes = append(modes, strings.HasPrefix(k, moyasarLivePrefix))
		}
	}
	if len(modes) == 0 {
		return "none"
	}
	for _, live := range modes {
		if !live {
			return "test"
		}
	}
	return "live"
}

const (
	moyasarLivePrefix = "sk_live_"
	moyasarTestPrefix = "sk_test_"
)

// validateMoyasarConfig checks the Moyasar keys at boot. A secret key of the
// wrong kind, or test and live keys mixed, is an error: the browser would
// create payments in one mode while the server reads them back in the other,
// so customers pay and invoices never settle. A missing publishable key
// (SAR checkout unavailable) or webhook secret (no settlement backstop when
// the checkout return is lost) is a warning.
func validateMoyasarConfig(secretKey, publishableKey, webhookSecret string) (warnings []string, err error) {
	var live bool
	switch {
	case strings.HasPrefix(secretKey, moyasarLivePrefix):
		live = true
	case strings.HasPrefix(secretKey, moyasarTestPrefix):
	default:
		return nil, fmt.Errorf("MOYASAR_SECRET_KEY must start with %s or %s", moyasarTestPrefix, moyasarLivePrefix)
	}
	switch {
	case publishableKey == "":
		warnings = append(warnings, "MOYASAR_PUBLISHABLE_KEY is not set: SAR checkout will answer 503")
	case !strings.HasPrefix(publishableKey, "pk_test_") && !strings.HasPrefix(publishableKey, "pk_live_"):
		return nil, errors.New("MOYASAR_PUBLISHABLE_KEY must start with pk_test_ or pk_live_")
	case strings.HasPrefix(publishableKey, "pk_live_") != live:
		return nil, errors.New("MOYASAR_SECRET_KEY and MOYASAR_PUBLISHABLE_KEY mix test and live mode")
	}
	if webhookSecret == "" {
		warnings = append(warnings, "MOYASAR_WEBHOOK_SECRET is not set: Moyasar webhooks will be refused, so a payment whose checkout return is lost is never settled")
	}
	return warnings, nil
}
