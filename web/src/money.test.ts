// Run with "npm test". Node reads TypeScript by itself, so there is no test
// framework to install.
import assert from "node:assert/strict";
import { test } from "node:test";
import { formatAmount, formatDollars, formatNumber, limitText, limitValue } from "./money.ts";

test("credits are shown as dollars", () => {
  assert.equal(formatDollars(0), "$0.00");
  assert.equal(formatDollars(1_250_000), "$12.50");
  assert.equal(formatDollars(4_294_967_295), "$42,949.67");
  // Below a cent the decimals that matter are kept.
  assert.equal(formatDollars(23), "$0.00023");
  assert.equal(formatDollars(578), "$0.00578");
});

test("an amount is shown in its model's unit", () => {
  assert.equal(formatAmount(2_000_000, "tokens"), "2,000,000 tokens");
  assert.equal(formatAmount(1, "tokens"), "1 token");
  assert.equal(formatAmount(2_000_000, "credits"), "$20.00");
  assert.equal(formatNumber(1500, "tokens"), "1,500");
  assert.equal(formatNumber(1500, "credits"), "$0.02");
});

test("a limit typed in dollars is stored in credits", () => {
  assert.equal(limitValue("12.5", "credits"), 1_250_000);
  // 0.1 + 0.2 is not 0.3 in binary; the credits still are.
  assert.equal(limitValue(String(0.1 + 0.2), "credits"), 30_000);
  assert.equal(limitValue("0.11574", "credits"), 11_574);
  // Never less than one credit: the gateway refuses a limit of 0.
  assert.equal(limitValue("0.000001", "credits"), 1);
  assert.equal(limitValue("2000000", "tokens"), 2_000_000);
});

test("a limit comes back from the field as it went in", () => {
  for (const credits of [1, 23, 578, 11_574, 1_250_000, 4_294_967_295]) {
    assert.equal(limitValue(limitText(credits, "credits"), "credits"), credits);
  }
  assert.equal(limitText(1_250_000, "credits"), "12.5");
  assert.equal(limitText(2_000_000, "tokens"), "2000000");
});
