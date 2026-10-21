import { describe, expect, it } from "vitest";

import {
  checkoutCopy,
  detectCheckoutLocale,
  saveCardConsentText,
  verifyErrorMessage,
  verifyErrorRetryable,
} from "../checkoutCopy";
import { formatCurrency } from "../utils";

describe("detectCheckoutLocale", () => {
  it.each([
    [{ languages: ["ar-SA", "en"], language: "ar-SA" }, "ar"],
    [{ languages: [], language: "AR" }, "ar"],
    [{ languages: ["en-GB"], language: "ar" }, "en"],
    [{ language: "fr-FR" }, "en"],
    [undefined, "en"],
  ])("%j → %s", (nav, want) => {
    expect(detectCheckoutLocale(nav)).toBe(want);
  });
});

describe("checkoutCopy", () => {
  it("has an Arabic string for every English one", () => {
    const en = checkoutCopy("en");
    const ar = checkoutCopy("ar");
    expect(Object.keys(ar).sort()).toEqual(Object.keys(en).sort());
    for (const k of Object.keys(en)) expect(typeof ar[k]).toBe(typeof en[k]);
  });

  it("falls back to English for an unknown locale", () => {
    expect(checkoutCopy("xx")).toBe(checkoutCopy("en"));
  });
});

describe("saveCardConsentText", () => {
  const terms = { merchant: "Acme Gym", amount: 9900, currency: "SAR", interval: "month", interval_count: 1 };
  const en = checkoutCopy("en");
  const ar = checkoutCopy("ar");
  const SAR99 = formatCurrency(9900, "SAR");

  it("names the merchant, the amount, the period, the replacement and how to stop", () => {
    expect(saveCardConsentText(en, terms)).toBe(
      `By paying, you authorise Acme Gym to save this card and charge it ${SAR99} every month until you cancel. ` +
        "This card replaces any card already saved for this subscription. You can cancel at any time from your billing portal."
    );
  });

  it.each([
    [{ interval: "month", interval_count: 3 }, "every 3 months"],
    [{ interval: "year", interval_count: 1 }, "every year"],
    [{ interval: "weekly" }, "every week"],
    [{ interval: "day", interval_count: 2 }, "every 2 days"],
  ])("English period for %j", (over, want) => {
    expect(saveCardConsentText(en, { ...terms, ...over })).toContain(`${SAR99} ${want} until`);
  });

  it.each([
    [{ interval_count: 1 }, "كل شهر"],
    [{ interval_count: 2 }, "كل شهرين"],
    [{ interval_count: 3 }, "كل 3 أشهر"],
    [{ interval_count: 12 }, "كل 12 شهرًا"],
    [{ interval: "year", interval_count: 2 }, "كل سنتين"],
  ])("Arabic period for %j", (over, want) => {
    expect(saveCardConsentText(ar, { ...terms, ...over })).toContain(want);
  });

  it("isolates the merchant and amount inside the Arabic sentence", () => {
    const text = saveCardConsentText(ar, terms);
    expect(text).toContain("⁨Acme Gym⁩");
    expect(text).toContain(`⁨${SAR99}⁩`);
    expect(text).toContain("تحل هذه البطاقة محل");
  });

  it.each([
    ["no terms", undefined],
    ["no merchant", { ...terms, merchant: " " }],
    ["no amount", { ...terms, amount: undefined }],
    ["zero amount", { ...terms, amount: 0 }],
    ["no currency", { ...terms, currency: "" }],
    ["unknown interval", { ...terms, interval: "fortnight" }],
    ["bad count", { ...terms, interval_count: 0 }],
  ])("falls back to the generic sentence with %s", (_, bad) => {
    expect(saveCardConsentText(en, bad)).toBe(en.saveCardConsent);
    expect(saveCardConsentText(ar, bad)).toBe(ar.saveCardConsent);
  });
});

describe("verifyErrorMessage", () => {
  const t = checkoutCopy("en");
  it("maps a plain status/code object", () => {
    const ids = { invoiceNumber: "INV-1", paymentId: "pay_1" };
    expect(verifyErrorMessage(t, { status: 400, code: "payment_mismatch" }, ids)).toBe(t.errMismatch("INV-1", "pay_1"));
    // A malformed request or a bad signature is not proof of a mismatch.
    expect(verifyErrorMessage(t, { status: 400, code: "validation_failed" }, ids)).toBe(t.errNotConfirmed("INV-1", "pay_1"));
    expect(verifyErrorMessage(t, { status: 500, code: "internal_error" })).toBe(t.errSettle);
    expect(verifyErrorMessage(t, { status: 502 })).toBe(t.errProvider);
    expect(verifyErrorMessage(t, undefined)).toBe(t.errNetwork);
  });

  it.each([
    [undefined, true],
    [{ status: 500 }, true],
    [{ status: 502 }, true],
    [{ status: 503 }, false],
    [{ status: 404 }, false],
    [{ response: { status: 400, data: { error: { code: "payment_mismatch" } } } }, false],
  ])("Check again is offered for %j: %s", (err, want) => {
    expect(verifyErrorRetryable(err)).toBe(want);
  });
});
