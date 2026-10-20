package service

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/recurso-dev/recurso/internal/core/port"
)

// captureDefaultLog routes the default slog logger into a buffer for the test.
func captureDefaultLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// A decline must be diagnosable from the log alone: which payment, which
// decline code, hard or soft. An empty "error" field says nothing.
func TestRenewalDeclineLogsCodeAndPaymentID(t *testing.T) {
	logs := captureDefaultLog(t)
	svc, _, sub := sarRenewalFixture()
	charger := &fakeRenewalCharger{result: &port.PaymentResult{
		Success: false, PaymentID: "pay_declined_1", ErrorCode: "expired_card", Permanent: true, ErrorMsg: "EXPIRED CARD (response code 54)",
	}}
	svc.SetSavedMethodCharging(nil, moyasarLookup(), &fakeRenewalSettler{})
	svc.SetChargerRouter(moyasarRouter(charger))

	if err := svc.RenewSubscription(context.Background(), sub); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	for _, want := range []string{"payment_id=pay_declined_1", "error_code=expired_card", "permanent=true", "response code 54"} {
		if !strings.Contains(out, want) {
			t.Errorf("decline log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "error=<nil>") {
		t.Errorf("decline logged a nil error:\n%s", out)
	}
}
