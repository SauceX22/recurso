package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/recurso-dev/recurso/internal/core/domain"
	"github.com/recurso-dev/recurso/internal/core/port"
)

// fakeGatewayMethodLookup returns a fixed provider-tagged saved method.
type fakeGatewayMethodLookup struct {
	method domain.SavedPaymentMethod
	err    error
	calls  int
}

func (f *fakeGatewayMethodLookup) GetSavedGatewayMethod(_ context.Context, _ uuid.UUID) (domain.SavedPaymentMethod, error) {
	f.calls++
	return f.method, f.err
}

// recordingSavedCharger records every argument of each off-session charge.
type recordingSavedCharger struct {
	mu     sync.Mutex
	result *port.PaymentResult
	err    error
	calls  []savedChargeCall
}

type savedChargeCall struct {
	customerRef, methodRef     string
	amount                     int64
	currency, invoiceID, idKey string
}

func (r *recordingSavedCharger) ChargeSavedPaymentMethod(_ context.Context, customerRef, methodRef string, amount int64, currency, invoiceID, idempotencyKey string) (*port.PaymentResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, savedChargeCall{customerRef, methodRef, amount, currency, invoiceID, idempotencyKey})
	return r.result, r.err
}

func TestProviderSavedMethods_Resolve(t *testing.T) {
	moyasar := &recordingSavedCharger{}
	custID := uuid.New()

	cases := []struct {
		name     string
		method   domain.SavedPaymentMethod
		err      error
		register bool
		wantOK   bool
	}{
		{"moyasar token resolves", domain.SavedPaymentMethod{Provider: domain.SavedMethodMoyasar, PaymentMethodRef: "token_1"}, nil, true, true},
		{"lookup error", domain.SavedPaymentMethod{Provider: domain.SavedMethodMoyasar, PaymentMethodRef: "token_1"}, errors.New("db down"), true, false},
		{"legacy empty provider is stripe", domain.SavedPaymentMethod{CustomerRef: "cus_1", PaymentMethodRef: "pm_1"}, nil, true, false},
		{"explicit stripe", domain.SavedPaymentMethod{Provider: "stripe", CustomerRef: "cus_1", PaymentMethodRef: "pm_1"}, nil, true, false},
		{"empty ref", domain.SavedPaymentMethod{Provider: domain.SavedMethodMoyasar}, nil, true, false},
		{"unregistered provider", domain.SavedPaymentMethod{Provider: "adyen", PaymentMethodRef: "x"}, nil, true, false},
		{"nothing registered", domain.SavedPaymentMethod{Provider: domain.SavedMethodMoyasar, PaymentMethodRef: "token_1"}, nil, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewProviderSavedMethods(&fakeGatewayMethodLookup{method: tc.method, err: tc.err})
			if tc.register {
				p.Register(domain.SavedMethodMoyasar, moyasar)
			}
			c, ref, ok := p.Resolve(context.Background(), custID)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && (c != SavedCardCharger(moyasar) || ref != tc.method.PaymentMethodRef) {
				t.Fatalf("resolved (%v, %q), want moyasar charger and %q", c, ref, tc.method.PaymentMethodRef)
			}
			if !ok && (c != nil || ref != "") {
				t.Fatalf("not-ok must return zero values, got (%v, %q)", c, ref)
			}
		})
	}
}

func TestProviderSavedMethods_StripeRegistrationNeverResolves(t *testing.T) {
	p := NewProviderSavedMethods(&fakeGatewayMethodLookup{method: domain.SavedPaymentMethod{Provider: "stripe", CustomerRef: "cus", PaymentMethodRef: "pm"}})
	p.Register("stripe", &recordingSavedCharger{})
	if _, _, ok := p.Resolve(context.Background(), uuid.New()); ok {
		t.Fatal("stripe must never resolve through the provider layer")
	}
}

func TestProviderSavedMethods_NilSafe(t *testing.T) {
	var p *ProviderSavedMethods
	p.Register(domain.SavedMethodMoyasar, &recordingSavedCharger{}) // must not panic
	if _, _, ok := p.Resolve(context.Background(), uuid.New()); ok {
		t.Fatal("nil resolver must resolve nothing")
	}
	// nil lookup
	if _, _, ok := NewProviderSavedMethods(nil).Resolve(context.Background(), uuid.New()); ok {
		t.Fatal("nil lookup must resolve nothing")
	}
	// A nil charger is not registered.
	lookup := &fakeGatewayMethodLookup{method: domain.SavedPaymentMethod{Provider: domain.SavedMethodMoyasar, PaymentMethodRef: "token_1"}}
	q := NewProviderSavedMethods(lookup)
	q.Register(domain.SavedMethodMoyasar, nil)
	if _, _, ok := q.Resolve(context.Background(), uuid.New()); ok {
		t.Fatal("nil charger must not resolve")
	}
	if lookup.calls != 0 {
		t.Fatalf("lookup called %d times with nothing registered, want 0", lookup.calls)
	}
}
