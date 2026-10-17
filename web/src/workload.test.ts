import assert from "node:assert/strict";
import { test } from "node:test";
import { byModel, cacheOver, percent, requestsOver, shortTokens, tokensOver, totals, type TokenPoint } from "./workload.ts";

const p = (at: string, model: string, requests: number, input: number, cached: number, output: number): TokenPoint => ({
  at,
  model_name: model,
  requests,
  input_tokens: input,
  cached_tokens: cached,
  output_tokens: output,
});

const points = [
  p("2026-10-16T09:00:00Z", "glm", 2, 3000, 2000, 100),
  p("2026-10-16T10:00:00Z", "glm", 1, 1000, 0, 50),
  p("2026-10-16T10:00:00Z", "qwen", 4, 400, 0, 40),
];
// glm: $10 input and $1 cached for a million tokens, in credits.
const prices = [{ model_name: "glm", price_input: 1_000_000, price_cached: 100_000, price_output: 4_000_000 }];

test("totals add up, with the cache's share and what it saved", () => {
  const t = totals(points, prices);
  assert.equal(t.requests, 7);
  assert.equal(t.input, 4400);
  assert.equal(t.cached, 2000);
  assert.equal(t.output, 190);
  assert.equal(t.cacheShare, 2000 / 4400);
  assert.equal(t.promptPerRequest, 4400 / 7);
  assert.equal(t.answerPerRequest, 190 / 7);
  // 2,000 cached tokens at $9 less a million: 1,800 credits.
  assert.equal(t.saved, 1800);
  assert.equal(t.noTokens, false);
});

test("nothing used, no prices, and requests without tokens", () => {
  const empty = totals([], prices);
  assert.equal(empty.cacheShare, null);
  assert.equal(empty.promptPerRequest, null);
  assert.equal(empty.saved, null);
  assert.equal(totals([p("2026-10-16T09:00:00Z", "qwen", 3, 0, 0, 0)], prices).noTokens, true);
  assert.equal(totals(points.slice(2), prices).saved, null);
});

test("per model, the busiest first", () => {
  const rows = byModel(points, prices);
  assert.deepEqual(
    rows.map((r) => [r.model, r.requests, r.input]),
    [
      ["glm", 3, 4000],
      ["qwen", 4, 400],
    ],
  );
  assert.equal(rows[1].saved, null);
});

test("tokens, requests and the cache's share per step", () => {
  const from = "2026-10-16T09:00:00Z";
  const now = new Date("2026-10-16T11:30:00Z");
  const tokens = tokensOver(points, from, "hour", now);
  assert.equal(tokens.steps.length, 3);
  assert.deepEqual(
    tokens.series.map((s) => [s.label, s.values]),
    [
      ["new input", [1000, 1400, 0]],
      ["cached input", [2000, 0, 0]],
      ["output", [100, 90, 0]],
    ],
  );
  assert.equal(tokens.peak, 3100);
  assert.deepEqual(requestsOver(points, from, "hour", now).series[0].values, [2, 5, 0]);
  const cache = cacheOver(points, from, "hour", now);
  assert.deepEqual(cache.series[0].values, [67, 0, 0]);
  assert.equal(cache.peak, 100);
  assert.equal(cacheOver([], from, "hour", now).peak, 0);
});

test("short token counts and percent", () => {
  assert.deepEqual([950, 12_400, 3_100_000, 45_000_000].map(shortTokens), ["950", "12.4k", "3.1M", "45M"]);
  assert.equal(percent(null), "—");
  assert.equal(percent(0.456), "46%");
});
