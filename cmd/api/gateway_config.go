package main

import "strings"

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
