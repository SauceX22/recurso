package tax

import (
	"context"

	"github.com/recurso-dev/recurso/internal/core/port"
)

// KSA VAT (Kingdom of Saudi Arabia). Modelled on EUVATEngine: the seller's
// jurisdiction picks the engine, a domestic supply carries the standard rate,
// and a genuine cross-border supply is zero-rated as an export.
//
// As of: 2026-01. The standard rate is 15% (raised from 5% on 1 Jul 2020).
// Reduced or exempt categories (residential rent, some financial services,
// qualifying medicines) need product-level classification the billing engine
// does not carry, so like the EU table this engine is standard-rate-only.
const (
	// KSAVATRate is the KSA standard VAT rate as a fraction.
	KSAVATRate = 0.15
	// ksaVATRateBasisPoints is KSAVATRate in basis points, so the tax is
	// computed in exact integer arithmetic on minor units (halalas).
	ksaVATRateBasisPoints = 1500
)

// KSAVATEngine handles KSA VAT for a seller established in Saudi Arabia.
//
//   - Domestic supply (buyer in SA, or no buyer country): 15% VAT for B2C and
//     B2B alike. KSA has no domestic reverse charge for standard supplies, so a
//     business buyer is charged VAT like a consumer.
//   - Buyer outside SA: zero-rated export of services (KSA VAT Implementing
//     Regulations, Art. 33). Until the GCC member states are linked for VAT,
//     other GCC states are treated as outside the Kingdom, so a GCC buyer is
//     zero-rated like any other export. The Art. 33 conditions (customer has
//     no KSA residence, service not consumed in KSA) are a back-office check,
//     the same stance the India path takes on LUTs; the note records the
//     treatment applied.
type KSAVATEngine struct{}

// NewKSAVATEngine builds the KSA VAT engine.
func NewKSAVATEngine() *KSAVATEngine { return &KSAVATEngine{} }

// isKSAExport reports whether a buyer country makes this a cross-border supply.
// An empty buyer country is treated as domestic, as EUVATEngine does.
func isKSAExport(buyerCountry string) bool {
	return buyerCountry != "" && buyerCountry != "SA"
}

// CalculateTax implements port.TaxEngine for KSA VAT.
func (e *KSAVATEngine) CalculateTax(ctx context.Context, req *port.TaxRequest) (*port.TaxCalculation, error) {
	if isKSAExport(req.BuyerCountry) {
		return &port.TaxCalculation{
			TotalTax: 0,
			TaxRate:  0,
			TaxType:  "export_exempt",
			Note:     "Export of services outside KSA: zero-rated (0% VAT)",
		}, nil
	}

	vatAmount := ksaVATAmount(req.Amount)
	return &port.TaxCalculation{
		TotalTax:  vatAmount,
		TaxRate:   KSAVATRate,
		TaxType:   "vat",
		VATRate:   KSAVATRate,
		VATAmount: vatAmount,
	}, nil
}

// GetApplicableRate returns the KSA VAT rate for a request, consistent with
// CalculateTax.
func (e *KSAVATEngine) GetApplicableRate(ctx context.Context, req *port.TaxRequest) (float64, error) {
	if isKSAExport(req.BuyerCountry) {
		return 0, nil
	}
	return KSAVATRate, nil
}

// ksaVATAmount is 15% of amount (minor units), rounded half away from zero in
// integer arithmetic so no float error can move a halala.
func ksaVATAmount(amount int64) int64 {
	n := amount * ksaVATRateBasisPoints
	if n < 0 {
		return -((-n + 5000) / 10000)
	}
	return (n + 5000) / 10000
}
