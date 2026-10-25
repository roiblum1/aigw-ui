// Run with "npm test".
import assert from "node:assert/strict";
import { test } from "node:test";
import { keyCost, peopleRows, type KeyUse } from "./people.ts";

const prices = [{ model_name: "glm", price_input: 60_000, price_cached: 11_000, price_output: 220_000 }];

function key(name: string, owner: string, models: KeyUse["models"], revoked: string | null = null): KeyUse {
  const totals = { requests: 0, input_tokens: 0, cached_tokens: 0, output_tokens: 0 };
  for (const m of models) {
    totals.requests += m.requests;
    totals.input_tokens += m.input_tokens;
    totals.cached_tokens += m.cached_tokens;
    totals.output_tokens += m.output_tokens;
  }
  return { key_id: name, name, owner, kind: "personal", revoked_at: revoked, me: owner === "alice", models, totals };
}

// Input from the cache costs the cached price, the rest the input price; a
// model without prices adds nothing, and only such models give null.
test("a key's cost at the models' prices", () => {
  const glm = { model_name: "glm", requests: 2, input_tokens: 1_000_000, cached_tokens: 400_000, output_tokens: 100_000 };
  assert.equal(keyCost([glm], prices), 36_000 + 4_400 + 22_000);
  assert.equal(keyCost([{ ...glm, model_name: "free" }], prices), null);
  assert.equal(keyCost([glm, { ...glm, model_name: "free" }], prices), 62_400);
});

// The most expensive first, each with its share of the cost; a revoked key
// only when it used something; an admin's key under its name.
test("the rows of the usage by person", () => {
  const rows = peopleRows(
    [
      key("bob", "bob", [{ model_name: "glm", requests: 1, input_tokens: 100_000, cached_tokens: 0, output_tokens: 0 }]),
      key("alice", "alice", [{ model_name: "glm", requests: 3, input_tokens: 1_000_000, cached_tokens: 0, output_tokens: 0 }]),
      key("build server", "", []),
      key("old", "carol", [], "2026-10-20T00:00:00Z"),
      key("gone", "dave", [{ model_name: "glm", requests: 1, input_tokens: 0, cached_tokens: 0, output_tokens: 0 }], "2026-10-20T00:00:00Z"),
    ],
    prices,
  );
  assert.deepEqual(
    rows.map((r) => [r.who, r.cost, r.share === null ? null : Math.round(r.share * 100)]),
    [
      ["alice", 60_000, 91],
      ["bob", 6_000, 9],
      ["dave", 0, 0],
      ["build server", null, 0],
    ],
  );
  // Without prices, the share is of the tokens.
  const free = peopleRows([key("a", "a", [{ model_name: "x", requests: 1, input_tokens: 30, cached_tokens: 0, output_tokens: 10 }]), key("b", "b", [{ model_name: "x", requests: 1, input_tokens: 10, cached_tokens: 0, output_tokens: 0 }])], prices);
  assert.deepEqual(free.map((r) => [r.who, r.share]), [["a", 0.8], ["b", 0.2]]);
});
