#!/usr/bin/env node
// The status line: the budget of the model in use, from what the hooks last
// read. It reads one small file and never asks the hub itself.

import { cache } from "./store.mjs";
import { level, line, NEAR, pick } from "./usage.mjs";

let input = {};
try {
  let text = "";
  for await (const chunk of process.stdin) text += chunk;
  input = JSON.parse(text);
} catch {
  // Run by hand, without the session's data.
}

const color = !process.env.NO_COLOR;
const paint = (code, s) => (color ? `\x1b[${code}m${s}\x1b[0m` : s);

const state = cache.read();
if (!state) {
  console.log(paint(2, "AI gateway usage: not read yet"));
} else if (!state.budgets) {
  console.log(paint(2, `AI gateway usage: ${state.error}`));
} else if (!state.usage_enabled && !state.error) {
  console.log(paint(2, "AI gateway usage: the hub keeps no usage"));
} else if (state.budgets.length === 0) {
  console.log(paint(2, "AI gateway usage: no budget"));
} else {
  const b = pick(state.budgets, input.model?.id);
  const l = level(b);
  let out = paint(l > NEAR ? 31 : l === NEAR ? 33 : 32, line(b));
  if (state.error) out += paint(2, ` · old: ${state.error}`);
  console.log(out);
}
