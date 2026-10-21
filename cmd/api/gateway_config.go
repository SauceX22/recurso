package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// gatewayModeFromEnv reports the payment mode of the configured gateways:
// "test" when a mode-setting gateway runs in test mode, "live" when all run
// live, "none" when no real gateway is configured (mock). It drives the
// dashboard's "Test mode" chip and which API keys (live or test) the server
// accepts. Stripe and Razorpay set the mode when either is configured; the
// currency-override gateways (GoCardless, Adyen, Moyasar) set it only on a
// server without them, so a sandbox override key (the way a new gateway is
// verified) can't flip a live server to "test" and lock out every live API
// key. Any live/test mix is returned as a warning naming the gateways.
// overrideGateways is false when the override gateways are not registered,
// as in demo mode.
func gatewayModeFromEnv(getenv func(string) string, overrideGateways bool) (mode string, warnings []string) {
	type gatewayMode struct {
		name string
		live bool
	}
	var primary, extra []gatewayMode
	if k := getenv("STRIPE_SECRET_KEY"); k != "" {
		primary = append(primary, gatewayMode{"Stripe", !strings.HasPrefix(k, "sk_test")})
	}
	if k := getenv("RAZORPAY_KEY_ID"); k != "" {
		primary = append(primary, gatewayMode{"Razorpay", !strings.HasPrefix(k, "rzp_test")})
	}
	if overrideGateways {
		if getenv("GOCARDLESS_ACCESS_TOKEN") != "" {
			// Same rule as NewGoCardlessGateway: live unless "sandbox".
			extra = append(extra, gatewayMode{"GoCardless", !strings.EqualFold(getenv("GOCARDLESS_ENV"), "sandbox")})
		}
		if getenv("ADYEN_API_KEY") != "" {
			// Same rule as NewAdyenGateway: the live endpoint needs a prefix.
			extra = append(extra, gatewayMode{"Adyen", !strings.EqualFold(getenv("ADYEN_ENV"), "test") && getenv("ADYEN_LIVE_URL_PREFIX") != ""})
		}
		if k := getenv("MOYASAR_SECRET_KEY"); k != "" {
			extra = append(extra, gatewayMode{"Moyasar", strings.HasPrefix(k, moyasarLivePrefix)})
		}
	}
	all := append(append([]gatewayMode{}, primary...), extra...)
	if len(all) == 0 {
		return "none", nil
	}
	deciding := primary
	if len(deciding) == 0 {
		deciding = extra
	}
	mode = "live"
	for _, g := range deciding {
		if !g.live {
			mode = "test"
		}
	}
	var live, test []string
	for _, g := range all {
		if g.live {
			live = append(live, g.name)
		} else {
			test = append(test, g.name)
		}
	}
	if len(live) > 0 && len(test) > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"payment gateways mix live (%s) and test (%s) mode; the server runs in %s mode and accepts only %s API keys",
			strings.Join(live, ", "), strings.Join(test, ", "), mode, mode))
	}
	return mode, warnings
}

const (
	moyasarLivePrefix = "sk_live_"
	moyasarTestPrefix = "sk_test_"
)

// moyasarConfig is the Moyasar boot configuration.
type moyasarConfig struct {
	secretKey, publishableKey, webhookSecret string
	// apiURL (MOYASAR_API_URL) replaces the Moyasar API base. Test only: the
	// e2e suite points it at a stub; refused with a live key.
	apiURL string
	// callbackBaseURL is the dashboard origin token charges send Moyasar as
	// their callback_url: DASHBOARD_URL, else BASE_URL.
	callbackBaseURL string
}

func moyasarConfigFromEnv(getenv func(string) string, baseURL string) moyasarConfig {
	callback := getenv("DASHBOARD_URL")
	if callback == "" {
		callback = baseURL
	}
	return moyasarConfig{
		secretKey:       getenv("MOYASAR_SECRET_KEY"),
		publishableKey:  getenv("MOYASAR_PUBLISHABLE_KEY"),
		webhookSecret:   getenv("MOYASAR_WEBHOOK_SECRET"),
		apiURL:          getenv("MOYASAR_API_URL"),
		callbackBaseURL: callback,
	}
}

// validateMoyasarConfig checks the Moyasar configuration at boot. A secret key
// of the wrong kind, or test and live keys mixed, is an error: the browser
// would create payments in one mode while the server reads them back in the
// other, so customers pay and invoices never settle. So is a callback origin
// Moyasar can't use (live needs absolute https on a real host), since it
// would refuse every token charge, and an API override on a live key. A
// missing publishable key (SAR checkout unavailable) or webhook secret (no
// settlement backstop when the checkout return is lost) is a warning.
func validateMoyasarConfig(c moyasarConfig) (warnings []string, err error) {
	var live bool
	switch {
	case strings.HasPrefix(c.secretKey, moyasarLivePrefix):
		live = true
	case strings.HasPrefix(c.secretKey, moyasarTestPrefix):
	default:
		return nil, fmt.Errorf("MOYASAR_SECRET_KEY must start with %s or %s", moyasarTestPrefix, moyasarLivePrefix)
	}
	switch {
	case c.publishableKey == "":
		warnings = append(warnings, "MOYASAR_PUBLISHABLE_KEY is not set: SAR checkout will answer 503")
	case !strings.HasPrefix(c.publishableKey, "pk_test_") && !strings.HasPrefix(c.publishableKey, "pk_live_"):
		return nil, errors.New("MOYASAR_PUBLISHABLE_KEY must start with pk_test_ or pk_live_")
	case strings.HasPrefix(c.publishableKey, "pk_live_") != live:
		return nil, errors.New("MOYASAR_SECRET_KEY and MOYASAR_PUBLISHABLE_KEY mix test and live mode")
	}
	if c.webhookSecret == "" {
		warnings = append(warnings, "MOYASAR_WEBHOOK_SECRET is not set: Moyasar webhooks will be refused, so a payment whose checkout return is lost is never settled")
	}
	cb, perr := url.Parse(c.callbackBaseURL)
	switch {
	case perr != nil || (cb.Scheme != "http" && cb.Scheme != "https") || cb.Host == "":
		return nil, fmt.Errorf("DASHBOARD_URL (or BASE_URL) %q must be an absolute URL: Moyasar token charges use it as their callback_url", c.callbackBaseURL)
	case live && (cb.Scheme != "https" || isLocalHost(cb.Hostname())):
		return nil, fmt.Errorf("DASHBOARD_URL (or BASE_URL) %q must be a public https URL with a live Moyasar key: it is every token charge's callback_url", c.callbackBaseURL)
	}
	if c.apiURL != "" {
		u, perr := url.Parse(c.apiURL)
		switch {
		case live:
			return nil, errors.New("MOYASAR_API_URL is a test-only override and is refused with a live Moyasar key")
		case perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
			return nil, fmt.Errorf("MOYASAR_API_URL %q must be an absolute URL", c.apiURL)
		}
		warnings = append(warnings, "MOYASAR_API_URL overrides the Moyasar API (test only): "+c.apiURL)
	}
	return warnings, nil
}

func isLocalHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}
