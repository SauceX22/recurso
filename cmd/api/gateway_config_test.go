package main

import "testing"

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// A server whose only real gateway is one of the override gateways must
// still know its mode: otherwise a Moyasar-only live deployment reads "none"
// and refuses every live API key.
func TestGatewayModeFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"nothing configured", nil, "none"},
		{"stripe test", map[string]string{"STRIPE_SECRET_KEY": "sk_test_1"}, "test"},
		{"stripe live", map[string]string{"STRIPE_SECRET_KEY": "sk_live_1"}, "live"},
		{"razorpay test", map[string]string{"RAZORPAY_KEY_ID": "rzp_test_1"}, "test"},
		{"moyasar live only", map[string]string{"MOYASAR_SECRET_KEY": "sk_live_1"}, "live"},
		{"moyasar test only", map[string]string{"MOYASAR_SECRET_KEY": "sk_test_1"}, "test"},
		{"stripe live, moyasar test", map[string]string{"STRIPE_SECRET_KEY": "sk_live_1", "MOYASAR_SECRET_KEY": "sk_test_1"}, "test"},
		{"gocardless live", map[string]string{"GOCARDLESS_ACCESS_TOKEN": "live_x", "GOCARDLESS_ENV": "live"}, "live"},
		{"gocardless sandbox", map[string]string{"GOCARDLESS_ACCESS_TOKEN": "sandbox_x", "GOCARDLESS_ENV": "sandbox"}, "test"},
		{"adyen live", map[string]string{"ADYEN_API_KEY": "k", "ADYEN_ENV": "live", "ADYEN_LIVE_URL_PREFIX": "abc"}, "live"},
		{"adyen test", map[string]string{"ADYEN_API_KEY": "k", "ADYEN_ENV": "test"}, "test"},
		{"adyen live without prefix runs on test", map[string]string{"ADYEN_API_KEY": "k", "ADYEN_ENV": "live"}, "test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := gatewayModeFromEnv(envOf(tc.env), true); got != tc.want {
				t.Fatalf("gatewayModeFromEnv = %q, want %q", got, tc.want)
			}
		})
	}
}

// In demo mode the override gateways aren't registered, so their keys don't
// make the server live.
func TestGatewayModeFromEnvIgnoresUnregisteredGateways(t *testing.T) {
	env := envOf(map[string]string{"MOYASAR_SECRET_KEY": "sk_live_1", "GOCARDLESS_ACCESS_TOKEN": "live_x", "ADYEN_API_KEY": "k"})
	if got := gatewayModeFromEnv(env, false); got != "none" {
		t.Fatalf("gatewayModeFromEnv = %q, want none", got)
	}
}
