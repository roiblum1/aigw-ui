---
description: Show the AI gateway budget in the status line
---

Set up the status line of the `aigw-usage` plugin for this user.

1. Read `~/.claude/settings.json`. If it has no `statusLine`, add this and
   keep everything else as it is:

   ```json
   "statusLine": {
     "type": "command",
     "command": "node ~/.claude/aigw-usage/statusline.mjs",
     "refreshInterval": 30
   }
   ```

2. If it already has a `statusLine`, do not replace it. Show the user the
   current command and ask which they want: replace it, or keep it and add
   the budget to it by calling `node ~/.claude/aigw-usage/statusline.mjs`
   from their own script, passing on the same stdin.

3. Check that `~/.claude/aigw-usage/statusline.mjs` exists. The plugin puts
   it there at the start of a session. If it is missing, tell the user to
   restart Claude Code once with the plugin enabled.

4. Run `node ~/.claude/aigw-usage/statusline.mjs < /dev/null` and show the
   user the line. If it says the hub address or the key is not set, tell
   them to set **Hub address** in the plugin's options (`/plugin`, then
   `aigw-usage`), or the environment variable `AIGW_HUB_URL`.
