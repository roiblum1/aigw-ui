import assert from "node:assert/strict";
import { test } from "node:test";
import { stack, stepLabel, totals, type Point } from "./history.ts";

const p = (at: string, tenant: string, model: string, used: number, best_effort = 0, unit: "credits" | "tokens" = "credits"): Point => ({
  at,
  tenant_slug: tenant,
  model_name: model,
  unit,
  used,
  best_effort,
});

const points = [
  p("2026-10-08T00:00:00Z", "team-a", "glm", 100),
  p("2026-10-09T00:00:00Z", "team-a", "glm", 300, 50),
  p("2026-10-09T00:00:00Z", "team-b", "glm", 20),
  p("2026-10-09T00:00:00Z", "team-b", "qwen", 5),
  p("2026-10-09T00:00:00Z", "team-c", "glm", 9000, 0, "tokens"),
  p("2026-10-01T00:00:00Z", "team-a", "glm", 7777),
];

test("stack lays usage out over every step, also the empty ones", () => {
  const s = stack(points, "credits", "2026-10-08T00:00:00Z", "day", new Date("2026-10-10T15:00:00Z"), "tenant");
  assert.equal(s.steps.length, 3);
  assert.deepEqual(
    s.series.map((x) => [x.label, x.values]),
    [
      ["team-a", [100, 350, 0]],
      ["team-b", [0, 25, 0]],
    ],
  );
  assert.equal(s.total, 475);
  assert.equal(s.bestEffort, 50);
  assert.equal(s.peak, 375);
  assert.equal(s.peakAt?.toISOString(), "2026-10-09T00:00:00.000Z");
});

test("stack keeps the largest series and folds the rest", () => {
  const many = Array.from({ length: 8 }, (_, i) => p("2026-10-09T00:00:00Z", `team-${i}`, "glm", i + 1));
  const s = stack(many, "credits", "2026-10-09T00:00:00Z", "day", new Date("2026-10-09T01:00:00Z"), "tenant", 3);
  assert.deepEqual(
    s.series.map((x) => [x.label, x.total]),
    [
      ["team-7", 8],
      ["team-6", 7],
      ["team-5", 6],
      ["5 others", 15],
    ],
  );
});

test("totals keep budget and best-effort apart and never mix units", () => {
  assert.deepEqual(totals(points.slice(0, 5), "credits", "model"), [
    { label: "glm", used: 420, bestEffort: 50 },
    { label: "qwen", used: 5, bestEffort: 0 },
  ]);
  assert.deepEqual(totals(points, "tokens", "tenant"), [{ label: "team-c", used: 9000, bestEffort: 0 }]);
});

test("step labels are in UTC", () => {
  assert.equal(stepLabel(new Date("2026-10-09T00:00:00Z"), "day"), "9 Oct");
  assert.equal(stepLabel(new Date("2026-10-09T07:00:00Z"), "hour"), "9 Oct 07:00");
});
