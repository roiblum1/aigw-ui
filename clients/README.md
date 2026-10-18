# Usage in the coding agent

For the people who use the models: `aigw-token`, which signs Claude Code in
to the gateway with a personal key, and two small add-ons that show a
tenant's budget inside the coding agent and warn before it is spent.

## From the hub

Every file here is also in the hub's binary and served at
`<hub>/clients/` with no sign-in, so a machine with no internet installs
from the hub (since 0.33.0). Sign in on `<hub>/my-usage` with your personal
key; **Set up Claude Code** there shows the one line to run:

```sh
curl -fsSL https://aigw.example.com/clients/install.sh | sh -s -- https://aigw.example.com https://<the gateway>
```

[`install/install.sh`](install/install.sh) installs `aigw-token` in
`~/.local/bin` and signs it in (it asks for the personal key on the terminal,
or reads `AIGW_PERSONAL_KEY`), sets `apiKeyHelper`, `ANTHROPIC_BASE_URL` and
the status line in `~/.claude/settings.json` with `node` (everything else is
kept; the old file is kept as `settings.json.aigw-backup`), and installs the
`aigw-usage` plugin from `<hub>/clients/claude-code.tar.gz` with `claude
plugin marketplace add` and `claude plugin install`. Without `node` or
`claude` it says what to do by hand. Run it again after a hub upgrade to get
the new files; it keeps the sign-in.

| `<hub>/clients/` | Is |
|---|---|
| `install.sh` | The installer above |
| `aigw-token` | [`aigw-token/aigw-token`](aigw-token/aigw-token) |
| `claude-code.tar.gz` | [`claude-code/`](claude-code) as the marketplace `aigw` |
| `aigw-usage.ts` | [`omp/aigw-usage.ts`](omp/aigw-usage.ts) |

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

Both show the budget across models as **All models** next to the budgets
per model, and the one of them furthest gone in the status line.

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

Point `omp` at the gateway in `~/.omp/agent/models.yml`, with `aigw-token`
giving it its tokens (the `!` runs the command):

```yaml
providers:
  aigw:
    baseUrl: https://<the gateway>/v1
    api: openai-completions
    apiKey: "!aigw-token"          # the full path if it is not on omp's PATH
    models:
      - id: GLM5.3                 # the name the gateway routes on
        reasoning: true            # offers omp's thinking levels
        contextWindow: 200000
        maxTokens: 16384
        cost: { input: 0.6, output: 2.2, cacheRead: 0.11, cacheWrite: 0 }
  aigw-anthropic:                  # the gateway's Anthropic path, if wanted
    baseUrl: https://<the gateway>/anthropic
    api: anthropic-messages
    apiKey: "!aigw-token"
    models:
      - id: GLM5.3
        reasoning: true
```

`omp models find aigw` lists them. `omp` runs `aigw-token` once per session
and keeps the token, and runs it again after a 401, so a change of budget,
quota or models reaches you in your next session (in Claude Code, within
minutes).

The extension shows the budgets in the status area and `/aigw-usage` lists
them all:

```sh
mkdir -p ~/.omp/agent/extensions
curl -fsSLo ~/.omp/agent/extensions/aigw-usage.ts \
  https://raw.githubusercontent.com/roiblum1/aigw-ui/main/clients/omp/aigw-usage.ts
```

With `aigw-token` signed in there is nothing to set: it asks the hub with
the personal key `aigw-token` keeps. Without it, set `AIGW_HUB_URL` and
`AIGW_API_KEY`. `AIGW_WARN_AT` changes the warning percent.

**Thinking.** With `reasoning: true`, omp's thinking level goes to the model
as `reasoning_effort` on `/v1` (`--thinking off` sends `minimal`), and as
Anthropic's `thinking` on the Anthropic path. The gateway passes both on to
the model server unchanged; on the Anthropic path it does not turn
`thinking` into `reasoning_effort`, so whether the model thinks is up to the
model server (vLLM accepts both). The model's `reasoning_content` comes back
as thinking on both paths, and reasoning tokens are charged as output.

## Tested

- Claude Code: the scripts, against a stand-in for the hub: the status
  line, each warning once, a hub that is down, a wrong key, no address. The
  manifests pass `claude plugin validate`. Not yet run inside a Claude Code
  session against a real hub.
- Oh My Pi 18.11.1 against the sandbox hub and the lab gateway
  (2026-10-16): models from `models.yml` with `!aigw-token`, a real model
  answered and a spent budget refused (429, with omp's retry time); the
  extension inside `omp`'s terminal UI showed the budget across models in
  the status area, the spent warning, and `/aigw-usage`, signed in through
  `aigw-token` with nothing set. Thinking with a stand-in model server that
  answers as vLLM's reasoning parser does: `reasoning_effort` and
  `thinking` reached it, and its reasoning came back as omp's thoughts on
  both paths.

- aigw-token: against a stand-in for the hub: sign-in, a new token, the
  token kept, a hub that fails, a revoked key, no hub.
- install.sh: against a stand-in for the hub and for `claude`: everything
  installed and the settings merged, a second run that keeps the sign-in
  and updates the marketplace, no `node` and no `claude`, a wrong key. On
  2026-10-18 against a local 0.33.0 hub with Claude Code 2.1.292 in an
  empty config directory: the plugin installed and enabled from the hub's
  archive, `aigw-token` signed in and printing a token.

```sh
node --test clients/claude-code/scripts/ clients/aigw-token/ clients/install/
```
