import { describe, expect, it } from "vitest";

import { cardUpdateViaInvoice } from "../cardUpdate";

describe("cardUpdateViaInvoice", () => {
  it.each([
    [{ card_update_supported: false, payment_method_provider: "stripe" }, true],
    [{ card_update_supported: true, payment_method_provider: "moyasar" }, false],
    [{ payment_method_provider: "moyasar" }, true],
    [{ payment_method_provider: "stripe" }, false],
    [{}, false],
    [null, false],
  ])("%j → %s", (profile, want) => {
    expect(cardUpdateViaInvoice(profile)).toBe(want);
  });
});
