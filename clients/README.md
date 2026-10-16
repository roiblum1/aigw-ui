# Usage in the coding agent

For the people who use the models: `aigw-token`, which signs Claude Code in
to the gateway with a personal key, and two small add-ons that show a
tenant's budget inside the coding agent and warn before it is spent.

## aigw-token

[`aigw-token/aigw-token`](aigw-token/aigw-token) is Claude Code's
`apiKeyHelper` for the gateway. It trades your personal key at the hub for a
token that carries your team's budget and models as they are now, and gets a
new one every 10 minutes (`AIGW_TOKEN_RENEW_SECONDS`), so you never handle a
token and a change of budget reaches you by itself. If the hub cannot be
reached it goes on printing the token it has until that expires (7 days by
default). Needs `sh` and `curl` (macOS, Linux, WSL).

```sh
curl -fsSLo ~/.local/bin/aigw-token \
  https://raw.githubusercontent.com/roiblum1/aigw-ui/main/clients/aigw-token/aigw-token
chmod +x ~/.local/bin/aigw-token
aigw-token login https://aigw.example.com   # paste your personal key when asked
```

Then in `~/.claude/settings.json`:

```json
{
  "apiKeyHelper": "aigw-token",
  "env": { "ANTHROPIC_BASE_URL": "https://<the gateway>" }
}
```

`aigw-token logout` forgets the key. Prompts go to the gateway; the hub is
only asked for tokens.

| | For | Shows |
|---|---|---|
| [`claude-code/`](claude-code) | Claude Code | The budget in the status line; a message when a budget is nearly spent, spent, or served as best-effort |
| [`omp/aigw-usage.ts`](omp/aigw-usage.ts) | Oh My Pi (`omp`) | The same in the status area, and `/aigw-usage` for all budgets |

```
GLM5.3 $47.00 of $50.00 (94%) · resets 18:00
```

Both ask the hub's `GET /api/v1/my/usage` with the tenant's own API key, the
same call the page `/my-usage` makes. They need `config.tenantPage` on, which
is the default. A model with prices is shown in dollars, any other in
tokens. No prompt and no answer is sent to the hub.

## What you need

- The address of the hub, for example `https://aigw.example.com`. It is not
  the gateway's address.
- Your key: the personal key `aigw-token` signed in with is found by itself,
  else a token or API key for the gateway.

## Claude Code

Needs `node` 18 or later on the machine.

```
/plugin marketplace add roiblum1/aigw-ui
/plugin install aigw-usage@aigw
```

Claude Code asks for three options when the plugin is enabled:

| Option | Empty means |
|---|---|
| Hub address | The environment variable `AIGW_HUB_URL` |
| API key | `AIGW_API_KEY`, then the key Claude Code already sends to the gateway (`ANTHROPIC_AUTH_TOKEN` or `ANTHROPIC_API_KEY`) |
| Warn at, percent | 90 |

Restart Claude Code once, then run `/aigw-usage:setup`. It adds the status
line to your `~/.claude/settings.json`. A plugin cannot do that by itself,
and the command asks before it replaces a status line you already have.

How it works:

- A hook asks the hub when a session starts, when you send a prompt and
  when an answer ends: at most every 30 seconds, and 5 seconds after an
  answer. It keeps the answer in `~/.claude/aigw-usage/usage.json`.
- The status line only reads that file. It has no key and makes no request.
- The key is never written to a file.
- A warning is shown once per budget, level and period: at the warning
  percent, when the budget is spent, when you are served as best-effort,
  and when you are refused.
- A hub that is down or a wrong key is asked again after one and five
  minutes, not on every prompt. The status line goes on showing the last
  figures, marked `old`.

## Oh My Pi

```sh
mkdir -p ~/.omp/agent/extensions
curl -fsSLo ~/.omp/agent/extensions/aigw-usage.ts \
  https://raw.githubusercontent.com/roiblum1/aigw-ui/main/clients/omp/aigw-usage.ts
export AIGW_HUB_URL=https://aigw.example.com
export AIGW_API_KEY=<your key>
```

`AIGW_WARN_AT` changes the warning percent. `/aigw-usage` lists every
budget.

## Tested

- Claude Code: the scripts, against a stand-in for the hub: the status
  line, each warning once, a hub that is down, a wrong key, no address. The
  manifests pass `claude plugin validate`. Not yet run inside a Claude Code
  session against a real hub.
- Oh My Pi: the extension was run with a stand-in for the agent's API, not
  inside `omp`. It was written against the extension API of version 18.8.9.

- aigw-token: against a stand-in for the hub: sign-in, a new token, the
  token kept, a hub that fails, a revoked key, no hub.

```sh
node --test clients/claude-code/scripts/ clients/aigw-token/
```
