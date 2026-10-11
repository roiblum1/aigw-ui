import assert from "node:assert/strict";
import { test } from "node:test";
import { amount, BEST_EFFORT, dollars, due, level, line, NEAR, OK, percent, pick, REFUSED, SPENT, tokens, warning } from "./usage.mjs";

const now = new Date("2026-10-11T10:00:00");
const b = (over = {}) => ({ model_name: "GLM5.3", unit: "credits", limit: 5_000_000, window: "1d", used: 318_400, resets_at: "2026-10-11T18:00:00", enforced: true, best_effort_used: 0, ...over });

test("amounts", () => {
  assert.equal(dollars(318_400), "$3.18");
  assert.equal(dollars(12), "$0.00012");
  assert.equal(dollars(0), "$0.00");
  assert.equal(tokens(950), "950");
  assert.equal(tokens(12_500), "12.5k");
  assert.equal(tokens(2_500_000), "2.5M");
  assert.equal(tokens(250_000_000), "250M");
  assert.equal(amount(2_500_000, "tokens"), "2.5M");
});

test("levels", () => {
  assert.equal(percent(b()), 6);
  assert.equal(percent(b({ limit: 0 })), 100);
  assert.equal(level(b()), OK);
  assert.equal(level(b({ used: 4_500_000 })), NEAR);
  assert.equal(level(b({ used: 4_500_000 }), 95), OK);
  assert.equal(level(b({ used: 5_000_001 })), SPENT);
  // A budget that is only counted refuses nobody, so there is nothing to warn about.
  assert.equal(level(b({ used: 5_000_001, enforced: false })), OK);
  assert.equal(level(b({ best_effort_until: "2026-10-11T18:00:00" })), BEST_EFFORT);
  assert.equal(level(b({ best_effort_until: "2026-10-11T18:00:00", best_effort_capped: true })), REFUSED);
});

test("the line", () => {
  assert.equal(line(b(), now), "GLM5.3 $3.18 of $50.00 (6%) · resets 18:00");
  assert.equal(line(b({ unit: "tokens", used: 1_200_000, limit: 2_500_000 }), now), "GLM5.3 1.2M of 2.5M tokens (48%) · resets 18:00");
  assert.equal(line(b({ enforced: false }), now), "GLM5.3 $3.18 of $50.00 (6%) · resets 18:00 · not enforced");
  assert.equal(line(b({ used: 5_100_000, best_effort_until: "2026-10-11T18:00:00" }), now), "GLM5.3 $51.00 of $50.00 (102%) · best-effort until 18:00");
  assert.equal(line(b({ resets_at: "2026-10-12T00:00:00" }), now), "GLM5.3 $3.18 of $50.00 (6%) · resets 12 Oct 00:00");
});

test("which budget is shown", () => {
  const hour = b({ window: "1h", limit: 500_000, used: 450_000 });
  const other = b({ model_name: "qwen", used: 4_900_000 });
  // The model in use, and of its budgets the one furthest gone.
  assert.equal(pick([b(), hour, other], "glm5.3"), hour);
  // A model without a budget: the furthest gone of all.
  assert.equal(pick([b(), other], "unknown"), other);
  assert.equal(pick([], "x"), undefined);
});

test("a budget is warned about once per level and window", () => {
  const near = b({ used: 4_600_000 });
  let r = due([near, b({ model_name: "qwen" })], {});
  assert.deepEqual(r.budgets, [near]);
  assert.equal(due([near], r.told).budgets.length, 0);
  const spent = b({ used: 5_000_000 });
  r = due([spent], r.told);
  assert.deepEqual(r.budgets, [spent]);
  // Going back down is not news, and is not forgotten either.
  r = due([near], r.told);
  assert.equal(r.budgets.length, 0);
  assert.equal(due([spent], r.told).budgets.length, 0);
  // The next window starts clean.
  assert.equal(due([b({ used: 4_600_000, resets_at: "2026-10-12T18:00:00" })], r.told).budgets.length, 1);
});

test("warnings", () => {
  assert.equal(warning(b({ used: 4_600_000 }), now), "GLM5.3: 92% of your budget is used ($46.00 of $50.00). It resets at 18:00.");
  assert.match(warning(b({ used: 5_000_000 }), now), /is spent \(\$50.00 of \$50.00\). Requests are refused until 18:00/);
  assert.match(warning(b({ used: 5_000_000, best_effort_until: "2026-10-11T18:00:00" }), now), /served as best-effort until 18:00/);
  assert.match(warning(b({ best_effort_until: "2026-10-11T18:00:00", best_effort_capped: true }), now), /refused until 18:00/);
});
