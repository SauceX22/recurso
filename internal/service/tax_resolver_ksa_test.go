package service

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
)

// --- KSA VAT ---

func ksaCustomer(country, taxID string) *domain.Customer {
	c := &domain.Customer{
		ID:             uuid.New(),
		BillingAddress: domain.BillingAddress{Country: country},
	}
	if taxID != "" {
		c.TaxID = domain.StringPtr(taxID)
	}
	return c
}

func TestResolveInvoiceTax_SASeller_Matrix(t *testing.T) {
	cases := []struct {
		name      string
		customer  *domain.Customer
		currency  string
		amount    int64
		wantTotal int64
		wantType  string
		wantRate  float64
		wantNote  string
	}{
		{"domestic B2C", ksaCustomer("Saudi Arabia", ""), "SAR", 10000, 1500, "vat", 0.15, ""},
		{"domestic B2B (VAT-registered buyer, no reverse charge)", ksaCustomer("SA", "300000000000003"), "SAR", 10000, 1500, "vat", 0.15, ""},
		{"domestic, buyer country unset", ksaCustomer("", ""), "SAR", 10000, 1500, "vat", 0.15, ""},
		{"domestic non-round amount 1234.57 SAR", ksaCustomer("KSA", ""), "SAR", 123457, 18519, "vat", 0.15, ""},
		{"export B2B to US", ksaCustomer("US", "12-3456789"), "USD", 10000, 0, "export_exempt", 0, "Export of services outside KSA: zero-rated (0% VAT)"},
		{"export B2C to UAE (GCC)", ksaCustomer("United Arab Emirates", ""), "SAR", 10000, 0, "export_exempt", 0, "Export of services outside KSA: zero-rated (0% VAT)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewTaxResolver(nil, "SA", "")
			res := r.ResolveInvoiceTax(context.Background(), uuid.New(), tc.customer, tc.currency, tc.amount, "")
			if res.Total != tc.wantTotal {
				t.Errorf("Total = %d, want %d", res.Total, tc.wantTotal)
			}
			if res.TaxType != tc.wantType {
				t.Errorf("TaxType = %q, want %q", res.TaxType, tc.wantType)
			}
			if res.Rate != tc.wantRate {
				t.Errorf("Rate = %v, want %v", res.Rate, tc.wantRate)
			}
			if res.Note != tc.wantNote {
				t.Errorf("Note = %q, want %q", res.Note, tc.wantNote)
			}
			if res.IGST != 0 || res.CGST != 0 || res.SGST != 0 || res.HSN != "" {
				t.Errorf("GST artifacts = %d/%d/%d hsn=%q, want none for KSA VAT", res.IGST, res.CGST, res.SGST, res.HSN)
			}
		})
	}
}

// A Saudi seller declared via the primary entity (env still defaulting to IN)
// must get KSA VAT, never the India GST path or its "no LUT" export note.
func TestResolveInvoiceTax_SAEntitySeller_NotIndiaGST(t *testing.T) {
	r := NewTaxResolver(&mockGSTConfigProvider{}, "IN", "TN").
		WithPrimaryEntityCountry(constCountry("SA"))
	ctx := context.Background()
	tenant := uuid.New()

	res := r.ResolveInvoiceTax(ctx, tenant, ksaCustomer("Saudi Arabia", ""), "SAR", 10000, "")
	if res.Total != 1500 || res.TaxType != "vat" {
		t.Fatalf("SA seller SAR invoice = %d %q, want 1500 vat", res.Total, res.TaxType)
	}
	if strings.Contains(res.Note, "LUT") || res.TaxType == "export" {
		t.Errorf("SA seller got the India GST export path: %+v", res)
	}
	if got := RegimeForCountry(r.SellerCountry(ctx, tenant)); got != domain.TaxRegimeVAT {
		t.Errorf("regime = %q, want %q", got, domain.TaxRegimeVAT)
	}
}

// Inbound (seller outside SA, Saudi business buyer): the seller's engine
// decides, as it does for every other jurisdiction. An EU seller zero-rates the
// export; the Saudi buyer self-assesses KSA VAT under its own reverse charge,
// which is outside the seller's invoice. Recorded here so the behaviour is a
// conscious decision.
func TestResolveInvoiceTax_InboundToSABuyer_SellerEngineDecides(t *testing.T) {
	r := NewTaxResolver(nil, "DE", "")
	res := r.ResolveInvoiceTax(context.Background(), uuid.New(), ksaCustomer("Saudi Arabia", "300000000000003"), "EUR", 10000, "")
	if res.Total != 0 || res.TaxType != "export_exempt" {
		t.Errorf("DE seller -> SA business = %d %q, want 0 export_exempt", res.Total, res.TaxType)
	}
}

// ENG-152 follow-through: an unsupported seller country must surface the
// NoTaxEngine's own result, not be routed into resolveIndiaGST (which labelled
// every non-INR invoice an India "Zero-rated export (no LUT …)").
func TestResolveInvoiceTax_UnsupportedSeller_NoIndiaExportNote(t *testing.T) {
	for _, currency := range []string{"AED", "USD", "INR"} {
		t.Run(currency, func(t *testing.T) {
			r := NewTaxResolver(nil, "AE", "")
			res := r.ResolveInvoiceTax(context.Background(), uuid.New(), ksaCustomer("United Arab Emirates", ""), currency, 10000, "")
			if res.Total != 0 || res.TaxType != "none" {
				t.Errorf("AE seller = %d %q, want 0 none", res.Total, res.TaxType)
			}
			if res.Note != "No tax configured for AE" {
				t.Errorf("Note = %q, want %q", res.Note, "No tax configured for AE")
			}
		})
	}
}

func TestTaxLineLabel_SAIsVAT(t *testing.T) {
	if got := taxLineLabel("SA"); got != "VAT" {
		t.Errorf("taxLineLabel(SA) = %q, want VAT", got)
	}
}
