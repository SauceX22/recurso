import { describe, expect, it } from "vitest";

import { checkoutCopy, detectCheckoutLocale, verifyErrorMessage } from "../checkoutCopy";

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

describe("verifyErrorMessage", () => {
  const t = checkoutCopy("en");
  it("maps a plain status/code object", () => {
    expect(verifyErrorMessage(t, { status: 400, code: "validation_failed" }, { invoiceNumber: "INV-1", paymentId: "pay_1" }))
      .toBe(t.errMismatch("INV-1", "pay_1"));
    expect(verifyErrorMessage(t, { status: 502 })).toBe(t.errProvider);
    expect(verifyErrorMessage(t, undefined)).toBe(t.errNetwork);
  });
});
