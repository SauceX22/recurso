package main

import (
	"strings"
	"testing"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// A server whose only real gateway is one of the override gateways must
// still know its mode: otherwise a Moyasar-only live deployment reads "none"
// and refuses every live API key. When Stripe or Razorpay is configured, they
// alone set the mode: an override gateway in the other mode (a sandbox
// Moyasar key on a live Stripe server) must not flip the server to "test" and
// lock out every live API key; it is reported as a warning here (and refused
// at boot by validateMoyasarConfig).
func TestGatewayModeFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name         string
		env          map[string]string
		want         string
		wantWarnings int
	}{
		{"nothing configured", nil, "none", 0},
		{"stripe test", map[string]string{"STRIPE_SECRET_KEY": "sk_test_1"}, "test", 0},
		{"stripe live", map[string]string{"STRIPE_SECRET_KEY": "sk_live_1"}, "live", 0},
		{"stripe restricted test", map[string]string{"STRIPE_SECRET_KEY": "rk_test_1"}, "test", 0},
		{"stripe restricted live", map[string]string{"STRIPE_SECRET_KEY": "rk_live_1"}, "live", 0},
		{"stripe restricted test, moyasar test", map[string]string{"STRIPE_SECRET_KEY": "rk_test_1", "MOYASAR_SECRET_KEY": "sk_test_1"}, "test", 0},
		{"razorpay live", map[string]string{"RAZORPAY_KEY_ID": "rzp_live_1"}, "live", 0},
		{"razorpay test", map[string]string{"RAZORPAY_KEY_ID": "rzp_test_1"}, "test", 0},
		{"stripe live, razorpay test", map[string]string{"STRIPE_SECRET_KEY": "sk_live_1", "RAZORPAY_KEY_ID": "rzp_test_1"}, "test", 1},
		{"moyasar live only", map[string]string{"MOYASAR_SECRET_KEY": "sk_live_1"}, "live", 0},
		{"moyasar test only", map[string]string{"MOYASAR_SECRET_KEY": "sk_test_1"}, "test", 0},
		{"stripe live, moyasar test", map[string]string{"STRIPE_SECRET_KEY": "sk_live_1", "MOYASAR_SECRET_KEY": "sk_test_1"}, "live", 1},
		{"stripe live, gocardless sandbox", map[string]string{"STRIPE_SECRET_KEY": "sk_live_1", "GOCARDLESS_ACCESS_TOKEN": "x", "GOCARDLESS_ENV": "sandbox"}, "live", 1},
		{"stripe live, adyen without prefix", map[string]string{"STRIPE_SECRET_KEY": "sk_live_1", "ADYEN_API_KEY": "k", "ADYEN_ENV": "live"}, "live", 1},
		{"razorpay test, moyasar live", map[string]string{"RAZORPAY_KEY_ID": "rzp_test_1", "MOYASAR_SECRET_KEY": "sk_live_1"}, "test", 1},
		{"stripe live, moyasar live", map[string]string{"STRIPE_SECRET_KEY": "sk_live_1", "MOYASAR_SECRET_KEY": "sk_live_1"}, "live", 0},
		{"moyasar live, adyen test", map[string]string{"MOYASAR_SECRET_KEY": "sk_live_1", "ADYEN_API_KEY": "k", "ADYEN_ENV": "test"}, "test", 1},
		{"gocardless live", map[string]string{"GOCARDLESS_ACCESS_TOKEN": "live_x", "GOCARDLESS_ENV": "live"}, "live", 0},
		{"gocardless sandbox", map[string]string{"GOCARDLESS_ACCESS_TOKEN": "sandbox_x", "GOCARDLESS_ENV": "sandbox"}, "test", 0},
		{"adyen live", map[string]string{"ADYEN_API_KEY": "k", "ADYEN_ENV": "live", "ADYEN_LIVE_URL_PREFIX": "abc"}, "live", 0},
		{"adyen test", map[string]string{"ADYEN_API_KEY": "k", "ADYEN_ENV": "test"}, "test", 0},
		{"adyen live without prefix runs on test", map[string]string{"ADYEN_API_KEY": "k", "ADYEN_ENV": "live"}, "test", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, warnings := gatewayModeFromEnv(envOf(tc.env), true)
			if got != tc.want {
				t.Fatalf("gatewayModeFromEnv = %q, want %q", got, tc.want)
			}
			if len(warnings) != tc.wantWarnings {
				t.Fatalf("warnings = %q, want %d", warnings, tc.wantWarnings)
			}
		})
	}
}

// The misconfigurations that silently break SAR payments must surface at boot:
// a key of the wrong kind or a test/live mix (the browser pays in one mode,
// the server reads back in the other and never settles) is fatal, as is a
// callback origin Moyasar can't redirect to (every token charge would be
// refused) and a test-only API override on a live key; a missing publishable
// key or webhook secret is a loud warning.
func TestValidateMoyasarConfig(t *testing.T) {
	complete := func(secret, publishable string) moyasarConfig {
		return moyasarConfig{secretKey: secret, publishableKey: publishable, webhookSecret: "s", callbackBaseURL: "https://app.example.com"}
	}
	with := func(c moyasarConfig, edit func(*moyasarConfig)) moyasarConfig { edit(&c); return c }
	for _, tc := range []struct {
		name         string
		cfg          moyasarConfig
		wantErr      bool
		wantWarnings int
	}{
		{"complete test config", complete("sk_test_a", "pk_test_b"), false, 0},
		{"complete live config", complete("sk_live_a", "pk_live_b"), false, 0},
		{"publishable key pasted as secret", complete("pk_test_a", "pk_test_b"), true, 0},
		{"live secret, test publishable", complete("sk_live_a", "pk_test_b"), true, 0},
		{"test secret, live publishable", complete("sk_test_a", "pk_live_b"), true, 0},
		{"publishable key of the wrong kind", complete("sk_test_a", "sk_test_b"), true, 0},
		{"no publishable key", complete("sk_test_a", ""), false, 1},
		{"no webhook secret", with(complete("sk_live_a", "pk_live_b"), func(c *moyasarConfig) { c.webhookSecret = "" }), false, 1},
		{"neither", with(complete("sk_live_a", ""), func(c *moyasarConfig) { c.webhookSecret = "" }), false, 2},
		{"test mode on a local http origin", with(complete("sk_test_a", "pk_test_b"), func(c *moyasarConfig) { c.callbackBaseURL = "http://localhost:5173" }), false, 0},
		{"live mode on http", with(complete("sk_live_a", "pk_live_b"), func(c *moyasarConfig) { c.callbackBaseURL = "http://app.example.com" }), true, 0},
		{"live mode on localhost", with(complete("sk_live_a", "pk_live_b"), func(c *moyasarConfig) { c.callbackBaseURL = "https://localhost" }), true, 0},
		{"relative callback origin", with(complete("sk_test_a", "pk_test_b"), func(c *moyasarConfig) { c.callbackBaseURL = "app.example.com" }), true, 0},
		{"api override in test mode", with(complete("sk_test_a", "pk_test_b"), func(c *moyasarConfig) { c.apiURL = "http://moyasar-stub:9000/v1" }), false, 1},
		{"api override with a live key", with(complete("sk_live_a", "pk_live_b"), func(c *moyasarConfig) { c.apiURL = "http://moyasar-stub:9000/v1" }), true, 0},
		{"api override not a URL", with(complete("sk_test_a", "pk_test_b"), func(c *moyasarConfig) { c.apiURL = "moyasar-stub" }), true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The key's own mode, as on a Moyasar-only server.
			mode := "test"
			if strings.HasPrefix(tc.cfg.secretKey, moyasarLivePrefix) {
				mode = "live"
			}
			warnings, err := validateMoyasarConfig(tc.cfg, mode)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && len(warnings) != tc.wantWarnings {
				t.Fatalf("warnings = %q, want %d", warnings, tc.wantWarnings)
			}
		})
	}
}

// The server's mode (set by Stripe or Razorpay when configured, else by the
// override gateways) must be Moyasar's: a test key on a live server lets
// Moyasar's public test cards settle real invoices, and a live key on a test
// server charges real cards for test tenants. Both refuse to boot, as does the
// test-only API override on a live server, whatever the Moyasar key.
func TestValidateMoyasarConfigAgainstServerMode(t *testing.T) {
	cfg := func(secret, publishable, apiURL string) moyasarConfig {
		return moyasarConfig{secretKey: secret, publishableKey: publishable, webhookSecret: "s", callbackBaseURL: "https://app.example.com", apiURL: apiURL}
	}
	for _, tc := range []struct {
		name       string
		cfg        moyasarConfig
		serverMode string
		wantErr    string
	}{
		{"live server, live key", cfg("sk_live_a", "pk_live_b", ""), "live", ""},
		{"test server, test key", cfg("sk_test_a", "pk_test_b", ""), "test", ""},
		{"api override on a test server", cfg("sk_test_a", "pk_test_b", "http://stub/v1"), "test", ""},
		{"live server, test key", cfg("sk_test_a", "pk_test_b", ""), "live", "server runs in live mode"},
		{"test server, live key", cfg("sk_live_a", "pk_live_b", ""), "test", "server runs in test mode"},
		{"api override on a live server", cfg("sk_test_a", "pk_test_b", "http://stub/v1"), "live", "MOYASAR_API_URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateMoyasarConfig(tc.cfg, tc.serverMode)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want boot to proceed", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want a fatal error mentioning %q", err, tc.wantErr)
			}
		})
	}
}

// Every deployment shape that should boot does: the server mode
// gatewayModeFromEnv derives always agrees with a Moyasar key of the same mode.
func TestValidDeploymentsBootWithMoyasar(t *testing.T) {
	for _, env := range []map[string]string{
		{"MOYASAR_SECRET_KEY": "sk_test_a", "MOYASAR_PUBLISHABLE_KEY": "pk_test_b"},
		{"MOYASAR_SECRET_KEY": "sk_live_a", "MOYASAR_PUBLISHABLE_KEY": "pk_live_b"},
		{"STRIPE_SECRET_KEY": "sk_live_s", "MOYASAR_SECRET_KEY": "sk_live_a", "MOYASAR_PUBLISHABLE_KEY": "pk_live_b"},
		{"STRIPE_SECRET_KEY": "sk_test_s", "MOYASAR_SECRET_KEY": "sk_test_a", "MOYASAR_PUBLISHABLE_KEY": "pk_test_b"},
	} {
		env["MOYASAR_WEBHOOK_SECRET"] = "s"
		mode, _ := gatewayModeFromEnv(envOf(env), true)
		if _, err := validateMoyasarConfig(moyasarConfigFromEnv(envOf(env), "https://app.example.com"), mode); err != nil {
			t.Errorf("env %v (server mode %s): %v, want boot to proceed", env, mode, err)
		}
	}
}

// The boot config comes from the environment: DASHBOARD_URL (else BASE_URL)
// is the callback origin, MOYASAR_API_URL the test-only API override.
func TestMoyasarConfigFromEnv(t *testing.T) {
	got := moyasarConfigFromEnv(envOf(map[string]string{
		"MOYASAR_SECRET_KEY": "sk_test_a", "MOYASAR_PUBLISHABLE_KEY": "pk_test_b", "MOYASAR_WEBHOOK_SECRET": "s",
		"MOYASAR_API_URL": "http://stub/v1", "DASHBOARD_URL": "https://app.example.com",
	}), "http://api.local")
	want := moyasarConfig{secretKey: "sk_test_a", publishableKey: "pk_test_b", webhookSecret: "s", apiURL: "http://stub/v1", callbackBaseURL: "https://app.example.com"}
	if got != want {
		t.Fatalf("config = %+v, want %+v", got, want)
	}
	if got := moyasarConfigFromEnv(envOf(nil), "http://api.local"); got.callbackBaseURL != "http://api.local" {
		t.Fatalf("callback origin = %q, want BASE_URL when DASHBOARD_URL is unset", got.callbackBaseURL)
	}
}

// In demo mode the override gateways aren't registered, so their keys don't
// make the server live.
func TestGatewayModeFromEnvIgnoresUnregisteredGateways(t *testing.T) {
	env := envOf(map[string]string{"MOYASAR_SECRET_KEY": "sk_live_1", "GOCARDLESS_ACCESS_TOKEN": "live_x", "ADYEN_API_KEY": "k"})
	if got, _ := gatewayModeFromEnv(env, false); got != "none" {
		t.Fatalf("gatewayModeFromEnv = %q, want none", got)
	}
}
