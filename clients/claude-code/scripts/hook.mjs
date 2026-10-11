#!/usr/bin/env node
// Runs at the start of a session, when a prompt is sent and when an answer
// ends. It asks the hub for the tenant's budgets, keeps them for the status
// line, and tells the user once when a budget is nearly spent, spent, or
// served as best-effort.
//
// It prints nothing but one JSON object: plain text from a prompt hook
// would be added to the conversation. It never fails the prompt.

import { copyFileSync, mkdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { config, dir, refresh, told } from "./store.mjs";
import { due, warning } from "./usage.mjs";

const event = process.argv[2];
const here = dirname(fileURLToPath(import.meta.url));

try {
  if (event === "start") {
    // The status line is a user setting and cannot point into the plugin,
    // whose directory changes with every update. Keep a copy at a path
    // that stays.
    mkdirSync(dir, { recursive: true, mode: 0o700 });
    for (const f of ["statusline.mjs", "usage.mjs", "store.mjs"]) copyFileSync(join(here, f), join(dir, f));
  }
  const cfg = config();
  // After an answer the counters have just moved, so ask again soon.
  const state = await refresh(cfg, event === "stop" ? 5 : 30);
  const messages = [];
  if (event === "start" && state.error && !state.budgets) messages.push(`AI gateway usage: ${state.error}.`);
  if (!state.error) {
    const { budgets, told: next } = due(state.budgets, told.read(), cfg.warnAt);
    told.write(next);
    for (const b of budgets) messages.push(warning(b));
  }
  if (messages.length) process.stdout.write(JSON.stringify({ systemMessage: messages.join("\n") }));
} catch {
  // A broken usage display must not get in the way of the work.
}
