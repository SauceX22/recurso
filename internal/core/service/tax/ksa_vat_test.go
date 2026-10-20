package tax

import (
	"context"
	"testing"

	"github.com/recurso-dev/recurso/internal/core/port"
)

func TestFactory_SARoutesToKSAVAT(t *testing.T) {
	for _, cc := range []string{"SA", "sa", " SA "} {
		if _, ok := NewTaxEngine(cc, "").(*KSAVATEngine); !ok {
			t.Errorf("NewTaxEngine(%q) = %T, want *KSAVATEngine", cc, NewTaxEngine(cc, ""))
		}
	}
}

func TestKSAVATEngine_Matrix(t *testing.T) {
	ctx := context.Background()
	eng := NewKSAVATEngine() // Saudi seller

	tests := []struct {
		name     string
		req      *port.TaxRequest
		wantTax  int64
		wantType string
		wantRate float64
		wantNote string
	}{
		{
			name:     "domestic B2C -> 15%",
			req:      &port.TaxRequest{Amount: 10000, Currency: "SAR", BuyerCountry: "SA"},
			wantTax:  1500,
			wantType: "vat",
			wantRate: 0.15,
		},
		{
			// KSA has no domestic reverse charge for standard supplies: a VAT-registered
			// business buyer is charged 15% like a consumer.
			name:     "domestic B2B -> 15% (no domestic reverse charge)",
			req:      &port.TaxRequest{Amount: 10000, Currency: "SAR", BuyerCountry: "SA", IsBusiness: true},
			wantTax:  1500,
			wantType: "vat",
			wantRate: 0.15,
		},
		{
			name:     "no buyer country -> treated as domestic 15%",
			req:      &port.TaxRequest{Amount: 10000, Currency: "SAR"},
			wantTax:  1500,
			wantType: "vat",
			wantRate: 0.15,
		},
		{
			name:     "export B2B to US -> zero-rated",
			req:      &port.TaxRequest{Amount: 10000, Currency: "USD", BuyerCountry: "US", IsBusiness: true},
			wantTax:  0,
			wantType: "export_exempt",
			wantNote: "Export of services outside KSA: zero-rated (0% VAT)",
		},
		{
			name:     "export B2C to DE -> zero-rated",
			req:      &port.TaxRequest{Amount: 10000, Currency: "EUR", BuyerCountry: "DE"},
			wantTax:  0,
			wantType: "export_exempt",
			wantNote: "Export of services outside KSA: zero-rated (0% VAT)",
		},
		{
			name:     "export to GCC (AE) -> zero-rated",
			req:      &port.TaxRequest{Amount: 10000, Currency: "SAR", BuyerCountry: "AE", IsBusiness: true},
			wantTax:  0,
			wantType: "export_exempt",
			wantNote: "Export of services outside KSA: zero-rated (0% VAT)",
		},
		// Rounding in halalas: half away from zero on the exact product.
		{"rounding 99.99 SAR -> 15.00 (14.9985)", &port.TaxRequest{Amount: 9999, BuyerCountry: "SA"}, 1500, "vat", 0.15, ""},
		{"rounding 3.33 SAR -> 0.50 (49.95 halalas)", &port.TaxRequest{Amount: 333, BuyerCountry: "SA"}, 50, "vat", 0.15, ""},
		{"rounding exact half 0.30 SAR -> 0.05 (4.5 halalas)", &port.TaxRequest{Amount: 30, BuyerCountry: "SA"}, 5, "vat", 0.15, ""},
		{"rounding 1 halala -> 0 (0.15)", &port.TaxRequest{Amount: 1, BuyerCountry: "SA"}, 0, "vat", 0.15, ""},
		{"rounding 1234.57 SAR -> 185.19 (18518.55)", &port.TaxRequest{Amount: 123457, BuyerCountry: "SA"}, 18519, "vat", 0.15, ""},
		{"large exact half 10,000,000.10 SAR", &port.TaxRequest{Amount: 1000000010, BuyerCountry: "SA"}, 150000002, "vat", 0.15, ""},
		{"negative (credit) rounds away from zero", &port.TaxRequest{Amount: -30, BuyerCountry: "SA"}, -5, "vat", 0.15, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := eng.CalculateTax(ctx, tc.req)
			if err != nil {
				t.Fatalf("CalculateTax: %v", err)
			}
			if got.TotalTax != tc.wantTax {
				t.Errorf("TotalTax = %d, want %d", got.TotalTax, tc.wantTax)
			}
			if got.TaxType != tc.wantType {
				t.Errorf("TaxType = %q, want %q", got.TaxType, tc.wantType)
			}
			if got.TaxRate != tc.wantRate {
				t.Errorf("TaxRate = %v, want %v", got.TaxRate, tc.wantRate)
			}
			if got.ReverseCharge {
				t.Error("ReverseCharge = true, want false (KSA seller never reverse-charges)")
			}
			if got.Note != tc.wantNote {
				t.Errorf("Note = %q, want %q", got.Note, tc.wantNote)
			}
			if got.TaxType == "vat" && (got.VATAmount != got.TotalTax || got.VATRate != KSAVATRate) {
				t.Errorf("VAT breakdown = %d @ %v, want %d @ %v", got.VATAmount, got.VATRate, got.TotalTax, KSAVATRate)
			}
			if got.IGST != 0 || got.CGST != 0 || got.SGST != 0 {
				t.Errorf("GST components = %d/%d/%d, want all zero", got.IGST, got.CGST, got.SGST)
			}
			// GetApplicableRate must stay consistent with CalculateTax.
			rate, err := eng.GetApplicableRate(ctx, tc.req)
			if err != nil {
				t.Fatalf("GetApplicableRate: %v", err)
			}
			if rate != tc.wantRate {
				t.Errorf("GetApplicableRate = %v, want %v", rate, tc.wantRate)
			}
		})
	}
}
