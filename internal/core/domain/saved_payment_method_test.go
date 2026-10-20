package domain

import "testing"

func TestSavedPaymentMethodChargeable(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    SavedPaymentMethod
		want bool
	}{
		{"stripe with customer and method", SavedPaymentMethod{Provider: GatewayStripe, CustomerRef: "cus_1", PaymentMethodRef: "pm_1"}, true},
		{"stripe without customer", SavedPaymentMethod{Provider: GatewayStripe, PaymentMethodRef: "pm_1"}, false},
		{"stripe without method", SavedPaymentMethod{Provider: GatewayStripe, CustomerRef: "cus_1"}, false},
		{"token-only provider", SavedPaymentMethod{Provider: GatewayMoyasar, PaymentMethodRef: "token_1"}, true},
		{"token-only provider without method", SavedPaymentMethod{Provider: GatewayMoyasar, CustomerRef: "cus_1"}, false},
		{"no provider", SavedPaymentMethod{PaymentMethodRef: "x"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.m.Chargeable(); got != tc.want {
				t.Fatalf("Chargeable() = %v, want %v", got, tc.want)
			}
		})
	}
}
