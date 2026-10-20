import { describe, expect, it } from "vitest";

import { cardUpdateViaInvoice } from "../cardUpdate";

describe("cardUpdateViaInvoice", () => {
  it.each([
    [{ payment_method: { provider: "moyasar" } }, true],
    [{ payment_method_provider: "moyasar" }, true],
    [{ pm_provider: "moyasar" }, true],
    [{ payment_method: { provider: "stripe" } }, false],
    [{}, false],
    [null, false],
  ])("%j → %s", (profile, want) => {
    expect(cardUpdateViaInvoice(profile)).toBe(want);
  });
});
